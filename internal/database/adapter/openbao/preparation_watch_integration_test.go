//go:build integration

package openbao_test

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	databasecontroller "go.ddupan.top/ayatori/internal/database/controller"
)

func testPreparationWatch(t *testing.T, f *preparationFixture) {
	database, _ := f.bound(t, "watch")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if err := f.api.Get(context.Background(), client.ObjectKeyFromObject(database), database); err == nil {
			if condition := meta.FindStatusCondition(database.Status.Conditions, "CredentialsReady"); condition != nil {
				t.Logf("失败时凭据条件: %s: %s", condition.Reason, condition.Message)
			}
		}
	})
	instance := &databasev1alpha1.PostgreSQLInstance{}
	if err := f.api.Get(t.Context(), client.ObjectKey{Name: "watch"}, instance); err != nil {
		t.Fatal(err)
	}
	setReady := func(ready metav1.ConditionStatus) {
		t.Helper()
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: ready, Reason: "FixtureObservation", Message: "隔离测试前置观察", ObservedGeneration: instance.Generation,
		})
		if err := f.api.Status().Update(t.Context(), instance); err != nil {
			t.Fatal(err)
		}
	}
	setReady(metav1.ConditionFalse)
	stop := startPreparationManager(t, f)
	waitForPreparation(t, func() bool {
		if err := f.api.Get(t.Context(), client.ObjectKeyFromObject(database), database); err != nil {
			return false
		}
		condition := meta.FindStatusCondition(database.Status.Conditions, "CredentialsReady")
		return condition != nil && condition.Reason == "DependencyUnavailable"
	})
	if database.Status.CredentialRef != nil {
		t.Fatal("Instance 未 Ready 时不应固定或写入凭据")
	}
	setReady(metav1.ConditionTrue)
	waitForPreparation(t, func() bool {
		return f.api.Get(t.Context(), client.ObjectKeyFromObject(database), database) == nil && database.Status.CredentialVersion == 1
	})
	stop()
	// 重启真实 manager 后继续观察；不得只依赖旧进程中的记忆。
	setReady(metav1.ConditionFalse)
	stop = startPreparationManager(t, f)
	defer stop()
	waitForPreparation(t, func() bool {
		if err := f.api.Get(t.Context(), client.ObjectKeyFromObject(database), database); err != nil {
			return false
		}
		condition := meta.FindStatusCondition(database.Status.Conditions, "CredentialsReady")
		return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "DependencyUnavailable"
	})
	setReady(metav1.ConditionTrue)
	waitForPreparation(t, func() bool {
		if err := f.api.Get(t.Context(), client.ObjectKeyFromObject(database), database); err != nil {
			return false
		}
		condition := meta.FindStatusCondition(database.Status.Conditions, "CredentialsReady")
		return condition != nil && condition.Status == metav1.ConditionTrue && database.Status.CredentialVersion == 1
	})
	stored, err := f.bao.KVv2("secret").Get(t.Context(), database.Status.CredentialRef.Path)
	if err != nil || stored.VersionMetadata.Version != 1 {
		t.Fatal("manager 重启不得重建凭据")
	}
}

func startPreparationManager(t *testing.T, f *preparationFixture) func() {
	t.Helper()
	manager, err := ctrl.NewManager(f.config, ctrl.Options{
		Scheme: f.scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		// 测试在同一进程顺序重启 manager；旧 worker 已停止，但名称注册表仍是进程级。
		Controller: controllerconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	binder := &databasecontroller.TenantReconciler{Client: manager.GetClient(), Reader: manager.GetAPIReader()}
	if err := binder.SetupWithManager(t.Context(), manager); err != nil {
		t.Fatal(err)
	}
	preparation := &databasecontroller.DatabaseReconciler{Client: manager.GetClient(), Reader: manager.GetAPIReader(), Credentials: fixtureStore(t, f.bao)}
	if err := preparation.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("凭据 manager 未停止")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitForPreparation(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("凭据 watch 没有在低频重查之前驱动协调")
}
