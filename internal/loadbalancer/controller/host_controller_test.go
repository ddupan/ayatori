package controller

import (
	"slices"
	"testing"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func reconcileSecondHost(t *testing.T, r *LoadBalancerHostReconciler) {
	t.Helper()
	key := client.ObjectKey{Name: secondHost}
	for range 3 {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
}

func hostNames(t *testing.T, c client.Client, lb *networkv1alpha1.LoadBalancer) []string {
	t.Helper()
	reload(t, c, lb)
	names := make([]string, 0, len(lb.Status.Hosts))
	for _, host := range lb.Status.Hosts {
		names = append(names, host.Name)
	}
	return names
}

func TestLoadBalancerHostLifecycle(t *testing.T) {
	c := environment(t)
	plane := newDataPlane()
	lbs := &LoadBalancerReconciler{Client: c, Pool: testPool, Proxy: plane}
	hosts := &LoadBalancerHostReconciler{Client: c, Proxy: plane}
	createHosts(t, c, "lb1")
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	converge(t, lbs, lb)
	plane.take()

	t.Run("新主机在 controller 加上 finalizer 后才落地", func(t *testing.T) {
		host := &networkv1alpha1.LoadBalancerHost{Spec: networkv1alpha1.LoadBalancerHostSpec{DataPlaneURL: "https://lb2.example.test:5555"}}
		host.Name = secondHost
		create(t, c, host)
		converge(t, lbs, lb)
		if events := plane.take(); len(events) != 0 {
			t.Fatalf("没有 finalizer 时删除不会清理，不得写入，got %v", events)
		}
		reconcileSecondHost(t, hosts)
		reload(t, c, host)
		reachable := meta.FindStatusCondition(host.Status.Conditions, dataPlaneReachableType)
		if !controllerutil.ContainsFinalizer(host, LoadBalancerHostFinalizer) || reachable == nil || reachable.Status != "True" {
			t.Fatalf("got %+v", host)
		}
		converge(t, lbs, lb)
		if events := plane.take(); !slices.Equal(events, []string{applySecond}) {
			t.Fatalf("新增主机无需重启即落地，got %v", events)
		}
		if names := hostNames(t, c, lb); !slices.Equal(names, []string{"lb1", secondHost}) {
			t.Fatalf("got %v", names)
		}
	})

	t.Run("不可达时报告并在删除时等待", func(t *testing.T) {
		plane.unreachable[secondHost] = true
		reconcileSecondHost(t, hosts)
		host := &networkv1alpha1.LoadBalancerHost{}
		host.Name = secondHost
		reload(t, c, host)
		if reachable := meta.FindStatusCondition(host.Status.Conditions, dataPlaneReachableType); reachable.Status != "False" {
			t.Fatalf("got %+v", reachable)
		}
		if err := c.Delete(t.Context(), host); err != nil {
			t.Fatal(err)
		}
		reconcileSecondHost(t, hosts)
		reload(t, c, host)
		if !controllerutil.ContainsFinalizer(host, LoadBalancerHostFinalizer) {
			t.Fatal("不可达时不得放行删除")
		}
		converge(t, lbs, lb)
		if names := hostNames(t, c, lb); !slices.Equal(names, []string{"lb1"}) {
			t.Fatalf("删除中的主机不再参与落地，got %v", names)
		}
	})

	t.Run("可达后整机清理再放行", func(t *testing.T) {
		delete(plane.unreachable, secondHost)
		reconcileSecondHost(t, hosts)
		if events := plane.take(); !slices.Equal(events, []string{"purge " + secondHost}) {
			t.Fatalf("got %v", events)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: secondHost}, &networkv1alpha1.LoadBalancerHost{}); !apierrors.IsNotFound(err) {
			t.Fatalf("清理确认后应放行删除: %v", err)
		}
		if _, ok := plane.configs["lb1/"+string(lb.UID)]; !ok {
			t.Fatal("其他主机的配置不应受影响")
		}
	})
}
