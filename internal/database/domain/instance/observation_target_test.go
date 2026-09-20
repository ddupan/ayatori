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
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func targetParts(t *testing.T) (instance.Identity, instance.Revision, instance.Definition) {
	t.Helper()
	identity, err := instance.NewIdentity("uid-1", "shared")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := instance.NewRevision(1)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := instance.NewEndpoint(validEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := instance.NewCredentialReference(validCredentialReference())
	if err != nil {
		t.Fatal(err)
	}
	definition, err := instance.NewDefinition(endpoint, credential)
	if err != nil {
		t.Fatal(err)
	}
	return identity, revision, definition
}

func TestDefinitionRejectsInvalidComponents(t *testing.T) {
	_, _, definition := targetParts(t)
	cases := []struct {
		endpoint   instance.Endpoint
		credential instance.CredentialReference
	}{
		{instance.Endpoint{}, definition.AdminCredential()},
		{definition.Endpoint(), instance.CredentialReference{}},
	}
	for _, tc := range cases {
		value, err := instance.NewDefinition(tc.endpoint, tc.credential)
		if err == nil || value != (instance.Definition{}) {
			t.Fatal("invalid component accepted or partial value returned")
		}
	}
	if err := (instance.Definition{}).Validate(); err == nil {
		t.Fatal("zero definition accepted")
	}
}

func TestObservationTargetRejectsInvalidComponents(t *testing.T) {
	identity, revision, definition := targetParts(t)
	cases := []struct {
		identity   instance.Identity
		revision   instance.Revision
		definition instance.Definition
	}{
		{instance.Identity{}, revision, definition},
		{identity, instance.Revision{}, definition},
		{identity, revision, instance.Definition{}},
	}
	for _, tc := range cases {
		value, err := instance.NewObservationTarget(tc.identity, tc.revision, tc.definition)
		if err == nil || value != (instance.ObservationTarget{}) {
			t.Fatal("invalid component accepted or partial target returned")
		}
	}
	zero := instance.ObservationTarget{}
	if err := zero.Validate(); err == nil {
		t.Fatal("zero target accepted")
	}
	if zero.Matches(zero) {
		t.Fatal("two invalid targets must not authorize observation reuse")
	}
}

// Acceptance: docs/database/domain-instance.md §2/§6, observations cannot cross target bindings.
func TestObservationTargetMatchesOnlySameBinding(t *testing.T) {
	identity, revision, definition := targetParts(t)
	original, err := instance.NewObservationTarget(identity, revision, definition)
	if err != nil {
		t.Fatal(err)
	}
	same, err := instance.NewObservationTarget(identity, revision, definition)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Matches(same) || original.Identity() != identity ||
		original.Revision() != revision || original.Definition() != definition {
		t.Fatal("target did not preserve its declared binding")
	}
	changedIdentity, err := instance.NewIdentity("uid-2", identity.Name())
	if err != nil {
		t.Fatal(err)
	}
	changedRevision, err := instance.NewRevision(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, parts := range []struct {
		identity instance.Identity
		revision instance.Revision
	}{{changedIdentity, revision}, {identity, changedRevision}} {
		changed, err := instance.NewObservationTarget(parts.identity, parts.revision, definition)
		if err != nil {
			t.Fatal(err)
		}
		if original.Matches(changed) || changed.Matches(original) {
			t.Fatal("different registration or revision matched")
		}
	}
	endpointValues := definition.Endpoint().Values()
	endpointValues.Host = "other.example"
	endpoint, err := instance.NewEndpoint(endpointValues)
	if err != nil {
		t.Fatal(err)
	}
	credentialValues := definition.AdminCredential().Values()
	credentialValues.PasswordKey = "replacement"
	credential, err := instance.NewCredentialReference(credentialValues)
	if err != nil {
		t.Fatal(err)
	}
	for _, components := range []struct {
		endpoint   instance.Endpoint
		credential instance.CredentialReference
	}{{endpoint, definition.AdminCredential()}, {definition.Endpoint(), credential}} {
		changedDefinition, err := instance.NewDefinition(components.endpoint, components.credential)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := instance.NewObservationTarget(identity, revision, changedDefinition)
		if err != nil {
			t.Fatal(err)
		}
		if original.Matches(changed) {
			t.Fatal("changed definition matched even with the same revision")
		}
	}
	if original.Matches(instance.ObservationTarget{}) {
		t.Fatal("valid target matched zero target")
	}
	if !original.Matches(same) {
		t.Fatal("constructing changed targets mutated the original")
	}
}
