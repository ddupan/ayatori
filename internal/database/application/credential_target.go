package application

import "git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"

func (r *CredentialRecord) check() *binding.Issue {
	database := r.Database
	if database.Deleting || database.Phase == binding.Deleting || database.Phase == "Released" {
		return &binding.Issue{Reason: "PreparationStopped", Message: "Database 正在删除或已释放；保留凭据与 finalizer，不执行供应或清理"}
	}
	if database.Tenant == nil || r.Tenant == nil || r.Tenant.Database == nil {
		return &binding.Issue{Reason: binding.DependencyUnavailable, Message: "等待 Database 与 Tenant 双向绑定完成"}
	}
	if *database.Tenant != r.Tenant.Identity || *r.Tenant.Database != database.Identity {
		return &binding.Issue{Reason: binding.Conflict, Message: "双向绑定的名称或 UID 不匹配，未创建凭据"}
	}
	if r.Tenant.Deleting || r.Tenant.Phase != binding.Bound || !r.DatabaseProtected || !r.TenantProtected {
		return &binding.Issue{Reason: "PreparationStopped", Message: "Tenant 未完成绑定、正在删除或缺少 finalizer 保护，未创建凭据"}
	}
	target, err := r.Tenant.Request.Resolve(r.Tenant.Identity)
	if err != nil || (target.Provision != nil && !database.MatchesProvision(target, r.Tenant.Identity)) || target.Name != database.Identity.Name {
		return &binding.Issue{Reason: binding.Conflict, Message: "Tenant 申请与 Database 目标不一致，未创建凭据"}
	}
	if r.Instance == nil || database.InstanceUID == "" {
		return &binding.Issue{Reason: binding.DependencyUnavailable, Message: "等待 Instance 与已记录的实例身份"}
	}
	return r.Instance.Check(&database.Database)
}
