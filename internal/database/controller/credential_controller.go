package controller

import (
	"context"
	"errors"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// CredentialReconciler 只连接事件、用例和重试，不在控制器中编排凭据写入。
type CredentialReconciler struct {
	Client  client.Client
	Service *application.CredentialPreparation
}

func NewCredentialReconciler(cache client.Client, service *application.CredentialPreparation) *CredentialReconciler {
	return &CredentialReconciler{Client: cache, Service: service}
}

func (r *CredentialReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if err := r.Service.Reconcile(ctx, request.Name); err != nil {
		return ctrl.Result{}, err
	}
	// Bao 的可用性和版本变化没有 Kubernetes watch；与已有依赖重查保持一致。
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

func (r *CredentialReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil || r.Service == nil {
		return errors.New("credential controller requires injected client and use case")
	}
	return ctrl.NewControllerManagedBy(manager).
		Named("database-credentials").For(&databasev1alpha1.PostgreSQLDatabase{}).
		Watches(&databasev1alpha1.PostgreSQLTenant{}, handler.EnqueueRequestsFromMapFunc(r.requestsForTenant)).
		Watches(&databasev1alpha1.PostgreSQLInstance{}, handler.EnqueueRequestsFromMapFunc(r.requestsForInstance)).
		Complete(r)
}

func (r *CredentialReconciler) requestsForTenant(_ context.Context, object client.Object) []ctrl.Request {
	tenant := object.(*databasev1alpha1.PostgreSQLTenant)
	if tenant.Status.DatabaseRef == nil {
		return nil
	}
	return []ctrl.Request{{Name: string(tenant.Status.DatabaseRef.Name)}}
}

func (r *CredentialReconciler) requestsForInstance(ctx context.Context, object client.Object) []ctrl.Request {
	databases := &databasev1alpha1.PostgreSQLDatabaseList{}
	if err := r.Client.List(ctx, databases); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "无法映射 Instance 凭据准备事件；等待低频重试")
		return nil
	}
	var requests []ctrl.Request
	for _, database := range databases.Items {
		if string(database.Spec.InstanceRef.Name) == object.GetName() {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&database)})
		}
	}
	return requests
}
