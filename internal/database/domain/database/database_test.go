package database_test

import (
	"errors"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const (
	replacedUID     = "new"
	testApplication = "sampleapp"
	testInstance    = "test-instance"
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
		Binding: binding.Database{Identity: databaseID, Tenant: &tenantID, Instance: testInstance, InstanceUID: "instance-id",
			Name: testApplication, LoginRole: testApplication, Source: "Provision"},
		Protected: true,
	}, database.Dependencies{
		Tenant: &binding.Tenant{Identity: tenantID, Database: &databaseID, Phase: binding.Bound,
			Request: binding.Request{Provision: &binding.ProvisionRequest{Instance: testInstance}}},
		TenantProtected: true,
		Instance:        &database.Instance{Identity: binding.Identity{Name: testInstance, UID: "instance-id"}, Ready: true, Endpoint: endpoint},
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
		{"中断创建", database.CredentialCreating, 0, database.Wait, database.CredentialConflict},
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
		if _, ok := database.Reconstitute(pinned, dependencies).BeginCredentialCreation(existing); ok {
			t.Fatal("固定位置已有值时不得生成凭据")
		}
	}
	if _, ok := database.Reconstitute(pinned, dependencies).BeginCredentialCreation(database.ErrUnavailable); ok {
		t.Fatal("无法确认位置为空时不得生成凭据")
	}
	for _, test := range []struct {
		name    string
		version int64
		err     error
		want    database.CredentialPhase
	}{
		{"明确权限拒绝", 0, database.ErrUnavailable, database.CredentialUnavailable},
		{"结果不确定", 0, database.ErrUncertain, database.CredentialConflict},
		{"并发冲突", 0, database.ErrConflict, database.CredentialConflict},
		{"非首次版本", 2, nil, database.CredentialConflict},
		{"首次创建回读", 1, nil, database.CredentialPrepared},
	} {
		t.Run(test.name, func(t *testing.T) {
			creating := database.Reconstitute(pinned, dependencies)
			value, ok := creating.BeginCredentialCreation(database.ErrNotFound)
			if !ok || creating.Snapshot().Credential.Phase != database.CredentialCreating || value.Password() == "" {
				t.Fatal("位置为空时应生成凭据并进入检查点")
			}
			creating.CredentialCreated(test.version, test.err)
			state := creating.Snapshot().Credential
			if state.Phase != test.want || (state.Version == 1) != (test.want == database.CredentialPrepared) {
				t.Fatal("确认时机不符")
			}
		})
	}
}

func TestResourceResumeAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  database.ResourceState
		resume bool
	}{
		{"初次", database.ResourceState{}, true},
		{"角色创建不确定", database.ResourceState{Phase: database.ResourceCreatingRole}, false},
		{"库创建不确定", database.ResourceState{RoleOID: 11, Phase: database.ResourceCreatingDatabase}, false},
		{"已确认角色", database.ResourceState{RoleOID: 11, Phase: database.ResourcePending}, true},
		{"已确认资源暂不可用", database.ResourceState{RoleOID: 11, DatabaseOID: 22, Phase: database.ResourceUnavailable}, true},
		{"冲突保持", database.ResourceState{RoleOID: 11, Phase: database.ResourceConflict, Message: "首次诊断"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies := fixture(t)
			snapshot.Credential = database.CredentialState{Location: &testLocation, Version: 1, Phase: database.CredentialPrepared}
			snapshot.Resources = test.state
			db := database.Reconstitute(snapshot, dependencies)
			if (db.PlanResources() == database.InspectResources) != test.resume {
				t.Fatal("恢复决策错误")
			}
			state := db.Snapshot().Resources
			if state.RoleOID != test.state.RoleOID || state.DatabaseOID != test.state.DatabaseOID {
				t.Fatal("恢复丢失确认")
			}
			if !test.resume && state.Phase != database.ResourceConflict {
				t.Fatal("不确定创建未转冲突")
			}
			if test.state.Phase == database.ResourceConflict && state != test.state {
				t.Fatal("冲突诊断被覆盖")
			}
		})
	}

	snapshot, dependencies := fixture(t)
	snapshot.Resources = database.ResourceState{RoleOID: 11, DatabaseOID: 22}
	db := database.Reconstitute(snapshot, dependencies)
	if err := db.CheckResources(database.ResourceFacts{RoleOID: 11, DatabaseOID: 22, OwnerOID: 11}); err != nil {
		t.Fatal(err)
	}
	for _, facts := range []database.ResourceFacts{
		{}, {RoleOID: 12, DatabaseOID: 22, OwnerOID: 12}, {RoleOID: 11, DatabaseOID: 23, OwnerOID: 11}, {RoleOID: 11, DatabaseOID: 22, OwnerOID: 99},
	} {
		if db.CheckResources(facts) == nil {
			t.Fatal("对象身份或 owner 漂移被接受")
		}
	}
	unconfirmed, _ := fixture(t)
	if database.Reconstitute(unconfirmed, dependencies).CheckResources(database.ResourceFacts{RoleOID: 11}) == nil {
		t.Fatal("未确认同名对象被认领")
	}
}

func TestResourceSteps(t *testing.T) {
	snapshot, dependencies := fixture(t)
	snapshot.Credential = database.CredentialState{Location: &testLocation, Version: 1, Phase: database.CredentialPending}
	db := database.Reconstitute(snapshot, dependencies)
	if db.PlanResources() != database.Wait {
		t.Fatal("凭据未在本轮验证时不得观察 PostgreSQL")
	}
	snapshot.Credential.Phase = database.CredentialPrepared
	db = database.Reconstitute(snapshot, dependencies)
	if db.PlanResources() != database.InspectResources || db.ObserveResources(database.ResourceFacts{}, nil) != database.CreateRole {
		t.Fatal("空实例应先创建角色")
	}
	db.BeginRoleCreation()
	db.RoleCreated(11, nil)
	if db.ObserveResources(database.ResourceFacts{RoleOID: 11}, nil) != database.CreateDatabase {
		t.Fatal("角色确认后应创建数据库")
	}
	db.BeginDatabaseCreation()
	db.DatabaseCreated(22, nil)
	if db.ObserveResources(database.ResourceFacts{RoleOID: 11, DatabaseOID: 22, OwnerOID: 11}, nil) != database.OpenAccess {
		t.Fatal("对象确认后应收敛访问权限")
	}
	db.AccessOpened(nil)
	if state := db.Snapshot().Resources; state.Phase != database.ResourceAvailable || state.RoleOID != 11 || state.DatabaseOID != 22 {
		t.Fatal("访问收敛后应 Available 且保留确认")
	}
	for _, test := range []struct {
		err  error
		want database.ResourcePhase
	}{
		{database.ErrUnavailable, database.ResourceUnavailable},
		{database.ErrUncertain, database.ResourceConflict},
		{nil, database.ResourceConflict}, // 成功但无 OID 视为不确定
	} {
		failed := database.Reconstitute(snapshot, dependencies)
		failed.BeginRoleCreation()
		failed.RoleCreated(0, test.err)
		if state := failed.Snapshot().Resources; state.Phase != test.want || state.RoleOID != 0 {
			t.Fatalf("创建失败分类错误：%v", test.err)
		}
	}
}
