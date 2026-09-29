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
	invalid.Status.RoleOID = 11
	if err := api.Status().Update(t.Context(), invalid); !apierrors.IsInvalid(err) {
		t.Fatal("无凭据不能确认角色", err)
	}
	object.Status.InstanceUID = "instance-uid"
	object.Status.CredentialRef = &databasev1alpha1.CredentialReference{Mount: "application-secrets", Path: "applications/test"}
	object.Status.CredentialVersion = 1
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
}
