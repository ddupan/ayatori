package openbao_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
)

func credentialVersionMetadata(version int) map[string]any {
	return map[string]any{kvVersionKey: version}
}

func TestConfirmedCredentialRead(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int64
		status  int
		meta    map[string]any
		invalid bool
		want    error
	}{
		{name: "已确认", version: 1, status: 200, meta: credentialVersionMetadata(1)},
		{name: "未确认禁止读取", version: 0, want: openbao.ErrConflict},
		{name: "版本已变化", version: 1, status: 200, meta: credentialVersionMetadata(2), want: openbao.ErrConflict},
		{name: "版本为零", version: 1, status: 200, meta: credentialVersionMetadata(0), want: openbao.ErrConflict},
		{name: "响应无法解析", version: 1, status: 200, want: openbao.ErrUnavailable},
		{name: "内容损坏", version: 1, status: 200, meta: credentialVersionMetadata(1), invalid: true, want: openbao.ErrConflict},
		{name: "凭据丢失", version: 1, status: 404, want: openbao.ErrConflict},
		{name: "读取被拒绝", version: 1, status: 403, want: openbao.ErrUnavailable},
		{name: "后端不可用", version: 1, status: 503, want: openbao.ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := fixtureCredential(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.version == 0 {
					t.Error("没有持久化确认记录不能读取已有值")
				}
				if r.Method != http.MethodGet || r.URL.RawQuery != "" {
					t.Error("只允许读取最新值，不能写入或回退历史版本")
				}
				w.WriteHeader(test.status)
				if test.status != http.StatusOK {
					return
				}
				data := credential.SecretData()
				if test.invalid {
					delete(data, "password")
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					kvDataKey: map[string]any{kvDataKey: data, "metadata": test.meta},
				}); err != nil {
					t.Error("测试响应编码失败")
				}
			}))
			defer server.Close()
			store := fixtureStore(t, fixtureClient(t, server.URL))
			observed, err := store.ReadConfirmed(t.Context(), credentialPath, test.version)
			if err != test.want {
				t.Fatalf("期望 %v，得到 %v", test.want, err)
			}
			if err != nil && observed.Validate() == nil {
				t.Fatal("失败时不能返回可用凭据")
			}
		})
	}
}
