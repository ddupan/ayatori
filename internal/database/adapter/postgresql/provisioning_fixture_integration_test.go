//go:build integration

package postgresql_test

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	databasedomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	bao "github.com/openbao/openbao/api/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type provisioningFixture struct {
	*credentialFixture
	api        client.WithWatch
	scheme     *runtime.Scheme
	store      *openbao.Credentials
	reconciler *databasecontroller.DatabaseReconciler
}

func newProvisioningFixture(t *testing.T) *provisioningFixture {
	t.Helper()
	f := newCredentialFixture(t)
	useNativeManager(t, f)
	if _, err := envtest.InstallCRDs(f.config, envtest.CRDInstallOptions{Paths: []string{"../../../../config/crd/bases"}, ErrorIfPathMissing: true}); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api, err := client.NewWithWatch(f.config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	object := &databasev1alpha1.PostgreSQLInstance{Name: "supply-instance", Spec: databasev1alpha1.PostgreSQLInstanceSpec{
		Endpoint:           databasev1alpha1.PostgreSQLEndpoint{Host: fixtureHost, HostAddr: fixtureAddress, Port: int32(f.port), SSLMode: "disable"},
		AdminCredentialRef: databasev1alpha1.AdminCredentialReference{Name: secretName, UsernameKey: managementUsernameKey, PasswordKey: managementPasswordKey},
	}}
	if err := api.Create(f.ctx, object); err != nil {
		t.Fatal(err)
	}
	instances := &databasecontroller.InstanceReconciler{Client: api, Reader: api, PostgreSQL: f.service, Secrets: f.secrets}
	if _, err := instances.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(f.ctx, client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") {
		t.Fatal("实际管理账号未就绪")
	}
	store := provisioningBao(t)
	return &provisioningFixture{credentialFixture: f, api: api, scheme: scheme, store: store,
		reconciler: &databasecontroller.DatabaseReconciler{Client: api, Reader: api, Credentials: store, PostgreSQL: f.service, Secrets: f.secrets}}
}

// 三后端组合验收只启动自己的 dev Bao；不读取环境 token 或外部地址。
func provisioningBao(t *testing.T) *openbao.Credentials {
	t.Helper()
	const image = "openbao/openbao@sha256:5b2486ab0fb90bbc788cc345b0a08616dfb375873ee8be5df3a2fd4d378a67e0"
	const token = "AYATORI-TEST-ONLY-supply-token"
	prepare, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	if exec.CommandContext(prepare, "docker", "image", "inspect", image).Run() != nil {
		if output, err := exec.CommandContext(prepare, "docker", "pull", image).CombinedOutput(); err != nil {
			t.Fatalf("隔离 Bao 镜像准备失败：%s", output)
		}
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	output, err := exec.CommandContext(ctx, "docker", "run", "--pull=never", "--rm", "-d", "-p", "127.0.0.1::8200", image,
		"server", "-dev", "-dev-root-token-id="+token, "-dev-listen-address=0.0.0.0:8200").Output()
	if err != nil {
		t.Fatal("隔离 Bao 启动失败")
	}
	id := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal("无效容器 ID")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if exec.CommandContext(cleanup, "docker", "rm", "-f", id).Run() != nil {
			t.Error("隔离 Bao 清理失败")
		}
	})
	output, err = exec.CommandContext(ctx, "docker", "inspect", "--format", `{{(index (index .NetworkSettings.Ports "8200/tcp") 0).HostPort}}`, id).Output()
	if err != nil {
		t.Fatal("隔离 Bao 端口不可读")
	}
	backend, err := bao.NewClient(&bao.Config{Address: "http://127.0.0.1:" + strings.TrimSpace(string(output)), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal("隔离 Bao client 构造失败")
	}
	backend.SetToken(token)
	for {
		if _, err := backend.Sys().HealthWithContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("隔离 Bao 健康检查超时")
		case <-time.After(100 * time.Millisecond):
		}
	}
	store, err := openbao.NewCredentials(backend, "secret", "applications")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (f *provisioningFixture) bound(t *testing.T, name string, extensions ...string) *databasev1alpha1.PostgreSQLDatabase {
	t.Helper()
	tenant := &databasev1alpha1.PostgreSQLTenant{Name: strings.ReplaceAll(name, "_", "-"), Namespace: controllerNamespace,
		Spec: databasev1alpha1.PostgreSQLTenantSpec{Provision: &databasev1alpha1.DatabaseProvisionRequest{
			InstanceRef: databasev1alpha1.InstanceReference{Name: "supply-instance"},
			Database:    databasev1alpha1.PostgreSQLIdentifier(name), LoginRole: databasev1alpha1.PostgreSQLIdentifier(name),
		}, Extensions: extensions}}
	if err := f.api.Create(f.ctx, tenant); err != nil {
		t.Fatal(err)
	}
	binder := &databasecontroller.TenantReconciler{Client: f.api, Reader: f.api}
	if _, err := binder.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)}); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(tenant), tenant); err != nil {
		t.Fatal(err)
	}
	database := &databasev1alpha1.PostgreSQLDatabase{}
	if err := f.api.Get(f.ctx, client.ObjectKey{Name: string(tenant.Status.DatabaseRef.Name)}, database); err != nil {
		t.Fatal(err)
	}
	// 先只准备凭据，之后每次 reconcile 恰好推进一步 PostgreSQL 资源。
	prepare := &databasecontroller.DatabaseReconciler{Client: f.api, Reader: f.api, Credentials: f.store}
	for range 3 {
		if _, err := prepare.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(database)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); err != nil || database.Status.CredentialVersion != 1 {
		t.Fatal("凭据未准备完成")
	}
	return database
}

func (f *provisioningFixture) reconcile(t *testing.T, database *databasev1alpha1.PostgreSQLDatabase, phase databasedomain.ResourcePhase) {
	t.Helper()
	if err := reconcileOnce(f.ctx, f.reconciler, database); err != nil {
		t.Fatal(err)
	}
	f.status(t, database, phase)
}

func reconcileOnce(ctx context.Context, reconciler *databasecontroller.DatabaseReconciler, object *databasev1alpha1.PostgreSQLDatabase) error {
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	return err
}

func (f *provisioningFixture) status(t *testing.T, database *databasev1alpha1.PostgreSQLDatabase, phase databasedomain.ResourcePhase) {
	t.Helper()
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(database.Status.Conditions, "ResourcesReady")
	if condition == nil || condition.Reason != string(phase) {
		t.Fatalf("期望资源阶段 %s，实际 %v", phase, condition)
	}
	// 只有资源、扩展收敛且应用凭据本轮登录成功，Database 才就绪。
	if meta.IsStatusConditionTrue(database.Status.Conditions, "Ready") != (phase == databasedomain.ResourceAvailable) {
		t.Fatalf("Ready 与资源阶段 %s 不一致", phase)
	}
}

// queryDatabase 以 superuser 在指定数据库内执行只读校验。
func (f *provisioningFixture) queryDatabase(t *testing.T, database, sql string) string {
	t.Helper()
	output, err := exec.CommandContext(f.ctx, "docker", dockerExec, f.containerID, "psql", "-U", fixtureUser, "-d", database, "-tAc", sql).Output()
	if err != nil {
		t.Fatal("fixture SQL failed")
	}
	return strings.TrimSpace(string(output))
}
