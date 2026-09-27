//go:build integration

package openbao_test

import (
	"testing"

	bao "github.com/openbao/openbao/api/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
)

const preparationNamespace = "credential-preparation"

type preparationFixture struct {
	api    client.Client
	config *rest.Config
	scheme *runtime.Scheme
	bao    *bao.Client
}

func newPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	environment := &envtest.Environment{CRDDirectoryPaths: []string{"../../../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Create(t.Context(), &corev1.Namespace{Name: preparationNamespace}); err != nil {
		t.Fatal(err)
	}
	return &preparationFixture{api: api, config: config, scheme: scheme, bao: baoFixture(t)}
}

func (f *preparationFixture) bound(t *testing.T, name string) (*databasev1alpha1.PostgreSQLDatabase, *databasev1alpha1.PostgreSQLTenant) {
	t.Helper()
	instance := &databasev1alpha1.PostgreSQLInstance{Name: name, Spec: databasev1alpha1.PostgreSQLInstanceSpec{
		Endpoint:           databasev1alpha1.PostgreSQLEndpoint{Host: "postgres.example", HostAddr: "192.0.2.1"},
		AdminCredentialRef: databasev1alpha1.AdminCredentialReference{Name: "management"},
	}}
	if err := f.api.Create(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	// 本切片不连接 PG；这里只声明供应前置观察，真实管理能力由既有 PG 集成测试覆盖。
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "FixtureReady", Message: "隔离测试前置观察", ObservedGeneration: instance.Generation,
	})
	if err := f.api.Status().Update(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	tenant := &databasev1alpha1.PostgreSQLTenant{Name: name, Namespace: preparationNamespace}
	tenant.Spec.Provision = &databasev1alpha1.DatabaseProvisionRequest{InstanceRef: databasev1alpha1.InstanceReference{Name: databasev1alpha1.ObjectName(name)}}
	if err := f.api.Create(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	binder := &databasecontroller.BindingReconciler{Client: f.api, Reader: f.api}
	if _, err := binder.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)}); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Get(t.Context(), client.ObjectKeyFromObject(tenant), tenant); err != nil {
		t.Fatal(err)
	}
	database := &databasev1alpha1.PostgreSQLDatabase{}
	if err := f.api.Get(t.Context(), client.ObjectKey{Name: string(tenant.Status.DatabaseRef.Name)}, database); err != nil {
		t.Fatal(err)
	}
	return database, tenant
}

func (f *preparationFixture) service(t *testing.T) application.CredentialPreparation {
	t.Helper()
	return application.CredentialPreparation{
		Resources: &kubernetes.CredentialResources{Client: f.api, Reader: f.api}, Store: fixtureStore(t, f.bao),
	}
}

func (f *preparationFixture) status(t *testing.T, database *databasev1alpha1.PostgreSQLDatabase, version int64, reason string) {
	t.Helper()
	if err := f.api.Get(t.Context(), client.ObjectKeyFromObject(database), database); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(database.Status.Conditions, application.CredentialsReady)
	if database.Status.CredentialVersion != version || condition == nil || condition.Reason != reason {
		t.Fatalf("凭据版本或条件不符：version=%d，期望 reason=%s", database.Status.CredentialVersion, reason)
	}
	if meta.IsStatusConditionTrue(database.Status.Conditions, "Ready") {
		t.Fatal("凭据准备不得宣告 Database Ready")
	}
}
