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

// Phase is a workflow checkpoint, never evidence of external resource state.
type Phase string

const (
	PhasePending              Phase = "Pending"
	PhaseValidating           Phase = "Validating"
	PhaseInitializingRegistry Phase = "InitializingRegistry"
	PhaseReady                Phase = "Ready"
	PhaseDeleting             Phase = "Deleting"
)

type Readiness string

const (
	Unknown  Readiness = "Unknown"
	Ready    Readiness = "Ready"
	NotReady Readiness = "NotReady"
)

// Snapshot contains persisted observations only, without credentials or live evidence.
// Failure detail mapping will be added with capability assessment, not intent transitions.
type Snapshot struct {
	Phase            Phase
	ObservedRevision int64
	Readiness        Readiness
	ReportedVersion  string
}

// Instance protects registration state and pure lifecycle transitions.
// Reconstitution does not establish live capability evidence, even for a Ready snapshot.
// This initial slice deliberately exposes no operation that authorizes provisioning.
type Instance struct {
	target     ObservationTarget
	snapshot   Snapshot
	deleting   bool
	extensions ExtensionSupport
}

func Reconstitute(target ObservationTarget, snapshot Snapshot, deleting bool) (*Instance, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	switch snapshot.Phase {
	case PhasePending, PhaseValidating, PhaseInitializingRegistry, PhaseReady, PhaseDeleting:
	default:
		snapshot.Phase = PhasePending
		snapshot.Readiness = Unknown
	}
	return &Instance{target: target, snapshot: snapshot, deleting: deleting}, nil
}

func (i *Instance) Target() ObservationTarget { return i.target }

// Snapshot returns a detached value. Persisting it remains the application's job.
func (i *Instance) Snapshot() Snapshot { return i.snapshot }

// BeginValidation records intent only; it does not claim a concluded observation.
func (i *Instance) BeginValidation() error {
	if err := i.target.Validate(); err != nil {
		return err
	}
	if i.deleting {
		return errors.New("cannot begin validation after deletion was requested")
	}
	i.snapshot.Phase = PhaseValidating
	i.snapshot.Readiness = Unknown
	i.extensions = ExtensionSupport{}
	return nil
}

// BeginDeletion stops the lifecycle from accepting validation. It does not delete
// resources, inspect Tenant references, close connections or modify finalizers.
func (i *Instance) BeginDeletion() error {
	if err := i.target.Validate(); err != nil {
		return err
	}
	if !i.deleting {
		return errors.New("cannot begin deletion without a deletion request")
	}
	i.snapshot.Phase = PhaseDeleting
	i.snapshot.Readiness = Unknown
	i.extensions = ExtensionSupport{}
	return nil
}

// ObserveExtensions accepts facts only for this registration and configuration.
// Unobserved support clears a previous list after a failed read; the application
// reports the dependency failure separately. This does not establish readiness.
// Same-target freshness and Secret refresh are enforced by the application.
func (i *Instance) ObserveExtensions(target ObservationTarget, support ExtensionSupport) error {
	if !i.target.Matches(target) {
		return errors.New("extension observation target does not match instance")
	}
	if i.deleting {
		return errors.New("cannot accept extension observations after deletion was requested")
	}
	i.extensions = support
	return nil
}

// CheckExtensions checks support only; Accepted is not authorization to provision.
// The aggregate does not perform IO, mutate its snapshot or uninstall extensions.
func (i *Instance) CheckExtensions(requested ExtensionSet) ExtensionCheck {
	return i.extensions.Check(requested)
}
