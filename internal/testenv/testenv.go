//go:build integration

// Package testenv 连接 hack/integration-test.sh 在 go test 之外启动的共享后端。
// 后端只启动一次：原来每个场景各起一次 API server 与容器，负载高时启动与清理超时。
// 测试串行运行，每个场景独占这些实例，开始前自行隔离或复位。
package testenv

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
)

const hint = "；经 make 的集成测试 target（hack/integration-test.sh）运行"

var scenarios atomic.Int64

// Scenario 返回进程内不重复的编号，用于 namespace 与用户名。共享的 API server 跨包使用，
// 调用方再加上自己的前缀。
func Scenario() int64 { return scenarios.Add(1) }

// APIServer 返回共享 API server（hack/apiserver）的管理员配置，并在场景结束时清除 cluster 级
// 对象。namespaced 对象随场景各自的 namespace 留下，互不可见。
func APIServer(t *testing.T) *rest.Config {
	t.Helper()
	kubeconfig, err := os.ReadFile(os.Getenv("AYATORI_IT_APISERVER"))
	if err != nil {
		t.Fatal("AYATORI_IT_APISERVER 不可读"+hint, err)
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetCluster(t, config) })
	return config
}

// resetCluster 删除 Database 领域的 cluster 级对象。场景的 manager 已先停止（Cleanup 后进先出），
// finalizer 不会再被处理，直接去掉后删除。
func resetCluster(t *testing.T, config *rest.Config) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, list := range []client.ObjectList{&databasev1alpha1.PostgreSQLDatabaseList{}, &databasev1alpha1.PostgreSQLInstanceList{}} {
		if err := api.List(ctx, list); err != nil {
			t.Fatal("cannot list cluster-scoped objects", err)
		}
		if err := meta.EachListItem(list, func(item runtime.Object) error {
			object := item.(client.Object)
			object.SetFinalizers(nil)
			if err := api.Update(ctx, object); client.IgnoreNotFound(err) != nil {
				return err
			}
			return client.IgnoreNotFound(api.Delete(ctx, object))
		}); err != nil {
			t.Fatal("cannot remove cluster-scoped objects", err)
		}
	}
}

// Container 返回共享后端的容器 ID 与发布到 127.0.0.1 的端口。只认脚本给出的容器 ID，
// 不接受外部地址。
func Container(t *testing.T, service string, containerPort int) (string, int) {
	t.Helper()
	variable := "AYATORI_IT_" + strings.ToUpper(service)
	id := os.Getenv(variable)
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal(variable + " 不是容器 ID" + hint)
	}
	output, err := exec.CommandContext(t.Context(), "docker", "port", id, strconv.Itoa(containerPort)).Output()
	if err != nil {
		t.Fatal("cannot inspect fixture port", err)
	}
	_, port, _ := strings.Cut(strings.TrimSpace(strings.Split(string(output), "\n")[0]), ":")
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal("invalid fixture port")
	}
	return id, value
}
