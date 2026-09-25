// Package controller 将 Database 用例接入 Kubernetes 事件和重试调度。
package controller

import (
	"context"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dependencyRetry = 30 * time.Second

type BindingReconciler struct {
	Client client.Client
	Reader client.Reader
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqltenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqltenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqltenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances,verbs=get;list;watch

func (r *BindingReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	resources := &kubernetes.BindingResources{Client: r.Client, Reader: r.Reader}
	service := application.BindingService{Resources: resources}
	result, err := service.Reconcile(ctx, request.Namespace, request.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := resources.Present(ctx, result); err != nil {
		return ctrl.Result{}, err
	}
	if result.RetrySoon {
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if result.Tenant == nil {
		return ctrl.Result{}, nil
	}
	// watch 是主入口，低频重查覆盖依赖事件映射失败，不做冲突忙循环。
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}
