package database_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"go.ddupan.top/ayatori/internal/database/domain/binding"
	"go.ddupan.top/ayatori/internal/database/domain/credential"
	"go.ddupan.top/ayatori/internal/database/domain/database"
	"go.ddupan.top/ayatori/internal/database/domain/instance"
)

const (
	instanceUID      = "instance-id"
	sourceProvision  = "Provision"
	extensionHstore  = "hstore"
	extensionPlpgsql = "plpgsql"
	extensionTrigram = "pg_trgm"
	replacedUID      = "new"
	testApplication  = "sampleapp"
	testInstance     = "test-instance"
)

var testLocation = credential.Location{Mount: "applications", Path: "database/uid"}

func fixture(t *testing.T) (database.Snapshot, database.Dependencies) {
	t.Helper()
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host: "postgres.example", HostAddr: "192.0.2.1", Port: 5432, ManagementDatabase: "management", TLSMode: instance.TLSVerifyFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := binding.TenantIdentity{Namespace: "apps", Name: testApplication, UID: "tenant"}
	databaseID := binding.Identity{Name: binding.DynamicDatabaseName(tenantID.UID), UID: "database"}
	return database.Snapshot{
		Binding: binding.Database{Identity: databaseID, Tenant: &tenantID, Instance: testInstance, InstanceUID: instanceUID,
			Name: testApplication, LoginRole: testApplication, Source: sourceProvision},
		Protected: true,
	}, database.Dependencies{
		Tenant: &binding.Tenant{Identity: tenantID, Database: &databaseID, Phase: binding.Bound,
			Request: binding.Request{Provision: &binding.ProvisionRequest{Instance: testInstance}}},
		TenantProtected: true,
		Instance:        &database.Instance{Identity: binding.Identity{Name: testInstance, UID: instanceUID}, Ready: true, Endpoint: endpoint},
	}
}

func TestEligibility(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*database.Snapshot, *database.Dependencies)
		want   database.CredentialPhase
	}{
		{"单向绑定", func(_ *database.Snapshot, d *database.Dependencies) { d.Tenant.Database = nil }, database.CredentialUnavailable},
		{"Tenant未出现", func(_ *database.Snapshot, d *database.Dependencies) { d.Tenant = nil }, database.CredentialUnavailable},
		{"旧租户身份", func(_ *database.Snapshot, d *database.Dependencies) { d.Tenant.Identity.UID = replacedUID }, database.CredentialConflict},
		{"旧资源身份", func(_ *database.Snapshot, d *database.Dependencies) { d.Tenant.Database.UID = replacedUID }, database.CredentialConflict},
		{"Instance同名重建", func(_ *database.Snapshot, d *database.Dependencies) { d.Instance.Identity.UID = replacedUID }, database.CredentialConflict},
		{"资源删除", func(s *database.Snapshot, _ *database.Dependencies) { s.Binding.Deleting = true }, database.CredentialStopped},
		{"Released", func(s *database.Snapshot, _ *database.Dependencies) { s.Binding.Phase = "Released" }, database.CredentialStopped},
		{"申请删除", func(_ *database.Snapshot, d *database.Dependencies) { d.Tenant.Deleting = true }, database.CredentialStopped},
		{"申请尚未Bound", func(_ *database.Snapshot, d *database.Dependencies) { d.Tenant.Phase = binding.Binding }, database.CredentialStopped},
		{"缺少资源保护", func(s *database.Snapshot, _ *database.Dependencies) { s.Protected = false }, database.CredentialStopped},
		{"缺少申请保护", func(_ *database.Snapshot, d *database.Dependencies) { d.TenantProtected = false }, database.CredentialStopped},
		{"Instance未出现", func(_ *database.Snapshot, d *database.Dependencies) { d.Instance = nil }, database.CredentialUnavailable},
		{"Instance正在删除", func(_ *database.Snapshot, d *database.Dependencies) { d.Instance.Deleting = true }, database.CredentialUnavailable},
		{"Instance未Ready", func(_ *database.Snapshot, d *database.Dependencies) { d.Instance.Ready = false }, database.CredentialUnavailable},
		{"Instance身份未记录", func(s *database.Snapshot, _ *database.Dependencies) { s.Binding.InstanceUID = "" }, database.CredentialUnavailable},
		{"目标变化", func(s *database.Snapshot, _ *database.Dependencies) { s.Binding.LoginRole = "other" }, database.CredentialConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies := fixture(t)
			test.change(&snapshot, &dependencies)
			db := database.Reconstitute(snapshot, dependencies)
			if db.PlanCredential(testLocation, nil) != database.Wait || db.PlanResources() != database.Wait {
				t.Fatal("前置条件不满足时不得进入外部操作")
			}
			state := db.Snapshot()
			if state.Credential.Phase != test.want || state.Credential.Location != nil {
				t.Fatalf("凭据阶段不符或提前固定位置：%v", state.Credential.Phase)
			}
		})
	}
}

