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

package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenBaoClientDoesNotUseEnvironmentIdentityOrAddress(t *testing.T) {
	t.Setenv("BAO_TOKEN", "TEST-ONLY-unwanted-static-token")
	t.Setenv("BAO_ADDR", "http://unwanted.invalid")
	t.Setenv("BAO_SKIP_VERIFY", "true")
	var options openBaoOptions
	options.bindFlags(flag.NewFlagSet("test", flag.ContinueOnError))
	options.address = "https://bao.example/"
	client, err := options.client()
	if err != nil {
		t.Fatal(err)
	}
	if client.Address() != "https://bao.example" || client.Token() != "" {
		t.Fatal("ambient environment replaced the explicit connection or identity")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	options.address = server.URL
	untrusted, err := options.client()
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
		if _, err := (openBaoOptions{address: address}).client(); err == nil {
			t.Fatal("accepted unsafe OpenBao address")
		}
	}
	if _, err := (openBaoOptions{address: "https://bao.example", caCert: "/nonexistent/fixture-ca"}).client(); err == nil {
		t.Fatal("accepted missing explicit CA")
	}
}
