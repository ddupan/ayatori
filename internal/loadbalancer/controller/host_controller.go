package controller

import (
	"context"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	LoadBalancerHostFinalizer = "network.ayatori.ddupan.top/loadbalancerhost"
	dataPlaneReachableType    = "DataPlaneReachable"
)

// HostBackend 观察与清理整台主机，不区分 LoadBalancer。
type HostBackend interface {
	Probe(context.Context, loadbalancer.Host) error
	// Purge 删除主机上全部由 Ayatori 写入的配置；返回 true 表示运行中的代理已不再服务它们。
	Purge(context.Context, loadbalancer.Host) (bool, error)
}

// LoadBalancerHostReconciler 报告主机可达性；删除时先清理主机上全部 LoadBalancer 配置再放行
// （ADR-0015）。主机不可达时删除等待，由管理员移除 finalizer 强制完成。
type LoadBalancerHostReconciler struct {
	Client client.Client
	Proxy  HostBackend
}

// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts/finalizers,verbs=update

func (r *LoadBalancerHostReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &networkv1alpha1.LoadBalancerHost{}
	if err := r.Client.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	deleting := !object.DeletionTimestamp.IsZero()
	if deleting && !controllerutil.ContainsFinalizer(object, LoadBalancerHostFinalizer) {
		return ctrl.Result{}, nil
	}
	// LoadBalancer controller 只使用已带 finalizer 的主机，所以写入配置之前清理一定会发生。
	if !deleting && controllerutil.AddFinalizer(object, LoadBalancerHostFinalizer) {
		return ctrl.Result{}, r.Client.Update(ctx, object)
	}
	host := hostOf(object)
	hostCtx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	probe := r.Proxy.Probe(hostCtx, host)
	if err := r.saveHost(ctx, object, probe); err != nil {
		return ctrl.Result{}, err
	}
	if !deleting || probe != nil {
		return ctrl.Result{RequeueAfter: observeInterval}, nil
	}
	done, err := r.Proxy.Purge(hostCtx, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: hostRetry}, nil
	}
	controllerutil.RemoveFinalizer(object, LoadBalancerHostFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Client.Update(ctx, object))
}

func (r *LoadBalancerHostReconciler) saveHost(ctx context.Context, object *networkv1alpha1.LoadBalancerHost, probe error) error {
	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	reachable := condition(dataPlaneReachableType, true, "Reachable", "", object.Generation)
	if probe != nil {
		reachable = condition(dataPlaneReachableType, false, "Unreachable", probe.Error(), object.Generation)
	}
	meta.SetStatusCondition(&status.Conditions, reachable)
	if equality.Semantic.DeepEqual(&object.Status, status) {
		return nil
	}
	object.Status = *status
	return r.Client.Status().Update(ctx, object)
}

func hostOf(object *networkv1alpha1.LoadBalancerHost) loadbalancer.Host {
	return loadbalancer.Host{Name: object.Name, ProxyURL: object.Spec.DataPlaneURL}
}

func (r *LoadBalancerHostReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("loadbalancerhost").
		For(&networkv1alpha1.LoadBalancerHost{}).
		Complete(r)
}
