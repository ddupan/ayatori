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
// 且绑定该地址的 frontend 全部处于 OPEN 的 VIP，按地址排序。HAProxy 不应答时调用方传入 nil，
// 结果为空，即撤回全部。后端健康不是输入：后端全部失败时仍宣告，由 HAProxy 拒绝连接。
// 同一 VIP 的多个端口有一个不 OPEN 即不宣告：该主机只能服务部分端口时把流量让给其他主机。
func Announcements(pool address.Pool, frontends []Frontend) []netip.Addr {
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
		if ok {
			vips = append(vips, vip)
		}
	}
	slices.SortFunc(vips, netip.Addr.Compare)
	return vips
}

// announces 判断某台 Host 是否满足宣告该 VIP 的规则。
func announces(vip netip.Addr, frontends []Frontend) bool {
	return slices.Contains(Announcements(address.Pool{Prefix: netip.PrefixFrom(vip, 32)}, frontends), vip)
}
