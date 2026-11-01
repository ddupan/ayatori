package controller

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	testNamespace = "lb"
	firstVIP      = "10.62.0.1"
)

var testPool = address.Pool{Prefix: netip.MustParsePrefix("10.62.0.0/24")}

func environment(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("运行 make test 启动真实 API server")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{networkv1alpha1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	crdPath, err := filepath.Abs("../../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	namespace.Name = testNamespace
	create(t, c, namespace)
	return c
}

func create(t *testing.T, c client.Client, object client.Object) {
	t.Helper()
	if err := c.Create(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

func reload(t *testing.T, c client.Client, object client.Object) {
	t.Helper()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
}

func newLoadBalancer(name, requested string) *networkv1alpha1.LoadBalancer {
	lb := &networkv1alpha1.LoadBalancer{Spec: networkv1alpha1.LoadBalancerSpec{
		Address: requested,
		Ports:   []networkv1alpha1.LoadBalancerPort{{Port: 6443}},
	}}
	lb.Namespace, lb.Name = testNamespace, name
	return lb
}

// converge 模拟 controller-runtime 的连续协调，直到不再请求立即重试。每轮之后像 LoadBalancerHost
// controller 一样刷新主机 status 中的 agent 报告。
func converge(t *testing.T, r *LoadBalancerReconciler, lb *networkv1alpha1.LoadBalancer) ctrl.Result {
	t.Helper()
	var result ctrl.Result
	for range 5 {
		var err error
		result, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lb)})
		if err != nil {
			t.Fatal(err)
		}
		refreshAgents(t, r)
	}
	return result
}

// refreshAgents 只观察已带 finalizer 且未在删除的主机：加 finalizer 与删除时的清理由各测试显式驱动。
func refreshAgents(t *testing.T, r *LoadBalancerReconciler) {
	t.Helper()
	plane, ok := r.Proxy.(*dataPlane)
	if !ok {
		return
	}
	hosts := &LoadBalancerHostReconciler{Client: r.Client, Proxy: plane, Agent: plane, AgentStaleAfter: time.Minute}
	list := &networkv1alpha1.LoadBalancerHostList{}
	if err := r.Client.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		object := &list.Items[i]
		if !object.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(object, LoadBalancerHostFinalizer) {
			continue
		}
		if _, err := hosts.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
			t.Fatal(err)
		}
	}
}

func programmed(t *testing.T, c client.Client, lb *networkv1alpha1.LoadBalancer) string {
	t.Helper()
	reload(t, c, lb)
	condition := meta.FindStatusCondition(lb.Status.Conditions, programmedType)
	if condition == nil {
		t.Fatal("缺少 Programmed condition")
	}
	return condition.Reason
}

func ipAddressOf(t *testing.T, c client.Client, addr string) *networkingv1.IPAddress {
	t.Helper()
	ip := &networkingv1.IPAddress{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: addr}, ip); err != nil {
		t.Fatalf("读取 IPAddress %s: %v", addr, err)
	}
	return ip
}

