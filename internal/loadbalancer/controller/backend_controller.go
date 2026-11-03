package controller

import (
	"cmp"
	"context"
	"slices"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	readyType = "Ready"
	// Backend Ready condition 的 Reason。
	reasonEndpointReady    = "EndpointReady"
	reasonNoReadyEndpoints = "NoReadyEndpoints"
	reasonUnreferenced     = "Unreferenced"
)

// BackendReconciler 是 Backend status 的唯一写入方：从引用它的 LoadBalancer 的 status.ports 汇总
// 各主机的观察（ADR-0023）。观察本身由 LoadBalancer controller 在收敛时取得。
type BackendReconciler struct {
	Client client.Client
}

// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=backends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers,verbs=get;list;watch

func (r *BackendReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &networkv1alpha1.Backend{}
	if err := r.Client.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	list := &networkv1alpha1.LoadBalancerList{}
	if err := r.Client.List(ctx, list, client.InNamespace(object.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	status := object.Status.DeepCopy()
	var referenced bool
	status.Observations, referenced = observations(object, list.Items)
	meta.SetStatusCondition(&status.Conditions, backendReady(status.Observations, referenced, object.Generation))
	if equality.Semantic.DeepEqual(&object.Status, status) {
		return ctrl.Result{}, nil
	}
	object.Status = *status
	return ctrl.Result{}, r.Client.Status().Update(ctx, object)
}

// observations 从引用本 Backend 的各 LoadBalancer 端口汇总当前成员的观察；第二个返回值表示是否有
// LoadBalancer 端口引用它。LoadBalancer status 尚未跟上成员变化时，只呈现当前成员。
func observations(object *networkv1alpha1.Backend, lbs []networkv1alpha1.LoadBalancer) ([]networkv1alpha1.BackendObservation, bool) {
	var referenced bool
	var result []networkv1alpha1.BackendObservation
	for i := range lbs {
		lb := &lbs[i]
		for _, port := range lb.Spec.Ports {
			if port.BackendRef.Name != object.Name {
				continue
			}
			referenced = true
			for _, observed := range lb.Status.Endpoints {
				if observed.Port != port.Port || !slices.Contains(object.Spec.Endpoints,
					networkv1alpha1.Endpoint{Address: observed.Address, Port: observed.EndpointPort}) {
					continue
				}
				result = append(result, networkv1alpha1.BackendObservation{
					Address: observed.Address, Port: observed.EndpointPort, LoadBalancer: lb.Name, LoadBalancerPort: port.Port,
					EndpointObservation: observed.EndpointObservation,
				})
			}
		}
	}
	slices.SortFunc(result, func(a, b networkv1alpha1.BackendObservation) int {
		return cmp.Or(cmp.Compare(a.Address, b.Address), cmp.Compare(a.Port, b.Port), cmp.Compare(a.LoadBalancer, b.LoadBalancer),
			cmp.Compare(a.LoadBalancerPort, b.LoadBalancerPort), cmp.Compare(a.Host, b.Host))
	})
	return result, referenced
}

func backendReady(observations []networkv1alpha1.BackendObservation, referenced bool, generation int64) metav1.Condition {
	switch {
	case !referenced:
		return condition(readyType, false, reasonUnreferenced, "没有 LoadBalancer 端口引用", generation)
	case slices.ContainsFunc(observations, func(o networkv1alpha1.BackendObservation) bool { return o.State == loadbalancer.StateUp }):
		return condition(readyType, true, reasonEndpointReady, "", generation)
	default:
		return condition(readyType, false, reasonNoReadyEndpoints, "没有成员在任何主机上 UP", generation)
	}
}

func (r *BackendReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("backend").
		For(&networkv1alpha1.Backend{}).
		// LoadBalancer 的 status 或引用变化都可能影响同 namespace 的任意 Backend（包括刚被取消引用的）。
		Watches(&networkv1alpha1.LoadBalancer{}, handler.EnqueueRequestsFromMapFunc(r.inNamespace)).
		Complete(r)
}

func (r *BackendReconciler) inNamespace(ctx context.Context, object client.Object) []ctrl.Request {
	list := &networkv1alpha1.BackendList{}
	if err := r.Client.List(ctx, list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}
