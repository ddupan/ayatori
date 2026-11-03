package loadbalancer

import (
	"errors"
	"net/netip"
	"testing"
)

func TestPlanRecord(t *testing.T) {
	other := netip.MustParseAddr("10.62.0.99")
	cases := []struct {
		name      string
		address   bool
		record    []netip.Addr
		err       error
		deleting  bool
		action    RecordAction
		published bool
		released  bool
	}{
		{"VIP 未确认时不发布", false, nil, nil, false, RecordWait, false, false},
		{"缺少记录时发布", true, nil, nil, false, PublishRecord, false, false},
		{"记录指向其他地址时覆盖", true, []netip.Addr{other}, nil, false, PublishRecord, false, false},
		{"多余的值也要覆盖", true, []netip.Addr{vip1, other}, nil, false, PublishRecord, false, false},
		{"记录一致即已发布", true, []netip.Addr{vip1}, nil, false, RecordWait, true, false},
		{"无法观察时等待", true, nil, errors.New("timeout"), false, RecordWait, false, false},
		{"删除时移除记录", true, []netip.Addr{vip1}, nil, true, RemoveRecord, false, false},
		{"删除时记录已不存在", true, nil, nil, true, RecordWait, false, true},
		{"删除时无法观察则不放行", true, nil, errors.New("timeout"), true, RecordWait, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snapshot := Snapshot{Owner: self}
			if c.address {
				snapshot.Address = vip1
			}
			lb := Reconstitute(snapshot)
			if action := lb.PlanRecord(c.record, c.err, c.deleting); action != c.action {
				t.Fatalf("action = %v, want %v", action, c.action)
			}
			if _, published, _ := lb.Published(); published != c.published {
				t.Fatalf("published = %v, want %v", published, c.published)
			}
			if lb.RecordReleased() != c.released {
				t.Fatalf("released = %v, want %v", lb.RecordReleased(), c.released)
			}
		})
	}
}

func TestHostnameAndDisabledDNS(t *testing.T) {
	lb := Reconstitute(Snapshot{Owner: self})
	if got := lb.Hostname("lb.lab.ddupan.top"); got != "pg.team.lb.lab.ddupan.top" {
		t.Fatalf("Hostname = %s", got)
	}
	if !lb.RecordReleased() {
		t.Fatal("未启用 DNS 时删除不应等待记录")
	}
}
