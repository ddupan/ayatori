//go:build integration

package openbao_test

import (
	"maps"
	"testing"

	bao "github.com/openbao/openbao/api/v2"

	"go.ddupan.top/ayatori/internal/database/adapter/openbao"
)

func TestConfirmedCredentialRecoveryWithRealOpenBao(t *testing.T) {
	root := baoFixture(t)
	store := fixtureStore(t, root)
	credential := fixtureCredential(t)
	if err := store.Create(t.Context(), credentialPath, credential); err != nil {
		t.Fatal(err)
	}
	// 新 adapter 模拟进程重启：只有已持久化的版本才能恢复读取。
	restarted := fixtureStore(t, root)
	if _, err := restarted.ReadConfirmed(t.Context(), credentialPath, 0); err != openbao.ErrConflict {
		t.Fatal("已有值不能替代缺失的确认记录")
	}
	observed, err := restarted.ReadConfirmed(t.Context(), credentialPath, 1)
	if err != nil || !maps.Equal(observed.SecretData(), credential.SecretData()) {
		t.Fatal("确认版本的凭据不能恢复读取")
	}
	// 即使内容相同，新增版本也不属于已确认写入；历史版本仍在不能掩盖改写。
	if _, err := root.KVv2("secret").Put(t.Context(), credentialPath, credential.SecretData(), bao.WithCheckAndSet(1)); err != nil {
		t.Fatal("无法准备测试中的版本变化")
	}
	if _, err := restarted.ReadConfirmed(t.Context(), credentialPath, 1); err != openbao.ErrConflict {
		t.Fatal("最新版本漂移必须报冲突")
	}
	if err := root.KVv2("secret").Delete(t.Context(), credentialPath); err != nil {
		t.Fatal("无法准备测试中的软删除")
	}
	if _, err := restarted.ReadConfirmed(t.Context(), credentialPath, 1); err != openbao.ErrConflict {
		t.Fatal("已确认凭据被删除必须报冲突")
	}
	metadata, err := root.KVv2("secret").GetMetadata(t.Context(), credentialPath)
	if err != nil || metadata.CurrentVersion != 2 {
		t.Fatal("恢复检查不得生成或覆盖凭据")
	}
}
