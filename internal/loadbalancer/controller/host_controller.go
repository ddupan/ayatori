package controller

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	LoadBalancerHostFinalizer = "network.ayatori.ddupan.top/loadbalancerhost"
	agentReadyType            = "AgentReady"
	// agentStale 是 controller 在 agent 的 Lease 过期时写入的 AgentReady reason；只有此时
	// status.vips 不是 agent 的报告（ADR-0022）。
	agentStale = "AgentStale"
	// agentRole 与 lb-agent 的角色名一致：Lease 为 kube-node-lease/<主机名>-loadbalancer。
	agentRole = "loadbalancer"
	// PurgedType 是 agent 在 LoadBalancerHost 删除中、本机配置已全部清除后写入的 condition（ADR-0022）。
	PurgedType = "Purged"
)

// LoadBalancerHostReconciler 不连接主机（ADR-0022）。agent 的报告（AgentReady 与 vips）由 agent
// 自己写入；它的 Lease 过期时，像 Node lifecycle controller 一样把 AgentReady 置为 Unknown 并清空
// vips。删除时等 agent 在本机清除全部配置并报告 Purged 再放行（ADR-0018）；agent 失联时删除一直
// 等待，由管理员移除 finalizer 强制完成。
type LoadBalancerHostReconciler struct {
	Client client.Client
	// AgentStaleAfter 是 agent 的 Lease 最近一次续约距今的上限，超过即视为 agent 失联。
	AgentStaleAfter time.Duration
	// Now 只供测试替换。
	Now func() time.Time
}

// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch

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
	stale, staleIn, err := r.agentStale(ctx, object.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.saveHost(ctx, object, stale); err != nil {
		return ctrl.Result{}, err
	}
	if !deleting || stale != "" || !meta.IsStatusConditionTrue(object.Status.Conditions, PurgedType) {
		return ctrl.Result{RequeueAfter: min(observeInterval, staleIn)}, nil
	}
	controllerutil.RemoveFinalizer(object, LoadBalancerHostFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Client.Update(ctx, object))
}

// agentStale 检查 agent 的心跳：Lease 缺失或超过 AgentStaleAfter 未续约时返回说明；否则返回空串与
// 距离过期的时间，调用方据此排下一次检查。过期是「没有事件」，只能定时检查（与 Node lifecycle
// controller 相同）；不 watch Lease，否则每次续约都会触发一轮对主机的 probe。
func (r *LoadBalancerHostReconciler) agentStale(ctx context.Context, name string) (string, time.Duration, error) {
	lease := &coordinationv1.Lease{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: name + "-" + agentRole}, lease)
	if apierrors.IsNotFound(err) {
		return "agent 尚未续约 Lease", observeInterval, nil
	}
	if err != nil {
		return "", 0, err
	}
	if lease.Spec.RenewTime == nil {
		return "agent 尚未续约 Lease", observeInterval, nil
	}
	left := lease.Spec.RenewTime.Add(r.AgentStaleAfter).Sub(r.now())
	if left < 0 {
		return fmt.Sprintf("agent 超过 %s 未续约 Lease", r.AgentStaleAfter), observeInterval, nil
	}
	return "", left + time.Second, nil
}

// saveHost 在 stale 不为空时覆盖 agent 写入的 AgentReady 与 vips。
func (r *LoadBalancerHostReconciler) saveHost(ctx context.Context, object *networkv1alpha1.LoadBalancerHost, stale string) error {
	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	if stale != "" {
		// 失联 agent 的报告不再可信：宁可报告未宣告，也不报告没有证实的宣告。
		status.VIPs = nil
		agentReady := condition(agentReadyType, false, agentStale, stale, object.Generation)
		agentReady.Status = metav1.ConditionUnknown
		meta.SetStatusCondition(&status.Conditions, agentReady)
	}
	if equality.Semantic.DeepEqual(&object.Status, status) {
		return nil
	}
	object.Status = *status
	return r.Client.Status().Update(ctx, object)
}

func (r *LoadBalancerHostReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// reportOf 把 status 中 agent 的报告还原为领域事实；agent 尚未报告或已失联时 Reported 为 false。
func reportOf(object *networkv1alpha1.LoadBalancerHost) loadbalancer.AgentReport {
	agentReady := meta.FindStatusCondition(object.Status.Conditions, agentReadyType)
	if agentReady == nil || agentReady.Reason == agentStale {
		return loadbalancer.AgentReport{}
	}
	report := loadbalancer.AgentReport{Reported: true, VIPs: map[netip.Addr]loadbalancer.Reason{}}
	for _, vip := range object.Status.VIPs {
		if addr, err := netip.ParseAddr(vip.Address); err == nil {
			report.VIPs[addr] = loadbalancer.Reason(vip.Reason)
			if vip.Announced {
				report.VIPs[addr] = ""
			}
		}
	}
	return report
}

func hostOf(object *networkv1alpha1.LoadBalancerHost) loadbalancer.Host {
	return loadbalancer.Host{Name: object.Name}
}

func (r *LoadBalancerHostReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("loadbalancerhost").
		For(&networkv1alpha1.LoadBalancerHost{}).
		Complete(r)
}
