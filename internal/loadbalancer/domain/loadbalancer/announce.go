package loadbalancer

import (
	"net/netip"
	"slices"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
)

// Frontend 是运行中的 HAProxy 里一个 bind 的事实：绑定地址与是否处于 OPEN。
type Frontend struct {
	Address netip.Addr
	Open    bool
}

// Announcements 是宣告规则（ADR-0014），LB agent 与 controller 共用：返回绑定地址在 pool 内、
// 内核把它当作本机地址（local 报告），且绑定该地址的 frontend 全部处于 OPEN 的 VIP，按地址排序。
// HAProxy 不应答时调用方传入 nil，结果为空，即撤回全部。后端健康不是输入：后端全部失败时
// 仍宣告，由 HAProxy 拒绝连接。同一 VIP 的多个端口有一个不 OPEN 即不宣告：该主机只能服务
// 部分端口时把流量让给其他主机。不是本机地址的 VIP 不宣告：路由器送来的包会被内核丢弃。
func Announcements(pool address.Pool, frontends []Frontend, local func(netip.Addr) bool) []netip.Addr {
	open := map[netip.Addr]bool{}
	for _, frontend := range frontends {
		if !pool.Contains(frontend.Address) {
			continue
		}
		previous, seen := open[frontend.Address]
		open[frontend.Address] = frontend.Open && (!seen || previous)
	}
	var vips []netip.Addr
	for vip, ok := range open {
		if ok && local(vip) {
			vips = append(vips, vip)
		}
	}
	slices.SortFunc(vips, netip.Addr.Compare)
	return vips
}

// Bound 返回 frontends 中绑定地址在 pool 内的 VIP，不论状态，按地址排序：agent 让这些 VIP
// 成为本机地址。frontend 被禁用时地址保留，已有连接继续由本机服务。
func Bound(pool address.Pool, frontends []Frontend) []netip.Addr {
	var vips []netip.Addr
	for _, frontend := range frontends {
		if pool.Contains(frontend.Address) && !slices.Contains(vips, frontend.Address) {
			vips = append(vips, frontend.Address)
		}
	}
	slices.SortFunc(vips, netip.Addr.Compare)
	return vips
}

// announces 判断某台 Host 是否满足宣告该 VIP 的规则。
// ponytail: controller 看不到主机的内核状态，视 VIP 为本机地址；ADR-0015 改为读取 agent 报告的宣告。
func announces(vip netip.Addr, frontends []Frontend) bool {
	everywhere := func(netip.Addr) bool { return true }
	return slices.Contains(Announcements(address.Pool{Prefix: netip.PrefixFrom(vip, 32)}, frontends, everywhere), vip)
}
