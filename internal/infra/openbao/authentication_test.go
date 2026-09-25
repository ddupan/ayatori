/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openbao_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	kubeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/openbao"
)

const (
	authRole     = "controller"
	fixtureToken = "AYATORI-TEST-ONLY-bao-token"
)

func fixtureClient(t *testing.T, address string) *bao.Client {
	t.Helper()
	config := bao.NewConfig()
	config.Address = address
	client, err := bao.NewClient(config)
	if err != nil {
		t.Fatal("cannot construct fixture client")
	}
	client.SetToken(fixtureToken)
	return client
}

var testIdentity = openbao.KubernetesIdentity{Namespace: "bao-controller", ServiceAccount: authRole, Audience: "openbao"}

func authenticationClient(t *testing.T, address string) kubeclient.Client {
	t.Helper()
	// HTTP 单元 fixture 只提供 TokenRequest；静态映射避免额外模拟 discovery API。
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ServiceAccount"), meta.RESTScopeNamespace)
	client, err := kubeclient.New(&rest.Config{Host: address, ContentType: "application/json"}, kubeclient.Options{
		Mapper: mapper,
	})
	if err != nil {
		t.Fatal("cannot construct fixture Kubernetes client")
	}
	return client
}

func authenticationServer(t *testing.T, token func() (string, bool), login http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/bao-controller/serviceaccounts/controller/token" {
			login(w, r)
			return
		}
		var request authenticationv1.TokenRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || r.Method != http.MethodPost ||
			len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != testIdentity.Audience ||
			request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != 600 {
			t.Error("unexpected TokenRequest target or lifetime")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		jwt, allowed := token()
		if !allowed {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(authenticationv1.TokenRequest{
			APIVersion: "authentication.k8s.io/v1", Kind: "TokenRequest",
			Status: authenticationv1.TokenRequestStatus{Token: jwt, ExpirationTimestamp: metav1.NewTime(time.Now().Add(10 * time.Minute))},
		}); err != nil {
			t.Error("cannot encode fixture TokenRequest response")
		}
	}))
}

func TestKubernetesSessionDoesNotFallbackFromMissingToken(t *testing.T) {
	var requests atomic.Int32
	for _, missing := range []bool{true, false} {
		server := authenticationServer(t, func() (string, bool) { return "", !missing }, func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusForbidden)
		})
		defer server.Close()
		client := fixtureClient(t, server.URL)
		session, err := openbao.NewKubernetesSession(client, authenticationClient(t, server.URL), "kubernetes", authRole, testIdentity)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err = session.Start(ctx)
		cancel()
		if err != nil || session.Ready() || client.Token() != "" || requests.Load() != 0 {
			t.Fatal("denied or empty TokenRequest must not fall back to another identity")
		}
	}
}

func waitForAuthentication(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatal("authentication condition timed out")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func startSession(t *testing.T, session interface{ Start(context.Context) error }) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- session.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("authentication did not stop")
		}
	})
	return cancel
}

func TestKubernetesSessionRequestsNewTokenAfterFailure(t *testing.T) {
	var attempts atomic.Int32
	var accepted atomic.Bool
	var rotated atomic.Bool
	server := authenticationServer(t, func() (string, bool) {
		if rotated.Load() {
			return "rotated-test-jwt", true
		}
		return "expired-test-jwt", true
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/kubernetes/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct{ JWT, Role string }
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Role != authRole {
			t.Error("unexpected authentication request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		attempts.Add(1)
		if request.JWT != "rotated-test-jwt" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		accepted.Store(true)
		if err := json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": fixtureToken, "lease_duration": 60, "renewable": false,
		}}); err != nil {
			t.Error("cannot encode authentication fixture response")
		}
	})
	defer server.Close()
	client := fixtureClient(t, server.URL)
	session, err := openbao.NewKubernetesSession(client, authenticationClient(t, server.URL), "kubernetes", authRole, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if client.Token() != "" {
		t.Fatal("constructor retained preexisting static token")
	}
	cancel := startSession(t, session)
	waitForAuthentication(t, func() bool { return attempts.Load() > 0 })
	if session.Ready() || client.Token() != "" {
		t.Fatal("failed login retained credentials")
	}
	rotated.Store(true)
	waitForAuthentication(t, func() bool { return accepted.Load() && session.Ready() })
	if client.Token() != fixtureToken {
		t.Fatal("successful login did not configure the client")
	}
	if session.Start(t.Context()) == nil {
		t.Fatal("allowed concurrent lifecycle owners")
	}
	cancel()
	waitForAuthentication(t, func() bool { return !session.Ready() && client.Token() == "" })
}

func TestKubernetesSessionRejectsUnboundedLease(t *testing.T) {
	var attempts atomic.Int32
	server := authenticationServer(t, func() (string, bool) { return "test-jwt", true }, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if err := json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": fixtureToken, "lease_duration": 0,
		}}); err != nil {
			t.Error("cannot encode authentication fixture response")
		}
	})
	defer server.Close()
	client := fixtureClient(t, server.URL)
	session, err := openbao.NewKubernetesSession(client, authenticationClient(t, server.URL), "kubernetes", authRole, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, session)
	waitForAuthentication(t, func() bool { return attempts.Load() > 0 })
	if session.Ready() || client.Token() != "" {
		t.Fatal("accepted a token without a finite lease")
	}
}
