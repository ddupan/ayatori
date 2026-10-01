package controller

import (
	"context"
	"errors"
	"testing"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type instanceBackend struct {
	application.ResourceDatabase
	checks  instance.ManagementChecks
	err     error
	inspect func()
	closed  int
}

func (b *instanceBackend) Read(context.Context, instance.CredentialReference) (application.Credentials, error) {
	return application.NewCredentials("fixture", "test-only-instance-password")
}

func (b *instanceBackend) Connect(context.Context, instance.Endpoint, application.Credentials) (application.Database, error) {
	return b, nil
}

func (b *instanceBackend) InspectMetadata(context.Context) (application.DatabaseMetadata, error) {
	return application.DatabaseMetadata{Version: "18"}, nil
}

func (b *instanceBackend) InspectManagement(context.Context) (application.DatabaseMetadata, error) {
	if b.inspect != nil {
		b.inspect()
	}
	return application.DatabaseMetadata{Version: "18", Management: b.checks}, b.err
}

func (b *instanceBackend) Close() { b.closed++ }

func newInstanceReconciler(t *testing.T, apiClient client.Client, backend *instanceBackend) *InstanceReconciler {
	t.Helper()
	service, err := application.NewInstanceService(backend, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	resources := &kubernetes.InstanceResources{Client: apiClient, Reader: apiClient}
	return NewInstanceReconciler(apiClient, &application.InstanceReconciliation{Resources: resources, Observer: service}, resources, "")
}

func reconcileInstance(t *testing.T, reconciler *InstanceReconciler, object *databasev1alpha1.PostgreSQLInstance) {
	t.Helper()
	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
}

func assertInstanceReason(t *testing.T, object *databasev1alpha1.PostgreSQLInstance, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(object.Status.Conditions, "Ready")
	if condition == nil || condition.Reason != reason || condition.ObservedGeneration != object.Generation {
		t.Fatalf("Instance 状态不是当前 generation 的 %s", reason)
	}
	if reason != "ManagementReady" && condition.Status != metav1.ConditionFalse {
		t.Fatal("失败状态仍为 Ready")
	}
}

func TestInstanceObservationAPI(t *testing.T) {
	apiClient, _, _ := bindingEnvironment(t)
	backend := &instanceBackend{checks: instance.ManagementChecks{
		Connection: instance.CheckPassed, Metadata: instance.CheckPassed,
		Roles: instance.CheckPassed, Databases: instance.CheckPassed,
		Grants: instance.CheckPassed, Extensions: instance.CheckPassed,
	}}
	reconciler := newInstanceReconciler(t, apiClient, backend)
	object := readyInstance(t, apiClient, "observed-instance")
	backend.inspect = func() {
		current := &databasev1alpha1.PostgreSQLInstance{}
		current.Name = object.Name
		reload(t, apiClient, current)
		if !controllerutil.ContainsFinalizer(current, kubernetes.InstanceFinalizer) {
			t.Fatal("观察早于 finalizer 持久化")
		}
	}
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "ManagementReady")
	if object.Status.Phase != string(instance.PhaseReady) || object.Status.PostgreSQLVersion != "18" {
		t.Fatal("当前成功观察未呈现")
	}
	before := object.ResourceVersion
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	if object.ResourceVersion != before {
		t.Fatal("相同观察不应反复写入 status")
	}
	backend.err = application.ErrAuthentication
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "AuthenticationFailed")
	if backend.closed != 1 || object.Status.PostgreSQLVersion != "" {
		t.Fatal("观察失败应释放连接并清除旧版本结果")
	}
	backend.err = nil
	backend.checks.Grants = instance.CheckUnobserved
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "ObservationIncomplete")
	backend.checks.Grants = instance.CheckPassed
	// 用新 service/reconciler 恢复；不依赖上轮领域对象或 Ready。
	reconciler = newInstanceReconciler(t, apiClient, backend)
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "ManagementReady")

	backend.inspect = func() {
		reload(t, apiClient, object)
		object.Annotations = map[string]string{"concurrent": "kept-by-instance-test"}
		if err := apiClient.Update(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	if !apierrors.IsConflict(err) {
		t.Fatal("旧观察不应覆盖在途 API 修改")
	}
	backend.inspect = nil
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	if object.Annotations["concurrent"] != "kept-by-instance-test" {
		t.Fatal("重试覆盖了其他字段")
	}
}

type failedReferenceReader struct{ client.Reader }

func (*failedReferenceReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("injected reference list failure")
}

func TestInstanceDeletionProtection(t *testing.T) {
	apiClient, _, _ := bindingEnvironment(t)
	backend := &instanceBackend{}
	reconciler := newInstanceReconciler(t, apiClient, backend)
	object := readyInstance(t, apiClient, "protected-instance")
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	database := availableDatabase(t, apiClient, "retained-database", object)
	database.Status.Phase = "Released"
	if err := apiClient.Status().Update(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	tenant := provisionTenant("pending-request", object.Name)
	requireCreate(t, apiClient, tenant)
	if err := apiClient.Delete(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	backend.inspect = func() { t.Fatal("删除中不应连接 PostgreSQL") }
	reconciler.Service.Resources.(*kubernetes.InstanceResources).Reader = &failedReferenceReader{Reader: apiClient}
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, reasonDependency)
	reconciler.Service.Resources.(*kubernetes.InstanceResources).Reader = apiClient
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "InstanceInUse")
	if object.Status.Phase != string(instance.PhaseDeleting) || backend.closed != 1 {
		t.Fatal("删除没有停止本地观察")
	}
	if err := apiClient.Delete(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "InstanceInUse")
	if err := apiClient.Delete(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	reconcileInstance(t, reconciler, object)
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
		t.Fatal("最后一个引用解除后 Instance 应可删除")
	}
	reconcileInstance(t, reconciler, object)
}
