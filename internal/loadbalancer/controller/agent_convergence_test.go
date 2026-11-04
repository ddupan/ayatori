package controller

import (
	"slices"
	"testing"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// agentReports 扮演主机上在本机收敛的 agent（ADR-0022）：续约 Lease，报告 AgentReady、已宣告的 VIP 与
// 各 LoadBalancer 的本机事实；purged 时报告 Purged。
func agentReports(t *testing.T, c client.Client, host string, purged bool, entries ...networkv1alpha1.HostLoadBalancerStatus) {
	t.Helper()
	lease := &coordinationv1.Lease{}
	lease.Namespace, lease.Name = corev1.NamespaceNodeLease, host+"-"+agentRole
	if _, err := controllerutil.CreateOrUpdate(t.Context(), c, lease, func() error {
		lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	object := &networkv1alpha1.LoadBalancerHost{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: host}, object); err != nil {
		t.Fatal(err)
	}
	object.Status.VIPs, object.Status.LoadBalancers = nil, entries
	for _, entry := range entries {
		lb := &networkv1alpha1.LoadBalancer{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, lb); err == nil && entry.Present {
			object.Status.VIPs = append(object.Status.VIPs, networkv1alpha1.AgentVIPStatus{Address: lb.Status.Address, Announced: true})
		}
	}
	meta.SetStatusCondition(&object.Status.Conditions, condition(agentReadyType, true, "Ready", "", object.Generation))
	if purged {
		meta.SetStatusCondition(&object.Status.Conditions, condition(PurgedType, true, "Purged", "", object.Generation))
	}
	if err := c.Status().Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

// converged 是 agent 对 lb 的报告：present 时本机配置与当前期望一致。
func converged(t *testing.T, c client.Client, lb *networkv1alpha1.LoadBalancer, present bool) networkv1alpha1.HostLoadBalancerStatus {
	t.Helper()
	reload(t, c, lb)
	entry := networkv1alpha1.HostLoadBalancerStatus{Namespace: lb.Namespace, Name: lb.Name, UID: string(lb.UID),
		Present: present, Reload: "Succeeded"}
	if present {
		desired, err := Desired(t.Context(), c, lb)
		if err != nil {
			t.Fatal(err)
		}
		entry.Digest = desired.Digest()
		entry.Servers = []networkv1alpha1.HostServerStatus{{Port: 6443, Address: "10.60.0.21", EndpointPort: 6443, State: "UP", CheckStatus: "L4OK"}}
	}
	return entry
}

func phases(t *testing.T, c client.Client, lb *networkv1alpha1.LoadBalancer) []string {
	t.Helper()
	reload(t, c, lb)
	result := make([]string, 0, len(lb.Status.Hosts))
	for _, host := range lb.Status.Hosts {
		result = append(result, host.Name+"="+host.Phase)
	}
	return result
}

// TestAgentConvergence：AgentConvergence 时 controller 不连接主机，按各主机 agent 的报告判定
// Attachment、Programmed 与 endpoint 健康；删除 LoadBalancer 与 LoadBalancerHost 都等 agent 报告
// 本机已清除再放行（ADR-0022）。
func TestAgentConvergence(t *testing.T) {
	c := environment(t)
	r := &LoadBalancerReconciler{Client: c, Pool: testPool, AgentConvergence: true}
	hosts := &LoadBalancerHostReconciler{Client: c, AgentConvergence: true, AgentStaleAfter: time.Minute}
	createHosts(t, c, "lb1", secondHost)
	backend := &networkv1alpha1.Backend{Spec: networkv1alpha1.BackendSpec{
		Endpoints: []networkv1alpha1.Endpoint{{Address: "10.60.0.21", Port: 6443}},
	}}
	backend.Namespace, backend.Name = testNamespace, "pg"
	create(t, c, backend)
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	converge(t, r, lb)
	if got := phases(t, c, lb); !slices.Equal(got, []string{"lb1=Unavailable", secondHost + "=Unavailable"}) || programmed(t, c, lb) == "Programmed" {
		t.Fatalf("agent 未报告时不可用，got %v", got)
	}

	t.Run("按 agent 报告的摘要判定落地", func(t *testing.T) {
		agentReports(t, c, "lb1", false, converged(t, c, lb, true))
		stale := converged(t, c, lb, true)
		stale.Digest = "0000"
		agentReports(t, c, secondHost, false, stale)
		converge(t, r, lb)
		if got := phases(t, c, lb); !slices.Equal(got, []string{"lb1=Ready", secondHost + "=Pending"}) {
			t.Fatalf("摘要不一致的主机仍在写入，got %v", got)
		}
		agentReports(t, c, secondHost, false, converged(t, c, lb, true))
		converge(t, r, lb)
		if got := phases(t, c, lb); !slices.Equal(got, []string{"lb1=Ready", secondHost + "=Ready"}) || programmed(t, c, lb) != "Programmed" {
			t.Fatalf("got %v", got)
		}
		if len(lb.Status.Endpoints) != 2 || lb.Status.Endpoints[0].State != "UP" {
			t.Fatalf("endpoint 健康取自 agent 报告的 server，got %+v", lb.Status.Endpoints)
		}
	})

	t.Run("删除等全部 agent 报告已清除", func(t *testing.T) {
		if err := c.Delete(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		converge(t, r, lb)
		if got := phases(t, c, lb); !slices.Equal(got, []string{"lb1=Configured", secondHost + "=Configured"}) {
			t.Fatalf("got %v", got)
		}
		agentReports(t, c, "lb1", false, converged(t, c, lb, false))
		// 已删除完成的 agent 可以不再报告该对象。
		agentReports(t, c, secondHost, false)
		converge(t, r, lb)
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(lb), lb); !apierrors.IsNotFound(err) {
			t.Fatalf("全部主机已清除后应放行删除: %v %+v", err, lb.Status.Hosts)
		}
	})

	t.Run("删除主机等 agent 报告 Purged", func(t *testing.T) {
		host := &networkv1alpha1.LoadBalancerHost{}
		host.Name = secondHost
		if err := c.Delete(t.Context(), host); err != nil {
			t.Fatal(err)
		}
		reconcile := func() {
			if _, err := hosts.Reconcile(t.Context(), ctrl.Request{Name: secondHost}); err != nil {
				t.Fatal(err)
			}
		}
		reconcile()
		if err := c.Get(t.Context(), client.ObjectKey{Name: secondHost}, host); err != nil || !controllerutil.ContainsFinalizer(host, LoadBalancerHostFinalizer) {
			t.Fatalf("agent 报告之前不得放行: %v", err)
		}
		if meta.FindStatusCondition(host.Status.Conditions, dataPlaneReachableType) != nil {
			t.Fatal("不连接主机时不报告 DataPlaneReachable")
		}
		agentReports(t, c, secondHost, true)
		reconcile()
		if err := c.Get(t.Context(), client.ObjectKey{Name: secondHost}, host); !apierrors.IsNotFound(err) {
			t.Fatalf("Purged 之后应放行: %v", err)
		}
	})
}
