package database_test

import (
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
)

func importFixture(t *testing.T) (database.Snapshot, database.Dependencies, credential.ApplicationCredential) {
	t.Helper()
	snapshot, dependencies := fixture(t)
	snapshot.Binding.Source, snapshot.Binding.Tenant, snapshot.Binding.InstanceUID = "Import", nil, ""
	location := testLocation
	snapshot.ImportLocation = &location
	value, err := credential.NewApplicationCredential(testApplication, "existing-password", testApplication, dependencies.Instance.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, dependencies, value
}

func TestImportRegistration(t *testing.T) {
	const manager, role, owned = 5, 11, 22
	snapshot, dependencies, value := importFixture(t)
	facts := database.ResourceFacts{RoleOID: role, DatabaseOID: owned, OwnerOID: role, ManagerOID: manager, ManagerConnect: true}
	db := database.Reconstitute(snapshot, dependencies)
	if db.PlanImport() != database.InspectResources || db.ObserveImport(value, 3, nil, facts, nil) != database.InspectExtensions {
		t.Fatal("完整的导入目标应进入扩展与登录验证")
	}
	if db.ObserveExtensions(database.ExtensionFacts{Available: []string{extensionPlpgsql}, Installed: []string{extensionPlpgsql}}, nil) != database.VerifyLogin {
		t.Fatal("扩展齐全后应验证登录")
	}
	if db.Snapshot().Resources.DatabaseOID != 0 {
		t.Fatal("登录验证前不得登记")
	}
	db.LoginVerified(nil)
	state := db.Snapshot()
	if !db.Ready() || state.Binding.Phase != database.Available || state.Resources.RoleOID != role || state.Resources.DatabaseOID != owned ||
		state.Credential.Version != 3 || *state.Credential.Location != testLocation || state.Binding.InstanceUID != instanceUID {
		t.Fatalf("登记结果不符：%+v", state)
	}

	// 登记后与动态供应一样拒绝重建。
	registered := database.Reconstitute(state, dependencies)
	if registered.ObserveImport(value, 3, nil, database.ResourceFacts{RoleOID: 12, DatabaseOID: owned, OwnerOID: 12, ManagerConnect: true}, nil) != database.Wait ||
		registered.Snapshot().Resources.Phase != database.ResourceConflict {
		t.Fatal("已登记角色被重建必须冲突")
	}
	if registered.PlanImport() != database.Wait {
		t.Fatal("冲突需要人工处理")
	}
}

func TestImportRejects(t *testing.T) {
	const manager, role, owned = 5, 11, 22
	good := database.ResourceFacts{RoleOID: role, DatabaseOID: owned, OwnerOID: role, ManagerOID: manager, ManagerConnect: true}
	for _, test := range []struct {
		name  string
		err   error
		facts database.ResourceFacts
		want  database.ResourcePhase
	}{
		{"凭据不存在", database.ErrNotFound, good, database.ResourceUnavailable},
		{"已确认凭据漂移", database.ErrConflict, good, database.ResourceConflict},
		{"目标不存在", nil, database.ResourceFacts{ManagerOID: manager}, database.ResourceUnavailable},
		{"owner 不是 loginRole", nil, database.ResourceFacts{RoleOID: role, DatabaseOID: owned, OwnerOID: manager, ManagerOID: manager, ManagerConnect: true}, database.ResourceConflict},
		{"管理账号无 CONNECT", nil, database.ResourceFacts{RoleOID: role, DatabaseOID: owned, OwnerOID: role, ManagerOID: manager}, database.ResourceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies, value := importFixture(t)
			db := database.Reconstitute(snapshot, dependencies)
			if db.ObserveImport(value, 1, test.err, test.facts, nil) != database.Wait || db.Snapshot().Resources.Phase != test.want {
				t.Fatalf("阶段 = %s, want %s", db.Snapshot().Resources.Phase, test.want)
			}
		})
	}
	snapshot, dependencies, value := importFixture(t)
	dependencies.Extensions = instanceExtensions(extensionHstore)
	db := database.Reconstitute(snapshot, dependencies)
	db.ObserveImport(value, 1, nil, good, nil)
	if db.ObserveExtensions(database.ExtensionFacts{Available: []string{extensionHstore}}, nil) != database.Wait {
		t.Fatal("导入资源不安装扩展")
	}
	snapshot.Binding.Phase = database.Released
	if database.Reconstitute(snapshot, dependencies).PlanImport() != database.Wait {
		t.Fatal("已释放的导入记录不再验证，需删除后重新导入")
	}
}
