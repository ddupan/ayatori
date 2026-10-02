package v1alpha1_test

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testExtensionsAppendOnly(t *testing.T, api client.Client) {
	tenant := validTenant("extensions-append-only")
	tenant.Spec.Extensions = []string{"hstore"}
	if err := api.Create(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	tenant.Spec.Extensions = []string{"hstore", "pg_trgm"}
	if err := api.Update(t.Context(), tenant); err != nil {
		t.Fatal("追加扩展应被接受", err)
	}
	for _, extensions := range [][]string{{"pg_trgm"}, nil} {
		changed := tenant.DeepCopy()
		changed.Spec.Extensions = extensions
		if err := api.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("移除扩展 %v = %v, want Invalid", extensions, err)
		}
	}
}
