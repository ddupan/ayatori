// Package controller 是 LoadBalancer 的 Kubernetes adapter：直接读写 CR 与 IPAddress，
// 规则由 domain 包判断。
package controller

import (
	"context"
	"errors"
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
	resolvedRefsType   = "ResolvedRefs"
	endpointsReadyType = "EndpointsReady"
	// pendingRetry 让池耗尽或冲突的对象在他人释放地址后重新尝试；释放没有指向它们的事件。
	pendingRetry = 30 * time.Second
	claimRetry   = time.Second
	// hostRetry 跟进 reload 与 frontend 状态等主机侧进度。
	hostRetry       = 5 * time.Second
	observeInterval = 30 * time.Second
	hostTimeout     = 10 * time.Second
)

// DNSBackend 维护 Ayatori 独占 zone 中的 A 记录。
type DNSBackend interface {
	Record(context.Context, string) ([]netip.Addr, error)
	Publish(context.Context, string, netip.Addr) error
	Remove(context.Context, string) error
}

// LoadBalancerReconciler 认领与释放 VIP，汇总各主机的落地结果，并写 LoadBalancer status。
// 路由由各主机的 LB agent 自行宣告（ADR-0017）；主机是 LoadBalancerHost 资源，Attachment 是否就绪
// 以其 status 中 agent 的报告为准（ADR-0018）。代理配置由各主机的 agent 在本机收敛，这里不连接
// 主机，只汇总它们的报告（ADR-0022）。DNS 为空时不维护 Hostname。
type LoadBalancerReconciler struct {
	Client  client.Client
	Pool    address.Pool
	DNS     DNSBackend
	DNSZone string
	// Now 只供测试替换。
	Now func() time.Time
}

// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancers/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ipaddresses,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=loadbalancerhosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=network.ayatori.ddupan.top,resources=backends,verbs=get;list;watch

func (r *LoadBalancerReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &networkv1alpha1.LoadBalancer{}
	if err := r.Client.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	taken, uids, err := r.addresses(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired, err := Desired(ctx, r.Client, object)
	if err != nil {
		return ctrl.Result{}, err
	}
	lb := loadbalancer.Reconstitute(desired)

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
		// 代理配置没有 watch，周期观察以发现漂移；agent 报告的变化经 LoadBalancerHost watch 触发。
		return ctrl.Result{RequeueAfter: observeInterval}, nil
	}
}

// converge 按各主机 agent 的报告判定 Attachment；写入由 agent 在本机执行（ADR-0022），
// 这里只用 PlanHost 判定，不执行它返回的动作。
func (r *LoadBalancerReconciler) converge(ctx context.Context, lb *loadbalancer.LoadBalancer, deleting bool) error {
	hosts, err := r.hosts(ctx)
	if err != nil {
		return err
	}
	for _, target := range hosts {
		facts, err := reported(target, lb)
		lb.PlanHost(target.host.Name, facts, target.agent, err, deleting)
	}
	return nil
}

// hostTarget 是一台参与落地的主机与其 agent 写入 status 的报告。
type hostTarget struct {
	host  loadbalancer.Host
	agent loadbalancer.AgentReport
	// loadBalancers 是 agent 在本机收敛的结果（ADR-0022）。
	loadBalancers []networkv1alpha1.HostLoadBalancerStatus
}

var errAgentNotReporting = errors.New("agent 未报告")

