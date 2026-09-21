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

func capability(t *testing.T, value *instance.Instance, checks instance.ManagementChecks,
	registry instance.RegistryState,
) instance.CapabilityObservation {
	t.Helper()
	o, err := instance.NewCapabilityObservation(value.Target(), testServerVersion, checks, registry)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func readyInstance(t *testing.T) *instance.Instance {
	t.Helper()
	i := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseInitializingRegistry}, false)
	if err := i.AssessRegistryResult(instance.RegistryReadBack(capability(t, i, completeChecks(), instance.RegistryUsable))); err != nil {
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
	absent := capability(t, i, completeChecks(), instance.RegistryAbsent)
	if err := i.AssessManagement(absent); err != nil {
		t.Fatal(err)
	}
	if s := i.Snapshot(); s.Phase != instance.PhaseInitializingRegistry || s.ObservedRevision != 0 || s.Readiness != instance.Unknown {
		t.Fatalf("management observation prematurely concluded readiness: %+v", s)
	}
	for range 2 {
		decision, err := i.PlanRegistryPreparation(absent)
		if err != nil || decision != instance.PreparationAllowed {
			t.Fatalf("preparation: %v, %v", decision, err)
		}
		if i.RequireProvisioningReady() == nil {
			t.Fatal("preparation authorized provisioning")
		}
	}
	if err := i.AssessRegistryResult(instance.RegistryReadBack(absent)); err != nil {
		t.Fatal(err)
	}
	if i.Snapshot().Failure != instance.RegistryNotUsable || i.RequireProvisioningReady() == nil {
		t.Fatal("absent registry accepted as ready")
	}
	usable := capability(t, i, completeChecks(), instance.RegistryUsable)
	decision, err := i.PlanRegistryPreparation(usable)
	if err != nil || decision != instance.AlreadyUsable {
		t.Fatalf("retry after external preparation: %v, %v", decision, err)
	}
	if err := i.AssessRegistryResult(instance.RegistryReadBack(usable)); err != nil {
		t.Fatal(err)
	}
	if s := i.Snapshot(); s.Readiness != instance.Ready || s.ReportedVersion != testServerVersion || s.ObservedRevision != i.Target().Revision().Value() {
		t.Fatalf("complete observation not accepted: %+v", s)
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
		if err := restored.AssessReadiness(capability(t, restored, completeChecks(), instance.RegistryUsable)); err != nil {
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
	for field := range 6 {
		for _, result := range []instance.CheckResult{instance.CheckUnobserved, instance.CheckUnavailable,
			instance.CheckAuthenticationFailed, instance.CheckInsufficientPrivileges, 255} {
			checks := completeChecks()
			fields := []*instance.CheckResult{&checks.Connection, &checks.Metadata, &checks.Roles,
				&checks.Databases, &checks.Grants, &checks.Extensions}
			*fields[field] = result
			i := readyInstance(t)
			if err := i.AssessReadiness(capability(t, i, checks, instance.RegistryUsable)); err != nil {
				t.Fatal(err)
			}
			if s := i.Snapshot(); s.Phase != instance.PhaseValidating || s.Readiness != instance.NotReady ||
				s.Failure == instance.NoFailure || i.RequireProvisioningReady() == nil {
				t.Fatalf("check %d result %d accepted: %+v", field, result, s)
			}
		}
	}
}

func TestRegistryDecisionsAndReadinessLoss(t *testing.T) {
	for _, tc := range []struct {
		state    instance.RegistryState
		decision instance.PreparationDecision
		failure  instance.Failure
	}{
		{instance.RegistryUsable, instance.AlreadyUsable, instance.NoFailure},
		{instance.RegistryAbsent, instance.PreparationAllowed, instance.RegistryNotUsable},
		{instance.RegistryNeedsMigration, instance.PreparationAllowed, instance.RegistryNotUsable},
		{instance.RegistryUnsupported, instance.PreparationDenied, instance.RegistryIncompatible},
		{instance.RegistryUnavailable, instance.PreparationDenied, instance.DependencyUnavailable},
		{instance.RegistryUnobserved, instance.PreparationDenied, instance.ObservationIncomplete},
		{255, instance.PreparationDenied, instance.ObservationIncomplete},
	} {
		i := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseInitializingRegistry}, false)
		o := capability(t, i, completeChecks(), tc.state)
		decision, err := i.PlanRegistryPreparation(o)
		if err != nil || decision != tc.decision {
			t.Fatalf("registry %d: %v, %v", tc.state, decision, err)
		}
		i = readyInstance(t)
		if err := i.AssessReadiness(o); err != nil {
			t.Fatal(err)
		}
		if i.Snapshot().Failure != tc.failure {
			t.Fatalf("registry %d: %+v", tc.state, i.Snapshot())
		}
		if tc.state != instance.RegistryUsable {
			wantPhase := instance.PhaseInitializingRegistry
			if tc.state == instance.RegistryUnavailable {
				wantPhase = instance.PhaseValidating
			}
			if i.Snapshot().Phase != wantPhase || i.RequireProvisioningReady() == nil {
				t.Fatal("registry drift retained readiness")
			}
		}
	}
}

