//go:build integration

package haproxy_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

const (
	// 与部署的 dataplaneapi v3.4.3 一致。
	fixtureImage    = "haproxytech/haproxy-debian@sha256:6e919b0fe19314bd36a4f67f42743af5a6a01067dfd26f4502de0f88b2c445e4"
	fixtureUser     = "admin"
	fixturePassword = "AYATORI-TEST-ONLY"
	// 测试 VIP 绑定所有地址，以便从宿主机经发布端口连接。
	fixtureVIP = "0.0.0.0"
	uid        = "11111111-2222-3333-4444-555555555555"
	otherUID   = "99999999-2222-3333-4444-555555555555"
	passPort   = 7000
	closedPort = 7001
)

const haproxyConfig = `global
    master-worker
    stats socket /var/run/haproxy.sock mode 600 level admin expose-fd listeners
    hard-stop-after 30s

defaults
    mode tcp
    timeout connect 5s
    timeout client 1m
    timeout server 1m

userlist dataplaneapi
    user ` + fixtureUser + ` insecure-password ` + fixturePassword + `
`

const dataplaneConfig = `config_version: 2
name: fixture
dataplaneapi:
  host: 0.0.0.0
  port: 5555
  userlist:
    userlist: dataplaneapi
  transaction:
    transaction_dir: /tmp/haproxy-transactions
haproxy:
  config_file: /usr/local/etc/haproxy/haproxy.cfg
  haproxy_bin: /usr/local/sbin/haproxy
  master_worker_mode: true
  master_runtime: /var/run/haproxy-master.sock
  reload:
    reload_delay: 1
    reload_strategy: custom
    reload_cmd: /bin/sh -c 'kill -USR2 $(cat /var/run/haproxy.pid)'
    restart_cmd: /bin/sh -c 'kill -USR2 $(cat /var/run/haproxy.pid)'
`

// 生产由 systemd 分别运行 haproxy 与 dataplaneapi；fixture 在同一容器内模拟。
const startScript = `#!/bin/sh
set -e
cp /fixture/haproxy.cfg /usr/local/etc/haproxy/haproxy.cfg
cp /fixture/dataplaneapi.yml /usr/local/etc/haproxy/dataplaneapi.yml
haproxy -W -D -S /var/run/haproxy-master.sock -p /var/run/haproxy.pid -f /usr/local/etc/haproxy/haproxy.cfg
exec dataplaneapi -f /usr/local/etc/haproxy/dataplaneapi.yml
`

type fixture struct {
	id    string
	host  loadbalancer.Host
	ports map[int]string
}

// startFixture 只创建自己的临时容器并按确切 ID 清理。
func startFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"haproxy.cfg": haproxyConfig, "dataplaneapi.yml": dataplaneConfig, "start.sh": startScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	output, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-d", "--user", "root",
		"--sysctl", "net.ipv4.ip_nonlocal_bind=1", "-v", dir+":/fixture:ro", "--entrypoint", "/fixture/start.sh",
		"-p", "127.0.0.1::5555", "-p", fmt.Sprintf("127.0.0.1::%d", passPort), "-p", fmt.Sprintf("127.0.0.1::%d", closedPort),
		fixtureImage).Output()
	if err != nil {
		t.Fatalf("cannot start HAProxy fixture: %v", err)
	}
	id := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal("unexpected container identifier")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, "docker", "rm", "-f", id).Run() != nil {
			t.Error("fixture cleanup failed")
		}
	})
	f := fixture{id: id, ports: map[int]string{}}
	for _, port := range []int{5555, passPort, closedPort} {
		out, err := exec.CommandContext(t.Context(), "docker", "port", id, fmt.Sprint(port)).Output()
		if err != nil {
			t.Fatal(err)
		}
		f.ports[port] = strings.TrimSpace(strings.Split(string(out), "\n")[0])
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
			Endpoints: []loadbalancer.Endpoint{{Address: "127.0.0.1", Port: 5555}}},
		// 唯一 endpoint 不可达：没有健康后端时必须拒绝，不回退。
		{Port: closedPort, IdleTimeout: time.Hour, ProxyProtocol: loadbalancer.ProxyProtocolV2, HealthCheck: check(""),
			Endpoints: []loadbalancer.Endpoint{{Address: "127.0.0.1", Port: 1}, {Address: "pg.example.invalid", Port: 5432}}},
	}

	other := []loadbalancer.Port{{Port: 7100, IdleTimeout: time.Minute, HealthCheck: check("")}}
	if err := hosts.Apply(t.Context(), f.host, aggregate(otherUID, other)); err != nil {
		t.Fatal(err)
	}

	if facts := converged(t, hosts, f, uid, ports); facts.Present || facts.Matches {
		t.Fatalf("写入前不应存在配置: %+v", facts)
	}
	if err := hosts.Apply(t.Context(), f.host, aggregate(uid, ports)); err != nil {
		t.Fatal(err)
	}
	if facts := converged(t, hosts, f, uid, ports); !facts.Present || !facts.Matches || facts.Reload != loadbalancer.ReloadSucceeded {
		t.Fatalf("写入后回读应与期望一致: %+v", facts)
	}
	config := f.config(t)
	for _, directive := range []string{
		"option httpchk GET /v3/health", "http-check expect rstatus ^(401)$", "balance leastconn",
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

	t.Run("变更后检测漂移并收敛", func(t *testing.T) {
		changed := []loadbalancer.Port{ports[0]}
		changed[0].Endpoints = append(changed[0].Endpoints, loadbalancer.Endpoint{Address: "127.0.0.2", Port: 5555})
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
	})
}
