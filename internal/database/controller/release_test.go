package controller

import (
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func reconcileDatabase(t *testing.T, reconciler *DatabaseReconciler, database *databasev1alpha1.PostgreSQLDatabase) {
	t.Helper()
	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)}); err != nil {
		t.Fatal(err)
	}
}

// 未启用凭据准备时 DatabaseReconciler 仍负责删除保护与 Retain 放行。
func TestTenantReleaseAndRetain(t *testing.T) {
	apiClient, _, _ := bindingEnvironment(t)
	instance := readyInstance(t, apiClient, "release-instance")
	tenant := provisionTenant("release", instance.Name)
	requireCreate(t, apiClient, tenant)
	tenants := bindingTestReconciler(apiClient, apiClient)
	databases := &DatabaseReconciler{Client: apiClient, Reader: apiClient}
	reconcileOK(t, tenants, tenant)
	reload(t, apiClient, tenant)
	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = string(tenant.Status.DatabaseRef.Name)
	reload(t, apiClient, database)

	// 绑定中直接删除 Database 不得绕过保护。
	if err := apiClient.Delete(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	reconcileDatabase(t, databases, database)
	reload(t, apiClient, database)
	if !controllerutil.ContainsFinalizer(database, DatabaseFinalizer) {
		t.Fatal("仍绑定的 Database 被放行删除")
	}

	if err := apiClient.Delete(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, tenants, tenant)
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(tenant), tenant); !apierrors.IsNotFound(err) {
		t.Fatal("记录释放后 Tenant 应可删除", err)
	}
	reload(t, apiClient, database)
	if database.Status.Phase != releasedPhase || database.Spec.TenantRef == nil || string(database.Spec.TenantRef.UID) != string(tenant.UID) {
		t.Fatal("释放应保留旧绑定身份并记录 Released")
	}
	reconcileDatabase(t, databases, database)
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(database), database); !apierrors.IsNotFound(err) {
		t.Fatal("Retain 释放后删除 Database 只应放行 finalizer", err)
	}
}

func TestReclaimPolicyFixedAfterRelease(t *testing.T) {
	apiClient, _, _ := bindingEnvironment(t)
	instance := readyInstance(t, apiClient, "policy-instance")
	database := availableDatabase(t, apiClient, "policy-database", instance)
	database.Spec.ReclaimPolicy = databasev1alpha1.ReclaimDelete
	if err := apiClient.Update(t.Context(), database); err != nil {
		t.Fatal("释放前应允许修改回收策略", err)
	}
	database.Status.Phase = releasedPhase
	if err := apiClient.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	database.Spec.ReclaimPolicy = databasev1alpha1.ReclaimRetain
	if err := apiClient.Update(t.Context(), database); !apierrors.IsInvalid(err) {
		t.Fatalf("释放后修改回收策略 = %v, want Invalid", err)
	}
}
