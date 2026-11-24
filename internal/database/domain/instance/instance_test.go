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

	"go.ddupan.top/ayatori/internal/database/domain/instance"
)

func lifecycleInstance(t *testing.T, snapshot instance.Snapshot, deleting bool) *instance.Instance {
	t.Helper()
	identity, revision, definition := targetParts(t)
	target, err := instance.NewObservationTarget(identity, revision, definition)
	if err != nil {
		t.Fatal(err)
	}
	value, err := instance.Reconstitute(target, snapshot, deleting)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// Acceptance: docs/database/domain-instance.md §3, checkpoint reconstruction and intent-only transitions.
func TestReconstituteCheckpoints(t *testing.T) {
	for _, phase := range []instance.Phase{
		instance.PhasePending, instance.PhaseValidating,
		instance.PhaseReady, instance.PhaseDeleting,
	} {
		snapshot := instance.Snapshot{Phase: phase, ObservedRevision: 1, Readiness: instance.Ready, ReportedVersion: "17"}
		value := lifecycleInstance(t, snapshot, false)
		if value.Snapshot() != snapshot {
			t.Fatal("known checkpoint was not preserved")
		}
		// A snapshot is detached; it is not a setter on the aggregate.
		copy := value.Snapshot()
		copy.Phase = instance.PhasePending
		copy.ReportedVersion = "changed"
		if value.Snapshot() != snapshot {
			t.Fatal("snapshot mutation changed aggregate")
		}
	}
	for _, phase := range []instance.Phase{"", "unknown"} {
		value := lifecycleInstance(t, instance.Snapshot{Phase: phase, Readiness: instance.Ready}, false)
		if value.Snapshot().Phase != instance.PhasePending || value.Snapshot().Readiness != instance.Unknown {
			t.Fatal("missing or unknown checkpoint did not restart conservatively")
		}
	}
	if value, err := instance.Reconstitute(instance.ObservationTarget{}, instance.Snapshot{}, false); err == nil || value != nil {
		t.Fatal("invalid target reconstructed an aggregate")
	}
}

func TestBeginValidationPreservesObservedRevision(t *testing.T) {
	snapshot := instance.Snapshot{
		Phase: instance.PhaseReady, ObservedRevision: 0, Readiness: instance.Ready, ReportedVersion: "17",
	}
	value := lifecycleInstance(t, snapshot, false)
	target := value.Target()
	for range 2 {
		if err := value.BeginValidation(); err != nil {
			t.Fatal(err)
		}
		got := value.Snapshot()
		if got.Phase != instance.PhaseValidating || got.Readiness != instance.Unknown ||
			got.ObservedRevision != snapshot.ObservedRevision || got.ReportedVersion != snapshot.ReportedVersion {
			t.Fatal("recording validation intent claimed a completed observation or erased diagnostic version")
		}
	}
	if value.Target() != target {
		t.Fatal("lifecycle action mutated identity or configuration")
	}
}

func TestDeletionRequiresRequestAndPreventsValidation(t *testing.T) {
	snapshot := instance.Snapshot{Phase: instance.PhaseReady, Readiness: instance.Ready, ObservedRevision: 1}
	active := lifecycleInstance(t, snapshot, false)
	if err := active.BeginDeletion(); err == nil {
		t.Fatal("deletion without a request accepted")
	}
	if active.Snapshot() != snapshot {
		t.Fatal("rejected deletion mutated state")
	}
	for _, phase := range []instance.Phase{
		instance.PhasePending, instance.PhaseValidating,
		instance.PhaseReady, instance.PhaseDeleting,
	} {
		snapshot.Phase = phase
		value := lifecycleInstance(t, snapshot, true)
		if err := value.BeginValidation(); err == nil {
			t.Fatal("validation accepted after deletion request")
		}
		if value.Snapshot() != snapshot {
			t.Fatal("rejected validation mutated state")
		}
		for range 2 {
			if err := value.BeginDeletion(); err != nil {
				t.Fatal(err)
			}
			if got := value.Snapshot(); got.Phase != instance.PhaseDeleting || got.Readiness != instance.Unknown ||
				got.ObservedRevision != snapshot.ObservedRevision {
				t.Fatal("incorrect deletion checkpoint")
			}
		}
	}
}

func TestZeroInstanceCannotTransition(t *testing.T) {
	var value instance.Instance
	if err := value.BeginValidation(); err == nil {
		t.Fatal("zero instance started validation")
	}
	if err := value.BeginDeletion(); err == nil {
		t.Fatal("zero instance started deletion")
	}
	if value.Snapshot() != (instance.Snapshot{}) {
		t.Fatal("invalid transition changed zero instance")
	}
}
