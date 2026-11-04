package loadbalancer

import (
	"errors"
	"reflect"
	"testing"
)

const (
	down  = "DOWN"
	drain = "DRAIN"
	l7ok  = "L7OK"
)

func TestNormalizeState(t *testing.T) {
	for in, want := range map[string]string{
		"UP": "UP", "UP 1/3": "UP", "DOWN 1/2": down, drain: drain, "MAINT": "MAINT", "no check": "NOCHECK",
	} {
		if got := NormalizeState(in); got != want {
			t.Errorf("NormalizeState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHealth(t *testing.T) {
	a, b := Endpoint{Address: "10.60.0.11", Port: 5432}, Endpoint{Address: "10.60.0.12", Port: 5432}
	lb := Reconstitute(Snapshot{Owner: self, Address: vip1, Ports: []Port{
		{Port: 5432, Endpoints: []Endpoint{a, b}},
		{Port: 6432, MissingBackend: true},
	}})
	stale := Endpoint{Address: "10.60.0.13", Port: 5432}
	lb.PlanHost("lb-2", ProxyFacts{Matches: true, Servers: []ServerHealth{
		{Port: 5432, Endpoint: a, State: "UP", CheckStatus: l7ok, CheckCode: 200},
		{Port: 5432, Endpoint: b, State: down, CheckStatus: "L7STS", CheckCode: 503},
		// 排空中、期望里已没有的 server 不呈现。
		{Port: 5432, Endpoint: stale, State: drain},
	}}, AgentReport{}, nil, false)
	lb.PlanHost("lb-1", ProxyFacts{Matches: true, Servers: []ServerHealth{
		{Port: 5432, Endpoint: a, State: "UP", CheckStatus: l7ok, CheckCode: 200},
	}}, AgentReport{}, nil, false)
	// 观察不到的主机不替任何 endpoint 作证。
	lb.PlanHost("lb-3", ProxyFacts{}, AgentReport{}, errors.New("timeout"), false)

	want := []PortHealth{
		{Port: 5432, Endpoints: []EndpointHealth{
			{Endpoint: a, Hosts: []HostHealth{
				{Host: "lb-1", State: "UP", CheckStatus: l7ok, CheckCode: 200},
				{Host: "lb-2", State: "UP", CheckStatus: l7ok, CheckCode: 200},
			}},
			{Endpoint: b, Hosts: []HostHealth{{Host: "lb-2", State: down, CheckStatus: "L7STS", CheckCode: 503}}},
		}},
		{Port: 6432},
	}
	if got := lb.Health(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Health() =\n%+v\nwant\n%+v", got, want)
	}
	ready, reason, message := lb.EndpointsReady()
	if ready || reason != ReasonNoReadyEndpoints || message != "没有可用成员的端口：6432" {
		t.Fatalf("EndpointsReady() = %v %q %q", ready, reason, message)
	}
}

func TestEndpointsReady(t *testing.T) {
	a := Endpoint{Address: "10.60.0.11", Port: 6443}
	lb := Reconstitute(Snapshot{Owner: self, Address: vip1, Ports: []Port{{Port: 6443, Endpoints: []Endpoint{a}}}})
	lb.PlanHost("lb-1", ProxyFacts{Matches: true, Servers: []ServerHealth{{Port: 6443, Endpoint: a, State: "UP", CheckStatus: "L4OK"}}},
		AgentReport{}, nil, false)
	if ready, reason, _ := lb.EndpointsReady(); !ready || reason != ReasonEndpointsReady {
		t.Fatalf("EndpointsReady() = %v %q", ready, reason)
	}
	// 删除中不记录观察。
	deleting := Reconstitute(Snapshot{Owner: self, Address: vip1, Ports: []Port{{Port: 6443, Endpoints: []Endpoint{a}}}})
	deleting.PlanHost("lb-1", ProxyFacts{Present: true, Servers: []ServerHealth{{Port: 6443, Endpoint: a, State: "UP"}}},
		AgentReport{}, nil, true)
	if got := deleting.Health()[0].Endpoints[0].Hosts; got != nil {
		t.Fatalf("删除中仍记录了观察：%+v", got)
	}
}
