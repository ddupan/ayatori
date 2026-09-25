package binding

import "testing"

const (
	testNamespace = "apps"
	testApp       = "app"
	testInstance  = "shared"
	testOwner     = "owner"
	testExisting  = "existing"
	testOther     = "other"
)

func TestRequestResolve(t *testing.T) {
	tenant := TenantIdentity{Namespace: testNamespace, Name: testApp, UID: "tenant-uid"}
	tests := []struct {
		name    string
		request Request
		valid   bool
	}{
		{"动态默认值", Request{Provision: &ProvisionRequest{Instance: testInstance}}, true},
		{"显式名称", Request{Provision: &ProvisionRequest{Instance: testInstance, Database: "custom", LoginRole: testOwner}}, true},
		{"已有资源", Request{ExistingDatabase: testExisting}, true},
		{"没有入口", Request{}, false},
		{"同时指定入口", Request{Provision: &ProvisionRequest{}, ExistingDatabase: testExisting}, false},
		{"非法库名", Request{Provision: &ProvisionRequest{Database: "bad-name"}}, false},
		{"非法角色名", Request{Provision: &ProvisionRequest{LoginRole: "bad-name"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, err := tc.request.Resolve(tenant)
			if (err == nil) != tc.valid {
				t.Fatalf("Resolve() = %v, valid = %v", err, tc.valid)
			}
			if !tc.valid {
				return
			}
			if tc.request.Provision == nil {
				if target.Name != testExisting || target.Provision != nil {
					t.Fatal("已有资源不应推导 Instance 或供应请求")
				}
				return
			}
			if target.Name != "tenant-tenant-uid" || target.Provision == tc.request.Provision {
				t.Fatal("目标名称不稳定，或 Resolve 未复制输入")
			}
			if tc.request.Provision.Database == "" && target.Provision.Database != tenant.Name {
				t.Fatal("数据库默认名称不符")
			}
			if tc.request.Provision.LoginRole == "" && target.Provision.LoginRole != tenant.Name {
				t.Fatal("角色默认名称不符")
			}
		})
	}
}

func TestDatabaseCanBind(t *testing.T) {
	tenant := Tenant{Identity: TenantIdentity{Namespace: testNamespace, Name: testApp, UID: "current"}}
	tests := []struct {
		name   string
		change func(*Database, *Tenant)
		reason string
	}{
		{"空闲且就绪", func(*Database, *Tenant) {}, ""},
		{"同一绑定重试", func(d *Database, t *Tenant) { d.Tenant = &t.Identity; d.Ready = false }, ""},
		{"尚未观察", func(d *Database, _ *Tenant) { d.Ready = false }, DependencyUnavailable},
		{"尚未Available", func(d *Database, _ *Tenant) { d.Phase = "Pending" }, DependencyUnavailable},
		{"Released", func(d *Database, _ *Tenant) { d.Phase = "Released" }, Conflict},
		{"删除标记", func(d *Database, _ *Tenant) { d.Deleting = true }, Conflict},
		{"删除阶段", func(d *Database, _ *Tenant) { d.Phase = Deleting }, Conflict},
		{"已被占用", func(d *Database, _ *Tenant) { d.Tenant = &TenantIdentity{UID: testOther} }, Conflict},
		{"同名新申请", func(d *Database, t *Tenant) { old := t.Identity; old.UID = "old"; d.Tenant = &old }, Conflict},
		{"同名新资源", func(_ *Database, t *Tenant) { t.Database = &Identity{Name: "resource", UID: "old"} }, Conflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := Database{Identity: Identity{Name: "resource", UID: "database-uid"}, Phase: "Available", Ready: true}
			currentTenant := tenant
			tc.change(&database, &currentTenant)
			checkIssue(t, database.CanBind(currentTenant), tc.reason)
		})
	}
}

func TestInstanceCheck(t *testing.T) {
	instance := Instance{Identity: Identity{UID: "instance"}, Ready: true}
	checkIssue(t, instance.Check(nil), "")
	checkIssue(t, instance.Check(&Database{InstanceUID: "instance"}), "")
	checkIssue(t, instance.Check(&Database{InstanceUID: "replaced"}), Conflict)
	instance.Ready = false
	checkIssue(t, instance.Check(nil), DependencyUnavailable)
	instance.Ready, instance.Deleting = true, true
	checkIssue(t, instance.Check(nil), DependencyUnavailable)
}

func TestMatchesProvision(t *testing.T) {
	tenant := TenantIdentity{Namespace: testNamespace, Name: testApp, UID: "tenant"}
	target := Target{Provision: &ProvisionRequest{Instance: testInstance, Database: testApp, LoginRole: testOwner}}
	database := Database{Source: "Provision", Instance: testInstance, Name: testApp, LoginRole: testOwner, Tenant: &tenant}
	if !database.MatchesProvision(target, tenant) {
		t.Fatal("相同目标与身份应允许重试")
	}
	mutations := []func(*Database){
		func(d *Database) { d.Source = "Import" },
		func(d *Database) { d.Instance = testOther },
		func(d *Database) { d.Name = testOther },
		func(d *Database) { d.LoginRole = testOther },
		func(d *Database) { d.Tenant = nil },
		func(d *Database) { d.Tenant = &TenantIdentity{UID: testOther} },
	}
	for _, mutate := range mutations {
		changed := database
		mutate(&changed)
		if changed.MatchesProvision(target, tenant) {
			t.Fatal("不匹配的记录不能仅靠名称被认领")
		}
	}
	if database.MatchesProvision(Target{}, tenant) {
		t.Fatal("已有资源申请不是动态供应重试")
	}
}

func checkIssue(t *testing.T, issue *Issue, reason string) {
	t.Helper()
	if reason == "" {
		if issue != nil {
			t.Fatalf("不应拒绝: %+v", issue)
		}
		return
	}
	if issue == nil || issue.Reason != reason || issue.Message == "" {
		t.Fatalf("issue = %+v, want %s 与可读诊断", issue, reason)
	}
}
