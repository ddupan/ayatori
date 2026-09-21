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

import "errors"

// Identity identifies one registration, not a physical PostgreSQL server.
// UID is opaque; a recreated resource with the same name has a different identity.
// Its zero value is invalid and must be rejected when constructing an aggregate.
type Identity struct {
	uid  string
	name string
}

func NewIdentity(uid, name string) (Identity, error) {
	identity := Identity{uid: uid, name: name}
	if err := identity.Validate(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (i Identity) UID() string  { return i.uid }
func (i Identity) Name() string { return i.name }

// Validate checks registration values without looking up any external identity.
func (i Identity) Validate() error {
	if i.uid == "" {
		return errors.New("instance UID is required")
	}
	if len(i.name) > 253 || !dnsSubdomainName.MatchString(i.name) {
		return errors.New("instance name must be a valid DNS subdomain of at most 253 characters")
	}
	return nil
}

// Revision is a positive configuration generation, separate from Identity.
// Zero is invalid for desired configuration; an unobserved status generation of
// zero must be represented separately when the aggregate is implemented.
type Revision struct {
	value int64
}

func NewRevision(value int64) (Revision, error) {
	revision := Revision{value: value}
	if err := revision.Validate(); err != nil {
		return Revision{}, err
	}
	return revision, nil
}

func (r Revision) Value() int64 { return r.value }

func (r Revision) Validate() error {
	if r.value <= 0 {
		return errors.New("instance revision must be positive")
	}
	return nil
}
