//go:build integration

package haproxy_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.ddupan.top/ayatori/internal/loadbalancer/adapter/haproxy"
	"go.ddupan.top/ayatori/internal/loadbalancer/domain/address"
	"go.ddupan.top/ayatori/internal/loadbalancer/domain/loadbalancer"
	"go.ddupan.top/ayatori/internal/testenv"
)

const (
	fixtureUser     = "admin"
	fixturePassword = "AYATORI-TEST-ONLY"
	// 测试 VIP 绑定所有地址，以便从宿主机经发布端口连接。
	fixtureVIP = "0.0.0.0"
	uid        = "11111111-2222-3333-4444-555555555555"
	otherUID   = "99999999-2222-3333-4444-555555555555"
	passPort   = 7000
	// dataplaneapi 监听所有地址，两个 loopback 地址可作为两个不同的健康 endpoint。
	loopback       = "127.0.0.1"
	secondLoopback = "127.0.0.2"
	closedPort     = 7001
)

type fixture struct {
	id    string
	host  loadbalancer.Host
	ports map[int]string
}

// startFixture 连接 hack/integration-test.sh 在 go test 之外启动的 HAProxy + Data Plane API
// （testdata/fixture），并复位到刚启动的状态。只认脚本给出的容器 ID。
func startFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{ports: map[int]string{}}
	for _, port := range []int{5555, passPort, closedPort} {
		id, published := testenv.Container(t, "haproxy", port)
		f.id, f.ports[port] = id, net.JoinHostPort(loopback, strconv.Itoa(published))
	}
	if out, err := exec.CommandContext(t.Context(), "docker", "exec", f.id, "/fixture/reset.sh").CombinedOutput(); err != nil {
		t.Fatalf("cannot reset HAProxy fixture: %v: %s", err, out)
	}
	f.host = loadbalancer.Host{Name: "fixture", ProxyURL: "http://" + f.ports[5555]}
	waitFor(t, func() bool {
		response, err := http.Get(f.host.ProxyURL + "/v3/health")
		if err == nil {
			_ = response.Body.Close()
		}
		return err == nil
	})
	return f
}

func (f fixture) config(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", "exec", f.id, "cat", "/usr/local/etc/haproxy/haproxy.cfg").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatal("condition not met within 30s")
}

func check(path string) loadbalancer.HealthCheck {
	return loadbalancer.HealthCheck{Path: path, Interval: time.Second, Timeout: time.Second, HealthyThreshold: 1, UnhealthyThreshold: 1}
}

func aggregate(id string, ports []loadbalancer.Port) *loadbalancer.LoadBalancer {
	return loadbalancer.Reconstitute(loadbalancer.Snapshot{
		Owner: address.Owner{UID: id}, Address: netip.MustParseAddr(fixtureVIP), Ports: ports,
	})
}

func withStatuses(c loadbalancer.HealthCheck, statuses ...int32) loadbalancer.HealthCheck {
	c.ExpectedStatuses = statuses
	return c
}

// converged 等待代理完成 reload 并返回事实。
func converged(t *testing.T, hosts *haproxy.Hosts, f fixture, id string, ports []loadbalancer.Port) loadbalancer.ProxyFacts {
	t.Helper()
	var facts loadbalancer.ProxyFacts
	waitFor(t, func() bool {
		var err error
		facts, err = hosts.Observe(t.Context(), f.host, aggregate(id, ports))
		if err != nil {
			t.Fatal(err)
		}
		return facts.Reload != loadbalancer.ReloadPending
	})
	return facts
}

