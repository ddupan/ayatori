//go:build integration

package openbao_test

import (
	"context"
	"errors"
	"testing"

	bao "github.com/openbao/openbao/api/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
)

// 在 API 边界模拟旧 schema 裁剪位置字段；剩余写入仍由真实 API server 处理。
type prunedLocationClient struct{ client.Client }

func (c prunedLocationClient) Status() client.SubResourceWriter {
	return prunedLocationWriter{SubResourceWriter: c.Client.Status()}
}

type prunedLocationWriter struct{ client.SubResourceWriter }

func (w prunedLocationWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if database, ok := object.(*databasev1alpha1.PostgreSQLDatabase); ok {
		database.Status.CredentialRef = nil
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func testPreparationPruning(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "pruning")
	service := f.reconciler(t)
	service.Client = prunedLocationClient{f.api}
	if err := f.settle(t, service, database); err == nil {
		t.Fatal("API 未保留位置时不得继续供应")
	}
	location, err := service.Credentials.ProvisionLocation(string(database.UID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.bao.KVv2(location.Mount).Get(t.Context(), location.Path); !errors.Is(err, bao.ErrSecretNotFound) {
		t.Fatal("没有持久化位置时产生了外部凭据")
	}
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "CredentialPrepared")
}
