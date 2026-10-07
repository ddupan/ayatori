package loadbalancer

import (
	"errors"
	"testing"
)

var (
	configured = ProxyFacts{Present: true, Matches: true, Reload: ReloadSucceeded}
	stale      = ProxyFacts{Present: true, Matches: false, Reload: ReloadSucceeded}
)

func TestPlanHost(t *testing.T) {
	cases := []struct {
		name     string
		address  bool
		facts    HostFacts
		err      error
		deleting bool
		action   HostAction
		phase    AttachmentPhase
	}{
		{"VIP 未确认时不写主机", false, HostFacts{}, nil, false, HostWait, AttachmentPending},
		{"缺少配置先写配置", true, HostFacts{}, nil, false, ApplyConfig, AttachmentPending},
		{"配置变化时保留已有宣告并重写", true, HostFacts{Proxy: stale, Announced: true}, nil, false, ApplyConfig, AttachmentPending},
		{"reload 未完成不宣告", true, HostFacts{Proxy: ProxyFacts{Present: true, Matches: true, Reload: ReloadPending}}, nil, false, HostWait, AttachmentPending},
		{"reload 失败不宣告", true, HostFacts{Proxy: ProxyFacts{Present: true, Matches: true, Reload: ReloadFailed, Message: "bind"}}, nil, false, HostWait, AttachmentUnavailable},
		{"配置生效后宣告", true, HostFacts{Proxy: configured}, nil, false, Announce, AttachmentConfigured},
		{"已宣告即完成", true, HostFacts{Proxy: configured, Announced: true}, nil, false, HostWait, AttachmentAnnounced},
		{"无法观察只影响该主机", true, HostFacts{}, errors.New("timeout"), false, HostWait, AttachmentUnavailable},
		{"删除先撤回", true, HostFacts{Proxy: configured, Announced: true}, nil, true, Withdraw, AttachmentAnnounced},
		{"撤回后删除配置", true, HostFacts{Proxy: configured}, nil, true, RemoveConfig, AttachmentConfigured},
		{"删除后等待 reload", true, HostFacts{Proxy: ProxyFacts{Reload: ReloadPending}}, nil, true, HostWait, AttachmentConfigured},
		{"删除完成", true, HostFacts{}, nil, true, HostWait, AttachmentWithdrawn},
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
	lb.PlanHost("lb1", HostFacts{Proxy: configured, Announced: true}, nil, false)
	lb.PlanHost("lb2", HostFacts{}, errors.New("unreachable"), false)
	if programmed, reason, _ := lb.Programmed(); !programmed || reason != ReasonProgrammed {
		t.Fatalf("一台主机宣告即应 Programmed，got %v %s", programmed, reason)
	}
	if degraded := lb.Degraded(); len(degraded) != 1 || degraded[0] != "lb2" {
		t.Fatalf("Degraded = %v", degraded)
	}

	none := Reconstitute(Snapshot{Owner: self, Address: vip1})
	none.AddressClaimed(vip1)
	none.PlanHost("lb1", HostFacts{}, nil, false)
	if programmed, reason, _ := none.Programmed(); programmed || reason != ReasonDataPlanePending {
		t.Fatalf("无主机宣告时不应 Programmed，got %v %s", programmed, reason)
	}
}

func TestHostsReleased(t *testing.T) {
	lb := Reconstitute(Snapshot{Owner: self, Address: vip1})
	lb.PlanHost("lb1", HostFacts{}, nil, true)
	lb.PlanHost("lb2", HostFacts{Proxy: configured}, nil, true)
	if lb.HostsReleased() {
		t.Fatal("仍有主机保留配置时不能释放 VIP")
	}
	done := Reconstitute(Snapshot{Owner: self, Address: vip1})
	done.PlanHost("lb1", HostFacts{}, nil, true)
	if !done.HostsReleased() {
		t.Fatal("全部主机清理后应可释放 VIP")
	}
	unreachable := Reconstitute(Snapshot{Owner: self, Address: vip1})
	unreachable.PlanHost("lb1", HostFacts{}, errors.New("timeout"), true)
	if unreachable.HostsReleased() {
		t.Fatal("主机不可达时删除应等待，不跳过")
	}
}
