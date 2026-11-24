//go:build integration

package powerdns_test

import (
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"

	pdns "github.com/joeig/go-powerdns/v3"

	"go.ddupan.top/ayatori/internal/loadbalancer/adapter/powerdns"
	"go.ddupan.top/ayatori/internal/testenv"
)

const (
	fixtureAPIKey = "AYATORI-TEST-ONLY"
	zoneName      = "lb.example.test"
	name          = "pg.team." + zoneName
)

// startFixture 连接 hack/integration-test.sh 在 go test 之外启动的 PowerDNS，删除上个场景的 zone
// 后重建，返回 adapter。只认脚本给出的容器 ID。
func startFixture(t *testing.T) *powerdns.Zone {
	t.Helper()
	_, port := testenv.Container(t, "pdns", 8081)
	zone := powerdns.New("http://127.0.0.1:"+strconv.Itoa(port), fixtureAPIKey, zoneName, http.DefaultClient)
	// 结果不必区分：zone 不存在或服务尚未就绪都正常，其余问题由下面的重建报告。
	_ = zone.Client.Zones.Delete(t.Context(), zoneName+".")
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
