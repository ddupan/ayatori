package v1alpha1_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	testNamespace    = "database-api"
	testInstanceName = "shared-postgres"
	readyPhase       = "Ready"
)

func TestDatabaseAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS 未设置；运行 make test 执行真实 API server 测试")
	}
	scheme := runtime.NewScheme()
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crdPath, err := filepath.Abs("../../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("启动 envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("停止 envtest: %v", err)
		}
	})
	client, err := ctrlclient.New(config, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	namespace.Name = testNamespace
	if err := client.Create(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}
	t.Run("作用域和默认值", func(t *testing.T) { testDefaults(t, client) })
	t.Run("拒绝非法声明", func(t *testing.T) { testInvalidDeclarations(t, client) })
	t.Run("status隔离和绑定并发", func(t *testing.T) { testBindingWrites(t, client) })
	t.Run("凭据位置与确认版本", func(t *testing.T) { testCredentialStatus(t, client) })
	t.Run("资源创建确认", func(t *testing.T) { testResourceStatus(t, client) })
	t.Run("扩展只追加", func(t *testing.T) { testExtensionsAppendOnly(t, client) })
	t.Run("仓库示例", func(t *testing.T) { testSamples(t, client, scheme) })
}

func testSamples(t *testing.T, client ctrlclient.Client, scheme *runtime.Scheme) {
	paths := []string{
		"database_v1alpha1_postgresqlinstance.yaml",
		"database_v1alpha1_postgresqldatabase.yaml",
		"database_v1alpha1_postgresqltenant.yaml",
	}
	for _, name := range paths {
		t.Run(name, func(t *testing.T) {
			file, err := os.Open(filepath.Join("../../../config/samples", name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
			for {
				var raw runtime.RawExtension
				if err := decoder.Decode(&raw); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(raw.Raw, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				resource, ok := object.(ctrlclient.Object)
				if !ok {
					t.Fatalf("示例不是资源对象: %T", object)
				}
				if resource.GetNamespace() != "" {
					resource.SetNamespace(testNamespace)
				}
				if err := client.Create(t.Context(), resource); err != nil {
					t.Fatalf("示例未通过 API 校验: %v", err)
				}
			}
		})
	}
}

func testDefaults(t *testing.T, client ctrlclient.Client) {
	instance := validInstance("defaults")
	if err := client.Create(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	endpoint := instance.Spec.Endpoint
	if endpoint.Port != 5432 || endpoint.Database != "postgres" || endpoint.SSLMode != "verify-full" {
		t.Fatalf("连接默认值不符: %+v", endpoint)
	}
	credentials := instance.Spec.AdminCredentialRef
	if credentials.UsernameKey != "username" || credentials.PasswordKey != "password" {
		t.Fatal("管理 Secret 字段默认值不符")
	}
	database := validDatabase("defaults")
	if err := client.Create(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	if database.Spec.ReclaimPolicy != databasev1alpha1.ReclaimRetain {
		t.Fatalf("默认回收策略 = %q", database.Spec.ReclaimPolicy)
	}
	// 进入删除流程前允许双向修改策略，不要求第二次审批字段。
	for _, policy := range []databasev1alpha1.ReclaimPolicy{databasev1alpha1.ReclaimDelete, databasev1alpha1.ReclaimRetain} {
		database.Spec.ReclaimPolicy = policy
		if err := client.Update(t.Context(), database); err != nil {
			t.Fatalf("修改回收策略: %v", err)
		}
	}
	objects := []struct {
		object     ctrlclient.Object
		namespaced bool
	}{
		{instance, false}, {database, false}, {validTenant("scope"), true},
	}
	for _, item := range objects {
		namespaced, err := client.IsObjectNamespaced(item.object)
		if err != nil || namespaced != item.namespaced {
			t.Fatalf("%T 作用域 = %v, error = %v", item.object, namespaced, err)
		}
	}
	// 导入不要求 Tenant 或 Instance 对象已经存在，跨对象就绪由 controller 判断。
	imported := validDatabase("imported")
	imported.Spec.Source = "Import"
	imported.Spec.CredentialRef = &databasev1alpha1.CredentialReference{Mount: "secret", Path: "existing/app"}
	if err := client.Create(t.Context(), imported); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		tenant := validTenant(name)
		tenant.Spec.Provision = nil
		tenant.Spec.DatabaseRef = &databasev1alpha1.DatabaseReference{Name: "imported"}
		if err := client.Create(t.Context(), tenant); err != nil {
			t.Fatalf("声明已有资源申请: %v", err)
		}
	}
	// 两个申请都可被 API 接受，不代表二者都已绑定或获得凭据。
}

func testInvalidDeclarations(t *testing.T, client ctrlclient.Client) {
	instanceCases := []struct {
		name   string
		mutate func(*databasev1alpha1.PostgreSQLInstance)
	}{
		{"port", func(i *databasev1alpha1.PostgreSQLInstance) { i.Spec.Endpoint.Port = -1 }},
		{"address", func(i *databasev1alpha1.PostgreSQLInstance) { i.Spec.Endpoint.HostAddr = "localhost" }},
		{"scoped-address", func(i *databasev1alpha1.PostgreSQLInstance) { i.Spec.Endpoint.HostAddr = "fe80::1%eth0" }},
		{"tls", func(i *databasev1alpha1.PostgreSQLInstance) { i.Spec.Endpoint.SSLMode = "prefer" }},
		{"identifier", func(i *databasev1alpha1.PostgreSQLInstance) { i.Spec.Endpoint.Database = "bad-name" }},
		{"secret-key", func(i *databasev1alpha1.PostgreSQLInstance) { i.Spec.AdminCredentialRef.PasswordKey = "bad/key" }},
	}
	for _, tc := range instanceCases {
		t.Run(tc.name, func(t *testing.T) {
			object := validInstance(tc.name)
			tc.mutate(object)
			requireInvalidCreate(t, client, object)
		})
	}
	databaseCases := []struct {
		name   string
		mutate func(*databasev1alpha1.PostgreSQLDatabase)
	}{
		{"missing-instance", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Spec.InstanceRef.Name = "" }},
		{"missing-role", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Spec.LoginRole = "" }},
		{"unknown-source", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Spec.Source = "Adopt" }},
		{"missing-credentials", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Spec.Source = "Import" }},
		{"provision-credentials", func(d *databasev1alpha1.PostgreSQLDatabase) {
			d.Spec.CredentialRef = &databasev1alpha1.CredentialReference{Mount: "secret", Path: "existing"}
		}},
		{"unknown-policy", func(d *databasev1alpha1.PostgreSQLDatabase) { d.Spec.ReclaimPolicy = "Recycle" }},
		{"binding-without-uid", func(d *databasev1alpha1.PostgreSQLDatabase) {
			d.Spec.TenantRef = &databasev1alpha1.TenantReference{Namespace: testNamespace, Name: "tenant"}
		}},
	}
	for _, tc := range databaseCases {
		t.Run(tc.name, func(t *testing.T) {
			object := validDatabase(tc.name)
			tc.mutate(object)
			requireInvalidCreate(t, client, object)
		})
	}
	t.Run("互斥申请入口", func(t *testing.T) {
		tenant := validTenant("ambiguous")
		tenant.Spec.DatabaseRef = &databasev1alpha1.DatabaseReference{Name: "existing"}
		requireInvalidCreate(t, client, tenant)
		tenant.Spec.Provision = nil
		tenant.Spec.DatabaseRef = nil
		requireInvalidCreate(t, client, tenant)
	})
}

