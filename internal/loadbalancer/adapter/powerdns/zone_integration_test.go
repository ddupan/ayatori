//go:build integration

package powerdns_test

import (
	"context"
	"net/http"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	pdns "github.com/joeig/go-powerdns/v3"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/powerdns"
)

const (
	// 与 homelab 部署的 PowerDNS Authoritative 5.0.7 一致。
	fixtureImage  = "powerdns/pdns-auth-50@sha256:4d6cc4fc42a28f2df7fb55f6f36d8323f96e0da66135ccdad057ca7349b223b4"
	fixtureAPIKey = "AYATORI-TEST-ONLY"
	zoneName      = "lb.example.test"
	name          = "pg.team." + zoneName
)

// startFixture 只创建自己的临时容器并按确切 ID 清理，返回已建好 zone 的 adapter。
func startFixture(t *testing.T) *powerdns.Zone {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-d", "-p", "127.0.0.1::8081",
		"-e", "PDNS_AUTH_API_KEY="+fixtureAPIKey, fixtureImage).Output()
	if err != nil {
		t.Fatalf("cannot start PowerDNS fixture: %v", err)
	}
	id := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal("unexpected container identifier")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, "docker", "rm", "-f", id).Run() != nil {
			t.Error("fixture cleanup failed")
		}
	})
	out, err := exec.CommandContext(t.Context(), "docker", "port", id, "8081").Output()
	if err != nil {
		t.Fatal(err)
	}
	zone := powerdns.New("http://"+strings.TrimSpace(strings.Split(string(out), "\n")[0]), fixtureAPIKey, zoneName, http.DefaultClient)
	// zone 由部署方创建；fixture 在此代为创建。
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		_, err := zone.Client.Zones.AddNative(t.Context(), zoneName+".", false, "", false, "", "", true, []string{"ns." + zoneName + "."})
		if err == nil {
			return zone
		}
		if time.Now().After(deadline) {
			t.Fatalf("cannot create fixture zone: %v", err)
		}
	}
}

func TestZoneContract(t *testing.T) {
	zone := startFixture(t)
	ctx := t.Context()
	vip, other := netip.MustParseAddr("10.62.0.10"), netip.MustParseAddr("10.62.0.11")

	if record, err := zone.Record(ctx, name); err != nil || len(record) != 0 {
		t.Fatalf("发布前 Record = %v %v", record, err)
	}
	if err := zone.Client.Records.Add(ctx, zoneName, "other.team."+zoneName+".", pdns.RRTypeA, 60, []string{other.String()}); err != nil {
		t.Fatal(err)
	}
	if err := zone.Publish(ctx, name, other); err != nil {
		t.Fatal(err)
	}
	if err := zone.Publish(ctx, name, vip); err != nil {
		t.Fatal(err)
	}
	if record, err := zone.Record(ctx, name); err != nil || !slices.Equal(record, []netip.Addr{vip}) {
		t.Fatalf("Publish 应以唯一值替换，got %v %v", record, err)
	}
	for range 2 { // 重复删除同样成功
		if err := zone.Remove(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if record, err := zone.Record(ctx, name); err != nil || len(record) != 0 {
		t.Fatalf("删除后 Record = %v %v", record, err)
	}
	if record, err := zone.Record(ctx, "other.team."+zoneName); err != nil || !slices.Equal(record, []netip.Addr{other}) {
		t.Fatalf("其他名称不应受影响，got %v %v", record, err)
	}
}
