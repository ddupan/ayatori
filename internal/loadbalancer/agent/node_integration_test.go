//go:build integration

package agent_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
	"go.ddupan.top/ayatori/internal/infra/clientcert"
	"go.ddupan.top/ayatori/internal/infra/node"
	"go.ddupan.top/ayatori/internal/loadbalancer/agent"
	"go.ddupan.top/ayatori/internal/testenv"
)

const lbHost = "lb-1"

// TestAgentJoins：agent 以 bootstrap token 申请证书（ADR-0021），批准后注册为 loadbalancer 角色的 Node、
// 续约自己的 Lease，并注册同名的 LoadBalancerHost、写它的 status（ADR-0022）；角色就绪跟随本机
// HAProxy 与 BGP 会话。deploy/flux/loadbalancer 的权限与准入策略只允许它写自己的 status。
func TestAgentJoins(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	authority := testenv.NewClientCA(t)
	root := filepath.Join("..", "..", "..")
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join(root, "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	env.ControlPlane.GetAPIServer().Authn = authority
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(cfg)
	for _, path := range []string{
		filepath.Join(root, "internal", "infra", "clientcert", "testdata", "rbac.yaml"),
		filepath.Join(root, "deploy", "flux", "nodes", "rbac.yaml"),
		filepath.Join(root, "deploy", "flux", "nodes", "admission.yaml"),
		filepath.Join(root, "deploy", "flux", "loadbalancer", "agent-rbac.yaml"),
		filepath.Join(root, "deploy", "flux", "loadbalancer", "agent-admission.yaml"),
	} {
		testenv.ApplyManifest(t, cfg, path)
	}
	go authority.SignApproved(t.Context(), admin, time.Hour)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, networkv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	k, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	port := freePort(t)
	startRouter(t, port)
	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithCancel(t.Context())
	// 部署方尚未 join、HAProxy 也未启动：agent 照常启动并宣告，加入控制面留到 join 之后。
	a, err := agent.New(ctx, agent.Config{
		ASN: 65100, RouterID: routerID, VIPPool: vipPool, HAProxySocket: socket,
		Neighbors: []agent.Neighbor{{Address: loopback, ASN: 65002, Port: uint32(port)}},
		Interval:  metav1.Duration{Duration: 200 * time.Millisecond},
		NodeName:  lbHost, CredentialsDir: dir,
	}, newFakeVIPs(), registry, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// join 晚于 agent 启动：agent 在后台重试，写入 bootstrap-kubeconfig 后才申请证书。
	time.Sleep(2 * time.Second)
	testenv.BootstrapToken(t, admin, cfg, dir, "lbjoin")

	approve, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	if err := clientcert.ApproveBootstrap(approve, admin, "lbjoin", node.UserPrefix+lbHost,
		[]string{node.NodesGroup, node.RoleGroupPrefix + agent.Role}, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	condition := func(status corev1.ConditionStatus, reason string) func() bool {
		return func() bool {
			var n corev1.Node
			if err := k.Get(t.Context(), client.ObjectKey{Name: lbHost}, &n); err != nil {
				return false
			}
			if _, ok := n.Labels[node.RoleLabelPrefix+agent.Role]; !ok {
				return false
			}
			for _, c := range n.Status.Conditions {
				if c.Type == node.ConditionType(agent.Role) {
					return c.Status == status && c.Reason == reason
				}
			}
			return false
		}
	}
	waitForLong(t, "注册 Node，HAProxy 不应答时角色未就绪", condition(corev1.ConditionFalse, "HAProxyDown"))
	if err := k.Get(t.Context(), client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: lbHost + "-" + agent.Role},
		&coordinationv1.Lease{}); err != nil {
		t.Fatalf("角色 Lease 未续约：%v", err)
	}
	if got := gauge(t, registry, "ayatori_lb_agent_client_certificate_not_after_timestamp_seconds"); got < float64(time.Now().Unix()) {
		t.Fatalf("证书到期时间指标 = %v", got)
	}

	waitForLong(t, "注册 LoadBalancerHost，HAProxy 不应答时未就绪", hostReports(t, k, metav1.ConditionFalse, 0))

	agentConfig, err := clientcert.RESTConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	self, err := client.New(agentConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("准入只允许 agent 写自己的 status", func(t *testing.T) { onlyOwnStatus(t, k, self) })

	// controller 加上 finalizer 之后，agent 的注册照常通过准入。
	host := &networkv1alpha1.LoadBalancerHost{}
	if err := k.Get(t.Context(), client.ObjectKey{Name: lbHost}, host); err != nil {
		t.Fatal(err)
	}
	host.Finalizers = []string{"network.ayatori.ddupan.top/loadbalancerhost"}
	if err := k.Update(t.Context(), host); err != nil {
		t.Fatal(err)
	}

	startHAProxy(t)
	waitForLong(t, "HAProxy 应答、会话建立后角色就绪", condition(corev1.ConditionTrue, "AgentReady"))
	waitForLong(t, "LoadBalancerHost 报告就绪与两个 VIP", hostReports(t, k, metav1.ConditionTrue, 2))
}

// hostReports 在 LoadBalancerHost 的 AgentReady 为 status、vips 有 vips 项时满足。
func hostReports(t *testing.T, k client.Client, status metav1.ConditionStatus, vips int) func() bool {
	return func() bool {
		host := &networkv1alpha1.LoadBalancerHost{}
		if err := k.Get(t.Context(), client.ObjectKey{Name: lbHost}, host); err != nil {
			return false
		}
		ready := meta.FindStatusCondition(host.Status.Conditions, "AgentReady")
		return ready != nil && ready.Status == status && len(host.Status.VIPs) == vips
	}
}

func onlyOwnStatus(t *testing.T, admin, self client.Client) {
	other := &networkv1alpha1.LoadBalancerHost{}
	other.Name = "lb-2"
	if err := admin.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	own := func() *networkv1alpha1.LoadBalancerHost {
		host := &networkv1alpha1.LoadBalancerHost{}
		if err := self.Get(t.Context(), client.ObjectKey{Name: lbHost}, host); err != nil {
			t.Fatal(err)
		}
		return host
	}
	for name, write := range map[string]func() error{
		"注册其他主机": func() error {
			host := &networkv1alpha1.LoadBalancerHost{}
			host.Name = "lb-3"
			return self.Create(t.Context(), host)
		},
		"写其他主机的 status": func() error {
			host := &networkv1alpha1.LoadBalancerHost{}
			if err := self.Get(t.Context(), client.ObjectKey{Name: "lb-2"}, host); err != nil {
				return err
			}
			host.Status.VIPs = []networkv1alpha1.AgentVIPStatus{{Address: "10.62.0.9", Announced: true}}
			return self.Status().Update(t.Context(), host)
		},
		"改 finalizer": func() error {
			host := own()
			host.Finalizers = append(host.Finalizers, "example.test/keep")
			return self.Update(t.Context(), host)
		},
		"删除自己": func() error { return self.Delete(t.Context(), own()) },
		"写 LoadBalancer": func() error {
			lb := &networkv1alpha1.LoadBalancer{}
			lb.Namespace, lb.Name = testNamespace, "x"
			lb.Spec.Ports = []networkv1alpha1.LoadBalancerPort{{Port: 80, BackendRef: networkv1alpha1.BackendRef{Name: "x"}}}
			return self.Create(t.Context(), lb)
		},
	} {
		if err := write(); !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
			t.Errorf("%s：应被拒绝，got %v", name, err)
		}
	}
}

// waitForLong 用于等待 Node 写入：没有变化时 Registrar 每 node.RenewInterval 才检查一次。
func waitForLong(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("%s：超时", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func gauge(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}
