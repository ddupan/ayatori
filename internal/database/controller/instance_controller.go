package controller

import (
	"context"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type InstanceReconciler struct {
	Client          client.Client
	Reader          client.Reader
	Observer        application.InstanceObserver
	SecretNamespace string
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances/finalizers,verbs=update
// Secret 权限单独声明为 namespace Role，不放入生成的 ClusterRole。

func (r *InstanceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	resources := &kubernetes.InstanceResources{Client: r.Client, Reader: r.Reader}
	service := application.InstanceReconciliation{Resources: resources, Observer: r.Observer}
	observationContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := service.Reconcile(observationContext, request.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	// 查询超时后仍用 worker context 保存安全失败结果；manager 停止时不强行写入。
	if err := resources.PresentInstance(ctx, result); err != nil {
		return ctrl.Result{}, err
	}
	if result.Record == nil || result.RemoveProtection {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}
