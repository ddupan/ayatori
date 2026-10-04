package controller

import (
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Tenant 只公布 OpenBao 中已确认凭据的位置，不在控制面投射 Secret。
func TestTenantPublishesConfirmedCredentialLocation(t *testing.T) {
	apiClient, _, _ := bindingEnvironment(t)
	instance := readyInstance(t, apiClient, "available-instance")
	tenant := provisionTenant("available", instance.Name)
	requireCreate(t, apiClient, tenant)
	reconciler := &TenantReconciler{Client: apiClient, Reader: apiClient, CredentialURLBase: "https://bao.example.test"}
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	assertNotReady(t, tenant, "CredentialsPending")
	if tenant.Status.CredentialRef != nil {
		t.Fatal("未确认凭据前不得公布位置")
	}

	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = string(tenant.Status.DatabaseRef.Name)
	reload(t, apiClient, database)
	database.Status.CredentialRef = &databasev1alpha1.CredentialReference{Mount: "secret", Path: "applications/" + string(database.UID)}
	database.Status.CredentialVersion = 1
	if err := apiClient.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	assertNotReady(t, tenant, "DatabaseNotReady")
	if *tenant.Status.CredentialRef != *database.Status.CredentialRef || tenant.Status.CredentialVersion != 1 ||
		tenant.Status.CredentialURL != "https://bao.example.test/v1/secret/data/"+database.Status.CredentialRef.Path {
		t.Fatalf("凭据位置公布不符：%+v", tenant.Status)
	}

	database.Status.Conditions = readyConditions(database.Generation)
	if err := apiClient.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	condition := meta.FindStatusCondition(tenant.Status.Conditions, readyCondition)
	if condition == nil || !meta.IsStatusConditionTrue(tenant.Status.Conditions, readyCondition) || condition.Reason != "CredentialsAvailable" {
		t.Fatalf("Database 就绪后 Tenant 应可用：%+v", condition)
	}
	before := tenant.ResourceVersion
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	if tenant.ResourceVersion != before {
		t.Fatal("幂等重试产生了无意义写入")
	}
}
