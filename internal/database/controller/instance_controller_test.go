package controller

import (
	"context"
	"errors"
	"testing"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	"go.ddupan.top/ayatori/internal/database/domain/instance"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type instanceBackend struct {
	facts     instance.ManagementFacts
	err       error
	inspect   func()
	forgotten int
}

func (b *instanceBackend) InspectManagement(context.Context, instance.ObservationTarget, instance.ManagementCredentials) (instance.ManagementFacts, error) {
	if b.inspect != nil {
		b.inspect()
	}
	return b.facts, b.err
}

func (b *instanceBackend) Forget(string) { b.forgotten++ }

var nativeManager = instance.ManagementFacts{Version: "18", CreateRole: true, CreateDatabase: true}

func newInstanceReconciler(apiClient client.Client, backend *instanceBackend) *InstanceReconciler {
	return &InstanceReconciler{Client: apiClient, Reader: apiClient, PostgreSQL: backend,
		Secrets: ManagementSecrets{Reader: apiClient, Namespace: bindingNamespace}}
}

func managementSecret(t *testing.T, apiClient client.Client) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{Data: map[string][]byte{"username": []byte("fixture"), "password": []byte("test-only-instance-password")}}
	secret.Name, secret.Namespace = "admin", bindingNamespace
	requireCreate(t, apiClient, secret)
	return secret
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
	backend := &instanceBackend{facts: nativeManager}
	reconciler := newInstanceReconciler(apiClient, backend)
	object := readyInstance(t, apiClient, "observed-instance")
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, reasonDependency)
	if backend.forgotten == 0 {
		t.Fatal("无法读取管理 Secret 时应释放旧连接")
	}
	secret := managementSecret(t, apiClient)
	backend.inspect = func() {
		current := &databasev1alpha1.PostgreSQLInstance{}
		current.Name = object.Name
		reload(t, apiClient, current)
		if !controllerutil.ContainsFinalizer(current, InstanceFinalizer) {
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
	backend.err = instance.ErrAuthentication
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "AuthenticationFailed")
	if object.Status.PostgreSQLVersion != "" {
		t.Fatal("观察失败应清除旧版本结果")
	}
	backend.err = nil
	backend.facts.CreateRole = false
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "InsufficientPrivileges")
	backend.facts = nativeManager
	delete(secret.Data, "password")
	if err := apiClient.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "InvalidCredentials")
	secret.Data["password"] = []byte("test-only-instance-password")
	if err := apiClient.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	// 用新 reconciler 恢复；不依赖上轮领域对象或 Ready。
	reconciler = newInstanceReconciler(apiClient, backend)
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
	backend.facts.Version = "18.1"
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

func TestInstanceFailurePresentation(t *testing.T) {
	for _, test := range []struct {
		err    error
		reason string
	}{
		{instance.ErrAuthentication, "AuthenticationFailed"},
		{instance.ErrCredentialsInvalid, "InvalidCredentials"},
		{ErrCredentialsUnavailable, instanceDependencyUnavailable},
		{context.DeadlineExceeded, instanceDependencyUnavailable},
		{errors.New("private backend detail"), instanceDependencyUnavailable},
	} {
		reason, message := observationFailure(test.err)
		if reason != test.reason || message == "" || message == test.err.Error() {
			t.Fatal("观察失败没有安全且可诊断的状态")
		}
	}
	for _, failure := range []instance.Failure{
		instance.NoFailure, instance.ObservationIncomplete, instance.DependencyUnavailable,
		instance.AuthenticationFailed, instance.InsufficientPrivileges,
	} {
		reason, message := managementResult(failure)
		if reason == "" || message == "" || (reason == "ManagementReady") != (failure == instance.NoFailure) {
			t.Fatal("领域能力判定与状态不一致")
		}
	}
}

type failedReferenceReader struct{ client.Reader }

func (*failedReferenceReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("injected reference list failure")
}

func TestInstanceDeletionProtection(t *testing.T) {
	apiClient, _, _ := bindingEnvironment(t)
	backend := &instanceBackend{facts: nativeManager}
	reconciler := newInstanceReconciler(apiClient, backend)
	managementSecret(t, apiClient)
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
	reconciler.Reader = &failedReferenceReader{Reader: apiClient}
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, reasonDependency)
	reconciler.Reader = apiClient
	reconcileInstance(t, reconciler, object)
	reload(t, apiClient, object)
	assertInstanceReason(t, object, "InstanceInUse")
	if object.Status.Phase != string(instance.PhaseDeleting) || backend.forgotten == 0 {
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
