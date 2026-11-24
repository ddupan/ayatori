package bootstrap

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.ddupan.top/ayatori/internal/database/adapter/postgresql"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const controllerLayer = "controller"

func TestDatabaseExplicitWiring(t *testing.T) {
	writer := fake.NewClientBuilder().Build()
	directReader := fake.NewClientBuilder().Build()
	tenants := wireTenantController(writer, directReader, "https://bao.example.test")
	if tenants.Client != writer || tenants.Reader != directReader || tenants.CredentialURLBase != "https://bao.example.test" {
		t.Fatal("Tenant Reconciler 没有共享显式注入的 writer 与直连 reader")
	}
	instances := &postgresql.Instances{}
	t.Cleanup(instances.Close)
	reconciler := wireInstanceController(writer, directReader, instances, "wiring-tests")
	if reconciler.Client != writer || reconciler.Reader != directReader || reconciler.PostgreSQL != instances ||
		reconciler.Secrets.Reader != directReader || reconciler.Secrets.Namespace != "wiring-tests" {
		t.Fatal("Instance 的连接或 Secret 读取边界未按依赖图注入")
	}
	databases := wireDatabaseController(writer, directReader, nil, instances, "wiring-tests")
	if databases.Client != writer || databases.Reader != directReader || databases.PostgreSQL != instances || databases.Secrets.Reader != directReader {
		t.Fatal("资源供应必须复用 Instance 的连接池登记和直连 Secret 读取")
	}
	lifecycleOnly := wireDatabaseController(writer, directReader, nil, nil, "wiring-tests")
	if lifecycleOnly.PostgreSQL != nil || lifecycleOnly.Credentials != nil {
		t.Fatal("未启用凭据准备与资源供应时只处理生命周期，不得注入后端")
	}
}

// 防止领域依赖 Kubernetes 或基础设施，也防止 controller 私自构造后端 adapter。
func TestDatabaseLayerImports(t *testing.T) {
	for _, layer := range []string{"domain", controllerLayer} {
		err := filepath.WalkDir(filepath.Join("../database", layer), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, dependency := range file.Imports {
				name, err := strconv.Unquote(dependency.Path.Value)
				if err != nil {
					return err
				}
				if strings.Contains(name, "/database/adapter/") ||
					(layer == "domain" && (strings.Contains(name, "k8s.io/") || strings.Contains(name, "/internal/infra/"))) {
					t.Errorf("%s 不得导入 %s", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
