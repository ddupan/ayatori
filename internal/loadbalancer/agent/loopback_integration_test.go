//go:build integration

package agent_test

import (
	"net"
	"net/netip"
	"os"
	"testing"

	"github.com/vishvananda/netlink"

	"go.ddupan.top/ayatori/internal/loadbalancer/agent"
)

// TestLoopback 修改 lo，需要 CAP_NET_ADMIN 与独立的网络 namespace，由 make test-lb-agent-netlink
// 在容器中运行；直接在宿主机上运行会删除宿主机 lo 上的地址，因此默认跳过。
func TestLoopback(t *testing.T) {
	if os.Getenv("AYATORI_NETLINK_TEST") != "1" {
		t.Skip("set AYATORI_NETLINK_TEST=1 inside a disposable network namespace")
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	// 他人留下的地址（包括 VIP 段外的）也归 agent，应被删除。
	stray := &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP("192.0.2.9").To4(), Mask: net.CIDRMask(24, 32)}}
	if err := netlink.AddrAdd(lo, stray); err != nil {
		t.Fatal(err)
	}
	vips := agent.Loopback{}
	a, b, c := netip.MustParseAddr("10.62.0.5"), netip.MustParseAddr("10.62.0.6"), netip.MustParseAddr("10.62.0.7")
	if vips.Local(a) {
		t.Fatal("尚未添加时不应是本机地址")
	}
	if err := vips.Sync([]netip.Addr{a, b}); err != nil {
		t.Fatal(err)
	}
	if !vips.Local(a) || !vips.Local(b) || vips.Local(c) {
		t.Fatal("只有期望的 VIP 是本机地址")
	}
	if vips.Local(netip.MustParseAddr("192.0.2.9")) {
		t.Fatal("lo 上的其他地址应被删除")
	}
	if !vips.Local(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("127.0.0.1/8 不归 agent，必须保留")
	}
	if err := vips.Sync([]netip.Addr{a, b}); err != nil {
		t.Fatal("重复 Sync 应幂等：", err)
	}
	if err := vips.Sync([]netip.Addr{b}); err != nil {
		t.Fatal(err)
	}
	if vips.Local(a) || !vips.Local(b) {
		t.Fatal("不在期望中的地址应被删除")
	}
}