func TestCredentialResume(t *testing.T) {
	for _, test := range []struct {
		name    string
		phase   database.CredentialPhase
		version int64
		want    database.Action
		result  database.CredentialPhase
	}{
		{"尚未创建", database.CredentialPinned, 0, database.CreateCredential, database.CredentialPinned},
		{"依赖恢复", database.CredentialUnavailable, 0, database.CreateCredential, database.CredentialUnavailable},
		{"未确认冲突", database.CredentialConflict, 0, database.Wait, database.CredentialConflict},
		{"已确认后读取失败", database.CredentialUnavailable, 1, database.VerifyCredential, database.CredentialUnavailable},
		{"已确认后冲突重验", database.CredentialConflict, 1, database.VerifyCredential, database.CredentialConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies := fixture(t)
			location := testLocation
			snapshot.Credential = database.CredentialState{Location: &location, Version: test.version, Phase: test.phase, Message: "保留原诊断"}
			db := database.Reconstitute(snapshot, dependencies)
			if db.PlanCredential(testLocation, nil) != test.want {
				t.Fatal("恢复决策错误")
			}
			state := db.Snapshot().Credential
			if state.Phase != test.result || state.Version != test.version {
				t.Fatal("恢复判定或确认版本发生变化")
			}
			if test.phase == database.CredentialConflict && state.Message != "保留原诊断" {
				t.Fatal("冲突重入应保留原诊断")
			}
		})
	}
}

func TestCredentialLocationAndCreation(t *testing.T) {
	snapshot, dependencies := fixture(t)
	db := database.Reconstitute(snapshot, dependencies)
	if db.PlanCredential(testLocation, errors.New("bad config")) != database.Wait || db.Snapshot().Credential.Location != nil {
		t.Fatal("配置无效时不得固定位置")
	}
	if db.PlanCredential(testLocation, nil) != database.Save || db.Snapshot().Credential.Phase != database.CredentialPinned {
		t.Fatal("首次应先持久化固定位置")
	}
	pinned := db.Snapshot()
	moved := database.Reconstitute(pinned, dependencies)
	if moved.PlanCredential(credential.Location{Mount: "other", Path: testLocation.Path}, nil) != database.Wait ||
		moved.Snapshot().Credential.Phase != database.CredentialUnavailable || *moved.Snapshot().Credential.Location != testLocation {
		t.Fatal("配置变化必须停止，不迁移已固定位置")
	}
	for _, existing := range []error{nil, credential.ErrApplicationCredentialInvalid} {
		if _, ok := database.Reconstitute(pinned, dependencies).GenerateCredential(existing); ok {
			t.Fatal("固定位置已有值时不得生成凭据")
		}
	}
	if _, ok := database.Reconstitute(pinned, dependencies).GenerateCredential(database.ErrUnavailable); ok {
		t.Fatal("无法确认位置为空时不得生成凭据")
	}
	for _, test := range []struct {
		name    string
		version int64
		err     error
		want    database.CredentialPhase
	}{
		{"明确权限拒绝", 0, database.ErrUnavailable, database.CredentialUnavailable},
		{"结果不确定交给下一轮观察", 0, database.ErrUncertain, database.CredentialUnavailable},
		{"并发冲突", 0, database.ErrConflict, database.CredentialConflict},
		{"非首次版本", 2, nil, database.CredentialConflict},
		{"首次创建回读", 1, nil, database.CredentialPrepared},
	} {
		t.Run(test.name, func(t *testing.T) {
			creating := database.Reconstitute(pinned, dependencies)
			value, ok := creating.GenerateCredential(database.ErrNotFound)
			if !ok || value.Password() == "" {
				t.Fatal("位置为空时应生成凭据")
			}
			creating.CredentialCreated(test.version, test.err)
			state := creating.Snapshot().Credential
			if state.Phase != test.want || (state.Version == 1) != (test.want == database.CredentialPrepared) {
				t.Fatal("确认时机不符")
			}
		})
	}
}

