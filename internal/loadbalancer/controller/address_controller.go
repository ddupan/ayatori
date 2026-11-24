package controller

import (
	"context"
	"time"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
	"go.ddupan.top/ayatori/internal/loadbalancer/domain/address"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// AddressReconciler 回收 LoadBalancer 已不存在（如 finalizer 被人工移除）的受管 IPAddress。
// 正常删除由 LoadBalancerReconciler 在 finalizer 中释放。
type AddressReconciler struct {
	Client client.Client
	// Now 只供测试替换。
	Now func() time.Time
}

func (r *AddressReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	ip := &networkingv1.IPAddress{}
	if err := r.Client.Get(ctx, request.NamespacedName, ip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owner, ok := ownerOf(ip)
	if !ok || !ip.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	parentUID := ""
	parent := &networkv1alpha1.LoadBalancer{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, parent)
	switch {
	case err == nil:
		parentUID = string(parent.UID)
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	orphan, wait := address.Orphaned(owner, parentUID, r.Now().Sub(ip.CreationTimestamp.Time))
	if !orphan {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Client.Delete(ctx, ip, client.Preconditions{UID: &ip.UID, ResourceVersion: &ip.ResourceVersion}))
}

func (r *AddressReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	managed := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[managedByLabel] == managedBy
	})
	return ctrl.NewControllerManagedBy(manager).
		Named("loadbalancer-address").
		For(&networkingv1.IPAddress{}, builder.WithPredicates(managed)).
		Complete(r)
}
