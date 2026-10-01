//go:build integration

package postgresql_test

import (
	"context"
	"errors"
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	databasedomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const suppliedName = "supplied"

func testProvisioningLogin(t *testing.T, f *provisioningFixture) {
	t.Helper()
	database := f.bound(t, suppliedName)
	f.reconcile(t, database, databasedomain.ResourcePending)
	if database.Status.DatabaseOID == 0 || database.Status.RoleOID != 0 {
		t.Fatal("数据库确认未独立保存，或角色未最后创建")
	}
	f.service.Forget("supply-instance")
	f.reconcile(t, database, databasedomain.ResourcePending)
	f.reconcile(t, database, databasedomain.ResourcePending)
	if database.Status.RoleOID == 0 {
		t.Fatal("角色未独立确认")
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
	config.User, config.Database, config.Password = suppliedName, suppliedName, value.Password()
	config.TLSConfig, config.Fallbacks = nil, nil
	if early, err := pgx.ConnectConfig(f.ctx, config); err == nil {
		_ = early.Close(f.ctx)
		t.Fatal("移交 owner 前应用角色不应能连接")
	}
	f.reconcile(t, database, databasedomain.ResourcePending)
	f.reconcile(t, database, databasedomain.ResourceAvailable)
	if f.queryPostgres(t, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='supplied'") != suppliedName ||
		f.queryPostgres(t, "SELECT has_database_privilege('native_manager', 'supplied', 'CONNECT')") != "t" {
		t.Fatal("owner 未移交，或管理账号失去 CONNECT")
	}
	beforeRole, beforeDB := database.Status.RoleOID, database.Status.DatabaseOID
	for range 2 {
		f.reconcile(t, database, databasedomain.ResourceAvailable)
	}
	if beforeRole != database.Status.RoleOID || beforeDB != database.Status.DatabaseOID {
		t.Fatal("幂等协调替换了对象")
	}
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
		f.reconcile(t, database, databasedomain.ResourceConflict)
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT oid FROM pg_roles WHERE rolname='occupied'") != before ||
			f.queryPostgres(t, "SELECT count(*) FROM pg_database WHERE datname='occupied'") != "0" {
			t.Fatal("未知角色被认领、修改或继续建库")
		}
	})
	t.Run("未知同名数据库不认领", func(t *testing.T) {
		database := f.bound(t, "occupied_database")
		f.queryPostgres(t, "CREATE DATABASE occupied_database")
		before := f.queryPostgres(t, "SELECT oid FROM pg_database WHERE datname='occupied_database'")
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.RoleOID != 0 || database.Status.DatabaseOID != 0 ||
			f.queryPostgres(t, "SELECT oid FROM pg_database WHERE datname='occupied_database'") != before ||
			f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='occupied_database'") != "0" {
			t.Fatal("未知数据库被认领、修改或继续创建了角色")
		}
	})
	t.Run("管理权限恢复后继续", func(t *testing.T) {
		database := f.bound(t, "permission_restore")
		f.queryPostgres(t, "ALTER ROLE native_manager NOCREATEDB")
		f.reconcile(t, database, databasedomain.ResourceUnavailable)
		if database.Status.DatabaseOID != 0 {
			t.Fatal("权限被拒绝时不应记录数据库")
		}
		f.queryPostgres(t, "ALTER ROLE native_manager CREATEDB")
		for range 4 {
			f.reconcile(t, database, databasedomain.ResourcePending)
		}
		f.reconcile(t, database, databasedomain.ResourceAvailable)
	})
	t.Run("删除前置阻止写入", func(t *testing.T) {
		database := f.bound(t, "deleting_supply")
		if err := f.api.Delete(f.ctx, database); err != nil {
			t.Fatal(err)
		}
		f.reconcile(t, database, databasedomain.ResourceStopped)
		if len(database.Finalizers) == 0 || database.Status.DatabaseOID != 0 {
			t.Fatal("删除边界被供应绕过")
		}
	})
	t.Run("确认保存失败转人工冲突", func(t *testing.T) {
		database := f.bound(t, "lost_confirmation")
		f.reconcile(t, database, databasedomain.ResourcePending)
		f.reconcile(t, database, databasedomain.ResourcePending)
		failed := *f.reconciler
		failed.Client = failStatusUpdate(f.api, func(object *databasev1alpha1.PostgreSQLDatabase) bool { return object.Status.RoleOID != 0 })
		if err := reconcileOnce(f.ctx, &failed, database); err == nil {
			t.Fatal("未注入确认写入故障")
		}
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='lost_confirmation'") != "1" {
			t.Fatal("失败恢复认领或清理了残留角色")
		}
	})
	t.Run("数据库确认失败不继续建角色", func(t *testing.T) {
		database := f.bound(t, "lost_database_confirmation")
		failed := *f.reconciler
		failed.Client = failStatusUpdate(f.api, func(object *databasev1alpha1.PostgreSQLDatabase) bool { return object.Status.DatabaseOID != 0 })
		if err := reconcileOnce(f.ctx, &failed, database); err == nil {
			t.Fatal("未注入数据库确认写入故障")
		}
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.DatabaseOID != 0 || f.queryPostgres(t, "SELECT count(*) FROM pg_database WHERE datname='lost_database_confirmation'") != "1" ||
			f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='lost_database_confirmation'") != "0" {
			t.Fatal("未确认数据库被认领、清理或继续创建角色")
		}
	})
	t.Run("创建成功但调用方丢失结果", func(t *testing.T) {
		database := f.bound(t, "lost_role_response")
		f.reconcile(t, database, databasedomain.ResourcePending)
		f.reconcile(t, database, databasedomain.ResourcePending)
		failed := *f.reconciler
		failed.PostgreSQL = &lostRoleResponse{PostgreSQLBackend: f.service}
		if err := reconcileOnce(f.ctx, &failed, database); err != nil {
			t.Fatal(err)
		}
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='lost_role_response'") != "1" {
			t.Fatal("丢失结果后应保留未认领角色")
		}
	})
	t.Run("同名角色被重建", func(t *testing.T) {
		database := f.bound(t, "replaced_role")
		for range 3 {
			f.reconcile(t, database, databasedomain.ResourcePending)
		}
		f.queryPostgres(t, "DROP ROLE replaced_role; CREATE ROLE replaced_role LOGIN")
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if f.queryPostgres(t, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='replaced_role'") != "native_manager" {
			t.Fatal("重建角色被当成原 owner 继续移交")
		}
	})
	t.Run("实际manager观察与重启", func(t *testing.T) { testProvisioningManager(t, f) })
}

// failStatusUpdate 只在确认保存边界注入故障；此前的外部写入使用真实 PostgreSQL。
func failStatusUpdate(api client.WithWatch, refuse func(*databasev1alpha1.PostgreSQLDatabase) bool) client.Client {
	return interceptor.NewClient(api, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if database, ok := object.(*databasev1alpha1.PostgreSQLDatabase); ok && refuse(database) {
				return errors.New("injected confirmation persistence failure")
			}
			return c.SubResource(subResource).Update(ctx, object, options...)
		},
	})
}

type lostRoleResponse struct {
	databasecontroller.PostgreSQLBackend
}

func (b *lostRoleResponse) CreateLoginRole(ctx context.Context, target instance.ObservationTarget, db *databasedomain.Database, value credential.ApplicationCredential) (uint32, error) {
	if _, err := b.PostgreSQLBackend.CreateLoginRole(ctx, target, db, value); err != nil {
		return 0, err
	}
	return 0, databasedomain.ErrUncertain
}
