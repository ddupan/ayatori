package application

import (
	"context"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
)

// 快照版本只用于协调读写，不进入领域规则。Generation 用于确认整个申请未在回读期间变化。
type BindingTenant struct {
	binding.Tenant
	Revision   string
	Generation int64
}

type BindingDatabase struct {
	binding.Database
	Revision string
}

// BindingResources 是这个用例所需的操作，不是通用 Repository 或跨系统事务接口。
// 查不到对象时返回 nil；写入必须检查传入快照的版本，不能覆盖并发修改。
type BindingResources interface {
	Tenant(context.Context, string, string) (*BindingTenant, error)
	Database(context.Context, string) (*BindingDatabase, error)
	Instance(context.Context, string) (*binding.Instance, error)
	BeginBinding(context.Context, *BindingTenant, *BindingStatus) (*BindingTenant, error)
	CreateDatabase(context.Context, binding.Target, binding.TenantIdentity) (*BindingDatabase, error)
	RecordInstance(context.Context, *BindingDatabase, string) (*BindingDatabase, error)
	BindDatabase(context.Context, *BindingDatabase, binding.TenantIdentity) (*BindingDatabase, error)
}

// BindingStatus 是用例结果，资源呈现层决定如何写成 Conditions/status。
type BindingStatus struct {
	Phase    string
	Reason   string
	Message  string
	Database *binding.Identity
}

type BindingResult struct {
	Tenant    *BindingTenant
	Status    BindingStatus
	RetrySoon bool
}

type BindingService struct {
	Resources BindingResources
}

func (s BindingService) Reconcile(ctx context.Context, namespace, name string) (BindingResult, error) {
	tenant, err := s.Resources.Tenant(ctx, namespace, name)
	if err != nil || tenant == nil {
		return BindingResult{}, err
	}
	if tenant.Deleting {
		return bindingResult(tenant, binding.Deleting, "DeletionPending",
			"删除清理尚未接入；保留 finalizer 和 Database 绑定，未执行后端删除"), nil
	}
	target, err := tenant.Request.Resolve(tenant.Identity)
	if err != nil {
		return bindingResult(tenant, tenant.Phase, "InvalidRequest", err.Error()), nil
	}
	// 持久固定申请，再创建/绑定资源；不是预先宣告双向绑定成功。
	var checkpoint *BindingStatus
	if tenant.Phase != binding.Binding && tenant.Phase != binding.Bound {
		checkpoint = &BindingStatus{Phase: binding.Binding, Reason: "BindingPending", Message: "申请目标已固定，等待资源侧绑定"}
	}
	tenant, err = s.Resources.BeginBinding(ctx, tenant, checkpoint)
	if err != nil {
		return BindingResult{}, err
	}
	database, issue, err := s.resolveDatabase(ctx, tenant, target)
	if err != nil {
		return BindingResult{}, err
	}
	if issue != nil {
		return bindingResult(tenant, tenant.Phase, issue.Reason, issue.Message), nil
	}
	if issue := database.CanBind(tenant.Tenant); issue != nil {
		return bindingResult(tenant, tenant.Phase, issue.Reason, issue.Message), nil
	}
	database, err = s.Resources.BindDatabase(ctx, database, tenant.Identity)
	if err != nil {
		return BindingResult{}, err
	}
	return s.confirmBinding(ctx, tenant, database)
}

func (s BindingService) resolveDatabase(ctx context.Context, tenant *BindingTenant, target binding.Target) (
	*BindingDatabase, *binding.Issue, error,
) {
	database, err := s.Resources.Database(ctx, target.Name)
	if err != nil {
		return nil, nil, err
	}
	if database == nil && target.Provision == nil {
		return nil, &binding.Issue{Reason: binding.DependencyUnavailable, Message: "指定的 Database 尚不存在，等待资源出现"}, nil
	}
	instanceName := ""
	var observed *binding.Database
	if database != nil {
		instanceName, observed = database.Instance, &database.Database
	}
	if target.Provision != nil {
		instanceName = target.Provision.Instance
		if database != nil && !database.MatchesProvision(target, tenant.Identity) {
			return nil, &binding.Issue{Reason: binding.Conflict,
				Message: "动态 Database 名称已存在，但目标或 Tenant UID 不匹配；请核实记录，未自动认领"}, nil
		}
	}
	instance, err := s.Resources.Instance(ctx, instanceName)
	if err != nil {
		return nil, nil, err
	}
	if instance == nil {
		return nil, &binding.Issue{Reason: binding.DependencyUnavailable, Message: "引用的 Instance 尚不存在"}, nil
	}
	if issue := instance.Check(observed); issue != nil {
		return nil, issue, nil
	}
	if database == nil {
		database, err = s.Resources.CreateDatabase(ctx, target, tenant.Identity)
		if err != nil {
			return nil, nil, err
		}
	}
	// 只有首次动态记录可以补入实例身份；导入必须先有资源观察。
	if database.InstanceUID == "" && target.Provision != nil {
		database, err = s.Resources.RecordInstance(ctx, database, instance.Identity.UID)
		if err != nil {
			return nil, nil, err
		}
	}
	if database.InstanceUID == "" {
		return nil, &binding.Issue{Reason: binding.DependencyUnavailable, Message: "Database 尚未完成实例身份验证"}, nil
	}
	return database, nil, nil
}

func (s BindingService) confirmBinding(ctx context.Context, tenant *BindingTenant, database *BindingDatabase) (BindingResult, error) {
	latest, err := s.Resources.Tenant(ctx, tenant.Identity.Namespace, tenant.Identity.Name)
	if err != nil || latest == nil {
		return BindingResult{}, err
	}
	if latest.Identity != tenant.Identity || latest.Deleting {
		return BindingResult{}, nil
	}
	if latest.Generation != tenant.Generation {
		return BindingResult{RetrySoon: true}, nil
	}
	observed, err := s.Resources.Database(ctx, database.Identity.Name)
	if err != nil {
		return BindingResult{}, err
	}
	if observed == nil || observed.Identity != database.Identity || observed.Tenant == nil {
		return bindingResult(latest, latest.Phase, binding.Conflict, "资源侧身份或绑定已变化，未完成申请侧绑定"), nil
	}
	if issue := observed.CanBind(latest.Tenant); issue != nil {
		return bindingResult(latest, latest.Phase, issue.Reason, issue.Message), nil
	}
	result := bindingResult(latest, binding.Bound, "BindingComplete",
		"双向绑定已记录；尚未执行供应、应用登录验证或凭据交付，不能 Ready")
	result.Status.Database = &observed.Identity
	return result, nil
}

func bindingResult(tenant *BindingTenant, phase, reason, message string) BindingResult {
	return BindingResult{Tenant: tenant, Status: BindingStatus{Phase: phase, Reason: reason, Message: message}}
}