func TestHostsContract(t *testing.T) {
	f := startFixture(t)
	hosts := &haproxy.Hosts{HTTP: http.DefaultClient, Username: fixtureUser, Password: fixturePassword, ConnectTimeout: 5 * time.Second}
	ports := []loadbalancer.Port{
		// dataplaneapi 自身作为健康后端：未认证的 /v3/health 返回 401，以此验证自定义期望状态码。
		{Port: passPort, IdleTimeout: time.Hour, HealthCheck: withStatuses(check("/v3/health"), 401),
			Endpoints: []loadbalancer.Endpoint{{Address: loopback, Port: 5555}}},
		// 唯一 endpoint 不可达：没有健康后端时必须拒绝，不回退。
		{Port: closedPort, IdleTimeout: time.Hour, ProxyProtocol: loadbalancer.ProxyProtocolV2, HealthCheck: check(""),
			Endpoints: []loadbalancer.Endpoint{{Address: "127.0.0.1", Port: 1}, {Address: "pg.example.invalid", Port: 5432}}},
	}

	// 引用缺失 Backend 的端口：没有健康检查参数与 server，回读仍须一致。
	other := []loadbalancer.Port{{Port: 7100, IdleTimeout: time.Minute, Backend: "gone", MissingBackend: true}}
	if err := hosts.Apply(t.Context(), f.host, aggregate(otherUID, other)); err != nil {
		t.Fatal(err)
	}

	if facts := converged(t, hosts, f, uid, ports); facts.Present || facts.Matches {
		t.Fatalf("写入前不应存在配置: %+v", facts)
	}
	if err := hosts.Apply(t.Context(), f.host, aggregate(uid, ports)); err != nil {
		t.Fatal(err)
	}
	facts := converged(t, hosts, f, uid, ports)
	if !facts.Present || !facts.Matches || facts.Reload != loadbalancer.ReloadSucceeded {
		t.Fatalf("写入后回读应与期望一致: %+v", facts)
	}
	config := f.config(t)
	for _, directive := range []string{
		"option socket-stats", "option httpchk GET /v3/health", "http-check expect rstatus ^(401)$", "balance leastconn",
		"on-marked-down shutdown-sessions", "init-state fully-down", "send-proxy-v2", "check-send-proxy", "init-addr none",
	} {
		if !strings.Contains(config, directive) {
			t.Errorf("生成配置缺少 %q:\n%s", directive, config)
		}
	}

	t.Run("健康后端转发", func(t *testing.T) {
		waitFor(t, func() bool {
			response, err := http.Get("http://" + f.ports[passPort] + "/v3/health")
			if err != nil {
				return false
			}
			defer func() { _ = response.Body.Close() }()
			return response.StatusCode == http.StatusUnauthorized
		})
	})

	t.Run("没有健康后端时拒绝连接", func(t *testing.T) {
		connection, err := net.DialTimeout("tcp", f.ports[closedPort], 5*time.Second)
		if err != nil {
			return // 直接拒绝同样满足 fail-closed
		}
		defer func() { _ = connection.Close() }()
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := connection.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
			t.Fatalf("连接应被关闭而不是转发或挂起: %v", err)
		} else if err != io.EOF && !strings.Contains(err.Error(), "reset") {
			t.Logf("连接以 %v 结束", err)
		}
	})

	t.Run("Observe 报告各 server 的检查结论", func(t *testing.T) { reportsServerHealth(t, hosts, f, ports) })

	t.Run("配置已写入但进程未加载时重新 reload", func(t *testing.T) {
		// 运行中的进程换回不含 LB 的配置，文件再恢复后重启 dataplaneapi：文件是期望配置，
		// 进程未加载，也没有任何排期的 reload（与 dataplaneapi 重启丢失排期相同）。
		script := `set -e
cfg=/usr/local/etc/haproxy/haproxy.cfg
cp $cfg /tmp/full.cfg
cp /fixture/haproxy.cfg $cfg
kill -USR2 $(cat /var/run/haproxy.pid)
sleep 2
cp /tmp/full.cfg $cfg
pkill -x dataplaneapi`
		if out, err := exec.CommandContext(t.Context(), "docker", "exec", f.id, "sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		waitFor(t, func() bool {
			_, err := hosts.Observe(t.Context(), f.host, aggregate(uid, ports))
			return err == nil
		})
		if facts := converged(t, hosts, f, uid, ports); facts.Matches {
			t.Fatalf("进程未加载时不应判为一致: %+v", facts)
		}
		applyUntilMatches(t, hosts, f, aggregate(uid, ports))
		if facts := converged(t, hosts, f, uid, ports); !facts.Matches || facts.Reload != loadbalancer.ReloadSucceeded {
			t.Fatalf("reload 后运行中的进程应已加载: %+v", facts)
		}
	})

	t.Run("变更后检测漂移并收敛", func(t *testing.T) {
		changed := []loadbalancer.Port{ports[0]}
		changed[0].Endpoints = append(changed[0].Endpoints, loadbalancer.Endpoint{Address: secondLoopback, Port: 5555})
		if facts := converged(t, hosts, f, uid, changed); facts.Matches {
			t.Fatal("期望变化后不应判为一致")
		}
		if err := hosts.Apply(t.Context(), f.host, aggregate(uid, changed)); err != nil {
			t.Fatal(err)
		}
		if facts := converged(t, hosts, f, uid, changed); !facts.Matches {
			t.Fatalf("应收敛到新期望: %+v", facts)
		}
		if strings.Contains(f.config(t), fmt.Sprintf("lb_%s_%d", uid, closedPort)) {
			t.Fatal("移除的端口应删除其 section")
		}
	})

	t.Run("删除只影响自身", func(t *testing.T) {
		if err := hosts.Remove(t.Context(), f.host, aggregate(uid, nil)); err != nil {
			t.Fatal(err)
		}
		if facts := converged(t, hosts, f, uid, nil); facts.Present {
			t.Fatal("删除后不应残留配置")
		}
		if facts := converged(t, hosts, f, otherUID, other); !facts.Matches {
			t.Fatalf("其他 LoadBalancer 的配置不应受影响: %+v", facts)
		}
		if owners, err := hosts.Owners(t.Context(), f.host); err != nil || !slices.Equal(owners, []string{otherUID}) {
			t.Fatalf("Owners() = %v, %v", owners, err)
		}
	})

	t.Run("Probe 以当前凭据确认应答", func(t *testing.T) { probes(t, hosts, f) })
	t.Run("Purge 删除全部 lb_ section 且等进程不再服务", func(t *testing.T) { purges(t, hosts, f) })
}

