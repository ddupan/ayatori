package controller

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
	"go.ddupan.top/ayatori/internal/loadbalancer/domain/loadbalancer"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// dataPlane 在内存中模拟各主机上在本机收敛的 agent（ADR-0022）：按期望写入本机配置，并像真实 agent
// 一样续约 Lease、把结果写进 LoadBalancerHost 的 status；写入顺序记入 events。真实代理的语义由
// adapter 的 contract test 验证，agent 本身由 agent 包的集成测试验证。
type dataPlane struct {
	configs     map[string]loadbalancer.Snapshot       // host/uid → 已写入的期望
	stopped     map[string]bool                        // host：frontend 未处于 OPEN
	unreachable map[string]bool                        // host：agent 失联，Lease 过期且不再写入
	records     map[string]netip.Addr                  // hostname
	health      map[string][]loadbalancer.ServerHealth // host → 运行中 server 的检查结论
	events      []string
}

func newDataPlane() *dataPlane {
	return &dataPlane{configs: map[string]loadbalancer.Snapshot{}, stopped: map[string]bool{}, unreachable: map[string]bool{}, records: map[string]netip.Addr{}}
}

func (d *dataPlane) lease(t *testing.T, c client.Client, host string, renewed time.Time) {
	t.Helper()
	lease := &coordinationv1.Lease{}
	lease.Namespace, lease.Name = corev1.NamespaceNodeLease, host+"-"+agentRole
	if _, err := controllerutil.CreateOrUpdate(t.Context(), c, lease, func() error {
		lease.Spec.RenewTime = &metav1.MicroTime{Time: renewed}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// agent 扮演 host 上的 agent 运行一轮：主机删除中时清除本机全部配置并报告 Purged；否则（已带
// finalizer 时）按每个已分配 VIP 的 LoadBalancer 的期望写入或删除本机配置，再报告结果。
func (d *dataPlane) agent(t *testing.T, c client.Client, host string) {
	t.Helper()
	if d.unreachable[host] {
		d.lease(t, c, host, time.Now().Add(-time.Hour))
		return
	}
	d.lease(t, c, host, time.Now())
	object := &networkv1alpha1.LoadBalancerHost{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: host}, object); err != nil {
		t.Fatal(err)
	}
	deleting := !object.DeletionTimestamp.IsZero()
	switch {
	case deleting:
		removed := false
		for key := range d.configs {
			if strings.HasPrefix(key, host+"/") {
				delete(d.configs, key)
				removed = true
			}
		}
		if removed {
			d.events = append(d.events, "purge "+host)
		}
	case controllerutil.ContainsFinalizer(object, LoadBalancerHostFinalizer):
		d.converge(t, c, host)
	}
	object.Status.VIPs, object.Status.LoadBalancers = nil, nil
	for key, snapshot := range d.configs {
		if !strings.HasPrefix(key, host+"/") {
			continue
		}
		vip := networkv1alpha1.AgentVIPStatus{Address: snapshot.Address.String(), Announced: !d.stopped[host]}
		if d.stopped[host] {
			vip.Reason = string(loadbalancer.FrontendNotOpen)
		}
		object.Status.VIPs = append(object.Status.VIPs, vip)
		entry := networkv1alpha1.HostLoadBalancerStatus{Namespace: snapshot.Owner.Namespace, Name: snapshot.Owner.Name,
			UID: snapshot.Owner.UID, Present: true, Digest: snapshot.Digest(), Reload: "Succeeded"}
		for _, s := range d.health[host] {
			entry.Servers = append(entry.Servers, networkv1alpha1.HostServerStatus{Port: s.Port, Address: s.Endpoint.Address,
				EndpointPort: s.Endpoint.Port, State: s.State, CheckStatus: s.CheckStatus, CheckCode: s.CheckCode})
		}
		object.Status.LoadBalancers = append(object.Status.LoadBalancers, entry)
	}
	meta.SetStatusCondition(&object.Status.Conditions, condition(agentReadyType, true, "Ready", "", object.Generation))
	if deleting {
		meta.SetStatusCondition(&object.Status.Conditions, condition(PurgedType, true, "Purged", "", object.Generation))
	}
	if err := c.Status().Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

func (d *dataPlane) converge(t *testing.T, c client.Client, host string) {
	t.Helper()
	list := &networkv1alpha1.LoadBalancerList{}
	if err := c.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		lb := &list.Items[i]
		key := host + "/" + string(lb.UID)
		got, present := d.configs[key]
		if !lb.DeletionTimestamp.IsZero() {
			if present {
				delete(d.configs, key)
				d.events = append(d.events, "remove "+host)
			}
			continue
		}
		if lb.Status.Address == "" {
			continue
		}
		want, err := Desired(t.Context(), c, lb)
		if err != nil {
			t.Fatal(err)
		}
		if !present || got.Digest() != want.Digest() {
			d.configs[key] = want
			d.events = append(d.events, "apply "+host)
		}
	}
}

// zone 是同一数据面上的 DNS 记录，事件记入同一序列。
type zone struct{ *dataPlane }

func (d zone) Record(_ context.Context, name string) ([]netip.Addr, error) {
	if addr, ok := d.records[name]; ok {
		return []netip.Addr{addr}, nil
	}
	return nil, nil
}

func (d zone) Publish(_ context.Context, name string, vip netip.Addr) error {
	d.records[name] = vip
	d.events = append(d.events, "publish "+name)
	return nil
}

func (d zone) Remove(_ context.Context, name string) error {
	delete(d.records, name)
	d.events = append(d.events, "unpublish "+name)
	return nil
}

func (d *dataPlane) take() []string {
	events := d.events
	d.events = nil
	return events
}

// createHosts 创建已由 LoadBalancerHost controller 加上 finalizer 的主机。
func createHosts(t *testing.T, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		host := &networkv1alpha1.LoadBalancerHost{}
		host.Name = name
		host.Finalizers = []string{LoadBalancerHostFinalizer}
		create(t, c, host)
	}
}

const (
	applyFirst  = "apply lb1"
	secondHost  = "lb2"
	applySecond = "apply " + secondHost
)

func conditionStatus(t *testing.T, c client.Client, lb *networkv1alpha1.LoadBalancer, kind string) string {
	t.Helper()
	reload(t, c, lb)
	condition := meta.FindStatusCondition(lb.Status.Conditions, kind)
	if condition == nil {
		t.Fatalf("缺少 %s condition", kind)
	}
	return string(condition.Status)
}

func TestLoadBalancerDataPlane(t *testing.T) {
	c := environment(t)
	createHosts(t, c, "lb1", secondHost)
	plane := newDataPlane()
	r := &LoadBalancerReconciler{Client: c, Pool: testPool}
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	converge(t, r, plane, lb)

	if events := plane.take(); !slices.Equal(events, []string{applyFirst, applySecond}) {
		t.Fatalf("got %v", events)
	}
	if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionFalse) {
		t.Fatalf("全部主机就绪后应 Programmed 且不 Degraded: %+v", lb.Status)
	}

	if conditionStatus(t, c, lb, resolvedRefsType) != string(metav1.ConditionFalse) {
		t.Fatalf("Backend 不存在时应报告 ResolvedRefs=False: %+v", lb.Status.Conditions)
	}

	backend := &networkv1alpha1.Backend{Spec: networkv1alpha1.BackendSpec{
		Endpoints: []networkv1alpha1.Endpoint{{Address: "192.168.10.127", Port: 6443}},
	}}
	backend.Namespace, backend.Name = testNamespace, lb.Spec.Ports[0].BackendRef.Name

	t.Run("Backend 出现后重写并解析引用", func(t *testing.T) {
		create(t, c, backend)
		if requests := r.referencing(t.Context(), backend); len(requests) != 1 || requests[0].Name != lb.Name {
			t.Fatalf("Backend 变化应收敛引用它的 LoadBalancer，got %v", requests)
		}
		converge(t, r, plane, lb)
		if events := plane.take(); !slices.Equal(events, []string{applyFirst, applySecond}) {
			t.Fatalf("got %v", events)
		}
		if conditionStatus(t, c, lb, resolvedRefsType) != string(metav1.ConditionTrue) || len(endpointsOf(plane, "lb1", lb)) != 1 {
			t.Fatalf("got %+v", lb.Status.Conditions)
		}
	})

	t.Run("删除 Backend 后端口拒绝连接", func(t *testing.T) {
		if err := c.Delete(t.Context(), backend); err != nil {
			t.Fatal(err)
		}
		converge(t, r, plane, lb)
		if events := plane.take(); !slices.Equal(events, []string{applyFirst, applySecond}) {
			t.Fatalf("got %v", events)
		}
		if conditionStatus(t, c, lb, resolvedRefsType) != string(metav1.ConditionFalse) || len(endpointsOf(plane, "lb1", lb)) != 0 {
			t.Fatalf("应 fail-closed: %+v", lb.Status.Conditions)
		}
	})

	t.Run("frontend 未 OPEN 的主机只降级自身", func(t *testing.T) {
		plane.stopped[secondHost] = true
		converge(t, r, plane, lb)
		if events := plane.take(); len(events) != 0 {
			t.Fatalf("frontend 状态不由 controller 修复，got %v", events)
		}
		if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionTrue) {
			t.Fatalf("got %+v", lb.Status.Conditions)
		}
		if lb.Status.Hosts[1].Phase != string(loadbalancer.AttachmentConfigured) {
			t.Fatalf("got %+v", lb.Status.Hosts)
		}
		delete(plane.stopped, secondHost)
	})

	t.Run("一台主机不可达只影响自身", func(t *testing.T) {
		plane.unreachable[secondHost] = true
		converge(t, r, plane, lb)
		if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionTrue) {
			t.Fatalf("got %+v", lb.Status.Conditions)
		}
		if lb.Status.Hosts[1].Name != secondHost || lb.Status.Hosts[1].Phase != string(loadbalancer.AttachmentUnavailable) {
			t.Fatalf("got %+v", lb.Status.Hosts)
		}
	})

	t.Run("删除时主机不可达则等待", func(t *testing.T) {
		if err := c.Delete(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, plane, lb)
		reload(t, c, lb)
		ipAddressOf(t, c, lb.Status.Address)
		if events := plane.take(); !slices.Equal(events, []string{"remove lb1"}) {
			t.Fatalf("可达主机直接删配置，got %v", events)
		}
	})

	t.Run("全部主机清理后释放 VIP", func(t *testing.T) {
		delete(plane.unreachable, secondHost)
		converge(t, r, plane, lb)
		if events := plane.take(); !slices.Equal(events, []string{"remove " + secondHost}) {
			t.Fatalf("got %v", events)
		}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(lb), lb); !apierrors.IsNotFound(err) {
			t.Fatalf("释放后应放行删除: %v", err)
		}
		if len(plane.configs) != 0 {
			t.Fatalf("数据面应已清空: %v", plane.configs)
		}
	})
}

