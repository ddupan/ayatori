package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

const (
	bindingNamespace = "binding-tests"
	phaseBinding     = "Binding"
	phaseBound       = "Bound"
	reasonConflict   = "Conflict"
	reasonDependency = "DependencyUnavailable"
)

func targetDatabaseName(tenant *databasev1alpha1.PostgreSQLTenant) string {
	return "tenant-" + string(tenant.UID)
}

func bindingTestReconciler(writer client.Client, reader client.Reader) *TenantReconciler {
	return &TenantReconciler{Client: writer, Reader: reader}
}

func expectedTenantRef(tenant *databasev1alpha1.PostgreSQLTenant) *databasev1alpha1.TenantReference {
	return &databasev1alpha1.TenantReference{
		Namespace: tenant.Namespace, Name: databasev1alpha1.ObjectName(tenant.Name), UID: tenant.UID,
	}
}

func TestTenantController(t *testing.T) {
	apiClient, config, scheme := bindingEnvironment(t)
	t.Run("动态申请和幂等重试", func(t *testing.T) { testDynamicBinding(t, apiClient) })
	t.Run("双向写入之间重启", func(t *testing.T) { testBindingRestart(t, apiClient) })
	t.Run("并发申请只有一个绑定", func(t *testing.T) { testConcurrentBinding(t, apiClient) })
	t.Run("Released和同名重建", func(t *testing.T) { testBindingIdentity(t, apiClient) })
	t.Run("目标固定与删除保护", func(t *testing.T) { testBindingProtection(t, apiClient) })
	t.Run("拒绝陈旧观察和新实例身份", func(t *testing.T) { testStaleObservation(t, apiClient) })
	t.Run("呈现结果不覆盖并发修改", func(t *testing.T) { testPresentationVersion(t, apiClient) })
	t.Run("依赖稍后出现的watch", func(t *testing.T) { testBindingWatch(t, apiClient, config, scheme) })
}

func bindingEnvironment(t *testing.T) (client.Client, *rest.Config, *runtime.Scheme) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("运行 make test 启动真实 API server")
	}
	scheme := runtime.NewScheme()
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crdPath, err := filepath.Abs("../../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	namespace.Name = bindingNamespace
	requireCreate(t, apiClient, namespace)
	return apiClient, config, scheme
}

func testDynamicBinding(t *testing.T, apiClient client.Client) {
	instance := readyInstance(t, apiClient, "dynamic-instance")
	tenant := provisionTenant("dynamic", instance.Name)
	requireCreate(t, apiClient, tenant)
	reconciler := bindingTestReconciler(apiClient, apiClient)
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	if tenant.Status.DatabaseRef == nil || tenant.Status.Phase != phaseBound {
		t.Fatal("动态申请未建立双向绑定")
	}
	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = targetDatabaseName(tenant)
	reload(t, apiClient, database)
	if database.Spec.Database != "dynamic" || database.Spec.LoginRole != "dynamic" ||
		database.Spec.ReclaimPolicy != databasev1alpha1.ReclaimRetain ||
		*database.Spec.TenantRef != *expectedTenantRef(tenant) || database.Status.InstanceUID != instance.UID {
		t.Fatal("动态资源目标、默认值或身份不符")
	}
	if len(database.OwnerReferences) != 0 || !controllerutil.ContainsFinalizer(database, DatabaseFinalizer) {
		t.Fatal("Database 不应随 Tenant GC，且必须先有删除保护")
	}
	beforeTenant, beforeDatabase := tenant.ResourceVersion, database.ResourceVersion
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	reload(t, apiClient, database)
	if tenant.ResourceVersion != beforeTenant || database.ResourceVersion != beforeDatabase {
		t.Fatal("幂等重试产生了无意义写入")
	}
	assertNotReady(t, tenant, "CredentialsPending")
}

// 只在真实 API 调用边界注入错误，底层仍使用 API server 的并发、status 与 CEL 语义。
type failedTenantStatusClient struct {
	client.Client
}

func (c *failedTenantStatusClient) Status() client.SubResourceWriter {
	return &failedTenantStatusWriter{SubResourceWriter: c.Client.Status()}
}

type failedTenantStatusWriter struct {
	client.SubResourceWriter
}

