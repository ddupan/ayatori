package agentstatus

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"haproxyUp":true,"neighbors":[{"address":"10.60.0.1","established":true}],` +
			`"vips":[{"address":"10.62.1.1","announced":false,"reason":"not-advertised"}]}`))
	}))
	defer server.Close()
	client := Client{HTTP: server.Client()}
	status, err := client.Status(t.Context(), server.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if !status.HAProxyUp || len(status.VIPs) != 1 || status.VIPs[0].Address != netip.MustParseAddr("10.62.1.1") || status.VIPs[0].Reason != "not-advertised" {
		t.Fatalf("got %+v", status)
	}
	if _, err := client.Status(t.Context(), server.URL+"/missing"); err == nil {
		t.Fatal("非 200 应返回错误")
	}
}