// reported 把主机 agent 对该 LoadBalancer 的报告还原为代理事实（ADR-0022）。agent 失联时无法观察；
// 没有该对象的报告（agent 尚未看到它，或已删除完成）时为不存在。配置与期望的摘要相同才算一致。
func reported(target hostTarget, lb *loadbalancer.LoadBalancer) (loadbalancer.ProxyFacts, error) {
	if !target.agent.Reported {
		return loadbalancer.ProxyFacts{}, errAgentNotReporting
	}
	snapshot := lb.Snapshot()
	i := slices.IndexFunc(target.loadBalancers, func(s networkv1alpha1.HostLoadBalancerStatus) bool {
		return s.Namespace == snapshot.Owner.Namespace && s.Name == snapshot.Owner.Name && s.UID == snapshot.Owner.UID
	})
	if i < 0 {
		return loadbalancer.ProxyFacts{}, nil
	}
	report := target.loadBalancers[i]
	facts := loadbalancer.ProxyFacts{
		Present: report.Present, Matches: report.Digest != "" && report.Digest == snapshot.Digest(),
		Reload: loadbalancer.ParseReload(report.Reload), Message: report.Message,
	}
	for _, s := range report.Servers {
		facts.Servers = append(facts.Servers, loadbalancer.ServerHealth{
			Port: s.Port, Endpoint: loadbalancer.Endpoint{Address: s.Address, Port: s.EndpointPort},
			State: s.State, CheckStatus: s.CheckStatus, CheckCode: s.CheckCode,
		})
	}
	return facts, nil
}

// hosts 返回参与落地的主机，按名称排序。只用已带 finalizer 且未在删除的主机：finalizer 保证
// 写入的配置在主机删除时被清理；删除中的主机由 LoadBalancerHost controller 整机清理。
func (r *LoadBalancerReconciler) hosts(ctx context.Context) ([]hostTarget, error) {
	list := &networkv1alpha1.LoadBalancerHostList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil, err
	}
	hosts := make([]hostTarget, 0, len(list.Items))
	for i := range list.Items {
		object := &list.Items[i]
		if object.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(object, LoadBalancerHostFinalizer) {
			hosts = append(hosts, hostTarget{host: hostOf(object), agent: reportOf(object), loadBalancers: object.Status.LoadBalancers})
		}
	}
	slices.SortFunc(hosts, func(a, b hostTarget) int { return strings.Compare(a.host.Name, b.host.Name) })
	return hosts, nil
}

