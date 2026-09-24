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

const testServerVersion = "17.6"

func completeChecks() instance.ManagementChecks {
	return instance.ManagementChecks{
		Connection: instance.CheckPassed, Metadata: instance.CheckPassed,
		Roles: instance.CheckPassed, Databases: instance.CheckPassed,
		Grants: instance.CheckPassed, Extensions: instance.CheckPassed,
	}
}

func capability(t *testing.T, value *instance.Instance, checks instance.ManagementChecks) instance.CapabilityObservation {
	t.Helper()
	o, err := instance.NewCapabilityObservation(value.Target(), testServerVersion, checks)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func readyInstance(t *testing.T) *instance.Instance {
	t.Helper()
	i := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseValidating}, false)
	if err := i.AssessManagement(capability(t, i, completeChecks())); err != nil {
		t.Fatal(err)
	}
	if err := i.RequireProvisioningReady(); err != nil {
		t.Fatal(err)
	}
	return i
}

func TestReadinessRequiresCompleteReadBack(t *testing.T) {
	i := lifecycleInstance(t, instance.Snapshot{}, false)
	if err := i.BeginValidation(); err != nil {
		t.Fatal(err)
	}
	if i.RequireProvisioningReady() == nil {
		t.Fatal("validation intent authorized provisioning")
	}
	if err := i.AssessManagement(capability(t, i, instance.ManagementChecks{})); err != nil {
		t.Fatal(err)
	}
	if snapshot := i.Snapshot(); snapshot.Phase != instance.PhaseValidating ||
		snapshot.Readiness != instance.NotReady || snapshot.Failure != instance.ObservationIncomplete {
		t.Fatalf("incomplete observation accepted: %+v", snapshot)
	}
	if i.RequireProvisioningReady() == nil {
		t.Fatal("incomplete observation authorized provisioning")
	}
	if err := i.AssessManagement(capability(t, i, completeChecks())); err != nil {
		t.Fatal(err)
	}
	snapshot := i.Snapshot()
	if snapshot.Phase != instance.PhaseReady || snapshot.Readiness != instance.Ready ||
		snapshot.ReportedVersion != testServerVersion ||
		snapshot.ObservedRevision != i.Target().Revision().Value() {
		t.Fatalf("complete management observation did not establish readiness: %+v", snapshot)
	}
	if err := i.RequireProvisioningReady(); err != nil {
		t.Fatal(err)
	}
}

