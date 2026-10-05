// Package controller 是 LoadBalancer 的 Kubernetes adapter：直接读写 CR 与 IPAddress，
// 规则由 domain 包判断。
package controller

import (
	"context"
	"net/netip"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	LoadBalancerFinalizer = "network.ayatori.ddupan.top/loadbalancer"
	// managedBy 不能是 ipallocator.k8s.io，否则 apiserver 的 repair loop 会回收这些对象。
	managedByLabel     = "ipaddress.kubernetes.io/managed-by"
	managedBy          = "loadbalancer.network.ayatori.ddupan.top"
	ownerUIDAnnotation = "network.ayatori.ddupan.top/loadbalancer-uid"
	programmedType     = "Programmed"
	// pendingRetry 让池耗尽或冲突的对象在他人释放地址后重新尝试；释放没有指向它们的事件。
	pendingRetry = 30 * time.Second
	claimRetry   = time.Second
)

// LoadBalancerReconciler 认领与释放 VIP，并写 LoadBalancer status。
type LoadBalancerReconciler struct {
	Client client.Client
	Pool   address.Pool
}

// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ipaddresses,verbs=get;list;watch;create;delete

func (r *LoadBalancerReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &networkv1alpha1.LoadBalancer{}
	if err := r.Client.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	taken, uids, err := r.addresses(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	lb := loadbalancer.Reconstitute(snapshot(object))

	if !object.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(object, LoadBalancerFinalizer) {
			return ctrl.Result{}, nil
		}
		owned, action := lb.PlanRelease(taken)
		if action == loadbalancer.Finalize {
			controllerutil.RemoveFinalizer(object, LoadBalancerFinalizer)
			return ctrl.Result{}, client.IgnoreNotFound(r.Client.Update(ctx, object))
		}
		for _, addr := range owned {
			// 以 UID 为前提删除，不会误删他人刚以同一地址创建的新对象。
			ip := &networkingv1.IPAddress{Name: addr.String()}
			uid := uids[addr]
			if err := r.Client.Delete(ctx, ip, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		// IPAddress 删除事件会触发下一轮放行 finalizer。
		return ctrl.Result{}, nil
	}

	// 先持久化 finalizer，再创建外部占用。
	if controllerutil.AddFinalizer(object, LoadBalancerFinalizer) {
		return ctrl.Result{}, r.Client.Update(ctx, object)
	}

	addr, action := lb.PlanAddress(r.Pool, taken)
	if action == loadbalancer.Claim {
		err := r.Client.Create(ctx, ipAddress(addr, object))
		if apierrors.IsAlreadyExists(err) {
			// cache 尚未看到他人或自己上一轮的占用；等待重新观察，不在本轮猜测归属。
			return ctrl.Result{RequeueAfter: claimRetry}, nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		lb.AddressClaimed(addr)
	}
	if err := r.save(ctx, object, lb); err != nil {
		return ctrl.Result{}, err
	}
	if object.Status.Address == "" {
		return ctrl.Result{RequeueAfter: pendingRetry}, nil
	}
	return ctrl.Result{}, nil
}

// addresses 返回全部已占用地址。非本 controller 管理的对象（如 ClusterIP）视为他人占用，
// 即使地址池误与 Service CIDR 重叠也只会跳过。
func (r *LoadBalancerReconciler) addresses(ctx context.Context) (map[netip.Addr]address.Owner, map[netip.Addr]types.UID, error) {
	list := &networkingv1.IPAddressList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil, nil, err
	}
	taken := make(map[netip.Addr]address.Owner, len(list.Items))
	uids := make(map[netip.Addr]types.UID, len(list.Items))
	for i := range list.Items {
		addr, err := netip.ParseAddr(list.Items[i].Name)
		if err != nil {
			continue
		}
		owner, _ := ownerOf(&list.Items[i])
		taken[addr], uids[addr] = owner, list.Items[i].UID
	}
	return taken, uids, nil
}

func (r *LoadBalancerReconciler) save(ctx context.Context, object *networkv1alpha1.LoadBalancer, lb *loadbalancer.LoadBalancer) error {
	status := object.Status.DeepCopy()
	status.ObservedGeneration = object.Generation
	if addr := lb.Snapshot().Address; addr.IsValid() {
		status.Address = addr.String()
	}
	reason, message := lb.Programmed()
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: programmedType, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		ObservedGeneration: object.Generation,
	})
	if equality.Semantic.DeepEqual(&object.Status, status) {
		return nil
	}
	object.Status = *status
	return r.Client.Status().Update(ctx, object)
}

func snapshot(object *networkv1alpha1.LoadBalancer) loadbalancer.Snapshot {
	// 地址格式由 CRD 校验；解析失败即为零值。
	requested, _ := netip.ParseAddr(object.Spec.Address)
	recorded, _ := netip.ParseAddr(object.Status.Address)
	return loadbalancer.Snapshot{
		Owner:     address.Owner{Namespace: object.Namespace, Name: object.Name, UID: string(object.UID)},
		Requested: requested,
		Address:   recorded,
	}
}

func ipAddress(addr netip.Addr, object *networkv1alpha1.LoadBalancer) *networkingv1.IPAddress {
	return &networkingv1.IPAddress{
		Name:        addr.String(),
		Labels:      map[string]string{managedByLabel: managedBy},
		Annotations: map[string]string{ownerUIDAnnotation: string(object.UID)},
		Spec: networkingv1.IPAddressSpec{ParentRef: &networkingv1.ParentReference{
			Group: networkv1alpha1.GroupVersion.Group, Resource: "loadbalancers",
			Namespace: object.Namespace, Name: object.Name,
		}},
	}
}

// ownerOf 返回受管 IPAddress 记录的 LoadBalancer；非受管对象返回零值与 false。
func ownerOf(ip *networkingv1.IPAddress) (address.Owner, bool) {
	ref := ip.Spec.ParentRef
	if ip.Labels[managedByLabel] != managedBy || ref == nil || ref.Group != networkv1alpha1.GroupVersion.Group {
		return address.Owner{}, false
	}
	return address.Owner{Namespace: ref.Namespace, Name: ref.Name, UID: ip.Annotations[ownerUIDAnnotation]}, true
}

func (r *LoadBalancerReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("loadbalancer").
		For(&networkv1alpha1.LoadBalancer{}).
		Watches(&networkingv1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(parentOf)).
		Complete(r)
}

func parentOf(_ context.Context, object client.Object) []ctrl.Request {
	owner, ok := ownerOf(object.(*networkingv1.IPAddress))
	if !ok {
		return nil
	}
	return []ctrl.Request{{Namespace: owner.Namespace, Name: owner.Name}}
}
