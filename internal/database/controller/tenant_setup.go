package controller

import (
	"context"
	"errors"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const targetDatabaseIndex = "database.bindingTarget"

func (r *TenantReconciler) SetupWithManager(ctx context.Context, manager ctrl.Manager) error {
	if r.Client == nil || r.Reader == nil {
		return errors.New("tenant controller requires injected client and API reader")
	}
	if err := manager.GetFieldIndexer().IndexField(ctx, &databasev1alpha1.PostgreSQLTenant{},
		targetDatabaseIndex, func(object client.Object) []string {
			tenant := object.(*databasev1alpha1.PostgreSQLTenant)
			if tenant.Spec.DatabaseRef != nil {
				return []string{string(tenant.Spec.DatabaseRef.Name)}
			}
			return []string{binding.DynamicDatabaseName(string(tenant.UID))}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(manager).
		Named("database-tenant").
		For(&databasev1alpha1.PostgreSQLTenant{}).
		Watches(&databasev1alpha1.PostgreSQLDatabase{}, handler.EnqueueRequestsFromMapFunc(r.requestsForDatabase)).
		Watches(&databasev1alpha1.PostgreSQLInstance{}, handler.EnqueueRequestsFromMapFunc(r.requestsForInstance)).
		Complete(r)
}

func (r *TenantReconciler) requestsForDatabase(ctx context.Context, object client.Object) []ctrl.Request {
	tenants := &databasev1alpha1.PostgreSQLTenantList{}
	if err := r.Client.List(ctx, tenants, client.MatchingFields{targetDatabaseIndex: object.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "无法映射 Database 事件；等待低频重试")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(tenants.Items))
	for _, tenant := range tenants.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&tenant)})
	}
	return requests
}

func (r *TenantReconciler) requestsForInstance(ctx context.Context, object client.Object) []ctrl.Request {
	// 当前只有 homelab 内部申请，使用 cache 列表过滤，不维护另一份实例/租户集合。
	tenants := &databasev1alpha1.PostgreSQLTenantList{}
	if err := r.Client.List(ctx, tenants); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "无法映射 Instance 事件；等待低频重试")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(tenants.Items))
	for _, tenant := range tenants.Items {
		instanceName := ""
		if tenant.Spec.Provision != nil {
			instanceName = string(tenant.Spec.Provision.InstanceRef.Name)
		} else if tenant.Spec.DatabaseRef != nil {
			database := &databasev1alpha1.PostgreSQLDatabase{}
			if err := r.Client.Get(ctx, types.NamespacedName{Name: string(tenant.Spec.DatabaseRef.Name)}, database); err != nil {
				continue
			}
			instanceName = string(database.Spec.InstanceRef.Name)
		}
		if instanceName == object.GetName() {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&tenant)})
		}
	}
	return requests
}
