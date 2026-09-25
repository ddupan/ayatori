//go:build integration

package postgresql_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	secretadapter "git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

const watchRevisionAnnotation = "test.ayatori/observation"

func TestInstanceControllerWithRealPostgreSQL(t *testing.T) {
	f := newCredentialFixture(t)
	if _, err := envtest.InstallCRDs(f.config, envtest.CRDInstallOptions{
		Paths: []string{"../../../../config/crd/bases"}, ErrorIfPathMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{databasev1alpha1.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	apiClient, err := client.New(f.config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	restricted := instanceControllerRBAC(t, f, apiClient)
	credentials, err := secretadapter.NewSecretCredentials(restricted, controllerNamespace)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewInstanceService(credentials, postgresql.Connector{})
	if err != nil {
		t.Fatal(err)
	}
	// 同进程 -count 重复启动测试 manager；生产继续校验 controller 名称唯一。
	skipRepeatedName := true
	manager, err := ctrl.NewManager(restricted, ctrl.Options{
		Scheme: scheme, Cache: databasecontroller.InstanceCacheOptions(controllerNamespace),
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: controllerconfig.Controller{SkipNameValidation: &skipRepeatedName},
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &databasecontroller.InstanceReconciler{Observer: service, SecretNamespace: controllerNamespace}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	managerContext, stop := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- manager.Start(managerContext) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Instance manager 未停止")
		}
		service.Close()
	})
	object := &databasev1alpha1.PostgreSQLInstance{}
	object.Name = "native-instance"
	object.Spec.Endpoint = databasev1alpha1.PostgreSQLEndpoint{
		Host: fixtureHost, HostAddr: fixtureAddress, Port: int32(f.port), SSLMode: "disable",
	}
	object.Spec.AdminCredentialRef = databasev1alpha1.AdminCredentialReference{
		Name: secretName, UsernameKey: managementUsernameKey, PasswordKey: managementPasswordKey,
	}
	if err := apiClient.Create(f.ctx, object); err != nil {
		t.Fatal(err)
	}
	awaitInstanceReason(t, f, apiClient, object, "DependencyUnavailable")
	// 30 秒轮询前必须收到 Secret 创建事件；实际 controller 使用 namespace Role + metadata watch。
	useNativeManager(t, f)
	awaitInstanceReason(t, f, apiClient, object, "ManagementReady")
	before := f.backendIDs(t)
	f.updateSecret(t, func(secret *corev1.Secret) {
		secret.Annotations = map[string]string{watchRevisionAnnotation: "changed"}
	})
	// 用实际 API 事件触发重验，metadata 改动不应换池。
	time.Sleep(200 * time.Millisecond)
	if f.backendIDs(t) != before {
		t.Fatal("无关 Secret metadata 修改重建了连接")
	}
	f.queryPostgres(t, "ALTER ROLE native_manager PASSWORD '"+rotatedPassword+"'")
	f.updateSecret(t, func(secret *corev1.Secret) { secret.Data[managementPasswordKey] = []byte("invalid-test-password") })
	awaitInstanceReason(t, f, apiClient, object, "AuthenticationFailed")
	f.updateSecret(t, func(secret *corev1.Secret) { secret.Data[managementPasswordKey] = []byte(rotatedPassword) })
	awaitInstanceReason(t, f, apiClient, object, "ManagementReady")
	if f.backendIDs(t) == before {
		t.Fatal("凭据轮换没有替换旧连接")
	}
	f.queryPostgres(t, "ALTER ROLE native_manager NOCREATEROLE")
	f.updateSecret(t, func(secret *corev1.Secret) { secret.Annotations[watchRevisionAnnotation] = "recheck" })
	awaitInstanceReason(t, f, apiClient, object, "InsufficientPrivileges")
	f.queryPostgres(t, "ALTER ROLE native_manager CREATEROLE")
	f.updateSecret(t, func(secret *corev1.Secret) { secret.Annotations[watchRevisionAnnotation] = "recovered" })
	awaitInstanceReason(t, f, apiClient, object, "ManagementReady")
	if err := f.client.CoreV1().Secrets(controllerNamespace).Delete(f.ctx, secretName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitInstanceReason(t, f, apiClient, object, "DependencyUnavailable")
	if f.backendIDs(t) != "" {
		t.Fatal("Secret 删除后旧连接未释放")
	}
}

func awaitInstanceReason(t *testing.T, f *credentialFixture, apiClient client.Client,
	object *databasev1alpha1.PostgreSQLInstance, reason string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := apiClient.Get(f.ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(object.Status.Conditions, "Ready")
		if condition != nil && condition.Reason == reason && condition.ObservedGeneration == object.Generation {
			if (condition.Status == metav1.ConditionTrue) != (reason == "ManagementReady") {
				t.Fatal("Ready 与检查结果不一致")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("watch 未及时推进到 %s", reason)
}

func instanceControllerRBAC(t *testing.T, f *credentialFixture, apiClient client.Client) *rest.Config {
	t.Helper()
	roleBytes, err := os.ReadFile("../../../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(roleBytes, role); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Create(f.ctx, role); err != nil {
		t.Fatal(err)
	}
	user := "instance-controller-test"
	binding := &rbacv1.ClusterRoleBinding{}
	binding.Name = user
	binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
	binding.Subjects = []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: user}}
	if err := apiClient.Create(f.ctx, binding); err != nil {
		t.Fatal(err)
	}
	credentialBytes, err := os.ReadFile("../../../../config/rbac/database_credentials_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	namespaceRole := &rbacv1.Role{}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(credentialBytes), 4096)
	if err := decoder.Decode(namespaceRole); err != nil {
		t.Fatal(err)
	}
	namespaceRole.Namespace = controllerNamespace
	if err := apiClient.Create(f.ctx, namespaceRole); err != nil {
		t.Fatal(err)
	}
	namespaceBinding := &rbacv1.RoleBinding{}
	namespaceBinding.Name, namespaceBinding.Namespace = user, controllerNamespace
	namespaceBinding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: namespaceRole.Name}
	namespaceBinding.Subjects = binding.Subjects
	if err := apiClient.Create(f.ctx, namespaceBinding); err != nil {
		t.Fatal(err)
	}
	config := rest.CopyConfig(f.config)
	config.Impersonate.UserName = user
	restrictedClient, err := client.New(config, client.Options{Scheme: apiClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	err = restrictedClient.Get(f.ctx, client.ObjectKey{Namespace: unrelatedNamespace, Name: secretName}, secret)
	if !apierrors.IsForbidden(err) {
		t.Fatal("Instance controller 可以跨 namespace 读取 Secret")
	}
	return config
}
