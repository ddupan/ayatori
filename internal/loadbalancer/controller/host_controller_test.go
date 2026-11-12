package controller

import (
	"slices"
	"testing"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func reconcileSecondHost(t *testing.T, r *LoadBalancerHostReconciler, plane *dataPlane) {
	t.Helper()
	key := client.ObjectKey{Name: secondHost}
	for range 3 {
		if err := r.Client.Get(t.Context(), key, &networkv1alpha1.LoadBalancerHost{}); err == nil {
			plane.agent(t, r.Client, secondHost)
		}
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
	lbs := &LoadBalancerReconciler{Client: c, Pool: testPool}
	hosts := &LoadBalancerHostReconciler{Client: c, AgentStaleAfter: time.Minute}
	createHosts(t, c, "lb1")
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	converge(t, lbs, plane, lb)
	plane.take()

	t.Run("新主机在 controller 加上 finalizer 后才落地", func(t *testing.T) {
		host := &networkv1alpha1.LoadBalancerHost{}
		host.Name = secondHost
		create(t, c, host)
		converge(t, lbs, plane, lb)
		if events := plane.take(); len(events) != 0 {
			t.Fatalf("没有 finalizer 时删除不会清理，不得写入，got %v", events)
		}
		reconcileSecondHost(t, hosts, plane)
		reload(t, c, host)
		if !controllerutil.ContainsFinalizer(host, LoadBalancerHostFinalizer) {
			t.Fatalf("got %+v", host)
		}
		converge(t, lbs, plane, lb)
		if events := plane.take(); !slices.Equal(events, []string{applySecond}) {
			t.Fatalf("新增主机无需重启即落地，got %v", events)
		}
		if names := hostNames(t, c, lb); !slices.Equal(names, []string{"lb1", secondHost}) {
			t.Fatalf("got %v", names)
		}
		reload(t, c, host)
		agentReady := meta.FindStatusCondition(host.Status.Conditions, agentReadyType)
		if agentReady == nil || agentReady.Status != "True" || len(host.Status.VIPs) != 1 ||
			host.Status.VIPs[0].Address != lb.Status.Address || !host.Status.VIPs[0].Announced {
			t.Fatalf("status 应记录 agent 报告的宣告，got %+v", host.Status)
		}
		if lb.Status.Hosts[1].Phase != "Ready" {
			t.Fatalf("Attachment 以 agent 报告为准，got %+v", lb.Status.Hosts)
		}
	})

	t.Run("Lease 过期时清空报告，agent 恢复后重新报告", func(t *testing.T) {
		plane.unreachable[secondHost] = true
		hosts.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
		reconcileSecondHost(t, hosts, plane)
		host := &networkv1alpha1.LoadBalancerHost{}
		host.Name = secondHost
		reload(t, c, host)
		agentReady := meta.FindStatusCondition(host.Status.Conditions, agentReadyType)
		if agentReady.Status != metav1.ConditionUnknown || agentReady.Reason != agentStale || host.Status.VIPs != nil {
			t.Fatalf("got %+v", host.Status)
		}
		if reportOf(host).Reported {
			t.Fatal("失联 agent 的报告不得用于 Attachment")
		}
		hosts.Now = nil
		delete(plane.unreachable, secondHost)
		reconcileSecondHost(t, hosts, plane)
		reload(t, c, host)
		if agentReady := meta.FindStatusCondition(host.Status.Conditions, agentReadyType); agentReady.Status != metav1.ConditionTrue || len(host.Status.VIPs) != 1 {
			t.Fatalf("got %+v", host.Status)
		}
	})

	t.Run("按 Lease 到期时间排下一次检查", func(t *testing.T) {
		hosts.AgentStaleAfter = 10 * time.Second
		defer func() { hosts.AgentStaleAfter = time.Minute }()
		result, err := hosts.Reconcile(t.Context(), ctrl.Request{Name: secondHost})
		if err != nil {
			t.Fatal(err)
		}
		// 刚续约：下一次检查排在到期之后，而不是固定的 observeInterval。
		if result.RequeueAfter <= 0 || result.RequeueAfter > 11*time.Second {
			t.Fatalf("RequeueAfter = %v", result.RequeueAfter)
		}
	})

	t.Run("agent 失联时报告并在删除时等待", func(t *testing.T) {
		plane.unreachable[secondHost] = true
		reconcileSecondHost(t, hosts, plane)
		host := &networkv1alpha1.LoadBalancerHost{}
		host.Name = secondHost
		reload(t, c, host)
		if agentReady := meta.FindStatusCondition(host.Status.Conditions, agentReadyType); agentReady.Status != metav1.ConditionUnknown {
			t.Fatalf("got %+v", agentReady)
		}
		if err := c.Delete(t.Context(), host); err != nil {
			t.Fatal(err)
		}
		reconcileSecondHost(t, hosts, plane)
		reload(t, c, host)
		if !controllerutil.ContainsFinalizer(host, LoadBalancerHostFinalizer) {
			t.Fatal("agent 报告 Purged 之前不得放行删除")
		}
		converge(t, lbs, plane, lb)
		if names := hostNames(t, c, lb); !slices.Equal(names, []string{"lb1"}) {
			t.Fatalf("删除中的主机不再参与落地，got %v", names)
		}
	})

	t.Run("agent 恢复后整机清理、报告 Purged 再放行", func(t *testing.T) {
		delete(plane.unreachable, secondHost)
		reconcileSecondHost(t, hosts, plane)
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
