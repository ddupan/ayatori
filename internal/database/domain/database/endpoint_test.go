package database_test

import (
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func movedEndpoint(t *testing.T) instance.Endpoint {
	t.Helper()
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host: "postgres.moved.example", HostAddr: "192.0.2.9", Port: 5432, ManagementDatabase: "management", TLSMode: instance.TLSVerifyFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func confirmed(t *testing.T, version int64) (database.Snapshot, database.Dependencies, credential.ApplicationCredential) {
	t.Helper()
	snapshot, dependencies := fixture(t)
	location := testLocation
	snapshot.Credential = database.CredentialState{Location: &location, Version: version, Phase: database.CredentialPrepared}
	value, err := credential.NewApplicationCredential(testApplication, "password", testApplication, dependencies.Instance.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, dependencies, value
}

// 连接信息是 Instance 的可变属性：只有它变化时以同一密码改写，不判定为新资源或冲突。
func TestCredentialFollowsInstanceEndpoint(t *testing.T) {
	snapshot, dependencies, value := confirmed(t, 2)
	dependencies.Instance.Endpoint = movedEndpoint(t)
	db := database.Reconstitute(snapshot, dependencies)
	if db.VerifyCredential(value, nil) {
		t.Fatal("连接信息变化时不能直接使用旧凭据")
	}
	rewritten, ok := db.CredentialRewrite()
	if !ok || db.Snapshot().Credential.Phase != database.CredentialPending {
		t.Fatal("连接信息变化应改写而不是冲突")
	}
	if rewritten.Password() != value.Password() || !rewritten.MatchesTarget(testApplication, testApplication, dependencies.Instance.Endpoint) {
		t.Fatal("改写应保留身份与密码，只替换连接信息")
	}
	db.CredentialRewritten(3, nil)
	if state := db.Snapshot().Credential; state.Version != 3 || state.Phase != database.CredentialPrepared {
		t.Fatalf("改写成功应记录下一版本：%+v", state)
	}
	if _, ok := db.CredentialRewrite(); ok {
		t.Fatal("改写完成后不应重复改写")
	}
}

func TestCredentialRewriteOutcomes(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int64
		err     error
		phase   database.CredentialPhase
		want    int64
	}{
		{"CAS 冲突", 0, database.ErrConflict, database.CredentialConflict, 2},
		{"跳过版本", 4, nil, database.CredentialConflict, 2},
		{"结果不确定", 0, database.ErrUncertain, database.CredentialUnavailable, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies, value := confirmed(t, 2)
			dependencies.Instance.Endpoint = movedEndpoint(t)
			db := database.Reconstitute(snapshot, dependencies)
			db.VerifyCredential(value, nil)
			db.CredentialRewritten(test.version, test.err)
			if state := db.Snapshot().Credential; state.Phase != test.phase || state.Version != test.want {
				t.Fatalf("改写结果判定错误：%+v", state)
			}
		})
	}
}

// 登录角色或数据库不同才是同一位置被换成了别的对象，仍然冲突且不改写。
func TestCredentialIdentityMismatchStaysConflict(t *testing.T) {
	snapshot, dependencies, _ := confirmed(t, 1)
	other, err := credential.NewApplicationCredential("otherrole", "password", testApplication, dependencies.Instance.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	db := database.Reconstitute(snapshot, dependencies)
	if db.VerifyCredential(other, nil) || db.Snapshot().Credential.Phase != database.CredentialConflict {
		t.Fatal("身份不一致应冲突")
	}
	if _, ok := db.CredentialRewrite(); ok {
		t.Fatal("身份不一致不得改写")
	}
}

// 管理员显式声明的导入凭据交由本资源管理：首次登记只读，登记后连接信息变化即改写。
func TestImportRewritesEndpointAfterRegistration(t *testing.T) {
	const role, owned = 11, 22
	snapshot, dependencies, value := importFixture(t)
	dependencies.Instance.Endpoint = movedEndpoint(t)
	facts := database.ResourceFacts{RoleOID: role, DatabaseOID: owned, OwnerOID: role, ManagerConnect: true}

	fresh := database.Reconstitute(snapshot, dependencies)
	if fresh.ObserveImport(value, 1, nil, facts, nil) != database.InspectExtensions {
		t.Fatal("首次登记不因连接信息过期而阻塞")
	}
	if _, ok := fresh.CredentialRewrite(); ok {
		t.Fatal("首次登记只读，不改写")
	}

	location := testLocation
	snapshot.Resources = database.ResourceState{RoleOID: role, DatabaseOID: owned, Phase: database.ResourceAvailable}
	snapshot.Credential = database.CredentialState{Location: &location, Version: 1, Phase: database.CredentialPrepared}
	registered := database.Reconstitute(snapshot, dependencies)
	if registered.ObserveImport(value, 1, nil, facts, nil) != database.Wait {
		t.Fatal("登记后连接信息变化应先改写凭据")
	}
	if rewritten, ok := registered.CredentialRewrite(); !ok || rewritten.Password() != value.Password() {
		t.Fatal("导入凭据应以同一密码改写连接信息")
	}
}
