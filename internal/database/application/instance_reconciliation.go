package application

import (
	"context"
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const (
	instanceDependencyUnavailable = "DependencyUnavailable"
	instanceAuthenticationFailed  = "AuthenticationFailed"
)

// InstanceRecord 是 API 快照；Revision 仅用于持久化并发保护，不是领域版本。
type InstanceRecord struct {
	Target   instance.ObservationTarget
	Revision string
	Deleting bool
}

type InstanceResources interface {
	LoadInstance(context.Context, string) (*InstanceRecord, error)
	ProtectInstance(context.Context, *InstanceRecord) (*InstanceRecord, error)
	// InstanceReferences 返回一个可定位的阻塞引用；空字符串表示没有引用。
	InstanceReferences(context.Context, string) (string, error)
}

type InstanceObserver interface {
	ObserveManagement(context.Context, instance.ObservationTarget) (InstanceObservation, error)
	Forget(string)
}

type InstanceResult struct {
	Record           *InstanceRecord
	Snapshot         instance.Snapshot
	Reason           string
	Message          string
	RemoveProtection bool
}

// InstanceReconciliation 协调 API 保护、实时观察和领域判断，不拼装 Kubernetes status。
type InstanceReconciliation struct {
	Resources InstanceResources
	Observer  InstanceObserver
}

func (s *InstanceReconciliation) Reconcile(ctx context.Context, name string) (InstanceResult, error) {
	record, err := s.Resources.LoadInstance(ctx, name)
	if err != nil {
		return InstanceResult{}, err
	}
	if record == nil {
		s.Observer.Forget(name)
		return InstanceResult{}, nil
	}
	if record.Deleting {
		return s.deleting(ctx, record)
	}
	record, err = s.Resources.ProtectInstance(ctx, record)
	if err != nil {
		return InstanceResult{}, err
	}
	// 每轮从无证据的领域对象开始；持久化 Ready 和连接存活不能替代本轮检查。
	aggregate, err := instance.Reconstitute(record.Target, instance.Snapshot{}, false)
	if err != nil {
		return InstanceResult{}, err
	}
	if err := aggregate.BeginValidation(); err != nil {
		return InstanceResult{}, err
	}
	observation, observationErr := s.Observer.ObserveManagement(ctx, record.Target)
	result := InstanceResult{Record: record}
	if observationErr != nil {
		result.Snapshot = aggregate.Snapshot()
		result.Snapshot.Readiness = instance.NotReady
		result.Snapshot.ObservedRevision = record.Target.Revision().Value()
		result.Reason, result.Message = observationFailure(observationErr)
		return result, nil
	}
	capabilities, err := observation.Capabilities()
	if err != nil {
		return InstanceResult{}, err
	}
	if err := aggregate.AssessManagement(capabilities); err != nil {
		return InstanceResult{}, err
	}
	result.Snapshot = aggregate.Snapshot()
	result.Snapshot.ReportedVersion = observation.Version()
	result.Reason, result.Message = managementResult(result.Snapshot.Failure)
	return result, nil
}

func (s *InstanceReconciliation) deleting(ctx context.Context, record *InstanceRecord) (InstanceResult, error) {
	name := record.Target.Identity().Name()
	s.Observer.Forget(name)
	aggregate, err := instance.Reconstitute(record.Target, instance.Snapshot{}, true)
	if err != nil {
		return InstanceResult{}, err
	}
	if err := aggregate.BeginDeletion(); err != nil {
		return InstanceResult{}, err
	}
	result := InstanceResult{Record: record, Snapshot: aggregate.Snapshot(), Reason: "Deleting"}
	reference, err := s.Resources.InstanceReferences(ctx, name)
	if err != nil {
		result.Reason = instanceDependencyUnavailable
		result.Message = "无法确认 Database/Tenant 引用已解除；保留 Instance 删除保护并重试"
		return result, nil
	}
	if reference != "" {
		result.Reason = "InstanceInUse"
		result.Message = "仍被 " + reference + " 引用；先处理该资源，不会级联删除外部数据库"
		return result, nil
	}
	result.Message = "引用已解除，仅移除登记保护；不删除 PostgreSQL 或凭据"
	result.RemoveProtection = true
	return result, nil
}

func observationFailure(err error) (string, string) {
	switch {
	case errors.Is(err, ErrAuthentication):
		return instanceAuthenticationFailed, "管理连接认证或 TLS 校验失败；检查管理 Secret 和 CA/证书配置"
	case errors.Is(err, ErrCredentialsInvalid):
		return "InvalidCredentials", "管理 Secret 的用户名或密码字段缺失；检查引用字段映射"
	case errors.Is(err, ErrCredentialsChanged):
		return "CredentialsChanged", "观察期间管理凭据变化，已丢弃结果并关闭旧连接；等待重新验证"
	case errors.Is(err, ErrCredentialsUnavailable):
		return instanceDependencyUnavailable, "无法读取管理 Secret；检查其是否存在及 controller namespace 内的读取权限"
	default:
		return instanceDependencyUnavailable, "管理连接或能力查询失败；检查 PostgreSQL 可达性、catalog 读取权限和超时"
	}
}

func managementResult(failure instance.Failure) (string, string) {
	switch failure {
	case instance.NoFailure:
		return "ManagementReady", "当前管理能力检查通过；具体资源授权和扩展安装仍需执行时验证"
	case instance.InsufficientPrivileges:
		return "InsufficientPrivileges", "原生管理要求非 superuser 且具备 CREATEDB/CREATEROLE；不会自动修改账号权限"
	case instance.DependencyUnavailable:
		return instanceDependencyUnavailable, "当前 PostgreSQL 不可写或所需管理能力暂不可用"
	case instance.AuthenticationFailed:
		return instanceAuthenticationFailed, "当前管理能力检查未通过认证"
	default:
		return "ObservationIncomplete", "管理能力检查尚有缺项，不能仅凭 metadata 查询成功标记 Ready"
	}
}
