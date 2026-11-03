package loadbalancer

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

var (
	serving    = ProxyFacts{Present: true, Matches: true, Reload: ReloadSucceeded}
	stale      = ProxyFacts{Present: true, Matches: false, Reload: ReloadSucceeded}
	reloading  = ProxyFacts{Present: true, Matches: true, Reload: ReloadPending}
	reloadFail = ProxyFacts{Present: true, Matches: true, Reload: ReloadFailed, Message: "bind"}

	announced   = AgentReport{Reported: true, VIPs: map[netip.Addr]Reason{vip1: ""}}
	notOpen     = AgentReport{Reported: true, VIPs: map[netip.Addr]Reason{vip1: FrontendNotOpen}}
	notBound    = AgentReport{Reported: true, VIPs: map[netip.Addr]Reason{}}
	notReported = AgentReport{}
)

func TestPlanHost(t *testing.T) {
	cases := []struct {
		name     string
		address  bool
		facts    ProxyFacts
		agent    AgentReport
		err      error
		deleting bool
		action   HostAction
		phase    AttachmentPhase
	}{
		{"VIP 未确认时不写主机", false, ProxyFacts{}, announced, nil, false, HostWait, AttachmentPending},
		{"缺少配置先写配置", true, ProxyFacts{}, announced, nil, false, ApplyConfig, AttachmentPending},
		{"配置变化时重写", true, stale, announced, nil, false, ApplyConfig, AttachmentPending},
		{"reload 未完成", true, reloading, announced, nil, false, HostWait, AttachmentPending},
		{"reload 失败", true, reloadFail, announced, nil, false, HostWait, AttachmentUnavailable},
		{"agent 报告未宣告", true, serving, notOpen, nil, false, HostWait, AttachmentConfigured},
		{"agent 尚未看到 bind", true, serving, notBound, nil, false, HostWait, AttachmentConfigured},
		{"agent 未报告时不可知", true, serving, notReported, nil, false, HostWait, AttachmentUnavailable},
		{"配置生效且 agent 已宣告即就绪", true, serving, announced, nil, false, HostWait, AttachmentReady},
		{"无法观察只影响该主机", true, ProxyFacts{}, announced, errors.New("timeout"), false, HostWait, AttachmentUnavailable},
		{"删除时直接删配置", true, serving, announced, nil, true, RemoveConfig, AttachmentConfigured},
		{"删除不依赖 agent", true, serving, notReported, nil, true, RemoveConfig, AttachmentConfigured},
		{"删除后等待 reload", true, ProxyFacts{Reload: ReloadPending}, announced, nil, true, HostWait, AttachmentConfigured},
		{"删除完成", true, ProxyFacts{}, announced, nil, true, HostWait, AttachmentRemoved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snapshot := Snapshot{Owner: self}
			if c.address {
				snapshot.Address = vip1
			}
			lb := Reconstitute(snapshot)
			if action := lb.PlanHost("lb1", c.facts, c.agent, c.err, c.deleting); action != c.action {
				t.Fatalf("action = %v, want %v", action, c.action)
			}
			if phase := lb.Attachments()[0].Phase; phase != c.phase {
				t.Fatalf("phase = %s, want %s", phase, c.phase)
			}
		})
	}
}

func TestProgrammedAndDegraded(t *testing.T) {
	lb := Reconstitute(Snapshot{Owner: self, Address: vip1})
	lb.AddressClaimed(vip1)
	lb.PlanHost("lb1", serving, announced, nil, false)
	lb.PlanHost("lb2", ProxyFacts{}, announced, errors.New("unreachable"), false)
	if programmed, reason, _ := lb.Programmed(); !programmed || reason != ReasonProgrammed {
		t.Fatalf("一台主机就绪即应 Programmed，got %v %s", programmed, reason)
	}
	if degraded := lb.Degraded(); len(degraded) != 1 || degraded[0] != "lb2" {
		t.Fatalf("Degraded = %v", degraded)
	}

	none := Reconstitute(Snapshot{Owner: self, Address: vip1})
	none.AddressClaimed(vip1)
	none.PlanHost("lb1", serving, notOpen, nil, false)
	if programmed, reason, _ := none.Programmed(); programmed || reason != ReasonDataPlanePending {
		t.Fatalf("无主机报告已宣告时不应 Programmed，got %v %s", programmed, reason)
	}
}

func TestHostsReleased(t *testing.T) {
	lb := Reconstitute(Snapshot{Owner: self, Address: vip1})
	lb.PlanHost("lb1", ProxyFacts{}, notReported, nil, true)
	lb.PlanHost("lb2", serving, notReported, nil, true)
	if lb.HostsReleased() {
		t.Fatal("仍有主机保留配置时不能释放 VIP")
	}
	done := Reconstitute(Snapshot{Owner: self, Address: vip1})
	done.PlanHost("lb1", ProxyFacts{}, notReported, nil, true)
	if !done.HostsReleased() {
		t.Fatal("全部主机清理后应可释放 VIP")
	}
	unreachable := Reconstitute(Snapshot{Owner: self, Address: vip1})
	unreachable.PlanHost("lb1", ProxyFacts{}, notReported, errors.New("timeout"), true)
	if unreachable.HostsReleased() {
		t.Fatal("主机不可达时删除应等待，不跳过")
	}
}

func TestAgentReady(t *testing.T) {
	now := time.Now()
	recent, old := now.Add(-5*time.Second), now.Add(-5*time.Minute)
	up := []NeighborStatus{{Address: "10.60.0.1", Established: true}}
	cases := map[string]struct {
		status AgentStatus
		ready  bool
	}{
		"正常":          {AgentStatus{HAProxyUp: true, Neighbors: up, LastSync: &recent}, true},
		"HAProxy 不应答": {AgentStatus{HAProxyUp: false, Neighbors: up, LastSync: &recent}, false},
		"没有会话":        {AgentStatus{HAProxyUp: true, Neighbors: []NeighborStatus{{Address: "10.60.0.1"}}, LastSync: &recent}, false},
		"收敛停滞":        {AgentStatus{HAProxyUp: true, Neighbors: up, LastSync: &old}, false},
		"从未收敛":        {AgentStatus{HAProxyUp: true, Neighbors: up}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if ready, reason := c.status.Ready(now, time.Minute); ready != c.ready || (!ready && reason == "") {
				t.Fatalf("got %v %q", ready, reason)
			}
		})
	}
}
