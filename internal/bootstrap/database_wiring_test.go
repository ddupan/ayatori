package bootstrap

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const controllerLayer = "controller"

func TestDatabaseExplicitWiring(t *testing.T) {
	writer := fake.NewClientBuilder().Build()
	directReader := fake.NewClientBuilder().Build()
	binder := wireBindingController(writer, directReader)
	bindingResources, ok := binder.Service.Resources.(*kubernetes.BindingResources)
	if !ok || binder.Client != writer || bindingResources.Reader != directReader || bindingResources.Client != writer || binder.Presenter != bindingResources {
		t.Fatal("绑定用例没有共享显式注入的 writer、直连 reader 与 presenter")
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
	if wireDatabaseController(writer, directReader, nil, nil, "wiring-tests").PostgreSQL != nil {
		t.Fatal("未启用资源供应时不得注入 PostgreSQL 后端")
	}
}

// 防止领域重新依赖用例/存储，也防止 controller 私自构造后端 adapter。
// Reconciler 本身就是 Kubernetes adapter，与 adapter/kubernetes 同层，允许复用其映射。
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
				backend := strings.Contains(name, "/database/adapter/") && !strings.HasSuffix(name, "/database/adapter/kubernetes")
				if backend || (layer == "domain" && (strings.Contains(name, "/database/adapter/") ||
					(strings.Contains(name, "/database/application") || strings.Contains(name, "k8s.io/") || strings.Contains(name, "/internal/infra/")))) {
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