// 本测试验证 API 写入语义，不模拟或宣称已经实现 controller 的恢复循环。
func testBindingWrites(t *testing.T, client ctrlclient.Client) {
	ctx := t.Context()
	tenant := validTenant("binding")
	tenant.Status.Phase = readyPhase
	if err := client.Create(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Phase != "" {
		t.Fatal("普通 Create 不应写入 status")
	}
	database := validDatabase("binding")
	if err := client.Create(ctx, database); err != nil {
		t.Fatal(err)
	}
	stale := database.DeepCopy()
	database.Spec.TenantRef = &databasev1alpha1.TenantReference{
		Namespace: tenant.Namespace, Name: databasev1alpha1.ObjectName(tenant.Name), UID: tenant.UID,
	}
	if err := client.Update(ctx, database); err != nil {
		t.Fatal(err)
	}
	stale.Spec.TenantRef = &databasev1alpha1.TenantReference{Namespace: tenant.Namespace, Name: "other", UID: "other-uid"}
	if err := client.Update(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("过期并发写入 = %v, want Conflict", err)
	}
	// 换用 API 回读的对象补第二步，证明恢复所需记录不依赖先前内存。
	observedDatabase := &databasev1alpha1.PostgreSQLDatabase{}
	if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(database), observedDatabase); err != nil {
		t.Fatal(err)
	}
	if observedDatabase.Spec.TenantRef.UID != tenant.UID {
		t.Fatal("资源侧绑定被竞争写入覆盖")
	}
	beforeGeneration := tenant.Generation
	tenant.Status.DatabaseRef = &databasev1alpha1.BoundDatabaseReference{
		Name: databasev1alpha1.ObjectName(database.Name), UID: database.UID,
	}
	if err := client.Status().Update(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Generation != beforeGeneration || tenant.Status.DatabaseRef.UID != database.UID {
		t.Fatal("status 更新错误地影响 generation 或绑定身份")
	}
	tenant.Status.Phase = readyPhase
	if err := client.Update(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Phase != "" {
		t.Fatal("普通 Update 不应修改 status")
	}
	condition := metav1.Condition{Type: readyPhase, Status: metav1.ConditionFalse,
		Reason: "Pending", Message: "尚未验证后端", LastTransitionTime: metav1.Now()}
	tenant.Status.Conditions = []metav1.Condition{condition, condition}
	if err := client.Status().Update(ctx, tenant); !apierrors.IsInvalid(err) {
		t.Fatalf("重复 Condition = %v, want Invalid", err)
	}
}

func requireInvalidCreate(t *testing.T, client ctrlclient.Client, object ctrlclient.Object) {
	t.Helper()
	if err := client.Create(context.Background(), object); !apierrors.IsInvalid(err) {
		t.Fatalf("Create %T = %v, want Invalid", object, err)
	}
}

func validInstance(name string) *databasev1alpha1.PostgreSQLInstance {
	object := &databasev1alpha1.PostgreSQLInstance{}
	object.Name = name
	object.Spec.Endpoint = databasev1alpha1.PostgreSQLEndpoint{Host: "postgres.example.test", HostAddr: "127.0.0.1"}
	object.Spec.AdminCredentialRef.Name = "postgres-admin"
	return object
}

func validDatabase(name string) *databasev1alpha1.PostgreSQLDatabase {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	object.Name = name
	object.Spec = databasev1alpha1.PostgreSQLDatabaseSpec{
		InstanceRef: databasev1alpha1.InstanceReference{Name: testInstanceName},
		Database:    "app", LoginRole: "app", Source: "Provision",
	}
	return object
}

func validTenant(name string) *databasev1alpha1.PostgreSQLTenant {
	object := &databasev1alpha1.PostgreSQLTenant{}
	object.Name = name
	object.Namespace = testNamespace
	object.Spec.Provision = &databasev1alpha1.DatabaseProvisionRequest{
		InstanceRef: databasev1alpha1.InstanceReference{Name: testInstanceName},
	}
	return object
}
