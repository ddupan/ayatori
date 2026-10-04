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
	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// available 供应到 Available，并按需在释放前把回收策略设为 Delete。
func (f *provisioningFixture) available(t *testing.T, name string, policy databasev1alpha1.ReclaimPolicy) *databasev1alpha1.PostgreSQLDatabase {
	t.Helper()
	database := f.bound(t, name)
	for range 4 {
		f.reconcile(t, database, databasedomain.ResourcePending)
	}
	f.reconcile(t, database, databasedomain.ResourceAvailable)
	database.Spec.ReclaimPolicy = policy
	if err := f.api.Update(f.ctx, database); err != nil {
		t.Fatal(err)
	}
	return database
}

// release 删除 Tenant 并由 TenantReconciler 记录释放。
func (f *provisioningFixture) release(t *testing.T, database *databasev1alpha1.PostgreSQLDatabase) {
	t.Helper()
	tenant := &databasev1alpha1.PostgreSQLTenant{}
	key := client.ObjectKey{Namespace: database.Spec.TenantRef.Namespace, Name: string(database.Spec.TenantRef.Name)}
	if err := f.api.Get(f.ctx, key, tenant); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Delete(f.ctx, tenant); err != nil {
		t.Fatal(err)
	}
	tenants := &databasecontroller.TenantReconciler{Client: f.api, Reader: f.api}
	if _, err := tenants.Reconcile(f.ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, key, tenant); !apierrors.IsNotFound(err) {
		t.Fatal("释放后 Tenant 应可删除", err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); err != nil || database.Status.Phase != databasedomain.Released {
		t.Fatal("Database 未记录释放", err)
	}
}

func (f *provisioningFixture) resourcesReason(t *testing.T, database *databasev1alpha1.PostgreSQLDatabase) string {
	t.Helper()
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(database.Status.Conditions, "ResourcesReady").Reason
}

func testReclaimDelete(t *testing.T, f *provisioningFixture) {
	database := f.available(t, "reclaim_delete", databasev1alpha1.ReclaimDelete)
	location := credential.Location{Mount: database.Status.CredentialRef.Mount, Path: database.Status.CredentialRef.Path}
	value, err := f.store.ReadCredential(f.ctx, location, database.Status.CredentialVersion)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	config.Host, config.Port, config.TLSConfig, config.Fallbacks = fixtureAddress, uint16(f.port), nil, nil
	config.User, config.Database, config.Password = "reclaim_delete", "reclaim_delete", value.Password()
	application, err := pgx.ConnectConfig(f.ctx, config)
	if err != nil {
		t.Fatal("应用应能在回收前登录", err)
	}
	defer func() { _ = application.Close(context.Background()) }()

	f.release(t, database)
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if f.resourcesReason(t, database) != string(databasedomain.ResourceReclaiming) ||
		f.queryPostgres(t, "SELECT count(*) FROM pg_database WHERE datname='reclaim_delete'") != "0" {
		t.Fatal("第一步应删除数据库")
	}
	if application.Ping(f.ctx) == nil {
		t.Fatal("应用会话应在删库前被终止")
	}
	if f.queryPostgres(t, "SELECT rolcanlogin FROM pg_roles WHERE rolname='reclaim_delete'") != "f" {
		t.Fatal("删库前应先阻止新登录")
	}
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='reclaim_delete'") != "0" {
		t.Fatal("第二步应删除登录角色")
	}
	// 第三步销毁凭据并删除 Database；下一轮在删除中移除 finalizer。
	for range 2 {
		if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); !apierrors.IsNotFound(err) {
		t.Fatal("清理完成后 Database 应被删除", err)
	}
	if _, err := f.store.ReadCredential(f.ctx, location, 0); !errors.Is(err, databasedomain.ErrNotFound) {
		t.Fatal("应用凭据应被永久删除", err)
	}
}

func testReclaimRetain(t *testing.T, f *provisioningFixture) {
	database := f.available(t, "reclaim_retain", databasev1alpha1.ReclaimRetain)
	f.release(t, database)
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Delete(f.ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); !apierrors.IsNotFound(err) {
		t.Fatal("Retain 释放后删除 Database 只应放行", err)
	}
	if f.queryPostgres(t, "SELECT count(*) FROM pg_database WHERE datname='reclaim_retain'") != "1" ||
		f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='reclaim_retain'") != "1" {
		t.Fatal("Retain 不得删除外部数据或角色")
	}
}

func testReclaimConflict(t *testing.T, f *provisioningFixture) {
	database := f.available(t, "reclaim_conflict", databasev1alpha1.ReclaimDelete)
	f.release(t, database)
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	f.queryPostgres(t, "DROP ROLE reclaim_conflict; CREATE ROLE reclaim_conflict LOGIN")
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if f.resourcesReason(t, database) != string(databasedomain.ResourceConflict) ||
		f.queryPostgres(t, "SELECT count(*) FROM pg_roles WHERE rolname='reclaim_conflict'") != "1" {
		t.Fatal("同名重建的角色不得被删除")
	}
}
