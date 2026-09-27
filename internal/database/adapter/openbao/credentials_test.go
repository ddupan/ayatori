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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"

	bao "github.com/openbao/openbao/api/v2"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
)

const (
	credentialPath  = "applications/database-uid"
	fixturePassword = "AYATORI-TEST-ONLY-application-password"
	fixtureToken    = "AYATORI-TEST-ONLY-bao-token"
	kvDataKey       = "data"
	kvVersionKey    = "version"
)

func fixtureCredential(t *testing.T) credentialdomain.ApplicationCredential {
	t.Helper()
	credential, err := credentialdomain.ParseApplicationCredential(map[string]any{
		"username": "app_owner", "password": fixturePassword, "database": "app",
		"host": "postgres.example", "hostaddr": "192.0.2.1", "port": "5432", "sslmode": "verify-full",
	})
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestCredentialReadbackMustConfirmTheWrite(t *testing.T) {
	for _, scenario := range []string{"read failure", "changed version", "changed password", "missing metadata"} {
		t.Run(scenario, func(t *testing.T) {
			credential := fixtureCredential(t)
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					writes.Add(1)
					var request struct {
						Options struct {
							CAS *int `json:"cas"`
						} `json:"options"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil || request.Options.CAS == nil || *request.Options.CAS != 0 {
						t.Error("create request must explicitly require CAS=0")
					}
					if err := json.NewEncoder(w).Encode(map[string]any{kvDataKey: map[string]any{kvVersionKey: 1}}); err != nil {
						t.Error("cannot encode fixture write response")
					}
					return
				}
				if scenario == "read failure" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				data := credential.SecretData()
				version := 1
				if scenario == "changed version" {
					version = 2
				}
				if scenario == "changed password" {
					data["password"] = "modified"
				}
				response := map[string]any{kvDataKey: data}
				if scenario != "missing metadata" {
					response["metadata"] = map[string]any{kvVersionKey: version}
				}
				if err := json.NewEncoder(w).Encode(map[string]any{kvDataKey: response}); err != nil {
					t.Error("cannot encode fixture read response")
				}
			}))
			defer server.Close()
			store := fixtureStore(t, fixtureClient(t, server.URL))
			if err := store.Create(t.Context(), credentialPath, credential); err != openbao.ErrUncertain || writes.Load() != 1 {
				t.Fatal("unconfirmed readback must stop after one write")
			}
		})
	}
}

func fixtureClient(t *testing.T, address string) *bao.Client {
	t.Helper()
	config := bao.DefaultConfig()
	config.Address = address
	config.MaxRetries = 0
	client, err := bao.NewClient(config)
	if err != nil {
		t.Fatal("cannot construct fixture client")
	}
	client.SetToken(fixtureToken)
	return client
}

func fixtureStore(t *testing.T, client *bao.Client) *openbao.Credentials {
	t.Helper()
	store, err := openbao.NewCredentials(client, "secret", "applications")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCredentialLocationScope(t *testing.T) {
	client := fixtureClient(t, "http://127.0.0.1:1")
	store := fixtureStore(t, client)
	path, err := store.ProvisionPath("database-uid")
	if err != nil || path != credentialPath {
		t.Fatal("unexpected stable location")
	}
	for _, path := range []string{"", "/absolute", "applications", "applications-other/key", "applications/../management", "applications/%2e%2e/key", "applications//key", "applications/data/key"} {
		if _, err := store.Read(t.Context(), path); !errors.Is(err, openbao.ErrInvalidLocation) {
			t.Fatal("accepted invalid location")
		}
		if err := store.Create(t.Context(), path, fixtureCredential(t)); !errors.Is(err, openbao.ErrInvalidLocation) {
			t.Fatal("accepted invalid create location")
		}
	}
	for _, uid := range []string{"", "../key", "a/b", "a?b"} {
		if _, err := store.ProvisionPath(uid); err == nil {
			t.Fatal("accepted invalid UID")
		}
	}
	for _, invalid := range []string{"", "data", "metadata", "../secret", "secret/", "secret?query"} {
		if _, err := openbao.NewCredentials(client, invalid, "applications"); err == nil {
			t.Fatal("accepted invalid mount")
		}
		if _, err := openbao.NewCredentials(client, "secret", invalid); err == nil {
			t.Fatal("accepted invalid base path")
		}
	}
}

func TestCredentialWriteFailureDoesNotRetryOrLeak(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, fixturePassword+fixtureToken, http.StatusInternalServerError)
	}))
	defer server.Close()
	store := fixtureStore(t, fixtureClient(t, server.URL))
	if err := store.Create(t.Context(), credentialPath, fixtureCredential(t)); err != openbao.ErrUncertain {
		t.Fatal("write error must be a redacted uncertain outcome")
	}
	if requests.Load() != 1 {
		t.Fatal("SDK retried an uncertain write")
	}
	if _, err := store.Read(t.Context(), credentialPath); err != openbao.ErrUnavailable {
		t.Fatal("read error must be redacted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Create(ctx, credentialPath, fixtureCredential(t)); err != openbao.ErrUnavailable || requests.Load() != 2 {
		t.Fatal("canceled operation must not write")
	}
}

func TestCredentialWriteDeniedBeforeExecution(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fixtureToken, status)
		}))
		store := fixtureStore(t, fixtureClient(t, server.URL))
		err := store.Create(t.Context(), credentialPath, fixtureCredential(t))
		server.Close()
		if err != openbao.ErrUnavailable {
			t.Fatalf("status %d: definite rejection should wait for dependency recovery, got %v", status, err)
		}
	}
}
