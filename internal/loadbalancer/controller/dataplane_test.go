package controller

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dataPlane 在内存中模拟各主机的代理配置与 frontend 状态，并记录写入顺序。
// 真实代理的语义由 adapter 的 contract test 验证；宣告由 LB agent 负责，不经 controller。
type dataPlane struct {
	configs     map[string]loadbalancer.Snapshot // host/uid → 已写入的期望
	stopped     map[string]bool                  // host：frontend 未处于 OPEN
	unreachable map[string]bool
	records     map[string]netip.Addr // hostname
	events      []string
}

func newDataPlane() *dataPlane {
	return &dataPlane{configs: map[string]loadbalancer.Snapshot{}, stopped: map[string]bool{}, unreachable: map[string]bool{}, records: map[string]netip.Addr{}}
}

var errUnreachable = errors.New("host unreachable")

func (d *dataPlane) Observe(_ context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) (loadbalancer.ProxyFacts, error) {
	if d.unreachable[host.Name] {
		return loadbalancer.ProxyFacts{}, errUnreachable
	}
	got, present := d.configs[host.Name+"/"+lb.Snapshot().Owner.UID]
	want := lb.Snapshot()
	facts := loadbalancer.ProxyFacts{Present: present, Matches: present && got.Address == want.Address && reflect.DeepEqual(got.Ports, want.Ports)}
	if facts.Matches {
		facts.Frontends = []loadbalancer.Frontend{{Address: got.Address, Open: !d.stopped[host.Name]}}
	}
	return facts, nil
}

func (d *dataPlane) Apply(_ context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) error {
	d.configs[host.Name+"/"+lb.Snapshot().Owner.UID] = lb.Snapshot()
	d.events = append(d.events, "apply "+host.Name)
	return nil
}

func (d *dataPlane) Remove(_ context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) error {
	delete(d.configs, host.Name+"/"+lb.Snapshot().Owner.UID)
	d.events = append(d.events, "remove "+host.Name)
	return nil
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

var testHosts = []loadbalancer.Host{{Name: "lb1"}, {Name: "lb2"}}

const applyFirst = "apply lb1"

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
	plane := newDataPlane()
	r := &LoadBalancerReconciler{Client: c, Pool: testPool, Hosts: testHosts, Proxy: plane}
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	converge(t, r, lb)

	if events := plane.take(); !slices.Equal(events, []string{applyFirst, "apply lb2"}) {
		t.Fatalf("got %v", events)
	}
	if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionFalse) {
		t.Fatalf("全部主机就绪后应 Programmed 且不 Degraded: %+v", lb.Status)
	}

	t.Run("配置变化时重写", func(t *testing.T) {
		lb.Spec.Ports[0].Endpoints = []networkv1alpha1.Endpoint{{Address: "192.168.10.127", Port: 6443}}
		if err := c.Update(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{applyFirst, "apply lb2"}) {
			t.Fatalf("got %v", events)
		}
	})

	t.Run("frontend 未 OPEN 的主机只降级自身", func(t *testing.T) {
		plane.stopped["lb2"] = true
		converge(t, r, lb)
		if events := plane.take(); len(events) != 0 {
			t.Fatalf("frontend 状态不由 controller 修复，got %v", events)
		}
		if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionTrue) {
			t.Fatalf("got %+v", lb.Status.Conditions)
		}
		if lb.Status.Hosts[1].Phase != string(loadbalancer.AttachmentConfigured) {
			t.Fatalf("got %+v", lb.Status.Hosts)
		}
		delete(plane.stopped, "lb2")
	})

	t.Run("一台主机不可达只影响自身", func(t *testing.T) {
		plane.unreachable["lb2"] = true
		converge(t, r, lb)
		if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionTrue) {
			t.Fatalf("got %+v", lb.Status.Conditions)
		}
		if lb.Status.Hosts[1].Name != "lb2" || lb.Status.Hosts[1].Phase != string(loadbalancer.AttachmentUnavailable) {
			t.Fatalf("got %+v", lb.Status.Hosts)
		}
	})

	t.Run("删除时主机不可达则等待", func(t *testing.T) {
		if err := c.Delete(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, lb)
		reload(t, c, lb)
		ipAddressOf(t, c, lb.Status.Address)
		if events := plane.take(); !slices.Equal(events, []string{"remove lb1"}) {
			t.Fatalf("可达主机直接删配置，got %v", events)
		}
	})

	t.Run("全部主机清理后释放 VIP", func(t *testing.T) {
		delete(plane.unreachable, "lb2")
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{"remove lb2"}) {
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

func TestLoadBalancerHostname(t *testing.T) {
	c := environment(t)
	plane := newDataPlane()
	r := &LoadBalancerReconciler{Client: c, Pool: testPool, Hosts: testHosts[:1], Proxy: plane,
		DNS: zone{plane}, DNSZone: "lb.lab.ddupan.top"}
	lb := newLoadBalancer("api", "")
	create(t, c, lb)
	converge(t, r, lb)
	const hostname = "api." + testNamespace + ".lb.lab.ddupan.top"
	if events := plane.take(); !slices.Equal(events, []string{applyFirst, "publish " + hostname}) {
		t.Fatalf("VIP 确认后即发布记录，got %v", events)
	}
	if conditionStatus(t, c, lb, hostnameType) != string(metav1.ConditionTrue) || lb.Status.Hostname != hostname {
		t.Fatalf("got %q %+v", lb.Status.Hostname, lb.Status.Conditions)
	}

	t.Run("记录被改写后恢复", func(t *testing.T) {
		plane.records[hostname] = netip.MustParseAddr("10.62.0.99")
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{"publish " + hostname}) {
			t.Fatalf("got %v", events)
		}
	})

	t.Run("删除时主机清理后先删记录再释放 VIP", func(t *testing.T) {
		vip := lb.Status.Address
		if err := c.Delete(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{"remove lb1", "unpublish " + hostname}) {
			t.Fatalf("got %v", events)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: vip}, &networkingv1.IPAddress{}); !apierrors.IsNotFound(err) {
			t.Fatalf("记录删除后应释放 VIP: %v", err)
		}
	})
}
