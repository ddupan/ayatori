package loadbalancer

import "time"

// Port 是期望配置中的一个 VIP 端口：controller 与代理 adapter 之间的契约。
// 字段合法性由 CRD 保证；翻译为具体代理配置由 adapter 负责。
type Port struct {
	Port          int32
	Endpoints     []Endpoint
	ProxyProtocol ProxyProtocol
	IdleTimeout   time.Duration
	HealthCheck   HealthCheck
}

// Endpoint 的 Address 可以是 IP 或 DNS 名称。
type Endpoint struct {
	Address string
	Port    int32
}

type ProxyProtocol string

const (
	ProxyProtocolNone ProxyProtocol = "None"
	ProxyProtocolV1   ProxyProtocol = "V1"
	ProxyProtocolV2   ProxyProtocol = "V2"
)

// HealthCheck 的 Path 非空即为 HTTP 检查；Port 为零时探测 endpoint 自身端口；
// ExpectedStatuses 为空时期望 200。
type HealthCheck struct {
	Port               int32
	Path               string
	ExpectedStatuses   []int32
	Interval           time.Duration
	Timeout            time.Duration
	HealthyThreshold   int32
	UnhealthyThreshold int32
}

// ProxyFacts 是某台 Host 上该 LoadBalancer 代理配置的原始事实。
type ProxyFacts struct {
	// Present 表示 Host 上存在属于该 LoadBalancer 的任何配置。
	Present bool
	// Matches 表示配置文件中的配置与期望完全一致。
	Matches bool
	// Reload 是 Host 最近一次 reload 的结果；配置一致且 reload 成功才表示运行中的进程已生效。
	Reload Reload
	// Message 是 reload 失败时代理返回的说明。
	Message string
}

type Reload uint8

const (
	ReloadSucceeded Reload = iota
	ReloadPending
	ReloadFailed
)
