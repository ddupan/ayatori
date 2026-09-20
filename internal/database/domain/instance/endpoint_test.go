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

func validEndpoint() instance.EndpointValues {
	return instance.EndpointValues{
		Host: "postgres.home.arpa", HostAddr: "192.0.2.10", Port: 5432,
		ManagementDatabase: "postgres", TLSMode: instance.TLSVerifyFull,
	}
}

// Acceptance: docs/database/domain-instance.md §2, explicit values and no implicit TLS downgrade.
func TestEndpointRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		change func(*instance.EndpointValues)
	}{
		{"empty host", func(v *instance.EndpointValues) { v.Host = "" }},
		{"missing address", func(v *instance.EndpointValues) { v.HostAddr = "" }},
		{"DNS instead of IP", func(v *instance.EndpointValues) { v.HostAddr = "postgres.home.arpa" }},
		{"invalid IP", func(v *instance.EndpointValues) { v.HostAddr = "192.0.2.999" }},
		{"address with port", func(v *instance.EndpointValues) { v.HostAddr = "192.0.2.10:5432" }},
		{"scoped address", func(v *instance.EndpointValues) { v.HostAddr = "fe80::1%eth0" }},
		{"zero port", func(v *instance.EndpointValues) { v.Port = 0 }},
		{"negative port", func(v *instance.EndpointValues) { v.Port = -1 }},
		{"large port", func(v *instance.EndpointValues) { v.Port = 65536 }},
		{"empty database", func(v *instance.EndpointValues) { v.ManagementDatabase = "" }},
		{"uppercase database", func(v *instance.EndpointValues) { v.ManagementDatabase = "Postgres" }},
		{"leading digit", func(v *instance.EndpointValues) { v.ManagementDatabase = "1postgres" }},
		{"punctuation", func(v *instance.EndpointValues) { v.ManagementDatabase = "post-gres" }},
		{"NUL", func(v *instance.EndpointValues) { v.ManagementDatabase = "post\x00gres" }},
		{"long identifier", func(v *instance.EndpointValues) { v.ManagementDatabase = strings.Repeat("a", 64) }},
		{"missing TLS mode", func(v *instance.EndpointValues) { v.TLSMode = "" }},
		{"unsupported TLS mode", func(v *instance.EndpointValues) { v.TLSMode = "prefer" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := validEndpoint()
			tc.change(&values)
			endpoint, err := instance.NewEndpoint(values)
			if err == nil {
				t.Fatal("invalid endpoint accepted")
			}
			if endpoint != (instance.Endpoint{}) {
				t.Fatal("constructor returned a partial endpoint on failure")
			}
		})
	}
}

func TestEndpointPreservesValidValues(t *testing.T) {
	for _, mode := range []instance.TLSMode{
		instance.TLSDisable, instance.TLSRequire, instance.TLSVerifyCA, instance.TLSVerifyFull,
	} {
		for _, address := range []string{"192.0.2.10", "2001:db8::10"} {
			for _, port := range []int{1, 65535} {
				values := validEndpoint()
				values.TLSMode, values.HostAddr, values.Port = mode, address, port
				values.ManagementDatabase = "a" + strings.Repeat("_", 62)
				endpoint, err := instance.NewEndpoint(values)
				if err != nil {
					t.Fatal(err)
				}
				if endpoint.Values() != values {
					t.Fatal("constructor changed explicit values")
				}
				if err := endpoint.Validate(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestEndpointIsAnImmutableComparableValue(t *testing.T) {
	values := validEndpoint()
	endpoint, err := instance.NewEndpoint(values)
	if err != nil {
		t.Fatal(err)
	}
	same, err := instance.NewEndpoint(values)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != same {
		t.Fatal("identical endpoint values must compare equal")
	}
	values.Host = "changed.example"
	snapshot := endpoint.Values()
	snapshot.Host = values.Host
	if endpoint.Values().Host == snapshot.Host {
		t.Fatal("caller mutated endpoint through a copy")
	}
	if err := (instance.Endpoint{}).Validate(); err == nil {
		t.Fatal("zero endpoint must not be valid")
	}
}
