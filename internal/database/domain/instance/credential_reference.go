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

import (
	"errors"
	"regexp"
)

// CredentialReferenceValues contains effective field mappings, not secret data.
// The application supplies defaults and fixes the namespace to the controller's.
// Namespace and provider-specific paths are deliberately not selectable here.
type CredentialReferenceValues struct {
	Name        string
	UsernameKey string
	PasswordKey string
}

// CredentialReference is an immutable reference to a management Secret.
// Its zero value is invalid; aggregate construction must Validate incoming values.
type CredentialReference struct {
	values CredentialReferenceValues
}

// Instance and Secret names share the DNS subdomain syntax and 253-character limit.
var dnsSubdomainName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func NewCredentialReference(values CredentialReferenceValues) (CredentialReference, error) {
	reference := CredentialReference{values: values}
	if err := reference.Validate(); err != nil {
		return CredentialReference{}, err
	}
	return reference, nil
}

// Values returns a copy of the reference, never secret contents.
func (r CredentialReference) Values() CredentialReferenceValues { return r.values }

// Validate enforces reference invariants without accessing Kubernetes or OpenBao.
// Checking that the referenced Secret contains nonempty credentials is an application
// responsibility. Errors omit input values and no implicit defaults are applied.
func (r CredentialReference) Validate() error {
	if len(r.values.Name) > 253 || !dnsSubdomainName.MatchString(r.values.Name) {
		return errors.New("management Secret name must be a valid DNS subdomain of at most 253 characters")
	}
	if r.values.UsernameKey == "" {
		return errors.New("management Secret username field is required")
	}
	if r.values.PasswordKey == "" {
		return errors.New("management Secret password field is required")
	}
	return nil
}
