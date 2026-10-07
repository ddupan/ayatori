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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dataPlane 在内存中模拟各主机的代理配置与路由宣告，并记录写入顺序。
// 真实代理与 gobgpd 的语义由 adapter 的 contract test 验证。
type dataPlane struct {
	configs     map[string]loadbalancer.Snapshot // host/uid → 已写入的期望
	routes      map[string]bool                  // host/vip
	unreachable map[string]bool
	events      []string
}

func newDataPlane() *dataPlane {
	return &dataPlane{configs: map[string]loadbalancer.Snapshot{}, routes: map[string]bool{}, unreachable: map[string]bool{}}
}

var errUnreachable = errors.New("host unreachable")

func (d *dataPlane) Observe(_ context.Context, host loadbalancer.Host, lb *loadbalancer.LoadBalancer) (loadbalancer.ProxyFacts, error) {
	if d.unreachable[host.Name] {
		return loadbalancer.ProxyFacts{}, errUnreachable
	}
	got, present := d.configs[host.Name+"/"+lb.Snapshot().Owner.UID]
	want := lb.Snapshot()
	return loadbalancer.ProxyFacts{Present: present, Matches: present && got.Address == want.Address && reflect.DeepEqual(got.Ports, want.Ports)}, nil
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

func (d *dataPlane) Announced(_ context.Context, host loadbalancer.Host, vip netip.Addr) (bool, error) {
	return d.routes[host.Name+"/"+vip.String()], nil
}

func (d *dataPlane) Announce(_ context.Context, host loadbalancer.Host, vip netip.Addr) error {
	d.routes[host.Name+"/"+vip.String()] = true
	d.events = append(d.events, "announce "+host.Name)
	return nil
}

func (d *dataPlane) Withdraw(_ context.Context, host loadbalancer.Host, vip netip.Addr) error {
	delete(d.routes, host.Name+"/"+vip.String())
	d.events = append(d.events, "withdraw "+host.Name)
	return nil
}

func (d *dataPlane) take() []string {
	events := d.events
	d.events = nil
	return events
}

var testHosts = []loadbalancer.Host{{Name: "lb1"}, {Name: "lb2"}}

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
	r := &LoadBalancerReconciler{Client: c, Pool: testPool, Hosts: testHosts, Proxy: plane, Routes: plane}
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	converge(t, r, lb)

	if events := plane.take(); !slices.Equal(events, []string{"apply lb1", "apply lb2", "announce lb1", "announce lb2"}) {
		t.Fatalf("每台主机必须先配置后宣告，got %v", events)
	}
	if conditionStatus(t, c, lb, programmedType) != string(metav1.ConditionTrue) || conditionStatus(t, c, lb, degradedType) != string(metav1.ConditionFalse) {
		t.Fatalf("全部主机宣告后应 Programmed 且不 Degraded: %+v", lb.Status)
	}

	t.Run("配置变化时重写且不撤回", func(t *testing.T) {
		lb.Spec.Ports[0].Endpoints = []networkv1alpha1.Endpoint{{Address: "192.168.10.127", Port: 6443}}
		if err := c.Update(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{"apply lb1", "apply lb2"}) {
			t.Fatalf("got %v", events)
		}
	})

	t.Run("宣告丢失后补宣告", func(t *testing.T) {
		delete(plane.routes, "lb2/"+lb.Status.Address)
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{"announce lb2"}) {
			t.Fatalf("got %v", events)
		}
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
		if events := plane.take(); !slices.Equal(events, []string{"withdraw lb1", "remove lb1"}) {
			t.Fatalf("可达主机应先撤回再删配置，got %v", events)
		}
	})

	t.Run("全部主机清理后释放 VIP", func(t *testing.T) {
		delete(plane.unreachable, "lb2")
		converge(t, r, lb)
		if events := plane.take(); !slices.Equal(events, []string{"withdraw lb2", "remove lb2"}) {
			t.Fatalf("got %v", events)
		}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(lb), lb); !apierrors.IsNotFound(err) {
			t.Fatalf("释放后应放行删除: %v", err)
		}
		if len(plane.configs) != 0 || len(plane.routes) != 0 {
			t.Fatalf("数据面应已清空: %v %v", plane.configs, plane.routes)
		}
	})
}
