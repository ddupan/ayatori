package credential_test

import (
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	credential "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
)

const (
	targetTestApplication = "sampleapp"
	targetTestInstance    = "test-instance"
)

func TestPreparationTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*credential.Target)
		want   credential.Phase
	}{
		{"完整绑定", func(*credential.Target) {}, credential.Pending},
		{"单向绑定", func(target *credential.Target) { target.Tenant.Database = nil }, credential.Unavailable},
		{"旧租户身份", func(target *credential.Target) { target.Tenant.Identity.UID = "new" }, credential.Conflict},
		{"旧实例身份", func(target *credential.Target) { target.Instance.Identity.UID = "new" }, credential.Conflict},
		{"资源删除", func(target *credential.Target) { target.Database.Deleting = true }, credential.Stopped},
		{"申请删除", func(target *credential.Target) { target.Tenant.Deleting = true }, credential.Stopped},
		{"Released", func(target *credential.Target) { target.Database.Phase = "Released" }, credential.Stopped},
		{"缺少保护", func(target *credential.Target) { target.DatabaseProtected = false }, credential.Stopped},
		{"实例未就绪", func(target *credential.Target) { target.Instance.Ready = false }, credential.Unavailable},
		{"目标变化", func(target *credential.Target) { target.Database.LoginRole = "other" }, credential.Conflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			tenantID := binding.TenantIdentity{Namespace: "apps", Name: targetTestApplication, UID: "tenant"}
			databaseID := binding.Identity{Name: binding.DynamicDatabaseName(tenantID.UID), UID: "database"}
			target := credential.Target{
				Database:          binding.Database{Identity: databaseID, Tenant: &tenantID, Instance: targetTestInstance, InstanceUID: "instance-id", Name: targetTestApplication, LoginRole: targetTestApplication, Source: "Provision"},
				Tenant:            &binding.Tenant{Identity: tenantID, Database: &databaseID, Phase: binding.Bound, Request: binding.Request{Provision: &binding.ProvisionRequest{Instance: targetTestInstance}}},
				Instance:          &credential.Instance{Identity: binding.Identity{Name: targetTestInstance, UID: "instance-id"}, Ready: true},
				DatabaseProtected: true, TenantProtected: true,
			}
			test.change(&target)
			issue := target.Check()
			if test.want == credential.Pending {
				if issue != nil {
					t.Fatalf("有效绑定被拒绝: %s", issue.Message)
				}
			} else if issue == nil || issue.Phase != test.want {
				t.Fatal("领域资格判定不符")
			}
		})
	}
}
