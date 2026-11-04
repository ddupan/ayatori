package loadbalancer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"time"
)

// Port 是期望配置中的一个 VIP 端口：controller 与代理 adapter 之间的契约。
// 字段合法性由 CRD 保证；翻译为具体代理配置由 adapter 负责。
type Port struct {
	Port int32
	// Backend 是引用的 Backend 名称。MissingBackend 表示它不存在：其余字段为零值，没有 endpoint，
	// 端口拒绝连接（fail-closed）。
	Backend        string
	MissingBackend bool
	Endpoints      []Endpoint
	ProxyProtocol  ProxyProtocol
	IdleTimeout    time.Duration
	DrainTimeout   time.Duration
	HealthCheck    HealthCheck
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
	// Servers 是运行中的进程里该 LoadBalancer 各 server 的健康检查结论。
	Servers []ServerHealth
}

type Reload uint8

const (
	ReloadSucceeded Reload = iota
	ReloadPending
	ReloadFailed
)

// Digest 是落地到主机的期望配置（UID、VIP 与端口）的摘要。agent 报告本机配置与哪份期望一致，
// controller 以同一函数计算后比较（ADR-0022）；Backend 变化不改变 LoadBalancer 的 generation，
// 所以不能用 generation。
func (s Snapshot) Digest() string {
	data, err := json.Marshal(struct {
		UID     string
		Address netip.Addr
		Ports   []Port
	}{s.Owner.UID, s.Address, s.Ports})
	if err != nil {
		// 只含基本类型，不会失败。
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
