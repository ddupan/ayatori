package database_test

import (
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
)

func TestLifecycle(t *testing.T) {
	for _, test := range []struct {
		name     string
		released bool
		deleting bool
		bound    bool
		policy   database.ReclaimPolicy
		source   string
		want     database.Lifecycle
	}{
		{"绑定中正常供应", false, false, true, database.ReclaimDelete, sourceProvision, database.Continue},
		{"绑定中删除受保护", false, true, true, database.ReclaimDelete, sourceProvision, database.Hold},
		{"从未绑定直接放行", false, true, false, database.ReclaimDelete, sourceProvision, database.Forget},
		{"从未绑定且未删除不清理", false, false, false, database.ReclaimDelete, sourceProvision, database.Continue},
		{"Retain 释放后保留", true, false, true, database.ReclaimRetain, sourceProvision, database.Continue},
		{"Retain 释放后删除只放行", true, true, true, database.ReclaimRetain, sourceProvision, database.Forget},
		{"Delete 释放后清理", true, false, true, database.ReclaimDelete, sourceProvision, database.Reclaim},
		{"Delete 释放后删除也清理", true, true, true, database.ReclaimDelete, sourceProvision, database.Reclaim},
		{"导入资源不隐含删除授权", true, true, true, database.ReclaimDelete, "Import", database.Hold},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dependencies := fixture(t)
			snapshot.Binding.Source, snapshot.Binding.Deleting, snapshot.ReclaimPolicy = test.source, test.deleting, test.policy
			if test.released {
				snapshot.Binding.Phase = database.Released
			}
			if !test.bound {
				snapshot.Binding.Tenant = nil
			}
			if got := database.Reconstitute(snapshot, dependencies).PlanLifecycle(); got != test.want {
				t.Fatalf("生命周期 = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReclaimSteps(t *testing.T) {
	const role, owned = 11, 22
	snapshot, dependencies := fixture(t)
	snapshot.Resources = database.ResourceState{RoleOID: role, DatabaseOID: owned}
	snapshot.Credential.Location = &testLocation
	for _, test := range []struct {
		name  string
		facts database.ResourceFacts
		want  database.Action
	}{
		{"先删数据库", database.ResourceFacts{RoleOID: role, DatabaseOID: owned, OwnerOID: role}, database.DropDatabase},
		{"再删角色", database.ResourceFacts{RoleOID: role}, database.DropRole},
		{"最后删凭据", database.ResourceFacts{}, database.DestroyCredential},
		{"同名数据库不是已确认对象", database.ResourceFacts{RoleOID: role, DatabaseOID: 99}, database.Wait},
		{"同名角色被重建", database.ResourceFacts{RoleOID: 98}, database.Wait},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := database.Reconstitute(snapshot, dependencies)
			if got := db.ObserveReclaim(test.facts, nil); got != test.want {
				t.Fatalf("清理步骤 = %v, want %v", got, test.want)
			}
			if test.want == database.Wait && db.Snapshot().Resources.Phase != database.ResourceConflict {
				t.Fatal("身份不符必须报冲突")
			}
		})
	}
	snapshot.Credential.Location = nil
	if database.Reconstitute(snapshot, dependencies).ObserveReclaim(database.ResourceFacts{}, nil) != database.Finalize {
		t.Fatal("外部资源都已清理后应完成回收")
	}
	db := database.Reconstitute(snapshot, dependencies)
	if db.ObserveReclaim(database.ResourceFacts{}, database.ErrUnavailable) != database.Wait || db.Snapshot().Resources.Phase != database.ResourceUnavailable {
		t.Fatal("观察失败应等待重试")
	}
	db.Reclaimed("删除角色", database.ErrConflict)
	if db.Snapshot().Resources.Phase != database.ResourceConflict {
		t.Fatal("依赖对象阻止删除时应交给人工")
	}
}