// 重启只恢复 checkpoint；依赖稍后恢复时必须重新取得完整事实。
func TestReadinessRecoveryAndInvalidation(t *testing.T) {
	i := readyInstance(t)
	restored, err := instance.Reconstitute(i.Target(), i.Snapshot(), false)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RequireProvisioningReady() == nil {
		t.Fatal("persisted Ready fabricated fresh evidence")
	}
	for range 2 {
		if err := restored.AssessReadiness(capability(t, restored, completeChecks())); err != nil {
			t.Fatal(err)
		}
		if err := restored.RequireProvisioningReady(); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.BeginValidation(); err != nil {
		t.Fatal(err)
	}
	if restored.RequireProvisioningReady() == nil {
		t.Fatal("validation retained evidence")
	}
	deleted, err := instance.Reconstitute(i.Target(), i.Snapshot(), true)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.RequireProvisioningReady() == nil {
		t.Fatal("deletion allowed provisioning")
	}
	if err := deleted.BeginDeletion(); err != nil {
		t.Fatal(err)
	}
	if deleted.RequireProvisioningReady() == nil {
		t.Fatal("deleting checkpoint allowed provisioning")
	}
}

func TestEachManagementCheckIsRequired(t *testing.T) {
	checkNames := []string{"connection", "metadata", "roles", "databases", "grants", "extensions"}
	failures := []struct {
		name   string
		result instance.CheckResult
		want   instance.Failure
	}{
		{"unobserved", instance.CheckUnobserved, instance.ObservationIncomplete},
		{"unavailable", instance.CheckUnavailable, instance.DependencyUnavailable},
		{"authentication", instance.CheckAuthenticationFailed, instance.AuthenticationFailed},
		{"privileges", instance.CheckInsufficientPrivileges, instance.InsufficientPrivileges},
		{"unknown", 255, instance.ObservationIncomplete},
	}
	for field, name := range checkNames {
		for _, failure := range failures {
			for _, phase := range []instance.Phase{instance.PhaseValidating, instance.PhaseReady} {
				t.Run(name+"/"+failure.name+"/"+string(phase), func(t *testing.T) {
					checks := completeChecks()
					fields := []*instance.CheckResult{
						&checks.Connection, &checks.Metadata, &checks.Roles,
						&checks.Databases, &checks.Grants, &checks.Extensions,
					}
					*fields[field] = failure.result
					value := lifecycleInstance(t, instance.Snapshot{Phase: phase}, false)
					assess := value.AssessManagement
					if phase == instance.PhaseReady {
						value = readyInstance(t)
						assess = value.AssessReadiness
					}
					if err := assess(capability(t, value, checks)); err != nil {
						t.Fatal(err)
					}
					snapshot := value.Snapshot()
					if snapshot.Phase != instance.PhaseValidating ||
						snapshot.Readiness != instance.NotReady ||
						snapshot.Failure != failure.want ||
						snapshot.ObservedRevision != value.Target().Revision().Value() {
						t.Fatalf("incorrect failed observation: %+v", snapshot)
					}
					if value.RequireProvisioningReady() == nil {
						t.Fatal("failed check authorized provisioning")
					}
					// 依赖恢复后重新验证，不保留失败或旧就绪证据。
					if err := value.AssessManagement(capability(t, value, completeChecks())); err != nil {
						t.Fatal(err)
					}
					if err := value.RequireProvisioningReady(); err != nil {
						t.Fatal("dependency recovery did not restore readiness", err)
					}
				})
			}
		}
	}
}

func TestReadinessMethodsRejectWrongPhaseAndDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, phase := range []instance.Phase{instance.PhasePending, instance.PhaseValidating,
			instance.PhaseReady, instance.PhaseDeleting} {
			for _, operation := range []struct {
				phase instance.Phase
				apply func(*instance.Instance, instance.CapabilityObservation) error
			}{
				{instance.PhaseValidating, (*instance.Instance).AssessManagement},
				{instance.PhaseReady, (*instance.Instance).AssessReadiness},
			} {
				if !deleting && operation.phase == phase {
					continue
				}
				i := lifecycleInstance(t, instance.Snapshot{Phase: phase}, deleting)
				before := i.Snapshot()
				if err := operation.apply(i, capability(t, i, completeChecks())); err == nil {
					t.Fatalf("phase %s deleting=%t accepted operation for %s", phase, deleting, operation.phase)
				}
				if i.Snapshot() != before {
					t.Fatal("rejected operation mutated snapshot")
				}
			}
		}
	}
}

func TestOldGenerationObservationDoesNotReplaceEvidence(t *testing.T) {
	i := readyInstance(t)
	target := i.Target()
	revision, err := instance.NewRevision(target.Revision().Value() + 1)
	if err != nil {
		t.Fatal(err)
	}
	other, err := instance.NewObservationTarget(target.Identity(), revision, target.Definition())
	if err != nil {
		t.Fatal(err)
	}
	o, err := instance.NewCapabilityObservation(other, testServerVersion, completeChecks())
	if err != nil {
		t.Fatal(err)
	}
	before := i.Snapshot()
	if err := i.AssessReadiness(o); err == nil || i.Snapshot() != before {
		t.Fatal("mismatched generation observation was accepted")
	}
	if err := i.RequireProvisioningReady(); err != nil {
		t.Fatal("rejected unrelated input changed previously accepted evidence", err)
	}
}

func TestCapabilityInputsAndLifecycleGuards(t *testing.T) {
	i := readyInstance(t)
	if _, err := instance.NewCapabilityObservation(instance.ObservationTarget{}, testServerVersion,
		completeChecks()); err == nil {
		t.Fatal("invalid target accepted")
	}
	for _, method := range []func(instance.CapabilityObservation) error{
		i.AssessManagement, i.AssessReadiness,
	} {
		before := i.Snapshot()
		if err := method(instance.CapabilityObservation{}); err == nil || i.Snapshot() != before {
			t.Fatal("mismatched observation accepted or mutated state")
		}
	}
	old := i.Snapshot()
	old.ObservedRevision = 0
	changed := lifecycleInstance(t, old, false)
	if err := changed.AssessReadiness(capability(t, changed, completeChecks())); err != nil {
		t.Fatal(err)
	}
	if s := changed.Snapshot(); s.Phase != instance.PhaseValidating || s.ObservedRevision != 0 || s.Readiness != instance.Unknown {
		t.Fatalf("changed generation accepted old checkpoint: %+v", s)
	}
	o, err := instance.NewCapabilityObservation(i.Target(), "", completeChecks())
	if err != nil {
		t.Fatal(err)
	}
	if err := i.AssessReadiness(o); err != nil {
		t.Fatal(err)
	}
	if i.Snapshot().Failure != instance.ObservationIncomplete {
		t.Fatal("missing version accepted")
	}
}
