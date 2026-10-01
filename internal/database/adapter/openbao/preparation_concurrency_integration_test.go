//go:build integration

package openbao_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
)

// 让前两次 Database 读取互相等待，两个 Reconciler 都拿到同一 resourceVersion；后续写入仍经过真实 API server。
func sameSnapshotReader(reader client.WithWatch) client.Reader {
	var readers atomic.Int32
	var loaded sync.WaitGroup
	loaded.Add(2)
	return interceptor.NewClient(reader, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			err := c.Get(ctx, key, object, options...)
			if _, ok := object.(*databasev1alpha1.PostgreSQLDatabase); ok && readers.Add(1) <= 2 {
				loaded.Done()
				loaded.Wait()
			}
			return err
		},
	})
}

func testPreparationConcurrency(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "concurrent")
	reconciler := f.reconciler(t)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)}
	// 第一轮只固定位置；两个 Reconciler 随后在创建检查点上竞争。
	if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	reconciler.Reader = sameSnapshotReader(f.api)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := reconciler.Reconcile(t.Context(), request)
			results <- err
		}()
	}
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case apierrors.IsConflict(err):
			conflicted++
		default:
			t.Fatalf("并发协调返回意外错误: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatal("同一快照只能有一个 Reconciler 进入创建并写入后端")
	}
	f.status(t, database, 1, "CredentialPrepared")
	stored, err := f.bao.KVv2("secret").Get(t.Context(), database.Status.CredentialRef.Path)
	if err != nil || stored.VersionMetadata.Version != 1 {
		t.Fatal("并发协调只能产生一个凭据版本")
	}
}
