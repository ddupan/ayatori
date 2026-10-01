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

// Failure 只表示安全类别；驱动错误、凭据和 Condition 文案留在应用边界。
type Failure uint8

const (
	NoFailure Failure = iota
	ObservationIncomplete
	DependencyUnavailable
	AuthenticationFailed
	InsufficientPrivileges
)

// CheckResult 的零值表示未观察，不能视为成功。
type CheckResult uint8

const (
	CheckUnobserved CheckResult = iota
	CheckPassed
	CheckUnavailable
	CheckAuthenticationFailed
	CheckInsufficientPrivileges
)

// ManagementChecks 分别记录所需能力；SQL 探测和同轮次关联由 adapter/Reconciler 保证。
// Extensions 不代表任意扩展均可安装；具体请求仍需支持检查、执行及回读。
type ManagementChecks struct {
	Connection CheckResult
	Metadata   CheckResult
	Roles      CheckResult
	Databases  CheckResult
	Grants     CheckResult
	Extensions CheckResult
}

func (c ManagementChecks) failure() Failure {
	for _, check := range []CheckResult{c.Connection, c.Metadata, c.Roles, c.Databases, c.Grants, c.Extensions} {
		switch check {
		case CheckPassed:
		case CheckUnavailable:
			return DependencyUnavailable
		case CheckAuthenticationFailed:
			return AuthenticationFailed
		case CheckInsufficientPrivileges:
			return InsufficientPrivileges
		default:
			return ObservationIncomplete
		}
	}
	return NoFailure
}

// CapabilityObservation 是值对象，不包含连接、凭据或可变集合。
type CapabilityObservation struct {
	target  ObservationTarget
	version string
	checks  ManagementChecks
}

func NewCapabilityObservation(
	target ObservationTarget,
	version string,
	checks ManagementChecks,
) (CapabilityObservation, error) {
	if err := target.Validate(); err != nil {
		return CapabilityObservation{}, err
	}
	return CapabilityObservation{target: target, version: version, checks: checks}, nil
}

func (o CapabilityObservation) managementFailure() Failure {
	if failure := o.checks.failure(); failure != NoFailure {
		return failure
	}
	if o.version == "" {
		return ObservationIncomplete
	}
	return NoFailure
}

func (i *Instance) acceptObservation(o CapabilityObservation, phase Phase) error {
	if !i.target.Matches(o.target) {
		return errors.New("capability observation target does not match instance")
	}
	if i.deleting || i.snapshot.Phase != phase {
		return errors.New("capability observation is not allowed in current lifecycle")
	}
	return nil
}

func (i *Instance) fail(failure Failure) {
	i.evidence = nil
	i.extensions = ExtensionSupport{}
	i.snapshot.Readiness = NotReady
	i.snapshot.Failure = failure
	i.snapshot.ObservedRevision = i.target.Revision().Value()
}

// AssessManagement 根据本轮完整能力观察完成验证，不执行外部写入。
func (i *Instance) AssessManagement(o CapabilityObservation) error {
	if err := i.acceptObservation(o, PhaseValidating); err != nil {
		return err
	}
	i.assessComplete(o)
	return nil
}

func (i *Instance) assessComplete(o CapabilityObservation) {
	if failure := o.managementFailure(); failure != NoFailure {
		i.fail(failure)
		return
	}
	i.snapshot = Snapshot{
		Phase:            PhaseReady,
		ObservedRevision: i.target.Revision().Value(),
		Readiness:        Ready,
		ReportedVersion:  o.version,
	}
	i.evidence = &o
}

// AssessReadiness 每轮接收完整事实，失败立即撤销本轮供应能力。
func (i *Instance) AssessReadiness(o CapabilityObservation) error {
	if err := i.acceptObservation(o, PhaseReady); err != nil {
		return err
	}
	if i.snapshot.ObservedRevision != i.target.Revision().Value() {
		return i.BeginValidation()
	}
	if o.managementFailure() != NoFailure {
		i.snapshot.Phase = PhaseValidating
	}
	i.assessComplete(o)
	return nil
}

// RequireProvisioningReady 仅检查 Instance 前置条件，不授予 Tenant 所有权或外部写入许可。
func (i *Instance) RequireProvisioningReady() error {
	if i.deleting || i.evidence == nil || i.snapshot.Phase != PhaseReady || i.snapshot.Readiness != Ready ||
		i.snapshot.ObservedRevision != i.target.Revision().Value() {
		return errors.New("instance is not ready for provisioning")
	}
	return nil
}