func (w *failedTenantStatusWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if tenant, ok := object.(*databasev1alpha1.PostgreSQLTenant); ok && tenant.Status.DatabaseRef != nil {
		return errors.New("injected tenant status write failure")
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func testBindingRestart(t *testing.T, apiClient client.Client) {
	instance := readyInstance(t, apiClient, "restart-instance")
	tenant := provisionTenant("restart", instance.Name)
	requireCreate(t, apiClient, tenant)
	first := bindingTestReconciler(&failedTenantStatusClient{Client: apiClient}, apiClient)
	if _, err := first.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)}); err == nil {
		t.Fatal("预期第二次绑定写入失败")
	}
	reload(t, apiClient, tenant)
	if tenant.Status.DatabaseRef != nil || tenant.Status.Phase != phaseBinding {
		t.Fatal("失败后不应伪造申请侧完成")
	}
	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = targetDatabaseName(tenant)
	reload(t, apiClient, database)
	if database.Spec.TenantRef == nil || database.Spec.TenantRef.UID != tenant.UID {
		t.Fatal("失败后资源侧绑定不应回滚")
	}
	// 新建 reconciler，无旧内存，只从 API 中读取进度。
	restarted := bindingTestReconciler(apiClient, apiClient)
	reconcileOK(t, restarted, tenant)
	reload(t, apiClient, tenant)
	if tenant.Status.DatabaseRef == nil || tenant.Status.DatabaseRef.UID != database.UID {
		t.Fatal("重启后未补齐同一资源绑定")
	}
}

func testConcurrentBinding(t *testing.T, apiClient client.Client) {
	instance := readyInstance(t, apiClient, "concurrent-instance")
	database := availableDatabase(t, apiClient, "concurrent-db", instance)
	tenants := []*databasev1alpha1.PostgreSQLTenant{
		existingTenant("contender-one", database.Name), existingTenant("contender-two", database.Name),
	}
	for _, tenant := range tenants {
		requireCreate(t, apiClient, tenant)
	}
	var workers sync.WaitGroup
	results := make(chan error, len(tenants))
	for _, tenant := range tenants {
		workers.Go(func() {
			reconciler := bindingTestReconciler(apiClient, apiClient)
			_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
			results <- err
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil && !apierrors.IsConflict(err) {
			t.Fatalf("并发协调出现非版本冲突错误: %v", err)
		}
	}
	reconciler := bindingTestReconciler(apiClient, apiClient)
	bound := 0
	for _, tenant := range tenants {
		reconcileOK(t, reconciler, tenant)
		reload(t, apiClient, tenant)
		if tenant.Status.DatabaseRef != nil {
			bound++
		} else {
			assertNotReady(t, tenant, reasonConflict)
		}
	}
	if bound != 1 {
		t.Fatalf("绑定申请数 = %d, want 1", bound)
	}
}

func testBindingIdentity(t *testing.T, apiClient client.Client) {
	instance := readyInstance(t, apiClient, "identity-instance")
	database := availableDatabase(t, apiClient, "released-db", instance)
	database.Spec.TenantRef = &databasev1alpha1.TenantReference{
		Namespace: bindingNamespace, Name: "identity", UID: "previous-tenant-uid",
	}
	if err := apiClient.Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	database.Status.Phase = "Released"
	if err := apiClient.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	tenant := existingTenant("identity", database.Name)
	requireCreate(t, apiClient, tenant)
	reconciler := bindingTestReconciler(apiClient, apiClient)
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	assertNotReady(t, tenant, reasonConflict)
	if tenant.Status.DatabaseRef != nil {
		t.Fatal("同名新 Tenant 不应继承旧 UID 的绑定")
	}
	// 同名动态记录没有匹配 UID，不能通过名称猜测这是先前创建的资源。
	dynamic := provisionTenant("collision", instance.Name)
	requireCreate(t, apiClient, dynamic)
	collision := availableDatabase(t, apiClient, targetDatabaseName(dynamic), instance)
	reconcileOK(t, reconciler, dynamic)
	reload(t, apiClient, dynamic)
	assertNotReady(t, dynamic, reasonConflict)
	reload(t, apiClient, collision)
	if collision.Spec.TenantRef != nil {
		t.Fatal("同名未知记录被认领")
	}
}

func testBindingProtection(t *testing.T, apiClient client.Client) {
	tenant := provisionTenant("protection", "missing-instance")
	requireCreate(t, apiClient, tenant)
	reconciler := bindingTestReconciler(apiClient, apiClient)
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	assertNotReady(t, tenant, reasonDependency)
	original := tenant.DeepCopy()
	tenant.Spec.Provision.InstanceRef.Name = "other-instance"
	if err := apiClient.Update(t.Context(), tenant); !apierrors.IsInvalid(err) {
		t.Fatalf("Binding 后目标修改 = %v, want Invalid", err)
	}
	tenant = original
	if err := apiClient.Delete(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	// 尚未产生 Database 时没有需要记录的释放，删除直接完成。
	reconcileOK(t, reconciler, tenant)
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(tenant), tenant); !apierrors.IsNotFound(err) {
		t.Fatal("没有关联 Database 的 Tenant 应可删除", err)
	}
}

func testStaleObservation(t *testing.T, apiClient client.Client) {
	instance := readyInstance(t, apiClient, "stale-instance")
	database := availableDatabase(t, apiClient, "stale-database", instance)
	// 被观察后不能更换实际数据库目标，修改回收策略仍允许。
	changed := database.DeepCopy()
	changed.Spec.Database = "different"
	if err := apiClient.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
		t.Fatalf("已观察目标修改 = %v, want Invalid", err)
	}
	database.Spec.ReclaimPolicy = databasev1alpha1.ReclaimDelete
	if err := apiClient.Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	tenant := existingTenant("stale", database.Name)
	requireCreate(t, apiClient, tenant)
	reconciler := bindingTestReconciler(apiClient, apiClient)
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	assertNotReady(t, tenant, reasonDependency)
	// 即使同名新 Instance 已 Ready，也不能覆盖 Database 记录的旧 Instance UID。
	if err := apiClient.Delete(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	readyInstance(t, apiClient, instance.Name)
	reconcileOK(t, reconciler, tenant)
	reload(t, apiClient, tenant)
	assertNotReady(t, tenant, reasonConflict)
}

func testBindingWatch(t *testing.T, apiClient client.Client, config *rest.Config, scheme *runtime.Scheme) {
	controllerConfig := bindingControllerConfig(t, apiClient, config)
	// controller-runtime 的名称登记跨 manager 生命周期保留；允许 go test -count 重复顺序启动。
	// 每轮 cleanup 等待旧 manager 退出，生产 manager 不关闭名称校验。
	skipRepeatedTestName := true
	manager, err := ctrl.NewManager(controllerConfig, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: controllerconfig.Controller{SkipNameValidation: &skipRepeatedTestName},
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := bindingTestReconciler(manager.GetClient(), manager.GetAPIReader())
	if err := reconciler.SetupWithManager(t.Context(), manager); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager 未及时停止")
		}
	})
	if !manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache 未同步")
	}
	tenant := existingTenant("watch", "late-database")
	requireCreate(t, apiClient, tenant)
	waitForTenant(t, apiClient, tenant, func(current *databasev1alpha1.PostgreSQLTenant) bool {
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return condition != nil && condition.Reason == reasonDependency
	})
	instance := readyInstance(t, apiClient, "late-instance")
	availableDatabase(t, apiClient, "late-database", instance)
	// 小于低频重试周期，只能靠 informer/watch 事件收敛，而不是手工调用 Reconcile。
	waitForTenant(t, apiClient, tenant, func(current *databasev1alpha1.PostgreSQLTenant) bool {
		return current.Status.Phase == phaseBound && current.Status.DatabaseRef != nil
	})
}

