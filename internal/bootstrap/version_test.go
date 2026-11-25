package bootstrap

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// 以 deploy/flux 的权限、manager 的真实身份验证：manager 能创建并更新 kube-public/ayatori-version，
// 登录的用户能读它；kube-public 的其他 ConfigMap 两者都改不了、读不到。
func TestVersionPublisher(t *testing.T) {
	env := &envtest.Environment{}
	admin, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := t.Context()

	as := func(name string) *rest.Config {
		t.Helper()
		user, err := env.AddUser(envtest.User{Name: name}, admin)
		if err != nil {
			t.Fatal(err)
		}
		return user.Config()
	}
	manager, err := client.New(as("ayatori-controller"), client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reader := kubernetes.NewForConfigOrDie(as("someone"))

	// 升级时 manager 先于新版本的 RBAC 启动：先被拒绝，RBAC 出现后重试写入。
	done := make(chan struct{})
	go func() { _ = (versionPublisher{client: manager, version: "v0.5.9"}).Start(ctx); close(done) }()
	time.Sleep(time.Second)
	createAll(t, admin, filepath.Join("..", "..", "deploy", "flux", "control-plane", "version.yaml"))
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the publisher did not publish once the RBAC appeared")
	}
	cm, err := reader.CoreV1().ConfigMaps(VersionNamespace).Get(ctx, VersionConfigMap, metav1.GetOptions{})
	if err != nil || cm.Data["version"] != "v0.5.9" {
		t.Fatalf("after retrying: %v %v", cm.Data, err)
	}

	for _, version := range []string{"v0.6.0", "v0.6.1"} {
		if err := (versionPublisher{client: manager, version: version}).publish(ctx); err != nil {
			t.Fatal(err)
		}
		cm, err := reader.CoreV1().ConfigMaps(VersionNamespace).Get(ctx, VersionConfigMap, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if cm.Data["version"] != version {
			t.Errorf("published %q, want %q", cm.Data["version"], version)
		}
	}

	// create 不能按名字限制，但别的 ConfigMap 建出来之后 manager 改不了、用户也读不到。
	other := &corev1.ConfigMap{Name: "other", Namespace: VersionNamespace}
	if err := client.IgnoreAlreadyExists(manager.Create(ctx, other)); err != nil {
		t.Fatal(err)
	}
	other.Data = map[string]string{"k": "v"}
	if err := manager.Update(ctx, other); !apierrors.IsForbidden(err) {
		t.Errorf("manager updated another ConfigMap in %s: %v", VersionNamespace, err)
	}
	if _, err := reader.CoreV1().ConfigMaps(VersionNamespace).Get(ctx, "other", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("a user read another ConfigMap in %s: %v", VersionNamespace, err)
	}
	if _, err := reader.CoreV1().ConfigMaps(VersionNamespace).List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("a user listed ConfigMaps in %s: %v", VersionNamespace, err)
	}
}

// createAll 以管理员创建多文档清单中的全部对象。
func createAll(t *testing.T, config *rest.Config, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if err := c.Create(t.Context(), &object); err != nil {
			t.Fatal(err)
		}
	}
}
