//go:build integration

package postgresql_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	databasedomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testProvisioningLogin(t *testing.T, f *provisioningFixture) {
	t.Helper()
	database := f.bound(t, "supplied")
	f.reconcile(t, database, databasedomain.ResourcePending)
	if database.Status.RoleOID == 0 || database.Status.DatabaseOID != 0 {
		t.Fatal("角色确认未独立保存")
	}
	f.service.Forget("supply-instance")
	f.reconcile(t, database, databasedomain.ResourcePending)
	if database.Status.DatabaseOID == 0 {
		t.Fatal("数据库未独立确认")
	}
	if got := f.queryPostgres(t, "SELECT datallowconn FROM pg_database WHERE datname='supplied'"); got != "f" {
		t.Fatal("ACL 收紧前连接入口应关闭")
	}
	f.reconcile(t, database, databasedomain.ResourceAvailable)
	beforeRole, beforeDB := database.Status.RoleOID, database.Status.DatabaseOID
	for range 2 {
		f.reconcile(t, database, databasedomain.ResourceAvailable)
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
		f.reconcile(t, database, databasedomain.ResourceConflict)
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.RoleOID != 0 || f.queryPostgres(t, "SELECT oid FROM pg_roles WHERE rolname='occupied'") != before {
			t.Fatal("未知角色被认领或修改")
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
	t.Run("已确认角色后权限恢复", func(t *testing.T) {
		database := f.bound(t, "permission_restore")
		f.reconcile(t, database, databasedomain.ResourcePending)
		f.queryPostgres(t, "ALTER ROLE native_manager NOCREATEDB")
		f.reconcile(t, database, databasedomain.ResourceUnavailable)
		f.queryPostgres(t, "ALTER ROLE native_manager CREATEDB")
		f.reconcile(t, database, databasedomain.ResourcePending)
		f.reconcile(t, database, databasedomain.ResourceAvailable)
	})
	t.Run("删除前置阻止写入", func(t *testing.T) {
		database := f.bound(t, "deleting_supply")
		if err := f.api.Delete(f.ctx, database); err != nil {
			t.Fatal(err)
		}
		f.reconcile(t, database, databasedomain.ResourceStopped)
		if len(database.Finalizers) == 0 || database.Status.RoleOID != 0 {
			t.Fatal("删除边界被供应绕过")
		}
	})
	t.Run("确认保存失败转人工冲突", func(t *testing.T) {
		database := f.bound(t, "lost_confirmation")
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
	t.Run("数据库确认失败保留关闭入口", func(t *testing.T) {
		database := f.bound(t, "lost_database_confirmation")
		f.reconcile(t, database, databasedomain.ResourcePending)
		failed := *f.reconciler
		failed.Client = failStatusUpdate(f.api, func(object *databasev1alpha1.PostgreSQLDatabase) bool { return object.Status.DatabaseOID != 0 })
		if err := reconcileOnce(f.ctx, &failed, database); err == nil {
			t.Fatal("未注入数据库确认写入故障")
		}
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.RoleOID == 0 || database.Status.DatabaseOID != 0 || f.queryPostgres(t, "SELECT datallowconn FROM pg_database WHERE datname='lost_database_confirmation'") != "f" {
			t.Fatal("未确认数据库被认领或开放连接")
		}
	})
	t.Run("创建成功但调用方丢失结果", func(t *testing.T) {
		database := f.bound(t, "lost_role_response")
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
		f.reconcile(t, database, databasedomain.ResourcePending)
		f.queryPostgres(t, "DROP ROLE replaced_role; CREATE ROLE replaced_role LOGIN")
		f.reconcile(t, database, databasedomain.ResourceConflict)
		if database.Status.DatabaseOID != 0 {
			t.Fatal("重建角色被当成原 owner 继续供应")
		}
	})
	t.Run("创建步骤并发只有一个获准", func(t *testing.T) {
		database := f.bound(t, "concurrent_supply")
		racing := *f.reconciler
		racing.Reader = sameSnapshotReader(f.api)
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- reconcileOnce(f.ctx, &racing, database) }()
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
		f.status(t, database, databasedomain.ResourcePending)
		if database.Status.RoleOID == 0 {
			t.Fatal("胜方未确认角色")
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

// 让前两次 Database 读取互相等待，两个 Reconciler 都拿到同一 resourceVersion；后续写入仍经过真实 API server。
func sameSnapshotReader(reader client.WithWatch) client.Reader {
	var readers atomic.Int32
	var loaded sync.WaitGroup
	loaded.Add(2)
	return interceptor.NewClient(reader, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			err := c.Get(ctx, key, object, options...)
			if _, ok := object.(*databasev1alpha1.PostgreSQLDatabase); ok && readers.Add(1) <= 2 {
				loaded.Done()
				loaded.Wait()
			}
			return err
		},
	})
}
