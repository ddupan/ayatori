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

package instance

// Definition is the immutable effective configuration of an Instance.
// Available extensions are observations, not part of the declared configuration.
type Definition struct {
	endpoint        Endpoint
	adminCredential CredentialReference
}

func NewDefinition(endpoint Endpoint, adminCredential CredentialReference) (Definition, error) {
	definition := Definition{endpoint: endpoint, adminCredential: adminCredential}
	if err := definition.Validate(); err != nil {
		return Definition{}, err
	}
	return definition, nil
}

func (d Definition) Endpoint() Endpoint                   { return d.endpoint }
func (d Definition) AdminCredential() CredentialReference { return d.adminCredential }

// Validate rejects invalid zero-value components even when constructors were bypassed.
func (d Definition) Validate() error {
	if err := d.endpoint.Validate(); err != nil {
		return err
	}
	return d.adminCredential.Validate()
}

// ObservationTarget binds facts to a registration and its declared configuration.
// It does not identify a physical PostgreSQL server or prove observation freshness.
// Secret content refresh and same-target observation freshness remain application
// responsibilities; no credentials or Secret contents are carried by this value.
type ObservationTarget struct {
	identity   Identity
	revision   Revision
	definition Definition
}

func NewObservationTarget(identity Identity, revision Revision, definition Definition) (ObservationTarget, error) {
	target := ObservationTarget{identity: identity, revision: revision, definition: definition}
	if err := target.Validate(); err != nil {
		return ObservationTarget{}, err
	}
	return target, nil
}

func (t ObservationTarget) Identity() Identity     { return t.identity }
func (t ObservationTarget) Revision() Revision     { return t.revision }
func (t ObservationTarget) Definition() Definition { return t.definition }

func (t ObservationTarget) Validate() error {
	if err := t.identity.Validate(); err != nil {
		return err
	}
	if err := t.revision.Validate(); err != nil {
		return err
	}
	return t.definition.Validate()
}

// Matches rejects invalid targets before comparing values. Matching is necessary,
// but not sufficient, for the aggregate to accept a fresh capability observation.
func (t ObservationTarget) Matches(other ObservationTarget) bool {
	return t.Validate() == nil && other.Validate() == nil && t == other
}
