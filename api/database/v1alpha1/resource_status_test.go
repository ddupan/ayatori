package v1alpha1_test

import (
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testResourceStatus(t *testing.T, api client.Client) {
	object := validDatabase("resource-status")
	if err := api.Create(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	invalid := object.DeepCopy()
	invalid.Status.DatabaseOID = 22
	if err := api.Status().Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
		t.Fatal("无凭据不能确认数据库", err)
	}
	object.Status.InstanceUID = "instance-uid"
	object.Status.CredentialRef = &databasev1alpha1.CredentialReference{Mount: "application-secrets", Path: "applications/test"}
	object.Status.CredentialVersion = 1
	invalid = object.DeepCopy()
	invalid.Status.RoleOID = 11
	if err := api.Status().Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
		t.Fatal("角色确认要求先有数据库确认", err)
	}
	object.Status.RoleOID, object.Status.DatabaseOID = 11, 22
	if err := api.Status().Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*databasev1alpha1.PostgreSQLDatabase){
		func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.RoleOID = 0 },
		func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.RoleOID = 12 },
		func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.DatabaseOID = 0 },
		func(d *databasev1alpha1.PostgreSQLDatabase) { d.Status.DatabaseOID = 23 },
	} {
		invalid = object.DeepCopy()
		change(invalid)
		if err := api.Status().Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
			t.Fatal("已确认 OID 不得覆盖或移除", err)
		}
	}
	// 改写连接信息会产生凭据下一版本：确认版本只能前进，不能回退或移除。
	for _, version := range []int64{0, -1} {
		invalid = object.DeepCopy()
		invalid.Status.CredentialVersion = version
		if err := api.Status().Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
			t.Fatal("确认版本不得回退或移除", err)
		}
	}
	object.Status.CredentialVersion = 2
	if err := api.Status().Update(t.Context(), object); err != nil {
		t.Fatal("确认版本应允许前进", err)
	}
}
