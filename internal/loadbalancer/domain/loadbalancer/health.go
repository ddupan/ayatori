package loadbalancer

import (
	"fmt"
	"slices"
	"strings"
)

// ServerHealth 是某台 Host 上一个端口的一个 server 的健康检查结论（ADR-0023），取自代理的 stats。
type ServerHealth struct {
	Port        int32
	Endpoint    Endpoint
	State       string
	CheckStatus string
	CheckCode   int32
}

// EndpointsReady condition 的 Reason。
const (
	ReasonEndpointsReady   = "EndpointsReady"
	ReasonNoReadyEndpoints = "NoReadyEndpoints"
)

// StateUp 是接收新连接的 server 状态。
const StateUp = "UP"

// NormalizeState 把代理报告的状态规范为一个词：翻转途中的「UP 1/3」「DOWN 1/2」取前者，
// 「no check」记为 NOCHECK。
func NormalizeState(state string) string {
	state = strings.ToUpper(strings.TrimSpace(state))
	if state == "NO CHECK" {
		return "NOCHECK"
	}
	first, _, _ := strings.Cut(state, " ")
	return first
}

// PortHealth 是一个端口的各 endpoint 在各 Host 上的观察，按期望配置中的 endpoint 顺序排列。
type PortHealth struct {
	Port      int32
	Endpoints []EndpointHealth
}

type EndpointHealth struct {
	Endpoint Endpoint
	// Hosts 按 Host 名称排序；本轮观察不到的 Host 不列出。
	Hosts []HostHealth
}

type HostHealth struct {
	Host        string
	State       string
	CheckStatus string
	CheckCode   int32
}

// Ready 表示至少一台 Host 上处于 UP。
func (e EndpointHealth) Ready() bool {
	return slices.ContainsFunc(e.Hosts, func(h HostHealth) bool { return h.State == StateUp })
}

// observeHealth 记录 Host 本轮的观察；只在 Host 可观察且配置未在删除时调用。
func (lb *LoadBalancer) observeHealth(host string, servers []ServerHealth) {
	if lb.health == nil {
		lb.health = map[string][]ServerHealth{}
	}
	lb.health[host] = servers
}

// Health 返回期望配置中每个端口的 endpoint 在各 Host 上的观察。须在本轮 PlanHost 之后调用。
// 期望配置中没有的 server（如正在排空移除的）不呈现。
func (lb *LoadBalancer) Health() []PortHealth {
	hosts := make([]string, 0, len(lb.health))
	for host := range lb.health {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	result := make([]PortHealth, 0, len(lb.snapshot.Ports))
	for _, port := range lb.snapshot.Ports {
		ph := PortHealth{Port: port.Port}
		for _, endpoint := range port.Endpoints {
			eh := EndpointHealth{Endpoint: endpoint}
			for _, host := range hosts {
				i := slices.IndexFunc(lb.health[host], func(s ServerHealth) bool { return s.Port == port.Port && s.Endpoint == endpoint })
				if i < 0 {
					continue
				}
				s := lb.health[host][i]
				eh.Hosts = append(eh.Hosts, HostHealth{Host: host, State: s.State, CheckStatus: s.CheckStatus, CheckCode: s.CheckCode})
			}
			ph.Endpoints = append(ph.Endpoints, eh)
		}
		result = append(result, ph)
	}
	return result
}

// EndpointsReady 表示每个端口都至少有一个 endpoint 在某台 Host 上 UP。与 Degraded 分开：
// 后端全挂由消费方处理，主机未收敛由 LB 运维处理（ADR-0023）。
func (lb *LoadBalancer) EndpointsReady() (ready bool, reason, message string) {
	var empty []string
	for _, port := range lb.Health() {
		if !slices.ContainsFunc(port.Endpoints, EndpointHealth.Ready) {
			empty = append(empty, fmt.Sprint(port.Port))
		}
	}
	if len(empty) > 0 {
		return false, ReasonNoReadyEndpoints, "没有可用成员的端口：" + strings.Join(empty, ", ")
	}
	return true, ReasonEndpointsReady, ""
}
