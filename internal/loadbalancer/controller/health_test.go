package controller

import (
	"testing"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEndpointHealthStatus(t *testing.T) {
	c := environment(t)
	createHosts(t, c, firstHost, secondHost)
	plane := newDataPlane()
	clock := time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC)
	r := &LoadBalancerReconciler{Client: c, Pool: testPool, Proxy: plane, Now: func() time.Time { return clock }}
	backends := &BackendReconciler{Client: c}

	primary, standby := loadbalancer.Endpoint{Address: "10.60.0.11", Port: 6443}, loadbalancer.Endpoint{Address: "10.60.0.12", Port: 6443}
	lb := newLoadBalancer("pg", "")
	create(t, c, lb)
	backend := &networkv1alpha1.Backend{Spec: networkv1alpha1.BackendSpec{Endpoints: []networkv1alpha1.Endpoint{
		{Address: primary.Address, Port: primary.Port}, {Address: standby.Address, Port: standby.Port},
	}}}
	backend.Namespace, backend.Name = testNamespace, "pg"
	create(t, c, backend)
	up := func(e loadbalancer.Endpoint) loadbalancer.ServerHealth {
		return loadbalancer.ServerHealth{Port: 6443, Endpoint: e, State: "UP", CheckStatus: "L7OK", CheckCode: 200}
	}
	down := func(e loadbalancer.Endpoint) loadbalancer.ServerHealth {
		return loadbalancer.ServerHealth{Port: 6443, Endpoint: e, State: "DOWN", CheckStatus: "L7STS", CheckCode: 503}
	}
	plane.health = map[string][]loadbalancer.ServerHealth{
		firstHost:  {up(primary), down(standby)},
		secondHost: {up(primary), down(standby)},
	}
	converge(t, r, lb)
	reconcileBackend(t, backends, backend)

	reload(t, c, lb)
	if port := lb.Status.Ports[0]; port.ReadyEndpoints != 1 || port.Endpoints != 2 {
		t.Fatalf("got %+v", lb.Status.Ports)
	}
	// 每个（端口、成员、主机）一项。
	if got := lb.Status.Endpoints; len(got) != 4 || observed(lb, standby, secondHost).CheckCode != 503 || observed(lb, primary, firstHost).State != "UP" {
		t.Fatalf("got %+v", got)
	}
	if conditionStatus(t, c, lb, endpointsReadyType) != string(metav1.ConditionTrue) {
		t.Fatalf("got %+v", lb.Status.Conditions)
	}
	reload(t, c, backend)
	if got := backend.Status.Observations; len(got) != 4 || got[0].Address != primary.Address || got[0].LoadBalancer != "pg" ||
		got[0].LoadBalancerPort != 6443 || got[0].Host != firstHost || got[0].State != "UP" {
		t.Fatalf("got %+v", got)
	}
	if !meta.IsStatusConditionTrue(backend.Status.Conditions, readyType) {
		t.Fatalf("got %+v", backend.Status.Conditions)
	}
	first := observed(lb, primary, firstHost).LastTransitionTime

	t.Run("state 不变时不更新时间、不产生新版本", func(t *testing.T) {
		version := lb.ResourceVersion
		clock = clock.Add(time.Minute)
		converge(t, r, lb)
		reload(t, c, lb)
		if at := observed(lb, primary, firstHost).LastTransitionTime; lb.ResourceVersion != version || !at.Equal(&first) {
			t.Fatalf("got %s → %s, %v", version, lb.ResourceVersion, at)
		}
	})

	t.Run("切主后两边各自翻转，时间随之更新", func(t *testing.T) {
		clock = clock.Add(time.Minute)
		plane.health[firstHost] = []loadbalancer.ServerHealth{down(primary), up(standby)}
		plane.health[secondHost] = []loadbalancer.ServerHealth{down(primary), up(standby)}
		converge(t, r, lb)
		reload(t, c, lb)
		if got := observed(lb, standby, firstHost); got.State != "UP" || !got.LastTransitionTime.Time.Equal(clock) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("全部成员 DOWN 时两个 Ready 都为 False", func(t *testing.T) {
		plane.health[firstHost] = []loadbalancer.ServerHealth{down(primary), down(standby)}
		plane.health[secondHost] = []loadbalancer.ServerHealth{down(primary), down(standby)}
		converge(t, r, lb)
		reconcileBackend(t, backends, backend)
		if conditionStatus(t, c, lb, endpointsReadyType) != string(metav1.ConditionFalse) {
			t.Fatalf("got %+v", lb.Status.Conditions)
		}
		reload(t, c, backend)
		if meta.IsStatusConditionTrue(backend.Status.Conditions, readyType) {
			t.Fatalf("got %+v", backend.Status.Conditions)
		}
	})

	t.Run("主机不可达时删去它的观察", func(t *testing.T) {
		plane.health[firstHost] = []loadbalancer.ServerHealth{up(primary), down(standby)}
		plane.unreachable[secondHost] = true
		converge(t, r, lb)
		reload(t, c, lb)
		if got := lb.Status.Endpoints; len(got) != 2 || got[0].Host != firstHost || got[1].Host != firstHost {
			t.Fatalf("got %+v", got)
		}
		delete(plane.unreachable, secondHost)
	})

	t.Run("不再被引用的 Backend 清空观察", func(t *testing.T) {
		lb.Spec.Ports[0].BackendRef.Name = "other"
		if err := c.Update(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		if requests := backends.inNamespace(t.Context(), lb); len(requests) != 1 || requests[0].Name != backend.Name {
			t.Fatalf("LoadBalancer 变化应触发同 namespace 的 Backend，got %v", requests)
		}
		reconcileBackend(t, backends, backend)
		reload(t, c, backend)
		if obs := backend.Status.Observations; len(obs) != 0 {
			t.Fatalf("got %+v", obs)
		}
		if c := meta.FindStatusCondition(backend.Status.Conditions, readyType); c == nil || c.Reason != reasonUnreferenced {
			t.Fatalf("got %+v", backend.Status.Conditions)
		}
	})
}

const firstHost = "lb1"

func reconcileBackend(t *testing.T, r *BackendReconciler, backend *networkv1alpha1.Backend) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(backend)}); err != nil {
		t.Fatal(err)
	}
}

func observed(lb *networkv1alpha1.LoadBalancer, endpoint loadbalancer.Endpoint, host string) networkv1alpha1.EndpointStatus {
	for _, e := range lb.Status.Endpoints {
		if e.Address == endpoint.Address && e.EndpointPort == endpoint.Port && e.Host == host {
			return e
		}
	}
	return networkv1alpha1.EndpointStatus{}
}
