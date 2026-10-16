//go:build integration

package agent_test

import (
	"net/netip"
	"os"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/agent"
)

// TestDummyInterface 需要 CAP_NET_ADMIN 与独立的网络 namespace，由 make test-lb-agent-netlink
// 在容器中运行；直接在宿主机上运行会修改宿主机网络，因此默认跳过。
func TestDummyInterface(t *testing.T) {
	if os.Getenv("AYATORI_NETLINK_TEST") != "1" {
		t.Skip("set AYATORI_NETLINK_TEST=1 inside a disposable network namespace")
	}
	vips := agent.DummyInterface{Name: "ayatori-vip"}
	a, b, c := netip.MustParseAddr("10.62.0.5"), netip.MustParseAddr("10.62.0.6"), netip.MustParseAddr("10.62.0.7")
	if vips.Local(a) {
		t.Fatal("尚未添加时不应是本机地址")
	}
	if err := vips.Sync([]netip.Addr{a, b}); err != nil {
		t.Fatal(err)
	}
	if !vips.Local(a) || !vips.Local(b) || vips.Local(c) {
		t.Fatal("接口创建并启用后，只有期望的 VIP 是本机地址")
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
