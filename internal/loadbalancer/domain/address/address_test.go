package address

import (
	"net/netip"
	"testing"
	"time"
)

func TestPoolNextSkipsTakenAndBoundaryAddresses(t *testing.T) {
	pool := Pool{Prefix: netip.MustParsePrefix("10.62.0.0/30")}
	taken := map[netip.Addr]Owner{}
	got, ok := pool.Next(taken)
	if !ok || got != netip.MustParseAddr("10.62.0.1") {
		t.Fatalf("Next = %v %v, want 10.62.0.1", got, ok)
	}
	taken[got] = Owner{}
	got, ok = pool.Next(taken)
	if !ok || got != netip.MustParseAddr("10.62.0.2") {
		t.Fatalf("Next = %v %v, want 10.62.0.2", got, ok)
	}
	taken[got] = Owner{}
	if got, ok := pool.Next(taken); ok {
		t.Fatalf("池耗尽后不应返回广播地址，got %v", got)
	}
}

func TestPoolContains(t *testing.T) {
	pool := Pool{Prefix: netip.MustParsePrefix("10.62.0.0/24")}
	for addr, want := range map[string]bool{
		"10.62.0.0": false, "10.62.0.1": true, "10.62.0.254": true, "10.62.0.255": false, "10.63.0.1": false,
	} {
		if got := pool.Contains(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", addr, got, want)
		}
	}
	single := Pool{Prefix: netip.MustParsePrefix("10.62.0.9/32")}
	if !single.Contains(netip.MustParseAddr("10.62.0.9")) {
		t.Fatal("/32 池应包含唯一地址")
	}
}

func TestOrphaned(t *testing.T) {
	owner := Owner{Namespace: "team", Name: "pg", UID: "uid-1"}
	if orphan, wait := Orphaned(owner, "uid-1", time.Hour); orphan || wait != 0 {
		t.Fatal("所有者存在时不是无主记录")
	}
	if orphan, wait := Orphaned(owner, "", 10*time.Second); orphan || wait != Grace-10*time.Second {
		t.Fatalf("宽限期内应等待，got %v %v", orphan, wait)
	}
	if orphan, _ := Orphaned(owner, "uid-2", Grace); !orphan {
		t.Fatal("同名重建的 LoadBalancer 不是原所有者")
	}
}
