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
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
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
	credentials, err := kubernetes.NewSecretCredentials(directReader, "wiring-tests")
	if err != nil {
		t.Fatal(err)
	}
	observer, err := application.NewInstanceService(credentials, postgresql.Connector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)
	reconciler := wireInstanceController(writer, directReader, observer, "wiring-tests")
	resources, ok := reconciler.Service.Resources.(*kubernetes.InstanceResources)
	if !ok || resources.Reader != directReader || resources.Client != writer || reconciler.Service.Observer != observer || reconciler.Presenter != resources {
		t.Fatal("Instance 的服务或读取边界未按依赖图注入")
	}
}

// 防止领域重新依赖用例/存储，也防止 controller 再次私自构造具体 adapter。
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
				if strings.Contains(name, "/database/adapter/") || (layer == "domain" &&
					(strings.Contains(name, "/database/application") || strings.Contains(name, "k8s.io/") || strings.Contains(name, "/internal/infra/"))) {
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
