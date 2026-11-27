package ayatoriclient

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
)

// jwt 只有 exp：RESTConfig 只读它决定何时换新。
func jwt(n int, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, exp.Unix()))
	return fmt.Sprintf("h.%s.%d", payload, n)
}

// 有效期内的 token 被复用；快过期的每次请求都换新。
func TestRESTConfigCachesToken(t *testing.T) {
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"major":"1","minor":"37"}`))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	for _, c := range []struct {
		name    string
		ttl     time.Duration
		fetches int
	}{{"valid", time.Hour, 1}, {"expiring", refreshEarly / 2, 3}} {
		t.Run(c.name, func(t *testing.T) {
			seen = nil
			fetches := 0
			config, err := RESTConfig(t.Context(), Profile{Server: server.URL, CertificateAuthority: string(ca)},
				func(context.Context) (string, error) { fetches++; return jwt(fetches, time.Now().Add(c.ttl)), nil })
			if err != nil {
				t.Fatal(err)
			}
			clientset := kubernetes.NewForConfigOrDie(config)
			for range 2 {
				if _, err := clientset.Discovery().ServerVersion(); err != nil {
					t.Fatal(err)
				}
			}
			if fetches != c.fetches {
				t.Errorf("%d fetches, want %d", fetches, c.fetches)
			}
			if (seen[0] == seen[1]) != (c.fetches == 1) {
				t.Errorf("requests sent %q and %q", seen[0], seen[1])
			}
		})
	}
}
