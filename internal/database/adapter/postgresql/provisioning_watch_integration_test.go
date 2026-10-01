//go:build integration

package postgresql_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	databasedomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
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
		backend := &countProvisioningObservations{PostgreSQLBackend: f.service}
		reconciler := &databasecontroller.DatabaseReconciler{Client: manager.GetClient(), Reader: manager.GetAPIReader(), Credentials: f.store, PostgreSQL: backend,
			Secrets: f.secrets}
		if err := reconciler.SetupWithManager(manager); err != nil {
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
	f.status(t, database, databasedomain.ResourceAvailable)
}

type countProvisioningObservations struct {
	databasecontroller.PostgreSQLBackend
	reads atomic.Int32
}

func (b *countProvisioningObservations) InspectResources(ctx context.Context, target instance.ObservationTarget,
	credentials instance.ManagementCredentials, db *databasedomain.Database) (databasedomain.ResourceFacts, error) {
	facts, err := b.PostgreSQLBackend.InspectResources(ctx, target, credentials, db)
	if err == nil && db.Name() == "watch_supply" {
		b.reads.Add(1)
	}
	return facts, err
}
