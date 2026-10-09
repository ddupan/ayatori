//go:build integration

package postgresql_test

import (
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	databasedomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// setInstanceHost 修改 Instance 的连接主机名并等 Instance 按新 generation 重新就绪。
func (f *provisioningFixture) setInstanceHost(t *testing.T, host string) {
	t.Helper()
	object := &databasev1alpha1.PostgreSQLInstance{}
	if err := f.api.Get(f.ctx, client.ObjectKey{Name: supplyInstance}, object); err != nil {
		t.Fatal(err)
	}
	object.Spec.Endpoint.Host = host
	if err := f.api.Update(f.ctx, object); err != nil {
		t.Fatal(err)
	}
	instances := &databasecontroller.InstanceReconciler{Client: f.api, Reader: f.api, PostgreSQL: f.service, Secrets: f.secrets}
	if _, err := instances.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(object), object); err != nil || !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") {
		t.Fatal("Instance 未按新连接信息就绪", err)
	}
}

// 修改 Instance 连接信息不改变资源身份：已确认凭据以同一密码改写为下一版本，随后照常就绪。
func testEndpointFollowsInstance(t *testing.T, f *provisioningFixture) {
	const moved = "fixture-moved.invalid"
	database := f.available(t, "endpoint_follow", databasev1alpha1.ReclaimRetain)
	location := credential.Location{Mount: database.Status.CredentialRef.Mount, Path: database.Status.CredentialRef.Path}
	before, err := f.store.ReadCredential(f.ctx, location, 1)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := database.Status.DeepCopy()
	f.setInstanceHost(t, moved)
	t.Cleanup(func() { f.setInstanceHost(t, fixtureHost) })

	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); err != nil {
		t.Fatal(err)
	}
	if database.Status.CredentialVersion != 2 || database.Status.CredentialPhase != string(databasedomain.CredentialPrepared) ||
		database.Status.DatabaseOID != confirmed.DatabaseOID || database.Status.RoleOID != confirmed.RoleOID {
		t.Fatalf("应改写为版本 2 且保留资源身份：%+v", database.Status)
	}
	after, err := f.store.ReadCredential(f.ctx, location, 2)
	if err != nil {
		t.Fatal(err)
	}
	if after.Password() != before.Password() || after.SecretData()["host"] != moved || before.SecretData()["host"] != fixtureHost {
		t.Fatal("改写应保留密码，只把 host 换成新值")
	}
	f.reconcile(t, database, databasedomain.ResourceAvailable)
}
