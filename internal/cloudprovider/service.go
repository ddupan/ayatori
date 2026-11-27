// Package cloudprovider 运行在 workload 集群中，把消费方的对象翻译为 Ayatori 资源（ADR-0028）。
// 它以消费方的身份调用 Ayatori API；Ayatori 不访问 workload 集群（ADR-0012）。
package cloudprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
)

const (
	// LoadBalancerClass 是 Service 选用 Ayatori LoadBalancer 的 spec.loadBalancerClass；未设置的 Service
	// 仍由集群原有的实现（如 k3s servicelb）处理，按 Service 逐个迁移。
	LoadBalancerClass = "ayatori.ddupan.top/loadbalancer"

	finalizer  = "ayatori.ddupan.top/loadbalancer-cleanup"
	fieldOwner = client.FieldOwner("ayatori-cloud-provider")

	labelManagedBy        = "app.kubernetes.io/managed-by"
	managedBy             = "ayatori-cloud-provider"
	labelServiceNamespace = "cloud.ayatori.ddupan.top/service-namespace"
	labelServiceName      = "cloud.ayatori.ddupan.top/service-name"

	// 与上游 service controller 相同：带此 label 的节点不作为后端。
	labelExcludeNode = "node.kubernetes.io/exclude-from-external-load-balancers"

	programmedType = "Programmed"
	// serviceConditionType 是同步到 Service status 的 LoadBalancer Programmed。
	serviceConditionType = "ayatori.ddupan.top/LoadBalancerProgrammed"
)

