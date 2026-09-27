//go:build integration

package openbao_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
)

// 两个用例都先拿到同一真实 resourceVersion，再竞争固定位置；后续写入仍经过 API server。
type concurrentCredentialResources struct {
	application.CredentialResources
	readers atomic.Int32
	loaded  sync.WaitGroup
}

func (r *concurrentCredentialResources) Load(ctx context.Context, name string) (*application.CredentialRecord, error) {
	record, err := r.CredentialResources.Load(ctx, name)
	if r.readers.Add(1) <= 2 {
		r.loaded.Done()
		r.loaded.Wait()
	}
	return record, err
}

func testPreparationConcurrency(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "concurrent")
	service := f.service(t)
	resources := &concurrentCredentialResources{CredentialResources: service.Resources}
	resources.loaded.Add(2)
	service.Resources = resources
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- service.Reconcile(t.Context(), database.Name) }()
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
			t.Fatalf("并发用例返回意外错误: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatal("同一快照只能有一个用例成功固定位置并继续创建")
	}
	f.status(t, database, 1, "CredentialPrepared")
	stored, err := f.bao.KVv2("secret").Get(t.Context(), database.Status.CredentialRef.Path)
	if err != nil || stored.VersionMetadata.Version != 1 {
		t.Fatal("并发准备用例只能产生一个凭据版本")
	}
}
