//go:build integration

package postgresql_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/provisioning"
	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func testProvisioningLogin(t *testing.T, f *provisioningFixture) {
	t.Helper()
	database := f.bound(t, "supplied")
	f.reconcile(t, database, provisioning.Pending)
	if database.Status.RoleOID == 0 || database.Status.DatabaseOID != 0 {
		t.Fatal("角色确认未独立保存")
	}
	f.service.Forget("supply-instance")
	f.reconcile(t, database, provisioning.Pending)
	if database.Status.DatabaseOID == 0 {
		t.Fatal("数据库未独立确认")
	}
	if got := f.queryPostgres(t, "SELECT datallowconn FROM pg_database WHERE datname='supplied'"); got != "f" {
		t.Fatal("ACL 收紧前连接入口应关闭")
	}
	f.reconcile(t, database, provisioning.Available)
	beforeRole, beforeDB := database.Status.RoleOID, database.Status.DatabaseOID
	for range 2 {
		f.reconcile(t, database, provisioning.Available)
	}
	if beforeRole != database.Status.RoleOID || beforeDB != database.Status.DatabaseOID {
		t.Fatal("幂等协调替换了对象")
	}
	value, err := f.store.ReadCredential(f.ctx, credential.Location{Mount: database.Status.CredentialRef.Mount, Path: database.Status.CredentialRef.Path}, database.Status.CredentialVersion)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal("fixture 配置失败")
	}
	config.Host, config.Port = fixtureAddress, uint16(f.port)
	config.User, config.Database, config.Password = "supplied", "supplied", value.SecretData()["password"].(string)
	config.TLSConfig, config.Fallbacks = nil, nil
	connection, err := pgx.ConnectConfig(f.ctx, config)
	if err != nil {
		t.Fatal("实际应用密码不能登录已供应数据库")
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err := connection.Exec(f.ctx, "CREATE TABLE app_data (id integer)"); err != nil {
		t.Fatal("应用 owner 不能创建表")
	}
	if _, err := connection.Exec(f.ctx, "CREATE ROLE should_be_denied"); err == nil {
		t.Fatal("应用具有 CREATEROLE")
	}
	if _, err := connection.Exec(f.ctx, "CREATE DATABASE should_be_denied"); err == nil {
		t.Fatal("应用具有 CREATEDB")
	}
	f.queryPostgres(t, "CREATE ROLE unrelated_login LOGIN PASSWORD '"+fixturePassword+"'")
	config.User, config.Password = "unrelated_login", fixturePassword
	if outsider, err := pgx.ConnectConfig(f.ctx, config); err == nil {
		_ = outsider.Close(f.ctx)
		t.Fatal("其他账号可连接受管数据库")
	}
}

