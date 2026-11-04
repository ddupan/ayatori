package loadbalancer

import (
	"net/netip"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
)

const namespace = "team"

var (
	pool  = address.Pool{Prefix: netip.MustParsePrefix("10.62.0.0/24")}
	self  = address.Owner{Namespace: namespace, Name: "pg", UID: "uid-1"}
	other = address.Owner{Namespace: namespace, Name: "other", UID: "uid-2"}
	vip1  = netip.MustParseAddr("10.62.0.1")
	vip2  = netip.MustParseAddr("10.62.0.2")
)

func TestPlanAddress(t *testing.T) {
	cases := []struct {
		name     string
		snapshot Snapshot
		taken    map[netip.Addr]address.Owner
		want     netip.Addr
		action   Action
		reason   string
	}{
		{"自动分配首个空闲地址", Snapshot{Owner: self}, nil, vip1, Claim, ReasonAddressPending},
		{"跳过他人占用", Snapshot{Owner: self}, map[netip.Addr]address.Owner{vip1: other}, vip2, Claim, ReasonAddressPending},
		{"采用 status 未记录的自身占用", Snapshot{Owner: self}, map[netip.Addr]address.Owner{vip1: other, vip2: self}, vip2, Save, ReasonDataPlanePending},
		{"同名重建不采用旧 UID 的占用", Snapshot{Owner: self}, map[netip.Addr]address.Owner{vip1: {Namespace: namespace, Name: "pg", UID: "old"}}, vip2, Claim, ReasonAddressPending},
		{"请求的地址空闲", Snapshot{Owner: self, Requested: vip2}, nil, vip2, Claim, ReasonAddressPending},
		{"请求的地址被占用", Snapshot{Owner: self, Requested: vip1}, map[netip.Addr]address.Owner{vip1: other}, netip.Addr{}, Wait, ReasonAddressConflict},
		{"请求的地址不在池内", Snapshot{Owner: self, Requested: netip.MustParseAddr("10.63.0.1")}, nil, netip.Addr{}, Wait, ReasonAddressOutOfPool},
		{"已记录且仍占用", Snapshot{Owner: self, Address: vip1}, map[netip.Addr]address.Owner{vip1: self}, vip1, Wait, ReasonDataPlanePending},
		{"已记录但占用丢失时重新认领原地址", Snapshot{Owner: self, Address: vip1}, nil, vip1, Claim, ReasonAddressPending},
		{"已记录但被他人抢占不换地址", Snapshot{Owner: self, Address: vip1}, map[netip.Addr]address.Owner{vip1: other}, netip.Addr{}, Wait, ReasonAddressConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lb := Reconstitute(c.snapshot)
			got, action := lb.PlanAddress(pool, c.taken)
			_, reason, _ := lb.Programmed()
			if got != c.want || action != c.action || reason != c.reason {
				t.Fatalf("PlanAddress = %v %v %s, want %v %v %s", got, action, reason, c.want, c.action, c.reason)
			}
		})
	}
}

func TestPoolExhausted(t *testing.T) {
	small := address.Pool{Prefix: netip.MustParsePrefix("10.62.0.0/30")}
	lb := Reconstitute(Snapshot{Owner: self})
	if _, action := lb.PlanAddress(small, map[netip.Addr]address.Owner{vip1: other, vip2: other}); action != Wait {
		t.Fatal("池耗尽时不应创建")
	}
	if _, reason, _ := lb.Programmed(); reason != ReasonPoolExhausted {
		t.Fatalf("reason = %s", reason)
	}
}

func TestPlanRelease(t *testing.T) {
	lb := Reconstitute(Snapshot{Owner: self})
	owned, action := lb.PlanRelease(map[netip.Addr]address.Owner{vip1: other, vip2: self})
	if action != Release || len(owned) != 1 || owned[0] != vip2 {
		t.Fatalf("应只释放自身占用（含 status 未记录的），got %v %v", owned, action)
	}
	if _, action := lb.PlanRelease(map[netip.Addr]address.Owner{vip1: other}); action != Finalize {
		t.Fatal("没有自身占用时应放行删除")
	}
}

func TestResolvedRefs(t *testing.T) {
	resolved := Port{Port: 5432, Backend: "pg"}
	missing := Port{Port: 8008, Backend: "patroni", MissingBackend: true}
	if ok, reason, _ := Reconstitute(Snapshot{Ports: []Port{resolved}}).ResolvedRefs(); !ok || reason != ReasonResolvedRefs {
		t.Fatalf("got %v %s", ok, reason)
	}
	again := Port{Port: 8009, Backend: "patroni", MissingBackend: true}
	ok, reason, message := Reconstitute(Snapshot{Ports: []Port{resolved, missing, again}}).ResolvedRefs()
	if ok || reason != ReasonBackendNotFound || message != "引用的 Backend 不存在：patroni" {
		t.Fatalf("got %v %s %q", ok, reason, message)
	}
}

func TestDigest(t *testing.T) {
	base := Snapshot{Owner: self, Address: vip1, Ports: []Port{{Port: 5432, Endpoints: []Endpoint{{Address: "10.60.0.21", Port: 5432}}}}}
	same := Snapshot{Owner: self, Address: vip1, Ports: []Port{{Port: 5432, Endpoints: []Endpoint{{Address: "10.60.0.21", Port: 5432}}}}}
	if same.Digest() != base.Digest() || len(base.Digest()) != 64 {
		t.Fatal("摘要应稳定且为 sha256")
	}
	changed := base
	changed.Ports = []Port{{Port: 5432, Endpoints: []Endpoint{{Address: "10.60.0.22", Port: 5432}}}}
	requested := base
	requested.Requested = vip1
	switch {
	case changed.Digest() == base.Digest():
		t.Fatal("endpoint 变化应改变摘要")
	case requested.Digest() != base.Digest():
		t.Fatal("Requested 不落地到主机，不应影响摘要")
	}
}

func TestParseReload(t *testing.T) {
	for _, r := range []Reload{ReloadSucceeded, ReloadPending, ReloadFailed} {
		if ParseReload(r.String()) != r {
			t.Errorf("ParseReload(%q) != %v", r.String(), r)
		}
	}
	if ParseReload("") != ReloadFailed {
		t.Error("未知的值应视为 Failed")
	}
}
