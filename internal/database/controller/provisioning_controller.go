package controller

import (
	"context"
	"errors"
	"time"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

type ProvisioningReconciler struct {
	Client  client.Client
	Service *application.DatabaseReconciliation
}

func NewProvisioningReconciler(cache client.Client, service *application.DatabaseReconciliation) *ProvisioningReconciler {
	return &ProvisioningReconciler{Client: cache, Service: service}
}

func (r *ProvisioningReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	operationContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := r.Service.Reconcile(operationContext, request.Name); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

func (r *ProvisioningReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil || r.Service == nil {
		return errors.New("provisioning controller requires injected client and use case")
	}
	return ctrl.NewControllerManagedBy(manager).Named("database-provisioning").
		For(&databasev1alpha1.PostgreSQLDatabase{}).
		Watches(&databasev1alpha1.PostgreSQLTenant{}, handler.EnqueueRequestsFromMapFunc(databaseRequestsForTenant)).
		Watches(&databasev1alpha1.PostgreSQLInstance{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []ctrl.Request {
			return databaseRequestsForInstance(ctx, r.Client, object)
		})).Complete(r)
}
