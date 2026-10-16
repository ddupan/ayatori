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

// DummyInterface 是 agent 独占的 dummy 接口：其上的 /32 使内核把路由器送来的 VIP 流量交给
// HAProxy（ip_nonlocal_bind 只允许 bind，不会让内核接收这些包）。接口上的地址全部由 agent 管理，
// 不属于期望集合的地址会被删除。agent 退出时地址保留：会话断开后路由器不再送来流量，下次启动
// 首轮即按 HAProxy 现状收敛。
type DummyInterface struct {
	Name string
}

func (d DummyInterface) Sync(want []netip.Addr) error {
	link, err := d.link()
	if err != nil {
		return err
	}
	have, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addresses on %s: %w", d.Name, err)
	}
	wanted := map[netip.Addr]bool{}
	for _, vip := range want {
		wanted[vip] = true
	}
	present := map[netip.Addr]bool{}
	var errs []error
	for _, a := range have {
		addr, _ := netip.AddrFromSlice(a.IP.To4())
		if wanted[addr] {
			present[addr] = true
			continue
		}
		if err := netlink.AddrDel(link, &a); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", addr, err))
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

// link 返回已启用的接口，不存在时创建。
func (d DummyInterface) link() (netlink.Link, error) {
	link, err := netlink.LinkByName(d.Name)
	if _, missing := errors.AsType[netlink.LinkNotFoundError](err); missing {
		if err := netlink.LinkAdd(&netlink.Dummy{Name: d.Name}); err != nil {
			return nil, fmt.Errorf("create %s: %w", d.Name, err)
		}
		link, err = netlink.LinkByName(d.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", d.Name, err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, fmt.Errorf("bring up %s: %w", d.Name, err)
		}
	}
	return link, nil
}

// Local 查询内核路由而不是回读接口地址：这才是路由器送来的包会不会交给本机的事实。
func (d DummyInterface) Local(vip netip.Addr) bool {
	routes, err := netlink.RouteGet(vip.AsSlice())
	return err == nil && len(routes) > 0 && routes[0].Type == unix.RTN_LOCAL
}
