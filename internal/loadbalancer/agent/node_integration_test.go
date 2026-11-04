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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
	"git.ddupan.top/panxiao81/ayatori/internal/infra/node"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/agent"
	"git.ddupan.top/panxiao81/ayatori/internal/testenv"
)

const lbHost = "lb-1"

// TestAgentJoins：agent 以 bootstrap token 申请证书（ADR-0021），批准后注册为 loadbalancer 角色的 Node
// 并续约自己的 Lease（ADR-0022）；角色就绪跟随本机 HAProxy 与 BGP 会话。
func TestAgentJoins(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	authority := testenv.NewClientCA(t)
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().Authn = authority
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(cfg)
	root := filepath.Join("..", "..", "..")
	for _, path := range []string{
		filepath.Join(root, "internal", "infra", "clientcert", "testdata", "rbac.yaml"),
		filepath.Join(root, "deploy", "flux", "nodes", "rbac.yaml"),
		filepath.Join(root, "deploy", "flux", "nodes", "admission.yaml"),
	} {
		testenv.ApplyManifest(t, cfg, path)
	}
	go authority.SignApproved(t.Context(), admin, time.Hour)
	k, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	testenv.BootstrapToken(t, admin, cfg, dir, "lbjoin")
	port := freePort(t)
	startRouter(t, port)
	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithCancel(t.Context())
	// HAProxy 尚未启动：agent 照常运行，角色报告未就绪。
	a, err := agent.New(ctx, agent.Config{
		ASN: 65100, RouterID: "10.60.0.30", VIPPool: "10.62.0.0/24", HAProxySocket: socket,
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

	startHAProxy(t)
	waitForLong(t, "HAProxy 应答、会话建立后角色就绪", condition(corev1.ConditionTrue, "AgentReady"))
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