func TestLoadBalancerAddressLifecycle(t *testing.T) {
	c := environment(t)
	r := &LoadBalancerReconciler{Client: c, Pool: testPool}

	first := newLoadBalancer("api", "")
	create(t, c, first)
	converge(t, r, first)
	if reason := programmed(t, c, first); reason != loadbalancer.ReasonDataPlanePending || first.Status.Address != firstVIP {
		t.Fatalf("自动分配 = %s %s", first.Status.Address, reason)
	}
	if !controllerutil.ContainsFinalizer(first, LoadBalancerFinalizer) {
		t.Fatal("认领前应先持久化 finalizer")
	}
	if owner, ok := ownerOf(ipAddressOf(t, c, firstVIP)); !ok || owner.UID != string(first.UID) {
		t.Fatalf("IPAddress 应记录所有者身份，got %+v", owner)
	}

	t.Run("指定地址被占用时报告冲突", func(t *testing.T) {
		lb := newLoadBalancer("conflict", firstVIP)
		create(t, c, lb)
		if result := converge(t, r, lb); result.RequeueAfter != pendingRetry {
			t.Fatalf("冲突时应低频重试，got %v", result)
		}
		if reason := programmed(t, c, lb); reason != loadbalancer.ReasonAddressConflict || lb.Status.Address != "" {
			t.Fatalf("got %s %s", lb.Status.Address, reason)
		}
	})

	t.Run("补认领 status 未记录的自身占用", func(t *testing.T) {
		lb := newLoadBalancer("crashed", "")
		create(t, c, lb)
		// 模拟上一轮已加 finalizer 并创建 IPAddress、但写 status 前崩溃。
		controllerutil.AddFinalizer(lb, LoadBalancerFinalizer)
		if err := c.Update(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		create(t, c, ipAddress(netip.MustParseAddr("10.62.0.9"), lb))
		converge(t, r, lb)
		reload(t, c, lb)
		if lb.Status.Address != "10.62.0.9" {
			t.Fatalf("应采用已占用的 10.62.0.9，got %q", lb.Status.Address)
		}
	})

	t.Run("占用丢失后重新认领原地址", func(t *testing.T) {
		if err := c.Delete(t.Context(), ipAddressOf(t, c, firstVIP)); err != nil {
			t.Fatal(err)
		}
		converge(t, r, first)
		reload(t, c, first)
		if first.Status.Address != firstVIP || ipAddressOf(t, c, firstVIP) == nil {
			t.Fatal("应重新认领原 VIP 而不是换地址")
		}
	})

	t.Run("cache 未见他人占用时等待重新观察", func(t *testing.T) {
		lb := newLoadBalancer("stale", "")
		create(t, c, lb)
		controllerutil.AddFinalizer(lb, LoadBalancerFinalizer)
		if err := c.Update(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		stale := &LoadBalancerReconciler{Client: staleList{c}, Pool: testPool}
		result, err := stale.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lb)})
		if err != nil || result.RequeueAfter != claimRetry {
			t.Fatalf("AlreadyExists 应短间隔重试，got %v %v", result, err)
		}
		reload(t, c, lb)
		if lb.Status.Address != "" {
			t.Fatal("不应记录未确认的地址")
		}
	})

	t.Run("删除时释放地址后放行", func(t *testing.T) {
		if err := c.Delete(t.Context(), first); err != nil {
			t.Fatal(err)
		}
		converge(t, r, first)
		if err := c.Get(t.Context(), client.ObjectKey{Name: firstVIP}, &networkingv1.IPAddress{}); !apierrors.IsNotFound(err) {
			t.Fatalf("VIP 应已释放: %v", err)
		}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(first), first); !apierrors.IsNotFound(err) {
			t.Fatalf("释放后应移除 finalizer: %v", err)
		}
	})
}

func TestAddressReconcilerCollectsOrphans(t *testing.T) {
	c := environment(t)
	gone := newLoadBalancer("gone", "")
	gone.UID = "deleted-uid"
	ip := ipAddress(netip.MustParseAddr("10.62.0.20"), gone)
	create(t, c, ip)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ip)}

	early := &AddressReconciler{Client: c, Now: time.Now}
	result, err := early.Reconcile(t.Context(), request)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("宽限期内应等待，got %v %v", result, err)
	}
	ipAddressOf(t, c, "10.62.0.20")

	late := &AddressReconciler{Client: c, Now: func() time.Time { return time.Now().Add(2 * address.Grace) }}
	if _, err := late.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(ip), &networkingv1.IPAddress{}); !apierrors.IsNotFound(err) {
		t.Fatalf("无主 IPAddress 应被回收: %v", err)
	}
}

// staleList 模拟 cache 尚未观察到已有 IPAddress。
type staleList struct{ client.Client }

func (s staleList) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*networkingv1.IPAddressList); ok {
		return nil
	}
	return s.Client.List(ctx, list, options...)
}