// afterBindClient 在资源侧绑定写入成功后执行一次回调，模拟另一个客户端的在途修改。
type afterBindClient struct {
	client.Client
	after func()
}

// 动态申请在创建 Database 时即写入资源侧绑定，已有资源则通过 Update 写入。
func (c *afterBindClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	return c.notify(object, c.Client.Create(ctx, object, options...))
}

func (c *afterBindClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	return c.notify(object, c.Client.Update(ctx, object, options...))
}

func (c *afterBindClient) notify(object client.Object, err error) error {
	if _, ok := object.(*databasev1alpha1.PostgreSQLDatabase); ok && err == nil && c.after != nil {
		c.after()
		c.after = nil
	}
	return err
}

func testPresentationVersion(t *testing.T, apiClient client.Client) {
	instance := readyInstance(t, apiClient, "presentation-instance")
	tenant := provisionTenant("presentation", instance.Name)
	requireCreate(t, apiClient, tenant)
	// 资源侧写入后，另一个客户端修改不属于绑定目标的字段；旧快照不能完成申请侧绑定，只能尽快重来。
	writer := &afterBindClient{Client: apiClient, after: func() {
		current := tenant.DeepCopy()
		reload(t, apiClient, current)
		current.Spec.Extensions = []string{"pgcrypto"}
		current.Annotations = map[string]string{"example.test/keep": "preserved"}
		if err := apiClient.Update(t.Context(), current); err != nil {
			t.Fatal(err)
		}
	}}
	result, err := bindingTestReconciler(writer, apiClient).Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
	if err != nil || result.RequeueAfter > time.Second {
		t.Fatalf("申请变化后应尽快重新协调: result=%+v, err=%v", result, err)
	}
	reload(t, apiClient, tenant)
	if tenant.Status.DatabaseRef != nil {
		t.Fatal("过期快照完成了申请侧绑定")
	}
	reconcileOK(t, bindingTestReconciler(apiClient, apiClient), tenant)
	reload(t, apiClient, tenant)
	if tenant.Status.Phase != phaseBound || len(tenant.Spec.Extensions) != 1 ||
		tenant.Annotations["example.test/keep"] != "preserved" {
		t.Fatal("重新协调未完成绑定或覆盖了其他字段")
	}
}

