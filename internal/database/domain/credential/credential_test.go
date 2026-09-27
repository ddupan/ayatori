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

package credential_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func TestApplicationCredential(t *testing.T) {
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host: "postgres.example", HostAddr: "192.0.2.1", Port: 5432,
		ManagementDatabase: "postgres", TLSMode: instance.TLSVerifyFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := credentialdomain.GenerateApplicationCredential("owner", "app", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	second, err := credentialdomain.GenerateApplicationCredential("owner", "app", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	data := first.SecretData()
	if len(data) != 7 || data["password"] == second.SecretData()["password"] || len(data["password"].(string)) != 43 {
		t.Fatal("expected seven keys and independent 256-bit passwords")
	}
	parsed, err := credentialdomain.ParseApplicationCredential(data)
	if err != nil || !maps.Equal(parsed.SecretData(), data) {
		t.Fatal("credential did not round trip")
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{fmt.Sprint(first), fmt.Sprintf("%+v", first), fmt.Sprintf("%#v", first), string(encoded)} {
		if strings.Contains(output, data["password"].(string)) {
			t.Fatal("credential formatting leaked the password")
		}
	}
	data["password"] = "changed"
	if first.SecretData()["password"] == "changed" {
		t.Fatal("caller mutated credential")
	}
	for key := range data {
		invalid := maps.Clone(data)
		delete(invalid, key)
		if _, err := credentialdomain.ParseApplicationCredential(invalid); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
		invalid[key] = 42
		if _, err := credentialdomain.ParseApplicationCredential(invalid); err == nil {
			t.Fatalf("accepted non-string %s", key)
		}
	}
	if (credentialdomain.ApplicationCredential{}).Validate() == nil {
		t.Fatal("accepted zero credential")
	}
}
