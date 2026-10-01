//go:build integration

package postgresql_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/provisioning"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func testProvisioningManager(t *testing.T, f *provisioningFixture) {
	t.Helper()
	database := f.bound(t, "watch_supply")
	// 首次 list/watch 自动供应；第二个新 manager 只观察确认记录，不重新建库。
	var roleOID, databaseOID int64
	for range 2 {
		skipRepeatedName := true
		manager, err := ctrl.NewManager(f.config, ctrl.Options{Scheme: f.scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Controller: controllerconfig.Controller{SkipNameValidation: &skipRepeatedName}})
		if err != nil {
			t.Fatal(err)
		}
		resources := &kubernetes.ProvisioningResources{Client: manager.GetClient(), Reader: manager.GetAPIReader()}
		backend := &countProvisioningObservations{ProvisioningBackend: f.service}
		service := &application.DatabaseReconciliation{
			Credentials:  &application.CredentialPreparation{Resources: &kubernetes.CredentialResources{Client: manager.GetClient(), Reader: manager.GetAPIReader()}, Store: f.store},
			Provisioning: &application.DatabaseProvisioning{Resources: resources, Credentials: f.store, Backend: backend},
		}
		if err := databasecontroller.NewProvisioningReconciler(manager.GetClient(), service).SetupWithManager(manager); err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(f.ctx)
		done := make(chan error, 1)
		go func() { done <- manager.Start(ctx) }()
		// 使用 scope 确保任何失败都先停止 worker，再由 fixture 关闭共享连接。
		func() {
			defer func() {
				stop()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("供应 manager 未停止")
				}
			}()
			if !manager.GetCache().WaitForCacheSync(ctx) {
				t.Fatal("供应 cache 未同步")
			}
			deadline := time.After(15 * time.Second)
			for {
				if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(database), database); err != nil {
					t.Fatal(err)
				}
				if meta.IsStatusConditionTrue(database.Status.Conditions, "ResourcesReady") && backend.reads.Load() > 0 {
					break
				}
				select {
				case <-deadline:
					t.Fatal("watch 未推动资源创建")
				case <-time.After(100 * time.Millisecond):
				}
			}
			if roleOID != 0 && (roleOID != database.Status.RoleOID || databaseOID != database.Status.DatabaseOID) {
				t.Fatal("manager 重启替换已确认资源")
			}
			roleOID, databaseOID = database.Status.RoleOID, database.Status.DatabaseOID
		}()
	}
	f.status(t, database, provisioning.Available)
}

type countProvisioningObservations struct {
	application.ProvisioningBackend
	reads atomic.Int32
}

func (b *countProvisioningObservations) InspectResources(ctx context.Context, target instance.ObservationTarget, name, role string) (provisioning.Observation, error) {
	observation, err := b.ProvisioningBackend.InspectResources(ctx, target, name, role)
	if err == nil && name == "watch_supply" {
		b.reads.Add(1)
	}
	return observation, err
}
