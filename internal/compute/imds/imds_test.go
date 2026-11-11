package imds

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
)

func TestReadPorts(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(kind, key, uid string, obj any) {
		data, _ := json.Marshal(obj)
		if _, err := store.Put(kind, key, uid, "1", data); err != nil {
			t.Fatal(err)
		}
	}
	const vmUID, ctUID = "11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002"
	put("LibvirtDomain", "infra/vm1", vmUID, computev1alpha1.LibvirtDomain{Namespace: "infra", Name: "vm1", UID: vmUID,
		Spec: computev1alpha1.LibvirtDomainSpec{NodeName: "n1", Interfaces: []computev1alpha1.DomainInterface{{Network: "a"}, {Network: "b"}}}})
	put("LXCContainer", "infra/ct1", ctUID, computev1alpha1.LXCContainer{Namespace: "infra", Name: "ct1", UID: ctUID,
		Spec: computev1alpha1.LXCContainerSpec{NodeName: "n1", Networks: []computev1alpha1.ContainerNetwork{{Network: "a"}}}})
	put("LXCContainer", "infra/gone", "33333333-0000-4000-8000-000000000003", computev1alpha1.LXCContainer{
		Spec: computev1alpha1.LXCContainerSpec{Networks: []computev1alpha1.ContainerNetwork{{Network: "a"}}}})
	if err := store.MarkDeleted("LXCContainer", "infra/gone"); err != nil {
		t.Fatal(err)
	}

	ports, err := readPorts(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ports {
		got = append(got, p.Name+"="+p.Instance.Kind+":"+p.Instance.Namespace+"/"+p.Instance.Name+"@"+p.Instance.Node)
	}
	want := "ay11111111n0=LibvirtDomain:infra/vm1@n1 ay11111111n1=LibvirtDomain:infra/vm1@n1 ay22222222n0=LXCContainer:infra/ct1@n1"
	if strings.Join(got, " ") != want {
		t.Errorf("ports = %v, want %s (deleted instances excluded)", got, want)
	}
}

func TestRenderRules(t *testing.T) {
	rules := renderRules([]binding{{port: "ay11111111n0", bridge: "br0", mac: "aa:bb:cc:dd:ee:ff", slot: 3}})
	for _, want := range []string{
		"destroy table bridge ayatori-imds\n",
		"arp operation request arp daddr ip 169.254.169.254 drop",
		`iifname "ay11111111n0" ip daddr 169.254.169.254 tcp dport 80 meta mark set 0x1a70003 ether daddr set aa:bb:cc:dd:ee:ff meta pkttype set host`,
		`meta mark 0x1a70003 oifname != "ay11111111n0" drop`,
		"meta mark 0x1a70003 ip daddr 169.254.169.254 tcp dport 80 ct mark set meta mark dnat to 169.254.169.254:16903",
		"ct mark & 0xffff0000 == 0x1a70000 meta mark set ct mark",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules lack %q:\n%s", want, rules)
		}
	}
}

func TestHandler(t *testing.T) {
	now := time.Unix(1000, 0)
	h := newHandler(Instance{Kind: "LXCContainer", Namespace: "infra", Name: "ct1", UID: "uid-1", Node: "n1"})
	h.now = func() time.Time { return now }
	do := func(method, path string, header map[string]string) (int, string) {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		body, _ := io.ReadAll(w.Result().Body)
		return w.Code, string(body)
	}
	if code, _ := do(http.MethodGet, "/latest/meta-data/instance-id", nil); code != http.StatusUnauthorized {
		t.Errorf("GET without token = %d", code)
	}
	if code, _ := do(http.MethodPut, "/latest/api/token", nil); code != http.StatusBadRequest {
		t.Errorf("PUT without TTL = %d", code)
	}
	if code, _ := do(http.MethodPut, "/latest/api/token", map[string]string{tokenTTLHeader: "21601"}); code != http.StatusBadRequest {
		t.Errorf("PUT with TTL over 6h = %d", code)
	}
	if code, _ := do(http.MethodPut, "/latest/api/token", map[string]string{tokenTTLHeader: "60", "X-Forwarded-For": "1.2.3.4"}); code != http.StatusForbidden {
		t.Errorf("PUT through a proxy = %d", code)
	}
	if code, _ := do(http.MethodGet, "/latest/api/token", map[string]string{tokenTTLHeader: "60"}); code != http.StatusMethodNotAllowed {
		t.Errorf("GET token = %d", code)
	}
	code, token := do(http.MethodPut, "/latest/api/token", map[string]string{tokenTTLHeader: "60"})
	if code != http.StatusOK || len(token) != 64 {
		t.Fatalf("PUT token = %d %q", code, token)
	}
	auth := map[string]string{tokenHeader: token}
	for path, want := range map[string]string{
		"/latest/meta-data/instance-id":    "uid-1",
		"/latest/meta-data/local-hostname": "ct1",
		"/latest/meta-data/namespace":      "infra",
		"/latest/meta-data/node":           "n1",
	} {
		if code, body := do(http.MethodGet, path, auth); code != http.StatusOK || body != want {
			t.Errorf("GET %s = %d %q, want %q", path, code, body, want)
		}
	}
	if code, body := do(http.MethodGet, "/latest/meta-data/", auth); code != http.StatusOK || !strings.Contains(body, "instance-id\n") {
		t.Errorf("listing = %d %q", code, body)
	}
	if code, _ := do(http.MethodGet, "/latest/meta-data/nope", auth); code != http.StatusNotFound {
		t.Errorf("unknown key = %d", code)
	}
	if code, _ := do(http.MethodGet, "/latest/meta-data/instance-id", map[string]string{tokenHeader: "forged"}); code != http.StatusUnauthorized {
		t.Errorf("forged token = %d", code)
	}
	now = now.Add(61 * time.Second)
	if code, _ := do(http.MethodGet, "/latest/meta-data/instance-id", auth); code != http.StatusUnauthorized {
		t.Errorf("expired token = %d", code)
	}
}
