package agent

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

// 取自 HAProxy 3.x 对 disable frontend lb_a_7001 后的 show stat（只保留用到的列）：
// frontend 行变为 PAUSED，但其 bind 行仍是 OPEN。
const stat = `# pxname,svname,status,type,addr
lb_a_7000,FRONTEND,OPEN,0,
lb_a_7000,vip,OPEN,3,10.62.0.5:7000
lb_a_7001,FRONTEND,PAUSED,0,
lb_a_7001,vip,OPEN,3,10.62.0.5:7001
plain,FRONTEND,OPEN,0,
be,s1,DOWN,2,127.0.0.1:1
be,BACKEND,DOWN,1,

`

func TestParseStat(t *testing.T) {
	frontends, err := parseStat(strings.NewReader(stat))
	if err != nil {
		t.Fatal(err)
	}
	vip := netip.MustParseAddr("10.62.0.5")
	want := []loadbalancer.Frontend{{Address: vip, Open: true}, {Address: vip, Open: false}}
	if !slices.Equal(frontends, want) {
		t.Fatalf("got %v, want %v（被禁用的 frontend 不算 OPEN；server 行与无 bind 行的 frontend 不计入）", frontends, want)
	}
	if _, err := parseStat(strings.NewReader("Unknown command\n")); err == nil {
		t.Fatal("非 CSV 应答应报错，而不是当作没有 frontend")
	}
}
