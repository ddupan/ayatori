package loadbalancer

import (
	"net/netip"
	"slices"
	"time"
)

// AgentStatus 是 LB agent 在 GET /status 上报告的本机事实（ADR-0018），controller 只读取、不推算。
type AgentStatus struct {
	HAProxyUp bool             `json:"haproxyUp"`
	Neighbors []NeighborStatus `json:"neighbors"`
	// LastSync 是最近一次成功收敛的时间；从未成功时为空。
	LastSync *time.Time `json:"lastSync,omitempty"`
	// VIPs 是 HAProxy 中绑定在 VIP 段内的全部地址；HAProxy 不应答时为空。
	VIPs []VIPStatus `json:"vips"`
}

// Ready 判断 agent 是否在正常收发：HAProxy 应答、至少一个 BGP 会话已建立、最近 staleAfter 内成功
// 收敛过。不满足时返回原因。
func (s AgentStatus) Ready(now time.Time, staleAfter time.Duration) (bool, string) {
	switch {
	case !s.HAProxyUp:
		return false, "HAProxy 不应答"
	case !slices.ContainsFunc(s.Neighbors, func(n NeighborStatus) bool { return n.Established }):
		return false, "没有已建立的 BGP 会话"
	case s.LastSync == nil || now.Sub(*s.LastSync) > staleAfter:
		return false, "收敛停滞"
	}
	return true, ""
}

type NeighborStatus struct {
	Address     string `json:"address"`
	Established bool   `json:"established"`
}

// VIPStatus 中 Announced 表示路径确实发给了至少一个邻居；为 false 时 Reason 说明是哪一层不满足。
type VIPStatus struct {
	Address   netip.Addr `json:"address"`
	Announced bool       `json:"announced"`
	Reason    Reason     `json:"reason,omitempty"`
}
