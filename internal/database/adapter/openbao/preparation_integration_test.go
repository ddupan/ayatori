//go:build integration

package openbao_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"

	bao "github.com/openbao/openbao/api/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
)

func TestCredentialPreparationWithRealBackends(t *testing.T) {
	fixture := newPreparationFixture(t)
	t.Run("创建重启和幂等", func(t *testing.T) { testPreparationRestart(t, fixture) })
	t.Run("已有值不认领", func(t *testing.T) { testPreparationExisting(t, fixture) })
	t.Run("确认写入失败", func(t *testing.T) { testPreparationLostConfirmation(t, fixture) })
	t.Run("后端结果不确定", func(t *testing.T) { testPreparationUncertain(t, fixture) })
	t.Run("创建中绑定变化", func(t *testing.T) { testPreparationChangedBinding(t, fixture) })
	t.Run("依赖恢复与固定位置", func(t *testing.T) { testPreparationDependencies(t, fixture) })
	t.Run("并发用例只有一个写入", func(t *testing.T) { testPreparationConcurrency(t, fixture) })
	t.Run("位置被API裁剪时拒绝外部写入", func(t *testing.T) { testPreparationPruning(t, fixture) })
	t.Run("实际manager的watch与重启", func(t *testing.T) { testPreparationWatch(t, fixture) })
}

func testPreparationRestart(t *testing.T, f *preparationFixture) {
	database, tenant := f.bound(t, "restart")
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "CredentialPrepared")
	path := database.Status.CredentialRef.Path
	before, err := f.bao.KVv2("secret").Get(t.Context(), path)
	if err != nil {
		t.Fatal("无法读取隔离测试凭据")
	}
	if before.Data["username"] != "restart" || before.Data["database"] != "restart" || len(before.Data) != 7 {
		t.Fatal("用例未使用绑定目标生成七键凭据")
	}
	// 两次新用例实例模拟重启，第二轮必须没有无意义状态更新。
	for range 2 {
		if err := f.settle(t, f.reconciler(t), database); err != nil {
			t.Fatal(err)
		}
	}
	f.status(t, database, 1, "CredentialPrepared")
	revision := database.ResourceVersion
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "CredentialPrepared")
	if database.ResourceVersion != revision {
		t.Fatal("幂等重试不应改写 status")
	}
	after, err := f.bao.KVv2("secret").Get(t.Context(), path)
	if err != nil || after.VersionMetadata.Version != 1 || !maps.Equal(before.Data, after.Data) {
		t.Fatal("重启后不应生成或覆盖凭据")
	}
	if err := f.api.Delete(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "PreparationStopped")
	if err := f.api.Delete(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "PreparationStopped")
	if len(database.Finalizers) == 0 {
		t.Fatal("本切片不得解除删除保护")
	}
	if _, err := f.bao.KVv2("secret").Get(t.Context(), path); err != nil {
		t.Fatal("删除流程不得在本切片清理凭据")
	}
}

func testPreparationExisting(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "existing")
	store := fixtureStore(t, f.bao)
	path, err := store.ProvisionPath(string(database.UID))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(t.Context(), path, fixtureCredential(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 0, binding.Conflict)
	// 即使管理员删除了后端值，未解除的不确定状态也不能自动创建。
	if err := f.bao.KVv2("secret").DeleteMetadata(t.Context(), path); err != nil {
		t.Fatal("无法清理隔离测试 key")
	}
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bao.KVv2("secret").Get(t.Context(), path); !errors.Is(err, bao.ErrSecretNotFound) {
		t.Fatal("Conflict 重入不应生成替代凭据")
	}
}

// 故障只注入确认保存边界；此前写入使用真实 API server 和 Bao。
type failedConfirmationClient struct{ client.Client }

func (c failedConfirmationClient) Status() client.SubResourceWriter {
	return failedConfirmationWriter{SubResourceWriter: c.Client.Status()}
}

type failedConfirmationWriter struct{ client.SubResourceWriter }

func (w failedConfirmationWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if database, ok := object.(*databasev1alpha1.PostgreSQLDatabase); ok && database.Status.CredentialVersion > 0 {
		return errors.New("fixture refuses confirmation update")
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func testPreparationLostConfirmation(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "confirmation")
	service := f.reconciler(t)
	service.Client = failedConfirmationClient{f.api}
	if err := f.settle(t, service, database); err == nil {
		t.Fatal("确认写入失败应返回 API 错误")
	}
	f.status(t, database, 0, "CreationStarted")
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 0, binding.Conflict)
	stored, err := f.bao.KVv2("secret").Get(t.Context(), database.Status.CredentialRef.Path)
	if err != nil || stored.VersionMetadata.Version != 1 {
		t.Fatal("写入结果必须保留且不能被重启认领")
	}
}

type afterCreateStore struct {
	databasecontroller.CredentialBackend
	after func() error
}

func (s afterCreateStore) CreateCredential(ctx context.Context, location credentialdomain.Location, credential credentialdomain.ApplicationCredential) (int64, error) {
	version, err := s.CredentialBackend.CreateCredential(ctx, location, credential)
	if err != nil {
		return 0, err
	}
	if err := s.after(); err != nil {
		return 0, err
	}
	return version, nil
}

func testPreparationUncertain(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "uncertain")
	service := f.reconciler(t)
	service.Credentials = afterCreateStore{CredentialBackend: service.Credentials, after: func() error { return openbao.ErrUncertain }}
	if err := f.settle(t, service, database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 0, binding.Conflict)
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 0, binding.Conflict)
}

func testPreparationChangedBinding(t *testing.T, f *preparationFixture) {
	database, tenant := f.bound(t, "changed")
	service := f.reconciler(t)
	service.Credentials = afterCreateStore{CredentialBackend: service.Credentials, after: func() error {
		return f.api.Delete(t.Context(), tenant)
	}}
	if err := f.settle(t, service, database); err == nil {
		t.Fatal("中途删除 Tenant 后不得确认凭据")
	}
	f.status(t, database, 0, "CreationStarted")
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 0, binding.Conflict)
}

func testPreparationDependencies(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "dependency")
	denied := fixtureClient(t, f.bao.Address())
	denied.ClearToken()
	service := f.reconciler(t)
	service.Credentials = fixtureStore(t, denied)
	if err := f.settle(t, service, database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 0, binding.DependencyUnavailable)
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "CredentialPrepared")
	if err := f.settle(t, service, database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, binding.DependencyUnavailable)
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, "CredentialPrepared")
	moved, err := openbao.NewCredentials(f.bao, "other", "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	service.Credentials = moved
	if err := f.settle(t, service, database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, binding.DependencyUnavailable)
	if database.Status.CredentialRef.Mount != "secret" {
		t.Fatal("配置变化不得迁移固定位置")
	}
	if err := f.bao.KVv2("secret").Delete(t.Context(), database.Status.CredentialRef.Path); err != nil {
		t.Fatal("无法准备测试软删除")
	}
	if err := f.settle(t, f.reconciler(t), database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, 1, binding.Conflict)
}