func probes(t *testing.T, hosts *haproxy.Hosts, f fixture) {
	if err := hosts.Probe(t.Context(), f.host); err != nil {
		t.Fatal(err)
	}
	wrong := &haproxy.Hosts{HTTP: http.DefaultClient, Username: fixtureUser, Password: "wrong"}
	if err := wrong.Probe(t.Context(), f.host); err == nil {
		t.Fatal("凭据错误时应报告不可达")
	}
}

// purges 在 otherUID 的 LoadBalancer 仍在时调用：主机删除时不区分归属。
func purges(t *testing.T, hosts *haproxy.Hosts, f fixture) {
	waitFor(t, func() bool {
		done, err := hosts.Purge(t.Context(), f.host)
		if err != nil {
			t.Fatal(err)
		}
		return done
	})
	config := f.config(t)
	if strings.Contains(config, "lb_") || !strings.Contains(config, "userlist dataplaneapi") {
		t.Fatalf("应只删除 lb_ section:\n%s", config)
	}
	if done, err := hosts.Purge(t.Context(), f.host); err != nil || !done {
		t.Fatalf("清理后再次调用应直接完成: %v %v", done, err)
	}
}

func reloadCount(t *testing.T, f fixture) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.host.ProxyURL+"/v3/services/haproxy/reloads", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(fixtureUser, fixturePassword)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(body), `"id"`)
}

// applyUntilMatches 模拟 Reconciler 逐轮 Apply，直到回读一致。
func applyUntilMatches(t *testing.T, hosts *haproxy.Hosts, f fixture, lb *loadbalancer.LoadBalancer) {
	t.Helper()
	waitFor(t, func() bool {
		facts, err := hosts.Observe(t.Context(), f.host, lb)
		if err != nil {
			t.Fatal(err)
		}
		if facts.Matches && facts.Reload == loadbalancer.ReloadSucceeded {
			return true
		}
		if facts.Reload != loadbalancer.ReloadPending {
			if err := hosts.Apply(t.Context(), f.host, lb); err != nil {
				t.Fatal(err)
			}
		}
		return false
	})
}

