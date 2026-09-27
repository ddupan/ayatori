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
	"net/http"
	"net/http/httptest"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/openbao"
)

func TestOpenBaoClientDoesNotUseEnvironmentIdentityOrAddress(t *testing.T) {
	t.Setenv("BAO_TOKEN", "TEST-ONLY-unwanted-static-token")
	t.Setenv("BAO_ADDR", "http://unwanted.invalid")
	t.Setenv("BAO_SKIP_VERIFY", "true")
	client, err := openbao.NewClient("https://bao.example/", "")
	if err != nil {
		t.Fatal(err)
	}
	if client.Address() != "https://bao.example" || client.Token() != "" {
		t.Fatal("ambient environment replaced the explicit connection or identity")
	}
	if client.MaxRetries() != 0 {
		t.Fatal("shared client must not automatically retry uncertain writes")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	untrusted, err := openbao.NewClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	untrusted.SetMaxRetries(0)
	if _, err := untrusted.Sys().HealthWithContext(t.Context()); err == nil {
		t.Fatal("BAO_SKIP_VERIFY bypassed TLS validation")
	}
}

func TestOpenBaoClientRejectsUnsafeConfiguration(t *testing.T) {
	for _, address := range []string{
		"", "http://bao.example", "https://user:secret@bao.example",
		"https://bao.example/?token=secret", "https://bao.example/#secret", "https://bao.example/path",
	} {
		if _, err := openbao.NewClient(address, ""); err == nil {
			t.Fatal("accepted unsafe OpenBao address")
		}
	}
	if _, err := openbao.NewClient("https://bao.example", "/nonexistent/fixture-ca"); err == nil {
		t.Fatal("accepted missing explicit CA")
	}
}
