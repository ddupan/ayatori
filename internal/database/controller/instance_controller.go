package controller

import (
	"context"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type InstanceReconciler struct {
	Client          client.Client
	Service         *application.InstanceReconciliation
	Presenter       InstancePresenter
	SecretNamespace string
}

type InstancePresenter interface {
	PresentInstance(context.Context, application.InstanceResult) error
}

func NewInstanceReconciler(cache client.Client, service *application.InstanceReconciliation, presenter InstancePresenter, namespace string) *InstanceReconciler {
	return &InstanceReconciler{Client: cache, Service: service, Presenter: presenter, SecretNamespace: namespace}
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances/finalizers,verbs=update
// Secret 权限单独声明为 namespace Role，不放入生成的 ClusterRole。

func (r *InstanceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	observationContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := r.Service.Reconcile(observationContext, request.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	// 查询超时后仍用 worker context 保存安全失败结果；manager 停止时不强行写入。
	if err := r.Presenter.PresentInstance(ctx, result); err != nil {
		return ctrl.Result{}, err
	}
	if result.Record == nil || result.RemoveProtection {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}
