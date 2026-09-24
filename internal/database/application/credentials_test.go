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

package application

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const testUsername = "test-user"

func TestCredentialsRejectEmptyValues(t *testing.T) {
	for _, values := range [][2]string{
		{"", "test-password"},
		{testUsername, ""},
		{"", ""},
	} {
		if _, err := NewCredentials(values[0], values[1]); err != ErrCredentialsInvalid {
			t.Fatal("empty credential was accepted")
		}
	}
}

func TestCredentialAndServiceFormattingIsRedacted(t *testing.T) {
	const canary = "SECRET-CANARY-never-log-this"
	credentials, err := NewCredentials(canary, canary)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username() != canary || credentials.Password() != canary {
		t.Fatal("explicit credential access changed values")
	}

	service, err := NewInstanceService(&sourceStub{credentials: credentials}, &connectorStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{
		string(encoded),
		fmt.Sprintf("%v %+v %#v", credentials, credentials, credentials),
		fmt.Sprintf("%v %+v %#v", service, service, service),
	}
	for _, output := range outputs {
		if strings.Contains(output, canary) {
			t.Fatal("formatting leaked credential data")
		}
	}
}
