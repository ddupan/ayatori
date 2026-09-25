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
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
)

const (
	authRole = "controller"
)

func writeAuthToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		t.Fatal("cannot write temporary projected token")
	}
}

func TestKubernetesSessionDoesNotFallbackFromMissingToken(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	for _, missing := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "token")
		if !missing {
			writeAuthToken(t, path, " \n")
		}
		client := fixtureClient(t, server.URL)
		session, err := openbao.NewKubernetesSession(client, "kubernetes", authRole, path)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err = session.Start(ctx)
		cancel()
		if err != nil || session.Ready() || client.Token() != "" || requests.Load() != 0 {
			t.Fatal("missing or empty token must not fall back to another identity")
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

func startSession(t *testing.T, session *openbao.KubernetesSession) context.CancelFunc {
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

func TestKubernetesSessionRereadsTokenAfterFailure(t *testing.T) {
	var attempts atomic.Int32
	var accepted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	writeAuthToken(t, path, "expired-test-jwt")
	client := fixtureClient(t, server.URL)
	session, err := openbao.NewKubernetesSession(client, "kubernetes", authRole, path)
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
	writeAuthToken(t, path, "rotated-test-jwt")
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if err := json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": fixtureToken, "lease_duration": 0,
		}}); err != nil {
			t.Error("cannot encode authentication fixture response")
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	writeAuthToken(t, path, "test-jwt")
	client := fixtureClient(t, server.URL)
	session, err := openbao.NewKubernetesSession(client, "kubernetes", authRole, path)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, session)
	waitForAuthentication(t, func() bool { return attempts.Load() > 0 })
	if session.Ready() || client.Token() != "" {
		t.Fatal("accepted a token without a finite lease")
	}
}