// ServiceReconciler 为 workload 集群中选用 LoadBalancerClass 的 Service 维护 Ayatori 中的
// LoadBalancer 与 Backend。Namespace 是 Ayatori 中代表这个集群的 namespace。
type ServiceReconciler struct {
	Client    client.Client
	Ayatori   client.Client
	Namespace string
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	service := &corev1.Service{}
	if err := r.Client.Get(ctx, request.NamespacedName, service); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !wanted(service) {
		return ctrl.Result{}, r.cleanup(ctx, service)
	}
	if controllerutil.AddFinalizer(service, finalizer) {
		if err := r.Client.Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := supported(service); err != nil {
		// spec 改动会重新触发；重试没有意义。
		logf.FromContext(ctx).Info("Service not supported", "reason", err.Error())
		return ctrl.Result{}, nil
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	name := loadBalancerName(service)
	lb := &networkv1alpha1.LoadBalancer{}
	if err := r.owned(ctx, name, service, lb); err != nil {
		return ctrl.Result{}, err
	}
	backends := desiredBackends(service, name, nodeAddresses(nodes.Items))
	for i := range backends {
		if err := r.owned(ctx, backends[i].Name, service, &networkv1alpha1.Backend{}); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.apply(ctx, &backends[i]); err != nil {
			return ctrl.Result{}, err
		}
	}
	desired := desiredLoadBalancer(service, name, backends)
	if lb.ResourceVersion != "" {
		// address 不可变：已存在时沿用，loadBalancerIP 改动不生效。
		desired.Spec.Address = lb.Spec.Address
	}
	if err := r.apply(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}
	// 端口减少后不再被引用的 Backend；先更新 LoadBalancer 再删，避免悬空引用。
	if err := r.deleteBackends(ctx, service, backends); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Ayatori.Get(ctx, client.ObjectKeyFromObject(desired), lb); err != nil {
		return ctrl.Result{}, err
	}
	// VIP 分配与数据面落地是异步的：LoadBalancer 的 watch 会在状态变化时再次触发。
	programmed := meta.FindStatusCondition(lb.Status.Conditions, programmedType)
	if programmed == nil {
		return ctrl.Result{}, nil
	}
	// 同步到 Service，地址不在池内、已被占用等拒绝原因在 Service 上可见。
	status := corev1ac.ServiceStatus().WithConditions(metav1ac.Condition().
		WithType(serviceConditionType).WithStatus(programmed.Status).WithReason(programmed.Reason).
		WithMessage(programmed.Message).WithLastTransitionTime(programmed.LastTransitionTime))
	// 首次 Programmed 后公布 ingress；address 一经记录不变，之后主机降级也不撤回。
	if lb.Status.Address != "" && (programmed.Status == metav1.ConditionTrue || len(service.Status.LoadBalancer.Ingress) > 0) {
		ingress := corev1ac.LoadBalancerIngress().WithIP(lb.Status.Address)
		if lb.Status.Hostname != "" {
			ingress.WithHostname(lb.Status.Hostname)
		}
		status.WithLoadBalancer(corev1ac.LoadBalancerStatus().WithIngress(ingress))
	}
	apply := corev1ac.Service(service.Name, service.Namespace).WithStatus(status)
	return ctrl.Result{}, r.Client.Status().Apply(ctx, apply, fieldOwner, client.ForceOwnership)
}

// cleanup 删除 Service 对应的 Ayatori 对象。LoadBalancer 有自己的 finalizer（DNS 记录、VIP 释放），
// 等它真正消失后才移除 Service 的 finalizer，重建同名 Service 时可以重新申请同一 VIP。
func (r *ServiceReconciler) cleanup(ctx context.Context, service *corev1.Service) error {
	if !controllerutil.ContainsFinalizer(service, finalizer) {
		return nil
	}
	lb := &networkv1alpha1.LoadBalancer{}
	err := r.owned(ctx, loadBalancerName(service), service, lb)
	switch {
	case errors.Is(err, errNotOwned):
		// 同名 LB 不是这个 Service 的（声明的对象或撞名）：不碰它，只清理自己的 Backend。
	case err != nil:
		return err
	case lb.ResourceVersion != "":
		if lb.DeletionTimestamp.IsZero() {
			if err := r.Ayatori.Delete(ctx, lb); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		// LoadBalancer 的删除事件会再次触发。
		return nil
	}
	if err := r.deleteBackends(ctx, service, nil); err != nil {
		return err
	}
	if service.DeletionTimestamp.IsZero() {
		// type 改离 LoadBalancer：撤掉自己写的 ingress。
		empty := corev1ac.Service(service.Name, service.Namespace).WithStatus(corev1ac.ServiceStatus())
		if err := r.Client.Status().Apply(ctx, empty, fieldOwner, client.ForceOwnership); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(service, finalizer)
	return r.Client.Update(ctx, service)
}

var errNotOwned = errors.New("exists but is not managed for this Service")

// owned 读取 Ayatori 中名为 name 的对象；不存在时 object 保持为空。同名对象不属于这个 Service 时
// 拒绝：它可能是声明的对象，或名字截断后与另一个 Service 撞名。
func (r *ServiceReconciler) owned(ctx context.Context, name string, service *corev1.Service, object client.Object) error {
	err := r.Ayatori.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, object)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	labels := object.GetLabels()
	if labels[labelManagedBy] != managedBy || labels[labelServiceNamespace] != service.Namespace || labels[labelServiceName] != service.Name {
		return fmt.Errorf("%s/%s for Service %s/%s: %w", r.Namespace, name, service.Namespace, service.Name, errNotOwned)
	}
	return nil
}

func (r *ServiceReconciler) deleteBackends(ctx context.Context, service *corev1.Service, keep []networkv1alpha1.Backend) error {
	list := &networkv1alpha1.BackendList{}
	if err := r.Ayatori.List(ctx, list, client.InNamespace(r.Namespace), client.MatchingLabels(sourceLabels(service))); err != nil {
		return err
	}
	for i := range list.Items {
		if slices.ContainsFunc(keep, func(b networkv1alpha1.Backend) bool { return b.Name == list.Items[i].Name }) {
			continue
		}
		if err := r.Ayatori.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// apply 以 SSA 写入：只拥有自己设置的字段，apiserver 填的默认值不会让每轮都产生写入。
func (r *ServiceReconciler) apply(ctx context.Context, object client.Object) error {
	object.SetNamespace(r.Namespace)
	data, err := json.Marshal(object)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(data, &u.Object); err != nil {
		return err
	}
	return r.Ayatori.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), fieldOwner, client.ForceOwnership)
}

func wanted(service *corev1.Service) bool {
	return service.DeletionTimestamp.IsZero() && service.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		service.Spec.LoadBalancerClass != nil && *service.Spec.LoadBalancerClass == LoadBalancerClass
}

// supported 检查 Ayatori LoadBalancer 能否表达这个 Service：首版只有 TCP，后端是节点的 nodePort。
func supported(service *corev1.Service) error {
	for _, port := range service.Spec.Ports {
		if port.Protocol != corev1.ProtocolTCP {
			return fmt.Errorf("port %d: only TCP is supported", port.Port)
		}
		if port.NodePort == 0 {
			return fmt.Errorf("port %d: no nodePort (allocateLoadBalancerNodePorts must not be false)", port.Port)
		}
	}
	return nil
}

// loadBalancerName 由 Service 的 namespace/name 确定性推导（ADR-0014），重建后 Hostname 不变。
// LoadBalancer 名是 DNS label，超长时截断并加 hash。
func loadBalancerName(service *corev1.Service) string {
	name := service.Namespace + "-" + service.Name
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(service.Namespace + "/" + service.Name))
	return strings.TrimRight(name[:54], "-") + "-" + hex.EncodeToString(sum[:4])
}

func sourceLabels(service *corev1.Service) map[string]string {
	return map[string]string{
		labelManagedBy:        managedBy,
		labelServiceNamespace: service.Namespace,
		labelServiceName:      service.Name,
	}
}

// nodeAddresses 返回作为后端的节点 InternalIP，按地址排序。不看 Ready：与上游 service controller
// 一致，节点能否接流量交给健康检查，避免节点状态抖动时反复改写 Backend。
func nodeAddresses(nodes []corev1.Node) []string {
	var addresses []string
	for i := range nodes {
		node := &nodes[i]
		if _, excluded := node.Labels[labelExcludeNode]; excluded || !node.DeletionTimestamp.IsZero() {
			continue
		}
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP && !strings.Contains(address.Address, ":") {
				addresses = append(addresses, address.Address)
				break
			}
		}
	}
	slices.Sort(addresses)
	return addresses
}

// desiredBackends 为每个 Service 端口生成一个 Backend：各端口的 nodePort 不同，而 LoadBalancer 的
// 一个端口只引用一个 Backend。
func desiredBackends(service *corev1.Service, name string, addresses []string) []networkv1alpha1.Backend {
	backends := make([]networkv1alpha1.Backend, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		backend := networkv1alpha1.Backend{}
		backend.SetGroupVersionKind(networkv1alpha1.GroupVersion.WithKind("Backend"))
		backend.Name = name + "-" + strconv.Itoa(int(port.Port))
		backend.Labels = sourceLabels(service)
		for _, address := range addresses {
			backend.Spec.Endpoints = append(backend.Spec.Endpoints, networkv1alpha1.Endpoint{Address: address, Port: port.NodePort})
		}
		// Local 时只有本机有 endpoint 的节点该接流量：kube-proxy 在 healthCheckNodePort 上报告。
		if service.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal && service.Spec.HealthCheckNodePort != 0 {
			port := service.Spec.HealthCheckNodePort
			backend.Spec.HealthCheck = networkv1alpha1.HealthCheck{Type: "HTTP", Port: &port, Path: "/healthz"}
		}
		backends = append(backends, backend)
	}
	return backends
}

func desiredLoadBalancer(service *corev1.Service, name string, backends []networkv1alpha1.Backend) *networkv1alpha1.LoadBalancer {
	lb := &networkv1alpha1.LoadBalancer{}
	lb.SetGroupVersionKind(networkv1alpha1.GroupVersion.WithKind("LoadBalancer"))
	lb.Name = name
	lb.Labels = sourceLabels(service)
	lb.Spec.Address = service.Spec.LoadBalancerIP
	for i, port := range service.Spec.Ports {
		lb.Spec.Ports = append(lb.Spec.Ports, networkv1alpha1.LoadBalancerPort{
			Name: port.Name, Port: port.Port, BackendRef: networkv1alpha1.BackendRef{Name: backends[i].Name},
		})
	}
	return lb
}

// SetupWithManager 监听本集群的 Service 与 Node，以及 Ayatori 中自己创建的 LoadBalancer
// （VIP 分配、落地进度、被他人修改或删除）。
func (r *ServiceReconciler) SetupWithManager(manager ctrl.Manager, ayatori cluster.Cluster) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("service").
		For(&corev1.Service{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.everyService), builder.WithPredicates(nodeChanged)).
		WatchesRawSource(source.Kind(ayatori.GetCache(), &networkv1alpha1.LoadBalancer{},
			handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, lb *networkv1alpha1.LoadBalancer) []ctrl.Request {
				labels := lb.Labels
				if labels[labelManagedBy] != managedBy {
					return nil
				}
				return []ctrl.Request{{Namespace: labels[labelServiceNamespace], Name: labels[labelServiceName]}}
			}))).
		Complete(r)
}

func (r *ServiceReconciler) everyService(ctx context.Context, _ client.Object) []ctrl.Request {
	list := &corev1.ServiceList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for i := range list.Items {
		if wanted(&list.Items[i]) {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return requests
}

// nodeChanged 只放行影响后端集合的变化，忽略心跳等 status 更新。
var nodeChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		old, current := e.ObjectOld.(*corev1.Node), e.ObjectNew.(*corev1.Node)
		_, oldExcluded := old.Labels[labelExcludeNode]
		_, excluded := current.Labels[labelExcludeNode]
		return oldExcluded != excluded || old.DeletionTimestamp.IsZero() != current.DeletionTimestamp.IsZero() ||
			!slices.Equal(nodeAddresses([]corev1.Node{*old}), nodeAddresses([]corev1.Node{*current}))
	},
}
