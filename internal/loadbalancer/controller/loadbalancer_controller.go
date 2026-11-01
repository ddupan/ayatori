// Package controller 是 LoadBalancer 的 Kubernetes adapter：直接读写 CR 与 IPAddress，
// 规则由 domain 包判断。
package controller

import (
	"context"
	"net/netip"
	"slices"
	"strings"
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
	degradedType       = "Degraded"
	hostnameType       = "HostnamePublished"
	// pendingRetry 让池耗尽或冲突的对象在他人释放地址后重新尝试；释放没有指向它们的事件。
	pendingRetry = 30 * time.Second
	claimRetry   = time.Second
	// hostRetry 跟进 reload 与 frontend 状态等主机侧进度。
	hostRetry       = 5 * time.Second
	observeInterval = 30 * time.Second
	hostTimeout     = 10 * time.Second
)

// ProxyBackend 读写 LB 主机代理上属于本 LoadBalancer 的配置，只返回事实。
type ProxyBackend interface {
	Observe(context.Context, loadbalancer.Host, *loadbalancer.LoadBalancer) (loadbalancer.ProxyFacts, error)
	Apply(context.Context, loadbalancer.Host, *loadbalancer.LoadBalancer) error
	Remove(context.Context, loadbalancer.Host, *loadbalancer.LoadBalancer) error
}

// DNSBackend 维护 Ayatori 独占 zone 中的 A 记录。
type DNSBackend interface {
	Record(context.Context, string) ([]netip.Addr, error)
	Publish(context.Context, string, netip.Addr) error
	Remove(context.Context, string) error
}

// LoadBalancerReconciler 认领与释放 VIP，逐主机收敛代理配置，并写 LoadBalancer status。
// 路由由各主机的 LB agent 按同一宣告规则自行宣告（ADR-0014）；主机是 LoadBalancerHost 资源
// （ADR-0015）。Proxy 为空时只分配 VIP；DNS 为空时不维护 Hostname。
type LoadBalancerReconciler struct {
	Client  client.Client
	Pool    address.Pool
	Proxy   ProxyBackend
	DNS     DNSBackend
	DNSZone string
}

// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ipaddresses,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts,verbs=get;list;watch

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
		// 先在全部主机删除配置（agent 随后撤回），再删除 DNS 记录，最后释放 VIP。
		if err := r.converge(ctx, lb, true); err != nil {
			return ctrl.Result{}, err
		}
		if !lb.HostsReleased() {
			return ctrl.Result{RequeueAfter: hostRetry}, r.save(ctx, object, lb)
		}
		r.record(ctx, lb, true)
		if !lb.RecordReleased() {
			return ctrl.Result{RequeueAfter: hostRetry}, r.save(ctx, object, lb)
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
	if err := r.converge(ctx, lb, false); err != nil {
		return ctrl.Result{}, err
	}
	r.record(ctx, lb, false)
	if err := r.save(ctx, object, lb); err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case object.Status.Address == "":
		return ctrl.Result{RequeueAfter: pendingRetry}, nil
	case len(lb.Degraded()) > 0:
		return ctrl.Result{RequeueAfter: hostRetry}, nil
	default:
		// 主机侧没有 watch；周期观察以发现配置漂移与 frontend 状态变化。
		return ctrl.Result{RequeueAfter: observeInterval}, nil
	}
}

// converge 在每台主机上至多执行一次写入；主机之间相互独立，失败只影响该主机的 Attachment。
func (r *LoadBalancerReconciler) converge(ctx context.Context, lb *loadbalancer.LoadBalancer, deleting bool) error {
	if r.Proxy == nil {
		return nil
	}
	hosts, err := r.hosts(ctx)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		hostCtx, cancel := context.WithTimeout(ctx, hostTimeout)
		facts, err := r.Proxy.Observe(hostCtx, host, lb)
		var write error
		switch lb.PlanHost(host.Name, facts, err, deleting) {
		case loadbalancer.ApplyConfig:
			write = r.Proxy.Apply(hostCtx, host, lb)
		case loadbalancer.RemoveConfig:
			write = r.Proxy.Remove(hostCtx, host, lb)
		}
		cancel()
		if write != nil {
			lb.HostFailed(host.Name, write)
		}
	}
	return nil
}

// hosts 返回参与落地的主机，按名称排序。只用已带 finalizer 且未在删除的主机：finalizer 保证
// 写入的配置在主机删除时被清理；删除中的主机由 LoadBalancerHost controller 整机清理。
func (r *LoadBalancerReconciler) hosts(ctx context.Context) ([]loadbalancer.Host, error) {
	list := &networkv1alpha1.LoadBalancerHostList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil, err
	}
	hosts := make([]loadbalancer.Host, 0, len(list.Items))
	for i := range list.Items {
		object := &list.Items[i]
		if object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, LoadBalancerHostFinalizer) {
			hosts = append(hosts, hostOf(object))
		}
	}
	slices.SortFunc(hosts, func(a, b loadbalancer.Host) int { return strings.Compare(a.Name, b.Name) })
	return hosts, nil
}

