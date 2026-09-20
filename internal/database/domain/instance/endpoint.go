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

// Package instance contains the pure domain model of a registered PostgreSQL instance.
// It does not depend on Kubernetes types, database drivers or credential providers.
package instance

import (
	"errors"
	"net/netip"
	"regexp"
)

// TLSMode is an explicit transport policy, not a driver-specific default.
type TLSMode string

const (
	TLSDisable    TLSMode = "disable"
	TLSRequire    TLSMode = "require"
	TLSVerifyCA   TLSMode = "verify-ca"
	TLSVerifyFull TLSMode = "verify-full"
)

// EndpointValues carries explicit, effective values across the application boundary.
// Defaults are supplied by the API/application mapping, never silently by the domain.
type EndpointValues struct {
	Host               string
	HostAddr           string
	Port               int
	ManagementDatabase string
	TLSMode            TLSMode
}

// Endpoint is an immutable connection target. Equality compares its declared values,
// not physical server identity. Its zero value is invalid; aggregate construction
// must Validate incoming endpoints, even if callers bypass NewEndpoint.
type Endpoint struct {
	values EndpointValues
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func NewEndpoint(values EndpointValues) (Endpoint, error) {
	endpoint := Endpoint{values: values}
	if err := endpoint.Validate(); err != nil {
		return Endpoint{}, err
	}
	return endpoint, nil
}

// Values returns a copy, without exposing mutable state.
func (e Endpoint) Values() EndpointValues { return e.values }

// Validate checks local invariants only; it does not resolve DNS or perform IO.
// Errors intentionally omit input values.
func (e Endpoint) Validate() error {
	if e.values.Host == "" {
		return errors.New("endpoint host is required")
	}
	address, err := netip.ParseAddr(e.values.HostAddr)
	if err != nil || address.Zone() != "" {
		return errors.New("endpoint host address must be an IPv4 or IPv6 address")
	}
	if e.values.Port < 1 || e.values.Port > 65535 {
		return errors.New("endpoint port must be between 1 and 65535")
	}
	if !identifier.MatchString(e.values.ManagementDatabase) {
		return errors.New("endpoint management database must be a valid PostgreSQL identifier")
	}
	switch e.values.TLSMode {
	case TLSDisable, TLSRequire, TLSVerifyCA, TLSVerifyFull:
		return nil
	default:
		return errors.New("endpoint TLS mode must be explicitly supported")
	}
}
