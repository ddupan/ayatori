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
	"slices"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const (
	testUUID    = "uuid-ossp"
	testTrigram = "pg_trgm"
	testVector  = "vector"
	testChanged = "changed"
)

// Acceptance: docs/database/domain-instance.md, extension support is based on observations,
// not a name regexp or an administrator allowlist.
func TestExtensionSupportDecisions(t *testing.T) {
	available := instance.ObserveExtensionSupport([]string{testTrigram, testUUID})
	cases := []struct {
		name        string
		support     instance.ExtensionSupport
		requested   []string
		want        instance.ExtensionDecision
		unsupported []string
	}{
		{"unobserved", instance.ExtensionSupport{}, []string{testTrigram}, instance.ExtensionSupportUnobserved, nil},
		{"observed empty", instance.ObserveExtensionSupport(nil), []string{testTrigram}, instance.ExtensionsUnsupported, []string{testTrigram}},
		{"empty request", instance.ExtensionSupport{}, nil, instance.ExtensionsAccepted, nil},
		{"supported", available, []string{testUUID, testTrigram, testTrigram}, instance.ExtensionsAccepted, nil},
		{"unsupported", available, []string{testVector, "hstore", testVector, testTrigram},
			instance.ExtensionsUnsupported, []string{"hstore", testVector}},
		{"exact names", available, []string{"PG_TRGM"}, instance.ExtensionsUnsupported, []string{"PG_TRGM"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.support.Check(instance.NewExtensionSet(tc.requested))
			if result.Decision != tc.want || !slices.Equal(result.Unsupported, tc.unsupported) {
				t.Fatalf("Check() = %v, want %v / %v", result, tc.want, tc.unsupported)
			}
		})
	}
}

func TestExtensionSetCopiesAndCanonicalizesNames(t *testing.T) {
	input := []string{testUUID, testTrigram, testUUID}
	set := instance.NewExtensionSet(input)
	input[0] = testChanged
	names := set.Names()
	want := []string{testTrigram, testUUID}
	if !slices.Equal(names, want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	names[0] = testChanged
	if !slices.Equal(set.Names(), want) {
		t.Fatal("returned slice mutated set")
	}
	if len((instance.ExtensionSet{}).Names()) != 0 {
		t.Fatal("zero set must be empty")
	}
	// Names are preserved exactly; actual server support, not a local regexp, is decisive.
	unusual := []string{"Vendor.Extension", testUUID}
	if result := instance.ObserveExtensionSupport(unusual).Check(instance.NewExtensionSet(unusual)); result.Decision != instance.ExtensionsAccepted {
		t.Fatal("imposed a local name restriction")
	}
}

func TestExtensionSupportCopiesObservationAndResults(t *testing.T) {
	input := []string{testTrigram}
	support := instance.ObserveExtensionSupport(input)
	input[0] = testVector
	requested := instance.NewExtensionSet([]string{testTrigram, testVector})
	result := support.Check(requested)
	if !slices.Equal(result.Unsupported, []string{testVector}) {
		t.Fatal("input mutation changed observation")
	}
	result.Unsupported[0] = testChanged
	again := support.Check(requested)
	if !slices.Equal(again.Unsupported, []string{testVector}) {
		t.Fatal("result mutation changed subsequent decision")
	}
	// Replacing an observation does not mutate the old value or produce uninstall actions.
	empty := instance.ObserveExtensionSupport(nil)
	if empty.Check(requested).Decision != instance.ExtensionsUnsupported {
		t.Fatal("empty observation ignored")
	}
	if support.Check(instance.NewExtensionSet([]string{testTrigram})).Decision != instance.ExtensionsAccepted {
		t.Fatal("new observation mutated old value")
	}
}
