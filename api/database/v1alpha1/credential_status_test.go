package v1alpha1_test

import (
	"testing"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func testCredentialStatus(t *testing.T, client ctrlclient.Client) {
	database := validDatabase("credential-status")
	if err := client.Create(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	withoutLocation := database.DeepCopy()
	withoutLocation.Status.CredentialVersion = 1
	if err := client.Status().Update(t.Context(), withoutLocation); !apierrors.IsInvalid(err) {
		t.Fatalf("没有位置不能确认版本: %v", err)
	}
	database.Status.CredentialRef = &databasev1alpha1.CredentialReference{
		Mount: "application-secrets", Path: "applications/" + string(database.UID),
	}
	if err := client.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	testCredentialStatusRemoval(t, client, database)
	changedTarget := database.DeepCopy()
	changedTarget.Spec.Database = "another_database"
	if err := client.Update(t.Context(), changedTarget); !apierrors.IsInvalid(err) {
		t.Fatalf("凭据位置固定后不得更换实际目标: %v", err)
	}
	stale := database.DeepCopy()
	database.Status.CredentialVersion = 1
	if err := client.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	stale.Status.Phase = "Binding"
	if err := client.Status().Update(t.Context(), stale); !apierrors.IsConflict(err) {
		t.Fatalf("旧 resourceVersion 不得覆盖确认结果: %v", err)
	}
	testCredentialStatusRemoval(t, client, database)
	// 确认版本只能前进（改写连接信息产生下一版本），前进的情形见 resource_status_test。
	for _, version := range []int64{0, -1} {
		changed := database.DeepCopy()
		changed.Status.CredentialVersion = version
		if err := client.Status().Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("不可移除或回退确认版本 %d: %v", version, err)
		}
	}
	database.Status.Conditions = []metav1.Condition{{
		Type: "CredentialsReady", Status: metav1.ConditionFalse,
		Reason: "DependencyUnavailable", Message: "凭据暂时无法读取",
		ObservedGeneration: database.Generation, LastTransitionTime: metav1.Now(),
	}}
	if err := client.Status().Update(t.Context(), database); err != nil {
		t.Fatalf("当前不可用不应阻止保留确认记录: %v", err)
	}
	reloaded := &databasev1alpha1.PostgreSQLDatabase{}
	if err := client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(database), reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Status.CredentialVersion != 1 || reloaded.Status.CredentialRef == nil {
		t.Fatal("重读丢失凭据确认记录")
	}
}

func testCredentialStatusRemoval(t *testing.T, client ctrlclient.Client, database *databasev1alpha1.PostgreSQLDatabase) {
	t.Helper()
	for _, change := range []struct {
		name string
		edit func(*databasev1alpha1.PostgreSQLDatabase)
	}{
		{"移除整个 status", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status = databasev1alpha1.PostgreSQLDatabaseStatus{} }},
		{"移除位置", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.CredentialRef = nil }},
		{"修改 mount", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.CredentialRef.Mount = "other" }},
		{"修改 path", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.CredentialRef.Path = "applications/other" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := database.DeepCopy()
			change.edit(changed)
			if err := client.Status().Update(t.Context(), changed); !apierrors.IsInvalid(err) {
				t.Fatalf("不应接受已固定凭据位置的更改: %v", err)
			}
		})
	}
}
