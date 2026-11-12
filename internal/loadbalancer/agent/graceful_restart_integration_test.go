//go:build integration

package agent_test

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/server"
)

// TestAgentGracefulRestart 验证 ADR-0025：路由器只在会话未经 NOTIFICATION 断开时保留本机路径。
// 会话经代理建立；先切断代理再停 agent 即模拟 SIGKILL：Stop 发出的 NOTIFICATION 到不了路由器。
func TestAgentGracefulRestart(t *testing.T) {
	startHAProxy(t)
	routerPort := freePort(t)
	router := startRouter(t, routerPort)
	proxy := startAPIProxy(t, fmt.Sprintf("tcp://%s:%d", loopback, routerPort))
	port := int(netip.MustParseAddrPort(proxy.addr).Port())
	vips := newFakeVIPs()
	top := t
	_, stopAgent := startAgent(t, port, vips, time.Minute)
	waitFor(t, "宣告", fresh(t, router, vip, another))

	t.Run("被杀后保留路径，重启后替换且不撤回", func(t *testing.T) {
		withdrawn := withdrawals(t, router)
		proxy.cut()
		stopAgent()
		waitFor(t, "会话断开", down(t, router))
		if got := received(t, router); !got[vip] || !got[another] {
			t.Fatalf("会话断开后应保留两条 stale 路径，got %v", got)
		}
		proxy.restore(t)
		// 用外层 t：subtest 的 Cleanup 会在 subtest 结束时停掉 agent。
		_, stopAgent = startAgent(top, port, vips, time.Minute)
		waitFor(t, "重连后以新路径替换 stale 路径", fresh(t, router, vip, another))
		if got := withdrawn(); len(got) > 0 {
			t.Fatalf("重启期间路由器不应撤回任何路径，got %v", got)
		}
	})

	t.Run("Stop 发出 NOTIFICATION 后立即撤回", func(t *testing.T) {
		withdrawn := withdrawals(t, router)
		stopAgent()
		// restart time 为 1 分钟，大于 waitFor 的 30 秒：路由器若进入保留，这里会超时。
		waitFor(t, "撤回全部路径", receives(t, router))
		if len(withdrawn()) == 0 {
			t.Fatal("撤回应出现在 best path 事件中，否则上一个 subtest 的检查无效")
		}
	})

	t.Run("agent 不回来时 restart time 到期后撤回", func(t *testing.T) {
		_, stopAgent = startAgent(top, port, vips, 3*time.Second)
		waitFor(t, "宣告", fresh(t, router, vip, another))
		proxy.cut()
		stopAgent()
		waitFor(t, "会话断开", down(t, router))
		if got := received(t, router); !got[vip] || !got[another] {
			t.Fatalf("restart time 内应保留 stale 路径，got %v", got)
		}
		waitFor(t, "restart time 到期后撤回", receives(t, router))
	})
}

// fresh 在对端恰好收到 want 且都不是 stale 时满足。
func fresh(t *testing.T, router *server.BgpServer, want ...netip.Addr) func() bool {
	return func() bool {
		if !receives(t, router, want...)() {
			return false
		}
		for _, stale := range received(t, router) {
			if stale {
				return false
			}
		}
		return true
	}
}

func down(t *testing.T, router *server.BgpServer) func() bool {
	return func() bool {
		up := false
		if err := router.ListPeer(t.Context(), &api.ListPeerRequest{}, func(peer *api.Peer) {
			up = peer.GetState().GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED
		}); err != nil {
			t.Fatal(err)
		}
		return !up
	}
}

// withdrawals 记录此后对端 best path 的撤回，返回读取已记录前缀的函数。
func withdrawals(t *testing.T, router *server.BgpServer) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := router.WatchEvent(ctx, server.WatchEventMessageCallbacks{OnBestPath: func(paths []*apiutil.Path, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range paths {
			if p.Withdrawal {
				got = append(got, p.Nlri.String())
			}
		}
	}}, server.WatchBestPath(false)); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}
