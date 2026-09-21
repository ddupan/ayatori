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

// Acceptance: docs/database/domain-instance.md §2/§3, extension observations are
// bound to the current registration and definition and never authorize provisioning.
func TestInstanceAcceptsExtensionObservationForCurrentTarget(t *testing.T) {
	value := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseValidating}, false)
	target := value.Target()
	snapshot := value.Snapshot()

	if err := value.ObserveExtensions(target, instance.ObserveExtensionSupport([]string{testTrigram})); err != nil {
		t.Fatal(err)
	}
	if got := value.CheckExtensions(instance.NewExtensionSet([]string{testTrigram})); got.Decision != instance.ExtensionsAccepted {
		t.Fatalf("CheckExtensions() = %v, want accepted", got)
	}
	if value.Snapshot() != snapshot {
		t.Fatal("extension observation changed the persisted lifecycle snapshot")
	}
}

func TestInstanceRejectsExtensionObservationForDifferentTarget(t *testing.T) {
	value := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseValidating}, false)
	identity, _, definition := targetParts(t)
	revision, err := instance.NewRevision(value.Target().Revision().Value() + 1)
	if err != nil {
		t.Fatal(err)
	}
	different, err := instance.NewObservationTarget(identity, revision, definition)
	if err != nil {
		t.Fatal(err)
	}

	if err := value.ObserveExtensions(different, instance.ObserveExtensionSupport([]string{testTrigram})); err == nil {
		t.Fatal("observation for a different target was accepted")
	}
	if got := value.CheckExtensions(instance.NewExtensionSet([]string{testTrigram})); got.Decision != instance.ExtensionSupportUnobserved {
		t.Fatalf("rejected observation changed support: %v", got)
	}
}

func TestInstanceClearsExtensionObservationAcrossLifecycleBoundaries(t *testing.T) {
	requested := instance.NewExtensionSet([]string{testTrigram})

	t.Run("validation", func(t *testing.T) {
		value := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseReady}, false)
		if err := value.ObserveExtensions(value.Target(), instance.ObserveExtensionSupport([]string{testTrigram})); err != nil {
			t.Fatal(err)
		}
		if err := value.BeginValidation(); err != nil {
			t.Fatal(err)
		}
		if got := value.CheckExtensions(requested); got.Decision != instance.ExtensionSupportUnobserved {
			t.Fatalf("validation retained stale support: %v", got)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		value := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseReady}, true)
		if err := value.ObserveExtensions(value.Target(), instance.ObserveExtensionSupport([]string{testTrigram})); err == nil {
			t.Fatal("deleting instance accepted a new observation")
		}
		if err := value.BeginDeletion(); err != nil {
			t.Fatal(err)
		}
		if got := value.CheckExtensions(requested); got.Decision != instance.ExtensionSupportUnobserved {
			t.Fatalf("deletion retained extension support: %v", got)
		}
	})
}

func TestInstanceCanExplicitlyInvalidateExtensionObservation(t *testing.T) {
	value := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseValidating}, false)
	requested := instance.NewExtensionSet([]string{testTrigram})
	if err := value.ObserveExtensions(value.Target(), instance.ObserveExtensionSupport([]string{testTrigram})); err != nil {
		t.Fatal(err)
	}
	if err := value.ObserveExtensions(value.Target(), instance.ExtensionSupport{}); err != nil {
		t.Fatal(err)
	}
	if got := value.CheckExtensions(requested); got.Decision != instance.ExtensionSupportUnobserved {
		t.Fatalf("explicit invalidation retained stale support: %v", got)
	}
}
