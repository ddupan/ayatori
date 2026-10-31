package loadbalancer

import (
	"net/netip"
	"time"
)

// AgentStatus 是 LB agent 在 GET /status 上报告的本机事实（ADR-0015），controller 只读取、不推算。
type AgentStatus struct {
	HAProxyUp bool             `json:"haproxyUp"`
	Neighbors []NeighborStatus `json:"neighbors"`
	// LastSync 是最近一次成功收敛的时间；从未成功时为空。
	LastSync *time.Time `json:"lastSync,omitempty"`
	// VIPs 是 HAProxy 中绑定在 VIP 段内的全部地址；HAProxy 不应答时为空。
	VIPs []VIPStatus `json:"vips"`
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
