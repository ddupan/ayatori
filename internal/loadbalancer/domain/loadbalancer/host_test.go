package loadbalancer

import (
	"errors"
	"testing"
)

var (
	serving    = ProxyFacts{Present: true, Matches: true, Reload: ReloadSucceeded, Frontends: []Frontend{{Address: vip1, Open: true}}}
	stopped    = ProxyFacts{Present: true, Matches: true, Reload: ReloadSucceeded, Frontends: []Frontend{{Address: vip1, Open: false}}}
	stale      = ProxyFacts{Present: true, Matches: false, Reload: ReloadSucceeded}
	reloading  = ProxyFacts{Present: true, Matches: true, Reload: ReloadPending}
	reloadFail = ProxyFacts{Present: true, Matches: true, Reload: ReloadFailed, Message: "bind"}
)

func TestPlanHost(t *testing.T) {
	cases := []struct {
		name     string
		address  bool
		facts    ProxyFacts
		err      error
		deleting bool
		action   HostAction
		phase    AttachmentPhase
	}{
		{"VIP 未确认时不写主机", false, ProxyFacts{}, nil, false, HostWait, AttachmentPending},
		{"缺少配置先写配置", true, ProxyFacts{}, nil, false, ApplyConfig, AttachmentPending},
		{"配置变化时重写", true, stale, nil, false, ApplyConfig, AttachmentPending},
		{"reload 未完成", true, reloading, nil, false, HostWait, AttachmentPending},
		{"reload 失败", true, reloadFail, nil, false, HostWait, AttachmentUnavailable},
		{"frontend 未 OPEN 时不满足宣告", true, stopped, nil, false, HostWait, AttachmentConfigured},
		{"配置生效且 frontend OPEN 即就绪", true, serving, nil, false, HostWait, AttachmentReady},
		{"无法观察只影响该主机", true, ProxyFacts{}, errors.New("timeout"), false, HostWait, AttachmentUnavailable},
		{"删除时直接删配置", true, serving, nil, true, RemoveConfig, AttachmentConfigured},
		{"删除后等待 reload", true, ProxyFacts{Reload: ReloadPending}, nil, true, HostWait, AttachmentConfigured},
		{"删除完成", true, ProxyFacts{}, nil, true, HostWait, AttachmentRemoved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snapshot := Snapshot{Owner: self}
			if c.address {
				snapshot.Address = vip1
			}
			lb := Reconstitute(snapshot)
			if action := lb.PlanHost("lb1", c.facts, c.err, c.deleting); action != c.action {
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
	lb.PlanHost("lb1", serving, nil, false)
	lb.PlanHost("lb2", ProxyFacts{}, errors.New("unreachable"), false)
	if programmed, reason, _ := lb.Programmed(); !programmed || reason != ReasonProgrammed {
		t.Fatalf("一台主机就绪即应 Programmed，got %v %s", programmed, reason)
	}
	if degraded := lb.Degraded(); len(degraded) != 1 || degraded[0] != "lb2" {
		t.Fatalf("Degraded = %v", degraded)
	}

	none := Reconstitute(Snapshot{Owner: self, Address: vip1})
	none.AddressClaimed(vip1)
	none.PlanHost("lb1", stopped, nil, false)
	if programmed, reason, _ := none.Programmed(); programmed || reason != ReasonDataPlanePending {
		t.Fatalf("无主机满足宣告规则时不应 Programmed，got %v %s", programmed, reason)
	}
}

func TestHostsReleased(t *testing.T) {
	lb := Reconstitute(Snapshot{Owner: self, Address: vip1})
	lb.PlanHost("lb1", ProxyFacts{}, nil, true)
	lb.PlanHost("lb2", serving, nil, true)
	if lb.HostsReleased() {
		t.Fatal("仍有主机保留配置时不能释放 VIP")
	}
	done := Reconstitute(Snapshot{Owner: self, Address: vip1})
	done.PlanHost("lb1", ProxyFacts{}, nil, true)
	if !done.HostsReleased() {
		t.Fatal("全部主机清理后应可释放 VIP")
	}
	unreachable := Reconstitute(Snapshot{Owner: self, Address: vip1})
	unreachable.PlanHost("lb1", ProxyFacts{}, errors.New("timeout"), true)
	if unreachable.HostsReleased() {
		t.Fatal("主机不可达时删除应等待，不跳过")
	}
}
