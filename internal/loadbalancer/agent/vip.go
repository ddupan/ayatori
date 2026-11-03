package agent

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// VIPs 让 VIP 成为本机地址并报告内核是否把它交给本机。
type VIPs interface {
	// Sync 使本机持有的 VIP 恰为 want。
	Sync(want []netip.Addr) error
	// Local 表示内核把发往该地址的包交给本机（路由类型为 local）。
	Local(netip.Addr) bool
}

// loopbackDefault 是 lo 上唯一不归 agent 管理的 IPv4 地址。
var loopbackDefault = netip.MustParsePrefix("127.0.0.1/8")

// Loopback 把 VIP 以 /32 加到 lo 上，使内核把路由器送来的 VIP 流量交给 HAProxy
// （ip_nonlocal_bind 只允许 bind，不会让内核接收这些包）。用 lo 而不是 dummy 接口：LB 主机是
// unprivileged LXC，不能加载 dummy 模块。
//
// 部署契约：lo 上除 127.0.0.1/8 外的 IPv4 地址都归 agent，不属于期望集合的会被删除；IPv6 不动。
// agent 退出时地址保留：会话断开后路由器不再送来流量，下次启动首轮即按 HAProxy 现状收敛。
type Loopback struct{}

func (Loopback) Sync(want []netip.Addr) error {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find lo: %w", err)
	}
	have, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addresses on lo: %w", err)
	}
	wanted := map[netip.Addr]bool{}
	for _, vip := range want {
		wanted[vip] = true
	}
	present := map[netip.Addr]bool{}
	var errs []error
	for _, a := range have {
		prefix, ok := netipx(a.IPNet)
		switch {
		case !ok || prefix == loopbackDefault:
			continue
		case prefix.Bits() == 32 && wanted[prefix.Addr()]:
			present[prefix.Addr()] = true
		default:
			if err := netlink.AddrDel(link, &a); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", prefix, err))
			}
		}
	}
	for _, vip := range want {
		if present[vip] {
			continue
		}
		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: &net.IPNet{IP: vip.AsSlice(), Mask: net.CIDRMask(32, 32)}}); err != nil {
			errs = append(errs, fmt.Errorf("add %s: %w", vip, err))
		}
	}
	return errors.Join(errs...)
}

// Local 查询内核路由而不是回读接口地址：这才是路由器送来的包会不会交给本机的事实。
func (Loopback) Local(vip netip.Addr) bool {
	routes, err := netlink.RouteGet(vip.AsSlice())
	return err == nil && len(routes) > 0 && routes[0].Type == unix.RTN_LOCAL
}

func netipx(n *net.IPNet) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(n.IP.To4())
	if !ok {
		return netip.Prefix{}, false
	}
	bits, _ := n.Mask.Size()
	return netip.PrefixFrom(addr, bits), true
}