func TestHostsRuntimeEndpoints(t *testing.T) {
	f := startFixture(t)
	now := time.Now()
	hosts := &haproxy.Hosts{HTTP: http.DefaultClient, Username: fixtureUser, Password: fixturePassword,
		ConnectTimeout: 5 * time.Second, Now: func() time.Time { return now }}
	port := loadbalancer.Port{Port: passPort, IdleTimeout: time.Hour, DrainTimeout: time.Minute,
		HealthCheck: withStatuses(check("/v3/health"), 401),
		Endpoints:   []loadbalancer.Endpoint{{Address: loopback, Port: 5555}}}
	applyUntilMatches(t, hosts, f, aggregate(uid, []loadbalancer.Port{port}))
	reloads := reloadCount(t, f)

	t.Run("新增 endpoint 不 reload 且被启用", func(t *testing.T) {
		port.Endpoints = append(port.Endpoints, loadbalancer.Endpoint{Address: secondLoopback, Port: 5555})
		applyUntilMatches(t, hosts, f, aggregate(uid, []loadbalancer.Port{port}))
		if got := reloadCount(t, f); got != reloads {
			t.Fatalf("reload 次数 %d → %d", reloads, got)
		}
	})

	t.Run("无会话的 endpoint 排空后删除且不 reload", func(t *testing.T) {
		port.Endpoints = port.Endpoints[:1]
		applyUntilMatches(t, hosts, f, aggregate(uid, []loadbalancer.Port{port}))
		if got := reloadCount(t, f); got != reloads {
			t.Fatalf("reload 次数 %d → %d", reloads, got)
		}
	})

	t.Run("有会话时在 drainTimeout 内保留", func(t *testing.T) {
		port.Endpoints = []loadbalancer.Endpoint{{Address: secondLoopback, Port: 5555}}
		applyUntilMatches(t, hosts, f, aggregate(uid, []loadbalancer.Port{port}))
		// 经 VIP 建立到唯一 endpoint 的会话，只发送半个请求使其保持。
		connection, err := net.DialTimeout("tcp", f.ports[passPort], 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = connection.Close() }()
		if _, err := connection.Write([]byte("GET /v3/health HTTP/1.1\r\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)

		port.Endpoints = []loadbalancer.Endpoint{{Address: loopback, Port: 5555}}
		removed := aggregate(uid, []loadbalancer.Port{port})
		for range 3 {
			if err := hosts.Apply(t.Context(), f.host, removed); err != nil {
				t.Fatal(err)
			}
		}
		if facts, err := hosts.Observe(t.Context(), f.host, removed); err != nil || facts.Matches {
			t.Fatalf("仍有会话时不应删除排空中的 endpoint: %+v %v", facts, err)
		}
		now = now.Add(2 * time.Minute)
		applyUntilMatches(t, hosts, f, removed)
	})
}

// reportsServerHealth 验证 Observe 从真实 stats 解析出各 server 的检查结论（ADR-0023）。
func reportsServerHealth(t *testing.T, hosts *haproxy.Hosts, f fixture, ports []loadbalancer.Port) {
	t.Helper()
	pass := loadbalancer.Endpoint{Address: loopback, Port: 5555}
	closed := loadbalancer.Endpoint{Address: "127.0.0.1", Port: 1}
	var servers []loadbalancer.ServerHealth
	waitFor(t, func() bool {
		facts, err := hosts.Observe(t.Context(), f.host, aggregate(uid, ports))
		if err != nil {
			t.Fatal(err)
		}
		servers = facts.Servers
		return slices.Contains(servers, loadbalancer.ServerHealth{Port: passPort, Endpoint: pass, State: "UP", CheckStatus: "L7OK", CheckCode: 401}) &&
			slices.ContainsFunc(servers, func(s loadbalancer.ServerHealth) bool {
				return s.Port == closedPort && s.Endpoint == closed && s.State == "DOWN" && s.CheckStatus == "L4CON"
			})
	})
	// 其他 LoadBalancer 的 server 不在结果中。
	if slices.ContainsFunc(servers, func(s loadbalancer.ServerHealth) bool { return s.Port == 7100 }) {
		t.Fatalf("got %+v", servers)
	}
}