// Desired 把 LoadBalancer 与同 namespace 的 Backend 翻译为期望配置；只允许同 namespace 引用。
// controller 与在本机收敛的 agent 共用它，两边算出的 Digest 才一致（ADR-0022）。
func Desired(ctx context.Context, c client.Reader, object *networkv1alpha1.LoadBalancer) (loadbalancer.Snapshot, error) {
	list := &networkv1alpha1.BackendList{}
	if err := c.List(ctx, list, client.InNamespace(object.Namespace)); err != nil {
		return loadbalancer.Snapshot{}, err
	}
	backends := make(map[string]*networkv1alpha1.BackendSpec, len(list.Items))
	for i := range list.Items {
		backends[list.Items[i].Name] = &list.Items[i].Spec
	}
	return snapshot(object, backends), nil
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
	resolved, reason, message := lb.ResolvedRefs()
	meta.SetStatusCondition(&status.Conditions, condition(resolvedRefsType, resolved, reason, message, object.Generation))
	programmed, reason, message := lb.Programmed()
	meta.SetStatusCondition(&status.Conditions, condition(programmedType, programmed, reason, message, object.Generation))
	status.Ports, status.Endpoints = endpointStatuses(object, lb.Health(), r.now())
	ready, reason, message := lb.EndpointsReady()
	meta.SetStatusCondition(&status.Conditions, condition(endpointsReadyType, ready, reason, message, object.Generation))
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

func (r *LoadBalancerReconciler) now() metav1.Time {
	if r.Now == nil {
		return metav1.Now()
	}
	return metav1.NewTime(r.Now())
}

// endpointStatuses 把本轮观察写成 status.ports 与平铺的 status.endpoints；state 未变时沿用上次的
// lastTransitionTime，否则每轮都会因时间不同而写入。
func endpointStatuses(object *networkv1alpha1.LoadBalancer, health []loadbalancer.PortHealth, now metav1.Time) ([]networkv1alpha1.PortStatus, []networkv1alpha1.EndpointStatus) {
	names := map[int32]string{}
	for _, port := range object.Spec.Ports {
		names[port.Port] = port.Name
	}
	since := map[networkv1alpha1.EndpointStatus]metav1.Time{}
	for _, previous := range object.Status.Endpoints {
		key := previous
		key.CheckStatus, key.CheckCode, key.LastTransitionTime = "", 0, metav1.Time{}
		since[key] = previous.LastTransitionTime
	}
	ports := make([]networkv1alpha1.PortStatus, 0, len(health))
	var endpoints []networkv1alpha1.EndpointStatus
	for _, port := range health {
		status := networkv1alpha1.PortStatus{Name: names[port.Port], Port: port.Port, Endpoints: int32(len(port.Endpoints))}
		for _, endpoint := range port.Endpoints {
			if endpoint.Ready() {
				status.ReadyEndpoints++
			}
			for _, host := range endpoint.Hosts {
				// 以端口、成员、主机与 state 为键查上次的时间：state 变了即为新的翻转。
				key := networkv1alpha1.EndpointStatus{Port: port.Port, Address: endpoint.Endpoint.Address, EndpointPort: endpoint.Endpoint.Port,
					Host: host.Host, State: host.State}
				at, ok := since[key]
				if !ok {
					at = now
				}
				key.CheckStatus, key.CheckCode, key.LastTransitionTime = host.CheckStatus, host.CheckCode, at
				endpoints = append(endpoints, key)
			}
		}
		ports = append(ports, status)
	}
	return ports, endpoints
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

func snapshot(object *networkv1alpha1.LoadBalancer, backends map[string]*networkv1alpha1.BackendSpec) loadbalancer.Snapshot {
	// 地址格式由 CRD 校验；解析失败即为零值。
	requested, _ := netip.ParseAddr(object.Spec.Address)
	recorded, _ := netip.ParseAddr(object.Status.Address)
	return loadbalancer.Snapshot{
		Owner:     address.Owner{Namespace: object.Namespace, Name: object.Name, UID: string(object.UID)},
		Requested: requested,
		Address:   recorded,
		Ports:     ports(object.Spec.Ports, backends),
	}
}

func ports(specs []networkv1alpha1.LoadBalancerPort, backends map[string]*networkv1alpha1.BackendSpec) []loadbalancer.Port {
	result := make([]loadbalancer.Port, 0, len(specs))
	for _, spec := range specs {
		port := loadbalancer.Port{Port: spec.Port, Backend: spec.BackendRef.Name, IdleTimeout: spec.IdleTimeout.Duration}
		backend, ok := backends[spec.BackendRef.Name]
		if !ok {
			port.MissingBackend = true
			result = append(result, port)
			continue
		}
		check := backend.HealthCheck
		port.ProxyProtocol = loadbalancer.ProxyProtocol(backend.ProxyProtocol)
		port.DrainTimeout = backend.DrainTimeout.Duration
		port.HealthCheck = loadbalancer.HealthCheck{
			Path: check.Path, ExpectedStatuses: check.ExpectedStatuses,
			Interval: check.Interval.Duration, Timeout: check.Timeout.Duration,
			HealthyThreshold: check.HealthyThreshold, UnhealthyThreshold: check.UnhealthyThreshold,
		}
		if check.Port != nil {
			port.HealthCheck.Port = *check.Port
		}
		for _, endpoint := range backend.Endpoints {
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
		// Backend 的增删改（含被引用前先创建、被删除）让引用它的 LoadBalancer 重新收敛。
		Watches(&networkv1alpha1.Backend{}, handler.EnqueueRequestsFromMapFunc(r.referencing)).
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

// referencing 返回同 namespace 中引用该 Backend 的 LoadBalancer。
func (r *LoadBalancerReconciler) referencing(ctx context.Context, backend client.Object) []ctrl.Request {
	list := &networkv1alpha1.LoadBalancerList{}
	if err := r.Client.List(ctx, list, client.InNamespace(backend.GetNamespace())); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for i := range list.Items {
		if slices.ContainsFunc(list.Items[i].Spec.Ports, func(port networkv1alpha1.LoadBalancerPort) bool {
			return port.BackendRef.Name == backend.GetName()
		}) {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
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