func TestResourceIdentity(t *testing.T) {
	const manager, role, owned = 5, 11, 22
	for _, test := range []struct {
		name   string
		state  database.ResourceState
		facts  database.ResourceFacts
		accept bool
	}{
		{"空实例", database.ResourceState{}, database.ResourceFacts{ManagerOID: manager}, true},
		{"已确认库仍归管理账号", database.ResourceState{DatabaseOID: owned}, database.ResourceFacts{DatabaseOID: owned, OwnerOID: manager, ManagerOID: manager}, true},
		{"已移交", database.ResourceState{DatabaseOID: owned, RoleOID: role}, database.ResourceFacts{DatabaseOID: owned, RoleOID: role, OwnerOID: role, ManagerOID: manager}, true},
		{"未知同名库", database.ResourceState{}, database.ResourceFacts{DatabaseOID: owned, OwnerOID: manager, ManagerOID: manager}, false},
		{"未知同名角色", database.ResourceState{DatabaseOID: owned}, database.ResourceFacts{DatabaseOID: owned, RoleOID: role, OwnerOID: manager, ManagerOID: manager}, false},
		{"库消失", database.ResourceState{DatabaseOID: owned}, database.ResourceFacts{ManagerOID: manager}, false},
		{"角色重建", database.ResourceState{DatabaseOID: owned, RoleOID: role}, database.ResourceFacts{DatabaseOID: owned, RoleOID: 12, OwnerOID: manager, ManagerOID: manager}, false},
		{"owner 为他人", database.ResourceState{DatabaseOID: owned, RoleOID: role}, database.ResourceFacts{DatabaseOID: owned, RoleOID: role, OwnerOID: 99, ManagerOID: manager}, false},
		{"未确认角色时 owner 不能是他人", database.ResourceState{DatabaseOID: owned}, database.ResourceFacts{DatabaseOID: owned, OwnerOID: 0, ManagerOID: manager}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies := fixture(t)
			snapshot.Resources = test.state
			if err := database.Reconstitute(snapshot, dependencies).CheckResources(test.facts); (err == nil) != test.accept {
				t.Fatalf("身份判定错误：%v", err)
			}
		})
	}
}

