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

// Reason 是 VIP 未宣告的原因，对应 ADR-0015 三层自检；空表示应宣告。
type Reason string

const (
	// NotLocal：内核不把该 VIP 当作本机地址，路由器送来的包会被丢弃（收）。
	NotLocal Reason = "not-local"
	// FrontendNotOpen：绑定该 VIP 的 frontend 或 bind 有一个不是 OPEN（处理）。
	FrontendNotOpen Reason = "frontend-not-open"
	// NotAdvertised：本机应宣告，但路径没有出现在任何已建立会话的 adj-rib-out 中（宣告）。
	// 它是结果而不是输入，只由 agent 观察后报告，Check 不返回它。
	NotAdvertised Reason = "not-advertised"
)

// Check 是宣告规则（ADR-0014、ADR-0015），返回 Bound 中每个 VIP 不应宣告的原因，按层的顺序取
// 第一个不满足的层。HAProxy 不应答时调用方传入 nil，结果为空，即撤回全部。后端健康不是输入：
// 后端全部失败时仍宣告，由 HAProxy 拒绝连接。同一 VIP 的多个端口有一个不 OPEN 即不宣告：该主机
// 只能服务部分端口时把流量让给其他主机。
func Check(pool address.Pool, frontends []Frontend, local func(netip.Addr) bool) map[netip.Addr]Reason {
	open := map[netip.Addr]bool{}
	for _, frontend := range frontends {
		if !pool.Contains(frontend.Address) {
			continue
		}
		previous, seen := open[frontend.Address]
		open[frontend.Address] = frontend.Open && (!seen || previous)
	}
	reasons := make(map[netip.Addr]Reason, len(open))
	for vip, ok := range open {
		switch {
		case !local(vip):
			reasons[vip] = NotLocal
		case !ok:
			reasons[vip] = FrontendNotOpen
		default:
			reasons[vip] = ""
		}
	}
	return reasons
}

// Announcements 返回 Check 判定应宣告的 VIP，按地址排序。
func Announcements(pool address.Pool, frontends []Frontend, local func(netip.Addr) bool) []netip.Addr {
	var vips []netip.Addr
	for vip, reason := range Check(pool, frontends, local) {
		if reason == "" {
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
