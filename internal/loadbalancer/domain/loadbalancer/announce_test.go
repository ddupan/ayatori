package loadbalancer

import (
	"maps"
	"net/netip"
	"slices"
	"testing"

	"go.ddupan.top/ayatori/internal/loadbalancer/domain/address"
)

func TestAnnouncements(t *testing.T) {
	pool := address.Pool{Prefix: netip.MustParsePrefix("10.62.0.0/24")}
	a, b, c := netip.MustParseAddr("10.62.0.5"), netip.MustParseAddr("10.62.0.6"), netip.MustParseAddr("10.62.0.7")
	cases := []struct {
		name      string
		frontends []Frontend
		want      []netip.Addr
	}{
		{"HAProxy 不应答时全部撤回", nil, nil},
		{"OPEN 即宣告", []Frontend{{Address: a, Open: true}}, []netip.Addr{a}},
		{"未 OPEN 不宣告", []Frontend{{Address: a, Open: false}}, nil},
		{"同一 VIP 任一端口未 OPEN 即不宣告", []Frontend{{Address: a, Open: true}, {Address: a, Open: false}, {Address: a, Open: true}}, nil},
		{"VIP 之间相互独立且按地址排序", []Frontend{{Address: c, Open: true}, {Address: b, Open: false}, {Address: a, Open: true}}, []netip.Addr{a, c}},
		{"VIP 段外的 bind 不宣告", []Frontend{{Address: netip.MustParseAddr("192.168.10.40"), Open: true}, {Address: netip.IPv4Unspecified(), Open: true}}, nil},
	}
	everywhere := func(netip.Addr) bool { return true }
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Announcements(pool, c.frontends, everywhere); !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}

	t.Run("不是本机地址的 VIP 不宣告", func(t *testing.T) {
		frontends := []Frontend{{Address: a, Open: true}, {Address: b, Open: true}}
		onlyB := func(addr netip.Addr) bool { return addr == b }
		if got := Announcements(pool, frontends, onlyB); !slices.Equal(got, []netip.Addr{b}) {
			t.Fatalf("got %v", got)
		}
	})
}

func TestCheckReasons(t *testing.T) {
	pool := address.Pool{Prefix: netip.MustParsePrefix("10.62.0.0/24")}
	a, b, c := netip.MustParseAddr("10.62.0.5"), netip.MustParseAddr("10.62.0.6"), netip.MustParseAddr("10.62.0.7")
	frontends := []Frontend{{Address: a, Open: true}, {Address: b, Open: false}, {Address: c, Open: false},
		{Address: netip.MustParseAddr("192.168.10.40"), Open: true}}
	local := func(addr netip.Addr) bool { return addr != c }
	got := Check(pool, frontends, local)
	want := map[netip.Addr]Reason{a: "", b: FrontendNotOpen, c: NotLocal}
	if !maps.Equal(got, want) {
		t.Fatalf("每个 VIP 报告第一个不满足的层（收先于处理），VIP 段外不计；got %v", got)
	}
}

func TestBound(t *testing.T) {
	pool := address.Pool{Prefix: netip.MustParsePrefix("10.62.0.0/24")}
	a, b := netip.MustParseAddr("10.62.0.5"), netip.MustParseAddr("10.62.0.6")
	frontends := []Frontend{{Address: b, Open: false}, {Address: a, Open: true}, {Address: a, Open: true},
		{Address: netip.MustParseAddr("192.168.10.40"), Open: true}}
	if got := Bound(pool, frontends); !slices.Equal(got, []netip.Addr{a, b}) {
		t.Fatalf("禁用的 frontend 也保留地址、VIP 段外不计、去重排序，got %v", got)
	}
}