// record 观察并至多写入一次 Hostname 的 A 记录。
func (r *LoadBalancerReconciler) record(ctx context.Context, lb *loadbalancer.LoadBalancer, deleting bool) {
	if r.DNS == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	name := lb.Hostname(r.DNSZone)
	record, err := r.DNS.Record(ctx, name)
	switch lb.PlanRecord(record, err, deleting) {
	case loadbalancer.PublishRecord:
		lb.RecordPublished(r.DNS.Publish(ctx, name, lb.Snapshot().Address))
	case loadbalancer.RemoveRecord:
		// 删除结果由下一轮回读确认后才放行 VIP 释放。
		_ = r.DNS.Remove(ctx, name)
	}
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
	status.Hostname = ""
	observed, published, recordMessage := lb.Published()
	if published {
		status.Hostname = lb.Hostname(r.DNSZone)
	}
	if observed {
		meta.SetStatusCondition(&status.Conditions, condition(hostnameType, published, hostnameReason(published), recordMessage, object.Generation))
	}
	status.Hosts = nil
	for _, attachment := range lb.Attachments() {
		status.Hosts = append(status.Hosts, networkv1alpha1.HostStatus{
			Name: attachment.Host, Phase: string(attachment.Phase), Message: attachment.Message,
		})
	}
	programmed, reason, message := lb.Programmed()
	meta.SetStatusCondition(&status.Conditions, condition(programmedType, programmed, reason, message, object.Generation))
	degraded := lb.Degraded()
	if len(degraded) > 0 {
		meta.SetStatusCondition(&status.Conditions, condition(degradedType, true, loadbalancer.ReasonHostsDegraded,
			"未就绪的主机："+strings.Join(degraded, ", "), object.Generation))
	} else {
		meta.SetStatusCondition(&status.Conditions, condition(degradedType, false, loadbalancer.ReasonAllHostsReady, "", object.Generation))
	}
	if equality.Semantic.DeepEqual(&object.Status, status) {
		return nil
	}
	object.Status = *status
	return r.Client.Status().Update(ctx, object)
}

func hostnameReason(published bool) string {
	if published {
		return "Published"
	}
	return "Pending"
}

func condition(kind string, value bool, reason, message string, generation int64) metav1.Condition {
	status := metav1.ConditionFalse
	if value {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: generation}
}

func snapshot(object *networkv1alpha1.LoadBalancer) loadbalancer.Snapshot {
	// 地址格式由 CRD 校验；解析失败即为零值。
	requested, _ := netip.ParseAddr(object.Spec.Address)
	recorded, _ := netip.ParseAddr(object.Status.Address)
	return loadbalancer.Snapshot{
		Owner:     address.Owner{Namespace: object.Namespace, Name: object.Name, UID: string(object.UID)},
		Requested: requested,
		Address:   recorded,
		Ports:     ports(object.Spec.Ports),
	}
}

func ports(specs []networkv1alpha1.LoadBalancerPort) []loadbalancer.Port {
	result := make([]loadbalancer.Port, 0, len(specs))
	for _, spec := range specs {
		check := spec.HealthCheck
		port := loadbalancer.Port{
			Port:          spec.Port,
			ProxyProtocol: loadbalancer.ProxyProtocol(spec.ProxyProtocol),
			IdleTimeout:   spec.IdleTimeout.Duration,
			DrainTimeout:  spec.DrainTimeout.Duration,
			HealthCheck: loadbalancer.HealthCheck{
				Path: check.Path, ExpectedStatuses: check.ExpectedStatuses,
				Interval: check.Interval.Duration, Timeout: check.Timeout.Duration,
				HealthyThreshold: check.HealthyThreshold, UnhealthyThreshold: check.UnhealthyThreshold,
			},
		}
		if check.Port != nil {
			port.HealthCheck.Port = *check.Port
		}
		for _, endpoint := range spec.Endpoints {
			port.Endpoints = append(port.Endpoints, loadbalancer.Endpoint{Address: endpoint.Address, Port: endpoint.Port})
		}
		result = append(result, port)
	}
	return result
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
		// 主机增删让全部 LoadBalancer 重新落地，无需重启 controller。
		Watches(&networkv1alpha1.LoadBalancerHost{}, handler.EnqueueRequestsFromMapFunc(r.everyLoadBalancer)).
		Complete(r)
}

func (r *LoadBalancerReconciler) everyLoadBalancer(ctx context.Context, _ client.Object) []ctrl.Request {
	list := &networkv1alpha1.LoadBalancerList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

func parentOf(_ context.Context, object client.Object) []ctrl.Request {
	owner, ok := ownerOf(object.(*networkingv1.IPAddress))
	if !ok {
		return nil
	}
	return []ctrl.Request{{Namespace: owner.Namespace, Name: owner.Name}}
}