func TestResourceSteps(t *testing.T) {
	const manager, role, owned = 5, 11, 22
	snapshot, dependencies := fixture(t)
	snapshot.Credential = database.CredentialState{Location: &testLocation, Version: 1, Phase: database.CredentialPending}
	if database.Reconstitute(snapshot, dependencies).PlanResources() != database.Wait {
		t.Fatal("凭据未在本轮验证时不得观察 PostgreSQL")
	}
	snapshot.Credential.Phase = database.CredentialPrepared
	db := database.Reconstitute(snapshot, dependencies)
	if db.PlanResources() != database.InspectResources {
		t.Fatal("凭据可用时应观察 PostgreSQL")
	}
	facts := database.ResourceFacts{ManagerOID: manager}
	for _, step := range []struct {
		want   database.Action
		record func()
	}{
		{database.CreateDatabase, func() {
			db.DatabaseCreated(owned, nil)
			facts.DatabaseOID, facts.OwnerOID, facts.PublicConnect, facts.ManagerConnect = owned, manager, true, true
		}},
		{database.RevokePublicConnect, func() { db.Converged("撤销", nil); facts.PublicConnect = false }},
		// 应用角色最后创建：此时 PUBLIC 已撤销，数据库仍归管理账号。
		{database.CreateRole, func() { db.RoleCreated(role, nil); facts.RoleOID = role }},
		{database.TransferOwnership, func() { db.Converged("移交", nil); facts.OwnerOID = role }},
	} {
		if action := db.ObserveResources(facts, nil); action != step.want {
			t.Fatalf("期望步骤 %v，实际 %v", step.want, action)
		}
		step.record()
	}
	if db.ObserveResources(facts, nil) != database.InspectExtensions ||
		db.ObserveExtensions(database.ExtensionFacts{Available: []string{extensionPlpgsql}, Installed: []string{extensionPlpgsql}}, nil) != database.VerifyLogin {
		t.Fatal("资源收敛且未申请扩展时应验证应用登录")
	}
	if db.Ready() {
		t.Fatal("登录验证前不得就绪")
	}
	db.LoginVerified(nil)
	if !db.Ready() || db.Snapshot().Resources.Phase != database.ResourceAvailable {
		t.Fatal("登录成功后应 Available 且就绪")
	}
	db.LoginVerified(database.ErrUnavailable)
	if db.Ready() || db.Snapshot().Resources.Phase != database.ResourceUnavailable {
		t.Fatal("应用凭据无法登录时不得就绪")
	}
	facts.ManagerConnect = false
	if db.ObserveResources(facts, nil) != database.TransferOwnership {
		t.Fatal("管理账号失去 CONNECT 时应重新收敛")
	}

	for _, test := range []struct {
		err  error
		want database.ResourcePhase
	}{
		{database.ErrUnavailable, database.ResourceUnavailable},
		{database.ErrUncertain, database.ResourceUnavailable},
		{database.ErrConflict, database.ResourceConflict},
		{nil, database.ResourceUnavailable}, // 成功但无 OID 视为不确定，交给下一轮观察
	} {
		failed := database.Reconstitute(snapshot, dependencies)
		failed.DatabaseCreated(0, test.err)
		if state := failed.Snapshot().Resources; state.Phase != test.want || state.DatabaseOID != 0 {
			t.Fatalf("创建失败分类错误：%v", test.err)
		}
	}
	snapshot.Resources.Phase = database.ResourceConflict
	if database.Reconstitute(snapshot, dependencies).PlanResources() != database.Wait {
		t.Fatal("冲突需人工处理，不自动重入")
	}
}

func TestExtensions(t *testing.T) {
	snapshot, dependencies := fixture(t)
	dependencies.Extensions = instance.NewExtensionSet([]string{extensionHstore, extensionTrigram})
	db := database.Reconstitute(snapshot, dependencies)
	available := []string{extensionHstore, extensionTrigram, extensionPlpgsql}
	if db.ObserveExtensions(database.ExtensionFacts{}, database.ErrUnavailable) != database.Wait ||
		db.Snapshot().Resources.Phase != database.ResourceUnavailable {
		t.Fatal("读取失败不得视为没有扩展")
	}
	if db.ObserveExtensions(database.ExtensionFacts{Available: []string{extensionHstore}}, nil) != database.Wait ||
		!strings.Contains(db.Snapshot().Resources.Message, extensionTrigram) {
		t.Fatal("实例不提供的扩展应等待并指出名称")
	}
	if db.ObserveExtensions(database.ExtensionFacts{Available: available, Installed: []string{extensionPlpgsql, extensionHstore}}, nil) != database.InstallExtensions ||
		!slices.Equal(db.MissingExtensions(), []string{extensionTrigram}) {
		t.Fatalf("应只安装缺少的扩展：%v", db.MissingExtensions())
	}
	db.ExtensionsInstalled(database.ErrUnavailable)
	if db.Snapshot().Resources.Phase != database.ResourceUnavailable {
		t.Fatal("安装被拒绝时应等待重试")
	}
	// 申请之外已安装的扩展不卸载，也不阻止就绪。
	if db.ObserveExtensions(database.ExtensionFacts{Available: available, Installed: []string{"citext", extensionHstore, extensionTrigram, extensionPlpgsql}}, nil) != database.VerifyLogin {
		t.Fatal("扩展齐全后应验证登录")
	}
}

func instanceExtensions(names ...string) instance.ExtensionSet {
	return instance.NewExtensionSet(names)
}
