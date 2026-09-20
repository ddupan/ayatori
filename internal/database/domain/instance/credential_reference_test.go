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

package instance_test

import (
	"strings"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func validCredentialReference() instance.CredentialReferenceValues {
	return instance.CredentialReferenceValues{
		Name: "shared-postgresql-admin", UsernameKey: "username", PasswordKey: "password",
	}
}

// Acceptance: docs/database/domain-instance.md §2. References carry names, never credentials or IO.
func TestCredentialReferenceRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		change func(*instance.CredentialReferenceValues)
	}{
		{"empty name", func(v *instance.CredentialReferenceValues) { v.Name = "" }},
		{"uppercase", func(v *instance.CredentialReferenceValues) { v.Name = "Admin" }},
		{"underscore", func(v *instance.CredentialReferenceValues) { v.Name = "pg_admin" }},
		{"leading hyphen", func(v *instance.CredentialReferenceValues) { v.Name = "-admin" }},
		{"trailing hyphen", func(v *instance.CredentialReferenceValues) { v.Name = "admin-" }},
		{"empty label", func(v *instance.CredentialReferenceValues) { v.Name = "pg..admin" }},
		{"trailing dot", func(v *instance.CredentialReferenceValues) { v.Name = "pg." }},
		{"namespace or path", func(v *instance.CredentialReferenceValues) { v.Name = "system/admin" }},
		{"whitespace", func(v *instance.CredentialReferenceValues) { v.Name = " admin" }},
		{"too long", func(v *instance.CredentialReferenceValues) { v.Name = strings.Repeat("a", 254) }},
		{"empty username key", func(v *instance.CredentialReferenceValues) { v.UsernameKey = "" }},
		{"empty password key", func(v *instance.CredentialReferenceValues) { v.PasswordKey = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := validCredentialReference()
			tc.change(&values)
			reference, err := instance.NewCredentialReference(values)
			if err == nil {
				t.Fatal("invalid credential reference accepted")
			}
			if reference != (instance.CredentialReference{}) {
				t.Fatal("constructor returned a partial reference on failure")
			}
		})
	}
}

func TestCredentialReferencePreservesExplicitValues(t *testing.T) {
	for _, name := range []string{"a", "1", "pg.admin-1", strings.Repeat("a", 253)} {
		values := validCredentialReference()
		values.Name = name
		values.UsernameKey = "PG_USER"
		values.PasswordKey = "pg.password"
		reference, err := instance.NewCredentialReference(values)
		if err != nil {
			t.Fatal(err)
		}
		if reference.Values() != values {
			t.Fatal("constructor changed the explicit field mapping")
		}
		if err := reference.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCredentialReferenceIsAnImmutableComparableValue(t *testing.T) {
	values := validCredentialReference()
	reference, err := instance.NewCredentialReference(values)
	if err != nil {
		t.Fatal(err)
	}
	same, err := instance.NewCredentialReference(values)
	if err != nil {
		t.Fatal(err)
	}
	if reference != same {
		t.Fatal("identical references must compare equal")
	}
	values.Name = "different"
	snapshot := reference.Values()
	snapshot.PasswordKey = "different-key"
	if reference.Values() != validCredentialReference() {
		t.Fatal("caller mutated reference through a copy")
	}
	if err := (instance.CredentialReference{}).Validate(); err == nil {
		t.Fatal("zero reference must be invalid")
	}
}

func TestCredentialReferenceErrorOmitsInput(t *testing.T) {
	values := validCredentialReference()
	values.Name = "canary-sensitive/input"
	_, err := instance.NewCredentialReference(values)
	if err == nil {
		t.Fatal("invalid reference accepted")
	}
	if strings.Contains(err.Error(), "canary") {
		t.Fatal("error included input")
	}
}
