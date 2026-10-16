//go:build integration

package agent_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/agent"
)

const (
	// 与 HAProxy adapter 的集成测试同一镜像。
	fixtureImage = "haproxytech/haproxy-debian@sha256:6e919b0fe19314bd36a4f67f42743af5a6a01067dfd26f4502de0f88b2c445e4"
	// socket 以相对路径访问：t.TempDir 的绝对路径可能超过 unix socket 的长度上限。
	socket   = "admin.sock"
	loopback = "127.0.0.1"
)

var (
	vip     = netip.MustParseAddr("10.62.0.5")
	another = netip.MustParseAddr("10.62.0.6")
)

// VIP 10.62.0.5 有两个端口；唯一后端不可达，所以全部后端都失败。10.62.0.6 只用于验证 VIP 互不影响。
// 192.168.10.40 在 VIP 段外，不得宣告。
const haproxyConfig = `global
    stats socket /run/haproxy/admin.sock mode 666 level admin

defaults
    mode tcp
    timeout connect 5s
    timeout client 1m
    timeout server 1m

frontend lb_a_7000
    bind 10.62.0.5:7000 name vip
    option socket-stats
    default_backend dead

frontend lb_a_7001
    bind 10.62.0.5:7001 name vip
    option socket-stats
    default_backend dead

frontend lb_b_7000
    bind 10.62.0.6:7000 name vip
    option socket-stats
    default_backend dead

frontend outside
    bind 192.168.10.40:7000 name vip
    option socket-stats
    default_backend dead

backend dead
    server s1 127.0.0.1:1 check inter 200ms fall 1
`

// startHAProxy 只创建自己的临时容器并按确切 ID 清理；返回停止它的函数。
func startHAProxy(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "haproxy.cfg"), []byte(haproxyConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(run, 0o777); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-d", "--user", "root",
		"--sysctl", "net.ipv4.ip_nonlocal_bind=1", "-v", dir+"/haproxy.cfg:/cfg/haproxy.cfg:ro", "-v", run+":/run/haproxy",
		"--entrypoint", "haproxy", fixtureImage, "-f", "/cfg/haproxy.cfg").Output()
	if err != nil {
		t.Fatalf("cannot start HAProxy fixture: %v", err)
	}
	id := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal("unexpected container identifier")
	}
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", id).Run()
	}
	t.Cleanup(stop)
	t.Chdir(run)
	waitFor(t, "HAProxy 应答", func() bool { return command(socket, "show info") == nil })
	return stop
}

func command(path, line string) error {
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if _, err := io.WriteString(connection, line+"\n"); err != nil {
		return err
	}
	_, err = io.ReadAll(connection)
	return err
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", loopback+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// startRouter 在进程内运行作为对端的 GoBGP（模拟 VyOS：被动，接受全部路径）。
func startRouter(t *testing.T, port int) *server.BgpServer {
	t.Helper()
	router := server.NewBgpServer()
	go router.Serve()
	t.Cleanup(router.Stop)
	if err := router.StartBgp(t.Context(), &api.StartBgpRequest{Global: &api.Global{
		Asn: 65002, RouterId: "10.60.0.1", ListenPort: int32(port), ListenAddresses: []string{loopback},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := router.AddPeer(t.Context(), &api.AddPeerRequest{Peer: &api.Peer{
		Conf: &api.PeerConf{NeighborAddress: loopback, PeerAsn: 65100}, Transport: &api.Transport{PassiveMode: true},
	}}); err != nil {
		t.Fatal(err)
	}
	return router
}

// received 返回对端从会话收到的 /32。
func received(t *testing.T, router *server.BgpServer) map[netip.Addr]bool {
	t.Helper()
	got := map[netip.Addr]bool{}
	if err := router.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: bgp.RF_IPv4_UC},
		func(nlri bgp.NLRI, paths []*apiutil.Path) {
			prefix := netip.MustParsePrefix(nlri.String())
			for _, p := range paths {
				if p.PeerAddress.IsValid() {
					got[prefix.Addr()] = true
				}
			}
		}); err != nil {
		t.Fatal(err)
	}
	return got
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatalf("%s：30 秒内未满足", what)
}

func receives(t *testing.T, router *server.BgpServer, want ...netip.Addr) func() bool {
	return func() bool {
		got := received(t, router)
		if len(got) != len(want) {
			return false
		}
		for _, addr := range want {
			if !got[addr] {
				return false
			}
		}
		return true
	}
}

// fakeVIPs 代替 lo（修改它需要 CAP_NET_ADMIN，由 TestLoopback 在容器中验证）。
// refused 中的地址无法成为本机地址。
type fakeVIPs struct {
	mu      sync.Mutex
	held    map[netip.Addr]bool
	refused map[netip.Addr]bool
}

func newFakeVIPs() *fakeVIPs {
	return &fakeVIPs{held: map[netip.Addr]bool{}, refused: map[netip.Addr]bool{}}
}

func (f *fakeVIPs) Sync(want []netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = map[netip.Addr]bool{}
	for _, vip := range want {
		f.held[vip] = !f.refused[vip]
	}
	return nil
}

func (f *fakeVIPs) Local(vip netip.Addr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[vip]
}

func (f *fakeVIPs) refuse(vip netip.Addr, refused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused[vip] = refused
}

func startAgent(t *testing.T, port int, vips agent.VIPs) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	a, err := agent.New(ctx, agent.Config{
		ASN: 65100, RouterID: "10.60.0.30", VIPPool: "10.62.0.0/24", HAProxySocket: socket,
		Neighbors: []agent.Neighbor{{Address: loopback, ASN: 65002, Port: uint32(port)}},
		Interval:  metav1.Duration{Duration: 200 * time.Millisecond},
	}, vips, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func TestAgentAnnouncesFromHAProxy(t *testing.T) {
	stopHAProxy := startHAProxy(t)
	port := freePort(t)
	router := startRouter(t, port)
	vips := newFakeVIPs()
	stopAgent := startAgent(t, port, vips)

	waitFor(t, "后端全部失败时仍宣告 VIP 段内的全部 VIP", receives(t, router, vip, another))

	t.Run("frontend 禁用后撤回、恢复后重新宣告", func(t *testing.T) {
		if err := command(socket, "disable frontend lb_a_7001"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "同一 VIP 任一端口禁用即撤回，其他 VIP 不受影响", receives(t, router, another))
		if err := command(socket, "enable frontend lb_a_7001"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "恢复后重新宣告", receives(t, router, vip, another))
	})

	t.Run("VIP 不是本机地址时不宣告", func(t *testing.T) {
		vips.refuse(another, true)
		waitFor(t, "无法接收流量的 VIP 撤回", receives(t, router, vip))
		vips.refuse(another, false)
		waitFor(t, "恢复后重新宣告", receives(t, router, vip, another))
	})

	t.Run("agent 重启后首轮即补回", func(t *testing.T) {
		stopAgent()
		waitFor(t, "agent 停止后对端撤回全部路径", receives(t, router))
		stopAgent = startAgent(t, port, vips)
		waitFor(t, "重启后补回", receives(t, router, vip, another))
	})

	t.Run("HAProxy 停止后撤回全部", func(t *testing.T) {
		stopHAProxy()
		waitFor(t, "HAProxy 不应答时撤回全部", receives(t, router))
	})
}
