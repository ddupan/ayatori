//go:build integration

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openbao_test

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"testing"

	bao "github.com/openbao/openbao/api/v2"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
	"git.ddupan.top/panxiao81/ayatori/internal/testenv"
)

// baoFixture 连接 hack/integration-test.sh 在 go test 之外启动的无持久卷 dev server（root token
// 为 fixtureToken），并复位到 dev server 的初始状态。不接受生产地址或环境 token。
func baoFixture(t *testing.T) *bao.Client {
	t.Helper()
	_, port := testenv.Container(t, "openbao", 8200)
	client := fixtureClient(t, "http://127.0.0.1:"+strconv.Itoa(port))
	resetBao(t, client)
	return client
}

// resetBao 卸载场景留下的 secrets engine、auth method 与 policy，重建 dev server 默认的 kv-v2 secret/。
func resetBao(t *testing.T, root *bao.Client) {
	t.Helper()
	ctx := t.Context()
	mounts, err := root.Sys().ListMountsWithContext(ctx)
	if err != nil {
		t.Fatal("cannot list fixture mounts", err)
	}
	for path := range mounts {
		if path != "sys/" && path != "cubbyhole/" && path != "identity/" {
			if err := root.Sys().UnmountWithContext(ctx, path); err != nil {
				t.Fatal("cannot reset fixture mount", err)
			}
		}
	}
	if err := root.Sys().MountWithContext(ctx, "secret", &bao.MountInput{Type: "kv", Options: map[string]string{"version": "2"}}); err != nil {
		t.Fatal("cannot recreate fixture KV mount", err)
	}
	auths, err := root.Sys().ListAuthWithContext(ctx)
	if err != nil {
		t.Fatal("cannot list fixture auth methods", err)
	}
	for path := range auths {
		if path != "token/" {
			if err := root.Sys().DisableAuthWithContext(ctx, path); err != nil {
				t.Fatal("cannot reset fixture auth method", err)
			}
		}
	}
	policies, err := root.Sys().ListPoliciesWithContext(ctx)
	if err != nil {
		t.Fatal("cannot list fixture policies", err)
	}
	for _, policy := range policies {
		if policy != "default" && policy != "root" {
			if err := root.Sys().DeletePolicyWithContext(ctx, policy); err != nil {
				t.Fatal("cannot reset fixture policy", err)
			}
		}
	}
}

func TestCredentialConcurrentCreateWithRealOpenBao(t *testing.T) {
	root := baoFixture(t)
	store := fixtureStore(t, root)
	credential := fixtureCredential(t)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { results <- store.Create(t.Context(), credentialPath, credential) })
	}
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		switch err {
		case nil:
			succeeded++
		case openbao.ErrConflict:
			conflicted++
		default:
			t.Fatal("unexpected concurrent create result")
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatal("CAS must allow exactly one creator")
	}
}

func TestCredentialLostWriteResponseWithRealOpenBao(t *testing.T) {
	root := baoFixture(t)
	address, err := url.Parse(root.Address())
	if err != nil {
		t.Fatal("invalid fixture address")
	}
	proxy := httputil.NewSingleHostReverseProxy(address)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodPut && response.StatusCode == http.StatusOK {
			return errors.New("fixture drops successful write response")
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusBadGateway)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	store := fixtureStore(t, fixtureClient(t, server.URL))
	credential := fixtureCredential(t)
	if err := store.Create(t.Context(), credentialPath, credential); err != openbao.ErrUncertain {
		t.Fatal("lost response must stop provisioning")
	}
	confirmed, err := root.KVv2("secret").Get(t.Context(), credentialPath)
	if err != nil || !maps.Equal(confirmed.Data, credential.SecretData()) || confirmed.VersionMetadata.Version != 1 {
		t.Fatal("fault injection did not preserve the original write")
	}
	if err := fixtureStore(t, root).Create(t.Context(), credentialPath, credential); err != openbao.ErrConflict {
		t.Fatal("restart must not adopt an unconfirmed write")
	}
}

func TestCredentialsWithRealOpenBao(t *testing.T) {
	root := baoFixture(t)
	ctx := t.Context()
	// root 仅用于 fixture 装配；实际读写使用固定前缀的短期 token。
	policy := `path "secret/data/applications/*" { capabilities = ["create", "update", "read"] }`
	if err := root.Sys().PutPolicyWithContext(ctx, "application-fixture", policy); err != nil {
		t.Fatal("cannot configure fixture policy")
	}
	secret, err := root.Auth().Token().CreateWithContext(ctx, &bao.TokenCreateRequest{
		Policies: []string{"application-fixture"}, NoDefaultPolicy: true, TTL: "5m",
	})
	if err != nil {
		t.Fatal("cannot create scoped fixture token")
	}
	client := fixtureClient(t, root.Address())
	client.SetToken(secret.Auth.ClientToken)
	store := fixtureStore(t, client)
	credential := fixtureCredential(t)
	if err := store.Create(ctx, credentialPath, credential); err != nil {
		t.Fatal(err)
	}
	// 重建适配器读取已确认路径；重复 Create 仍报冲突，不把读取当作认领。
	restarted := fixtureStore(t, client)
	observed, err := restarted.Read(ctx, credentialPath)
	if err != nil || !maps.Equal(observed.SecretData(), credential.SecretData()) {
		t.Fatal("confirmed credential was not preserved across adapter restart")
	}
	if err := restarted.Create(ctx, credentialPath, credential); !errors.Is(err, openbao.ErrConflict) {
		t.Fatal("existing credential must conflict even if contents match")
	}
	metadata, err := root.KVv2("secret").GetMetadata(ctx, credentialPath)
	if err != nil || metadata.CurrentVersion != 1 {
		t.Fatal("duplicate create changed credential version")
	}
	if _, err := client.KVv2("secret").Get(ctx, "management/instance"); err == nil {
		t.Fatal("scoped token accessed management credentials")
	}
	if err := root.KVv2("secret").Delete(ctx, credentialPath); err != nil {
		t.Fatal("cannot soft-delete fixture credential")
	}
	if _, err := store.Read(ctx, credentialPath); err != openbao.ErrNotFound {
		t.Fatal("soft-deleted credential must not be usable")
	}
	if err := store.Create(ctx, credentialPath, credential); err != openbao.ErrConflict {
		t.Fatal("soft-deleted credential must not be recreated")
	}
}