func TestDatabaseProvisioningWithRealBackends(t *testing.T) {
	f := newProvisioningFixture(t)
	t.Run("创建重启幂等与实际登录", func(t *testing.T) { testProvisioningLogin(t, f) })
	t.Run("未知同名不认领", func(t *testing.T) {
		database := f.bound(t, "occupied")
		f.queryPostgres(t, "CREATE ROLE occupied LOGIN")
		before := f.queryPostgres(t, "SELECT oid FROM pg_roles WHERE rolname='occupied'")
		f.reconcile(t, database, provisioning.Conflict)
		f.reconcile(t, database, provisioning.Conflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT oid FROM pg_roles WHERE rolname='occupied'") != before {
			t.Fatal("未知角色被认领或修改")
		}
	})
	t.Run("未知同名数据库不认领", func(t *testing.T) {
		database := f.bound(t, "occupied_database")
		f.queryPostgres(t, "CREATE DATABASE occupied_database")
		before := f.queryPostgres(t, "SELECT oid FROM pg_database WHERE datname='occupied_database'")
		f.reconcile(t, database, provisioning.Conflict)
		if database.Status.RoleOID != 0 || database.Status.DatabaseOID != 0 ||
			f.queryPostgres(t, "SELECT oid FROM pg_database WHERE datname='occupied_database'") != before ||
			f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='occupied_database'") != "0" {
			t.Fatal("未知数据库被认领、修改或继续创建了角色")
		}
	})
	t.Run("已确认角色后权限恢复", func(t *testing.T) {
		database := f.bound(t, "permission_restore")
		f.reconcile(t, database, provisioning.Pending)
		f.queryPostgres(t, "ALTER ROLE native_manager NOCREATEDB")
		f.reconcile(t, database, provisioning.Unavailable)
		f.queryPostgres(t, "ALTER ROLE native_manager CREATEDB")
		f.reconcile(t, database, provisioning.Pending)
		f.reconcile(t, database, provisioning.Available)
	})
	t.Run("删除前置阻止写入", func(t *testing.T) {
		database := f.bound(t, "deleting_supply")
		if err := f.api.Delete(f.ctx, database); err != nil {
			t.Fatal(err)
		}
		f.reconcile(t, database, provisioning.Stopped)
		if len(database.Finalizers) == 0 || database.Status.RoleOID != 0 {
			t.Fatal("删除边界被供应绕过")
		}
	})
	t.Run("确认保存失败转人工冲突", func(t *testing.T) {
		database := f.bound(t, "lost_confirmation")
		failed := f.usecase
		failed.Resources = &failRoleConfirmation{ProvisioningResources: f.resources}
		if err := failed.Reconcile(f.ctx, database.Name); err == nil {
			t.Fatal("未注入确认写入故障")
		}
		f.reconcile(t, database, provisioning.Conflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='lost_confirmation'") != "1" {
			t.Fatal("失败恢复认领或清理了残留角色")
		}
	})
	t.Run("数据库确认失败保留关闭入口", func(t *testing.T) {
		database := f.bound(t, "lost_database_confirmation")
		f.reconcile(t, database, provisioning.Pending)
		failed := f.usecase
		failed.Resources = &failDatabaseConfirmation{ProvisioningResources: f.resources}
		if err := failed.Reconcile(f.ctx, database.Name); err == nil {
			t.Fatal("未注入数据库确认写入故障")
		}
		f.reconcile(t, database, provisioning.Conflict)
		if database.Status.RoleOID == 0 || database.Status.DatabaseOID != 0 || f.queryPostgres(t, "SELECT datallowconn FROM pg_database WHERE datname='lost_database_confirmation'") != "f" {
			t.Fatal("未确认数据库被认领或开放连接")
		}
	})
	t.Run("创建成功但调用方丢失结果", func(t *testing.T) {
		database := f.bound(t, "lost_role_response")
		failed := f.usecase
		failed.Backend = &lostRoleResponse{ProvisioningBackend: f.service}
		if err := failed.Reconcile(f.ctx, database.Name); err != nil {
			t.Fatal(err)
		}
		f.reconcile(t, database, provisioning.Conflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='lost_role_response'") != "1" {
			t.Fatal("丢失结果后应保留未认领角色")
		}
	})
	t.Run("同名角色被重建", func(t *testing.T) {
		database := f.bound(t, "replaced_role")
		f.reconcile(t, database, provisioning.Pending)
		f.queryPostgres(t, "DROP ROLE replaced_role; CREATE ROLE replaced_role LOGIN")
		f.reconcile(t, database, provisioning.Conflict)
		if database.Status.DatabaseOID != 0 {
			t.Fatal("重建角色被当成原 owner 继续供应")
		}
	})
	t.Run("创建步骤并发只有一个获准", func(t *testing.T) {
		database := f.bound(t, "concurrent_supply")
		service := f.usecase
		resources := &concurrentProvisioningResources{ProvisioningResources: f.resources}
		resources.loaded.Add(2)
		service.Resources = resources
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- service.Reconcile(f.ctx, database.Name) }()
		}
		success, conflict := 0, 0
		for range 2 {
			err := <-results
			if err == nil {
				success++
			} else if apierrors.IsConflict(err) {
				conflict++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatal("同一快照不得重复授权创建")
		}
		f.status(t, database, provisioning.Pending)
		if database.Status.RoleOID == 0 {
			t.Fatal("胜方未确认角色")
		}
	})
	t.Run("实际manager观察与重启", func(t *testing.T) { testProvisioningManager(t, f) })
}

type failRoleConfirmation struct {
	application.ProvisioningResources
}

func (r *failRoleConfirmation) Save(ctx context.Context, record *application.ProvisioningRecord, state provisioning.State) (*application.ProvisioningRecord, error) {
	if state.RoleOID != 0 {
		return nil, errors.New("injected confirmation persistence failure")
	}
	return r.ProvisioningResources.Save(ctx, record, state)
}

type failDatabaseConfirmation struct {
	application.ProvisioningResources
}

func (r *failDatabaseConfirmation) Save(ctx context.Context, record *application.ProvisioningRecord, state provisioning.State) (*application.ProvisioningRecord, error) {
	if state.DatabaseOID != 0 {
		return nil, errors.New("injected database confirmation persistence failure")
	}
	return r.ProvisioningResources.Save(ctx, record, state)
}

type lostRoleResponse struct {
	application.ProvisioningBackend
}

func (b *lostRoleResponse) CreateLoginRole(ctx context.Context, target instance.ObservationTarget, value credential.ApplicationCredential) (uint32, error) {
	if _, err := b.ProvisioningBackend.CreateLoginRole(ctx, target, value); err != nil {
		return 0, err
	}
	return 0, application.ErrResourceUncertain
}

type concurrentProvisioningResources struct {
	application.ProvisioningResources
	readers atomic.Int32
	loaded  sync.WaitGroup
}

func (r *concurrentProvisioningResources) Load(ctx context.Context, name string) (*application.ProvisioningRecord, error) {
	record, err := r.ProvisioningResources.Load(ctx, name)
	if r.readers.Add(1) <= 2 {
		r.loaded.Done()
		r.loaded.Wait()
	}
	return record, err
}
