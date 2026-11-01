package controller

import (
	"context"
	"net/netip"
	"time"

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
	agentReadyType            = "AgentReady"
	// agentUnreachable 是 AgentReady 在 agent 不应答时的 reason；只有此时 status.vips 不是 agent 的报告。
	agentUnreachable = "AgentUnreachable"
)

// HostBackend 观察与清理整台主机，不区分 LoadBalancer。
type HostBackend interface {
	Probe(context.Context, loadbalancer.Host) error
	// Purge 删除主机上全部由 Ayatori 写入的配置；返回 true 表示运行中的代理已不再服务它们。
	Purge(context.Context, loadbalancer.Host) (bool, error)
}

// AgentBackend 读取 LB agent 报告的本机事实。
type AgentBackend interface {
	Status(ctx context.Context, agentURL string) (loadbalancer.AgentStatus, error)
}

// LoadBalancerHostReconciler 报告主机可达性与 agent 状态；删除时先清理主机上全部 LoadBalancer 配置
// 再放行（ADR-0015）。主机不可达时删除等待，由管理员移除 finalizer 强制完成。
type LoadBalancerHostReconciler struct {
	Client client.Client
	Proxy  HostBackend
	Agent  AgentBackend
	// AgentStaleAfter 是 agent 最近一次成功收敛距今的上限，超过即视为收敛停滞。
	AgentStaleAfter time.Duration
	// Now 只供测试替换。
	Now func() time.Time
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
	agent, agentErr := r.Agent.Status(hostCtx, object.Spec.AgentURL)
	if err := r.saveHost(ctx, object, probe, agent, agentErr); err != nil {
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

func (r *LoadBalancerHostReconciler) saveHost(ctx context.Context, object *networkv1alpha1.LoadBalancerHost,
	probe error, agent loadbalancer.AgentStatus, agentErr error) error {
	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	reachable := condition(dataPlaneReachableType, true, "Reachable", "", object.Generation)
	if probe != nil {
		reachable = condition(dataPlaneReachableType, false, "Unreachable", probe.Error(), object.Generation)
	}
	meta.SetStatusCondition(&status.Conditions, reachable)
	status.VIPs = nil
	if agentErr != nil {
		meta.SetStatusCondition(&status.Conditions, condition(agentReadyType, false, agentUnreachable, agentErr.Error(), object.Generation))
	} else {
		ready, reason := agent.Ready(r.now(), r.AgentStaleAfter)
		agentReady := condition(agentReadyType, true, "Ready", "", object.Generation)
		if !ready {
			agentReady = condition(agentReadyType, false, "NotReady", reason, object.Generation)
		}
		meta.SetStatusCondition(&status.Conditions, agentReady)
		for _, vip := range agent.VIPs {
			status.VIPs = append(status.VIPs, networkv1alpha1.AgentVIPStatus{
				Address: vip.Address.String(), Announced: vip.Announced, Reason: string(vip.Reason),
			})
		}
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

// reportOf 把 status 中 agent 的报告还原为领域事实；agent 不应答或尚未观察时 Reported 为 false。
func reportOf(object *networkv1alpha1.LoadBalancerHost) loadbalancer.AgentReport {
	agentReady := meta.FindStatusCondition(object.Status.Conditions, agentReadyType)
	if agentReady == nil || agentReady.Reason == agentUnreachable {
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
	return loadbalancer.Host{Name: object.Name, ProxyURL: object.Spec.DataPlaneURL}
}

func (r *LoadBalancerHostReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("loadbalancerhost").
		For(&networkv1alpha1.LoadBalancerHost{}).
		Complete(r)
}
