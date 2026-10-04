//go:build integration

package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestBootstrapWithRealAPIServer(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	user, err := environment.AddUser(envtest.User{Name: "bootstrap-fixture", Groups: []string{"system:masters"}}, config)
	if err != nil {
		t.Fatal(err)
	}
	data, err := user.KubeConfig()
	if err != nil {
		t.Fatal("cannot generate isolated fixture kubeconfig")
	}
	// 仅写临时 envtest 身份，不读取现场 kubeconfig；t.TempDir 会自动清理。
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	t.Setenv("POD_NAMESPACE", "")
	options := parseTestOptions(t, "--metrics-bind-address=0", "--health-probe-bind-address=0", "--webhook-port=-1")
	manager, err := newManager(options)
	if err != nil {
		t.Fatal(err)
	}
	baoClient, err := setupOpenBaoAuthentication(manager, options.openBao)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// 空 API 中没有供应目标；启用凭据准备以验证完整装配路径确实注册 controller，不访问外部 Bao。
	fixtureConfig := bao.NewConfig()
	fixtureConfig.Address = "http://127.0.0.1:1"
	fixtureClient, err := bao.NewClient(fixtureConfig)
	if err != nil {
		t.Fatal(err)
	}
	if baoClient == nil {
		baoClient = fixtureClient
	}
	options.database.credentialMount = "secret"
	closeDatabaseConnections, err := registerDatabaseControllers(ctx, manager, options.database, baoClient)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDatabaseConnections()
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("manager did not stop before component closeDatabaseConnections")
		}
	}()
	if !manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("assembled controller cache did not synchronize")
	}
}