func endpointsOf(plane *dataPlane, host string, lb *networkv1alpha1.LoadBalancer) []loadbalancer.Endpoint {
	return plane.configs[host+"/"+string(lb.UID)].Ports[0].Endpoints
}

func TestLoadBalancerHostname(t *testing.T) {
	c := environment(t)
	createHosts(t, c, "lb1")
	plane := newDataPlane()
	r := &LoadBalancerReconciler{Client: c, Pool: testPool, DNS: zone{plane}, DNSZone: "lb.lab.ddupan.top"}
	lb := newLoadBalancer("api", "")
	create(t, c, lb)
	converge(t, r, plane, lb)
	const hostname = "api." + testNamespace + ".lb.lab.ddupan.top"
	// 写入由 agent 在 controller 之后执行：记录不等主机落地，VIP 确认后即发布。
	if events := plane.take(); !slices.Equal(events, []string{"publish " + hostname, applyFirst}) {
		t.Fatalf("VIP 确认后即发布记录，got %v", events)
	}
	if conditionStatus(t, c, lb, hostnameType) != string(metav1.ConditionTrue) || lb.Status.Hostname != hostname {
		t.Fatalf("got %q %+v", lb.Status.Hostname, lb.Status.Conditions)
	}

	t.Run("记录被改写后恢复", func(t *testing.T) {
		plane.records[hostname] = netip.MustParseAddr("10.62.0.99")
		converge(t, r, plane, lb)
		if events := plane.take(); !slices.Equal(events, []string{"publish " + hostname}) {
			t.Fatalf("got %v", events)
		}
	})

	t.Run("删除时主机清理后先删记录再释放 VIP", func(t *testing.T) {
		vip := lb.Status.Address
		if err := c.Delete(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, plane, lb)
		if events := plane.take(); !slices.Equal(events, []string{"remove lb1", "unpublish " + hostname}) {
			t.Fatalf("got %v", events)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: vip}, &networkingv1.IPAddress{}); !apierrors.IsNotFound(err) {
			t.Fatalf("记录删除后应释放 VIP: %v", err)
		}
	})
}
