//go:build integration

package postgresql_test

import (
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	databasedomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	importedPassword = "AYATORI-TEST-ONLY-imported-password"
	importedName     = "imported_app"
	supplyInstance   = "supply-instance"
)

// existing 以 superuser 准备“已有”的数据库、角色与凭据，模拟导入前由别处创建的对象。
func (f *provisioningFixture) existing(t *testing.T, name string, ownedByRole bool) {
	t.Helper()
	f.queryPostgres(t, "CREATE ROLE "+name+" LOGIN PASSWORD '"+importedPassword+"'")
	owner := name
	if !ownedByRole {
		owner = nativeManager
	}
	f.queryPostgres(t, "CREATE DATABASE "+name+" OWNER "+owner)
	f.queryPostgres(t, "GRANT CONNECT ON DATABASE "+name+" TO "+nativeManager)
	value, err := credential.NewApplicationCredential(name, importedPassword, name, target(t, f.port, instance.TLSDisable).Definition().Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Create(f.ctx, "applications/"+name, value); err != nil {
		t.Fatal(err)
	}
}

func (f *provisioningFixture) importRecord(t *testing.T, recordName, name string) *databasev1alpha1.PostgreSQLDatabase {
	t.Helper()
	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = recordName
	database.Spec = databasev1alpha1.PostgreSQLDatabaseSpec{
		InstanceRef: databasev1alpha1.InstanceReference{Name: supplyInstance},
		Database:    databasev1alpha1.PostgreSQLIdentifier(name), LoginRole: databasev1alpha1.PostgreSQLIdentifier(name),
		Source:        "Import",
		CredentialRef: &databasev1alpha1.CredentialReference{Mount: "secret", Path: "applications/" + name},
	}
	if err := f.api.Create(f.ctx, database); err != nil {
		t.Fatal(err)
	}
	return database
}

func testImport(t *testing.T, f *provisioningFixture) {
	f.existing(t, importedName, true)
	before := f.queryPostgres(t, "SELECT oid, rolpassword IS NOT NULL FROM pg_authid WHERE rolname='imported_app'")
	database := f.importRecord(t, "imported-app", importedName)
	f.reconcile(t, database, databasedomain.ResourceAvailable)
	if database.Status.Phase != databasedomain.Available || database.Status.RoleOID == 0 || database.Status.DatabaseOID == 0 ||
		database.Status.CredentialVersion != 1 || database.Status.InstanceUID == "" {
		t.Fatalf("导入未登记确认身份：%+v", database.Status)
	}
	if f.queryPostgres(t, "SELECT oid, rolpassword IS NOT NULL FROM pg_authid WHERE rolname='imported_app'") != before ||
		f.queryPostgres(t, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='imported_app'") != importedName {
		t.Fatal("导入不得修改角色或 owner")
	}
	config, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	config.Host, config.Port, config.TLSConfig, config.Fallbacks = fixtureAddress, uint16(f.port), nil, nil
	config.User, config.Database, config.Password = importedName, importedName, importedPassword
	application, err := pgx.ConnectConfig(f.ctx, config)
	if err != nil {
		t.Fatal("导入不得改密", err)
	}
	_ = application.Close(f.ctx)

	// Tenant 显式申请已导入资源，并在 status 公布凭据位置。
	tenant := &databasev1alpha1.PostgreSQLTenant{Name: "imported-tenant", Namespace: controllerNamespace,
		Spec: databasev1alpha1.PostgreSQLTenantSpec{DatabaseRef: &databasev1alpha1.DatabaseReference{Name: databasev1alpha1.ObjectName(database.Name)}}}
	if err := f.api.Create(f.ctx, tenant); err != nil {
		t.Fatal(err)
	}
	tenants := &databasecontroller.TenantReconciler{Client: f.api, Reader: f.api}
	if _, err := tenants.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)}); err != nil {
		t.Fatal(err)
	}
	// 绑定写入 spec.tenantRef 会推进 Database generation；按当前 generation 重新验证后 Tenant 才可用。
	f.reconcile(t, database, databasedomain.ResourceAvailable)
	if _, err := tenants.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)}); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(tenant), tenant); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(tenant.Status.Conditions, "Ready") || tenant.Status.CredentialRef == nil ||
		tenant.Status.CredentialRef.Path != "applications/imported_app" {
		t.Fatalf("Tenant 未能申请导入资源：%+v", tenant.Status)
	}

	// 重新开放 = 释放后删除 Retain 旧记录，再重新导入；新记录拥有新 UID。
	f.release(t, database)
	if err := f.api.Delete(f.ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); !apierrors.IsNotFound(err) {
		t.Fatal("Retain 旧记录应可删除", err)
	}
	reopened := f.importRecord(t, "imported-app-reopened", importedName)
	f.reconcile(t, reopened, databasedomain.ResourceAvailable)
	if reopened.UID == database.UID || reopened.Spec.TenantRef != nil {
		t.Fatal("重新导入应是不带旧绑定的新记录")
	}
}

func testImportOwnerMismatch(t *testing.T, f *provisioningFixture) {
	f.existing(t, "foreign_owner", false)
	database := f.importRecord(t, "foreign-owner", "foreign_owner")
	f.reconcile(t, database, databasedomain.ResourceConflict)
	if database.Status.DatabaseOID != 0 || f.queryPostgres(t, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='foreign_owner'") != nativeManager {
		t.Fatal("owner 不符时不得登记或修改 owner")
	}
}