func bindingControllerConfig(t *testing.T, apiClient client.Client, config *rest.Config) *rest.Config {
	t.Helper()
	content, err := os.ReadFile("../../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(content, role); err != nil {
		t.Fatal(err)
	}
	requireCreate(t, apiClient, role)
	binding := &rbacv1.ClusterRoleBinding{}
	binding.Name = "binding-controller-test"
	binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
	binding.Subjects = []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: binding.Name}}
	requireCreate(t, apiClient, binding)
	controllerConfig := rest.CopyConfig(config)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: binding.Name}
	restricted, err := client.New(controllerConfig, client.Options{Scheme: apiClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	// 绑定角色没有凭据读取权限，也不需要测试中的管理用户权限。
	secret := &corev1.Secret{}
	if err := restricted.Get(t.Context(), client.ObjectKey{Namespace: bindingNamespace, Name: "not-readable"}, secret); !apierrors.IsForbidden(err) {
		t.Fatalf("绑定 controller 读取 Secret = %v, want Forbidden", err)
	}
	return controllerConfig
}

func waitForTenant(t *testing.T, apiClient client.Client, tenant *databasev1alpha1.PostgreSQLTenant,
	predicate func(*databasev1alpha1.PostgreSQLTenant) bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := &databasev1alpha1.PostgreSQLTenant{}
		if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(tenant), current); err == nil && predicate(current) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("Tenant %s 未在 watch 期限内收敛", tenant.Name)
		case <-ticker.C:
		}
	}
}

func readyInstance(t *testing.T, apiClient client.Client, name string) *databasev1alpha1.PostgreSQLInstance {
	t.Helper()
	instance := &databasev1alpha1.PostgreSQLInstance{}
	instance.Name = name
	instance.Spec.Endpoint = databasev1alpha1.PostgreSQLEndpoint{Host: "postgres.example.test", HostAddr: "127.0.0.1"}
	instance.Spec.AdminCredentialRef.Name = "admin"
	requireCreate(t, apiClient, instance)
	instance.Status.Conditions = readyConditions(instance.Generation)
	if err := apiClient.Status().Update(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	return instance
}

func availableDatabase(t *testing.T, apiClient client.Client, name string,
	instance *databasev1alpha1.PostgreSQLInstance) *databasev1alpha1.PostgreSQLDatabase {
	t.Helper()
	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = name
	database.Spec = databasev1alpha1.PostgreSQLDatabaseSpec{
		InstanceRef: databasev1alpha1.InstanceReference{Name: databasev1alpha1.ObjectName(instance.Name)},
		Database:    "existing", LoginRole: "existing", Source: "Import",
		CredentialRef: &databasev1alpha1.CredentialReference{Mount: "secret", Path: "existing/app"},
	}
	requireCreate(t, apiClient, database)
	database.Status.InstanceUID = instance.UID
	database.Status.Phase = "Available"
	database.Status.Conditions = readyConditions(database.Generation)
	if err := apiClient.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

func readyConditions(generation int64) []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Verified",
		Message: "测试提供的后端观察", ObservedGeneration: generation, LastTransitionTime: metav1.Now()}}
}

func provisionTenant(name, instance string) *databasev1alpha1.PostgreSQLTenant {
	tenant := &databasev1alpha1.PostgreSQLTenant{}
	tenant.Name, tenant.Namespace = name, bindingNamespace
	tenant.Spec.Provision = &databasev1alpha1.DatabaseProvisionRequest{
		InstanceRef: databasev1alpha1.InstanceReference{Name: databasev1alpha1.ObjectName(instance)},
	}
	return tenant
}

func existingTenant(name, database string) *databasev1alpha1.PostgreSQLTenant {
	tenant := &databasev1alpha1.PostgreSQLTenant{}
	tenant.Name, tenant.Namespace = name, bindingNamespace
	tenant.Spec.DatabaseRef = &databasev1alpha1.DatabaseReference{Name: databasev1alpha1.ObjectName(database)}
	return tenant
}

func requireCreate(t *testing.T, apiClient client.Client, object client.Object) {
	t.Helper()
	if err := apiClient.Create(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

func reload(t *testing.T, apiClient client.Client, object client.Object) {
	t.Helper()
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
}

func reconcileOK(t *testing.T, reconciler *TenantReconciler, tenant *databasev1alpha1.PostgreSQLTenant) {
	t.Helper()
	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(tenant),
	}); err != nil {
		t.Fatal(err)
	}
}

func assertNotReady(t *testing.T, tenant *databasev1alpha1.PostgreSQLTenant, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(tenant.Status.Conditions, "Ready")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reason {
		t.Fatalf("Ready condition 不符: %+v", condition)
	}
}
