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
	"math"
	"strings"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// Acceptance: docs/database/domain-instance.md §2: registration identity is UID + name,
// independent of the configuration revision or physical PostgreSQL server.
func TestIdentityDistinguishesRecreatedInstances(t *testing.T) {
	first, err := instance.NewIdentity("opaque-uid-1", "shared")
	if err != nil {
		t.Fatal(err)
	}
	same, err := instance.NewIdentity("opaque-uid-1", "shared")
	if err != nil {
		t.Fatal(err)
	}
	recreated, err := instance.NewIdentity("opaque-uid-2", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if first != same || first == recreated {
		t.Fatal("identity must distinguish same-name registrations by UID")
	}
	if first.UID() != "opaque-uid-1" || first.Name() != "shared" {
		t.Fatal("identity changed declared values")
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityValidation(t *testing.T) {
	for _, name := range []string{"", "Shared", "shared_name", "ns/shared", "-shared", "pg..shared", strings.Repeat("a", 254)} {
		identity, err := instance.NewIdentity("uid", name)
		if err == nil || identity != (instance.Identity{}) {
			t.Fatal("invalid name accepted or partial identity returned")
		}
	}
	if _, err := instance.NewIdentity("", "shared"); err == nil {
		t.Fatal("empty UID accepted")
	}
	if err := (instance.Identity{}).Validate(); err == nil {
		t.Fatal("zero identity accepted")
	}
	for _, name := range []string{"a", "1", "pg.shared-1", strings.Repeat("a", 253)} {
		if _, err := instance.NewIdentity("opaque-not-a-uuid", name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRevisionRequiresPositiveValue(t *testing.T) {
	for _, value := range []int64{math.MinInt64, -1, 0} {
		revision, err := instance.NewRevision(value)
		if err == nil || revision != (instance.Revision{}) {
			t.Fatal("invalid revision accepted or partial value returned")
		}
	}
	for _, value := range []int64{1, 2, math.MaxInt64} {
		revision, err := instance.NewRevision(value)
		if err != nil {
			t.Fatal(err)
		}
		if revision.Value() != value {
			t.Fatal("revision changed declared value")
		}
		if err := revision.Validate(); err != nil {
			t.Fatal(err)
		}
		same, err := instance.NewRevision(value)
		if err != nil {
			t.Fatal(err)
		}
		if revision != same {
			t.Fatal("identical revisions must compare equal")
		}
	}
	if err := (instance.Revision{}).Validate(); err == nil {
		t.Fatal("zero revision accepted")
	}
}