func TestInitializationRejectsIncompleteOrFailedManagement(t *testing.T) {
	for _, tc := range []struct {
		checks   instance.ManagementChecks
		registry instance.RegistryState
		failure  instance.Failure
	}{
		{instance.ManagementChecks{}, instance.RegistryUsable, instance.ObservationIncomplete},
		{completeChecks(), instance.RegistryUnsupported, instance.RegistryIncompatible},
		{completeChecks(), instance.RegistryUnavailable, instance.DependencyUnavailable},
	} {
		i := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseValidating}, false)
		if err := i.AssessManagement(capability(t, i, tc.checks, tc.registry)); err != nil {
			t.Fatal(err)
		}
		if s := i.Snapshot(); s.Phase != instance.PhaseValidating || s.Failure != tc.failure ||
			s.ObservedRevision != i.Target().Revision().Value() || s.Readiness != instance.NotReady {
			t.Fatalf("invalid management accepted: %+v", s)
		}
	}
	i := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseInitializingRegistry}, false)
	decision, err := i.PlanRegistryPreparation(capability(t, i, instance.ManagementChecks{}, instance.RegistryAbsent))
	if err != nil || decision != instance.PreparationDenied || i.Snapshot().Failure != instance.ObservationIncomplete {
		t.Fatal("incomplete management allowed registry writes")
	}
	if err := i.AssessRegistryResult(instance.RegistryReadBack(capability(t, i, completeChecks(), instance.RegistryUsable))); err != nil {
		t.Fatal(err)
	}
	if err := i.RequireProvisioningReady(); err != nil {
		t.Fatal("dependency recovery did not restore readiness", err)
	}
}

func TestReadinessMethodsRejectWrongPhaseAndDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, phase := range []instance.Phase{instance.PhasePending, instance.PhaseValidating,
			instance.PhaseInitializingRegistry, instance.PhaseReady, instance.PhaseDeleting} {
			for _, operation := range []struct {
				phase instance.Phase
				apply func(*instance.Instance, instance.CapabilityObservation) error
			}{
				{instance.PhaseValidating, (*instance.Instance).AssessManagement},
				{instance.PhaseReady, (*instance.Instance).AssessReadiness},
				{instance.PhaseInitializingRegistry, func(i *instance.Instance, o instance.CapabilityObservation) error {
					_, err := i.PlanRegistryPreparation(o)
					return err
				}},
				{instance.PhaseInitializingRegistry, func(i *instance.Instance, o instance.CapabilityObservation) error {
					return i.AssessRegistryResult(instance.RegistryReadBack(o))
				}},
			} {
				if !deleting && operation.phase == phase {
					continue
				}
				i := lifecycleInstance(t, instance.Snapshot{Phase: phase}, deleting)
				before := i.Snapshot()
				if err := operation.apply(i, capability(t, i, completeChecks(), instance.RegistryUsable)); err == nil {
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
	o, err := instance.NewCapabilityObservation(other, testServerVersion, completeChecks(), instance.RegistryUsable)
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

func TestPreparationFailureCannotEstablishReadiness(t *testing.T) {
	i := lifecycleInstance(t, instance.Snapshot{Phase: instance.PhaseInitializingRegistry}, false)
	for _, failure := range []instance.Failure{instance.DependencyUnavailable, instance.AuthenticationFailed,
		instance.InsufficientPrivileges, instance.RegistryIncompatible} {
		result, err := instance.RegistryPreparationFailed(i.Target(), failure)
		if err != nil {
			t.Fatal(err)
		}
		if err := i.AssessRegistryResult(result); err != nil {
			t.Fatal(err)
		}
		if s := i.Snapshot(); s.Failure != failure || s.Readiness != instance.NotReady ||
			s.Phase != instance.PhaseInitializingRegistry || i.RequireProvisioningReady() == nil {
			t.Fatalf("failed operation accepted: %+v", s)
		}
	}
	for _, failure := range []instance.Failure{instance.NoFailure, 255} {
		if _, err := instance.RegistryPreparationFailed(i.Target(), failure); err == nil {
			t.Fatal("invalid failure accepted")
		}
	}
}

func TestCapabilityInputsAndLifecycleGuards(t *testing.T) {
	i := readyInstance(t)
	if _, err := instance.NewCapabilityObservation(instance.ObservationTarget{}, testServerVersion,
		completeChecks(), instance.RegistryUsable); err == nil {
		t.Fatal("invalid target accepted")
	}
	if _, err := instance.RegistryPreparationFailed(instance.ObservationTarget{}, instance.DependencyUnavailable); err == nil {
		t.Fatal("invalid failure target accepted")
	}
	for _, method := range []func(instance.CapabilityObservation) error{
		i.AssessManagement, i.AssessReadiness,
		func(o instance.CapabilityObservation) error { _, err := i.PlanRegistryPreparation(o); return err },
		func(o instance.CapabilityObservation) error {
			return i.AssessRegistryResult(instance.RegistryReadBack(o))
		},
	} {
		before := i.Snapshot()
		if err := method(instance.CapabilityObservation{}); err == nil || i.Snapshot() != before {
			t.Fatal("mismatched observation accepted or mutated state")
		}
	}
	old := i.Snapshot()
	old.ObservedRevision = 0
	changed := lifecycleInstance(t, old, false)
	if err := changed.AssessReadiness(capability(t, changed, completeChecks(), instance.RegistryUsable)); err != nil {
		t.Fatal(err)
	}
	if s := changed.Snapshot(); s.Phase != instance.PhaseValidating || s.ObservedRevision != 0 || s.Readiness != instance.Unknown {
		t.Fatalf("changed generation accepted old checkpoint: %+v", s)
	}
	o, err := instance.NewCapabilityObservation(i.Target(), "", completeChecks(), instance.RegistryUsable)
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
