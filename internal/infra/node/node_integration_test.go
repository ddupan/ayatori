//go:build integration

package node_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1ac "k8s.io/client-go/applyconfigurations/coordination/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.ddupan.top/ayatori/internal/infra/clientcert"
	"go.ddupan.top/ayatori/internal/infra/node"
	"go.ddupan.top/ayatori/internal/testenv"
)

const (
	host         = "host-a"
	compute      = "compute"
	loadbalancer = "loadbalancer"
)

func eventually(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %v", timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// join 让 host 上 role 角色的 agent 以 bootstrap token 加入（ADR-0021），并开始注册；返回它证书的客户端。
func join(t *testing.T, admin kubernetes.Interface, cfg *rest.Config, role, tokenID string) client.Client {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	testenv.BootstrapToken(t, admin, cfg, dir, tokenID)
	stop, err := clientcert.Start(ctx, node.Identity(dir, host, role))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	approve, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := clientcert.ApproveBootstrap(approve, admin, tokenID, node.UserPrefix+host,
		[]string{node.NodesGroup, node.RoleGroupPrefix + role}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	var config *rest.Config
	eventually(t, time.Minute, func() error {
		var err error
		config, err = clientcert.RESTConfig(dir)
		return err
	})
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &node.Registrar{Client: c, Node: host, Role: role}
	go r.Run(ctx, func() node.Status { return node.Status{Ready: true, Reason: "Testing"} }, func(string, error) {})
	return c
}

// TestRoles：同一台主机上的两个角色各自加入，共同持有一个 Node，各写各的角色；deploy/flux/nodes 的
// 准入策略不允许一个角色的 agent 改动另一台主机、另一个角色或 Node 的 Ready。
func TestRoles(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	ctx := t.Context()
	authority := testenv.NewClientCA(t)
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().Authn = authority
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(cfg)
	for _, path := range []string{
		filepath.Join("..", "clientcert", "testdata", "rbac.yaml"),
		filepath.Join("..", "..", "..", "deploy", "flux", "nodes", "rbac.yaml"),
		filepath.Join("..", "..", "..", "deploy", "flux", "nodes", "admission.yaml"),
	} {
		testenv.ApplyManifest(t, cfg, path)
	}
	go authority.SignApproved(ctx, admin, time.Hour)
	k, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	agent := join(t, admin, cfg, compute, "hostcp")
	join(t, admin, cfg, loadbalancer, "hostlb")

	t.Run("both roles share one Node and keep separate leases", func(t *testing.T) {
		eventually(t, time.Minute, func() error {
			var n corev1.Node
			if err := k.Get(ctx, client.ObjectKey{Name: host}, &n); err != nil {
				return err
			}
			for _, role := range []string{compute, loadbalancer} {
				if _, ok := n.Labels[node.RoleLabelPrefix+role]; !ok {
					return fmt.Errorf("no %s label: %v", role, n.Labels)
				}
				if !hasCondition(n, node.ConditionType(role)) {
					return fmt.Errorf("no %s condition: %+v", role, n.Status.Conditions)
				}
				if err := k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: host + "-" + role},
					&coordinationv1.Lease{}); err != nil {
					return err
				}
			}
			return nil
		})
	})

	// 以 compute agent 的证书越权：全部被拒绝。
	owner := client.FieldOwner("intruder")
	denied := map[string]func() error{
		"another Node": func() error {
			return agent.Apply(ctx, corev1ac.Node("host-b").WithLabels(map[string]string{node.RoleLabelPrefix + compute: ""}), owner)
		},
		"a role it does not hold": func() error {
			return agent.Apply(ctx, corev1ac.Node(host).WithLabels(map[string]string{node.RoleLabelPrefix + "storage": ""}), owner)
		},
		"another role's label": func() error {
			var n corev1.Node
			if err := k.Get(ctx, client.ObjectKey{Name: host}, &n); err != nil {
				return err
			}
			delete(n.Labels, node.RoleLabelPrefix+loadbalancer)
			return agent.Update(ctx, &n)
		},
		"another role's condition": func() error {
			return agent.Status().Apply(ctx, corev1ac.Node(host).WithStatus(corev1ac.NodeStatus().WithConditions(
				corev1ac.NodeCondition().WithType(node.ConditionType(loadbalancer)).WithStatus(corev1.ConditionFalse))),
				client.FieldOwner(loadbalancer+"-agent"), client.ForceOwnership)
		},
		"the Node Ready condition": func() error {
			return agent.Status().Apply(ctx, corev1ac.Node(host).WithStatus(corev1ac.NodeStatus().WithConditions(
				corev1ac.NodeCondition().WithType(corev1.NodeReady).WithStatus(corev1.ConditionFalse))), owner)
		},
		"another role's lease": func() error {
			return agent.Apply(ctx, lease(host+"-"+loadbalancer), owner, client.ForceOwnership)
		},
		"the lease named after the node only": func() error {
			return agent.Apply(ctx, lease(host), owner, client.ForceOwnership)
		},
	}
	// 准入策略创建后要过一会儿才生效。
	eventually(t, time.Minute, func() error {
		if err := denied["another Node"](); err == nil {
			return fmt.Errorf("policy not in effect yet")
		}
		return nil
	})
	for name, write := range denied {
		t.Run("compute agent cannot write "+name, func(t *testing.T) {
			err := write()
			if err == nil {
				t.Fatal("allowed")
			}
			if !strings.Contains(err.Error(), "may only") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func hasCondition(n corev1.Node, t corev1.NodeConditionType) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func lease(name string) *coordinationv1ac.LeaseApplyConfiguration {
	return coordinationv1ac.Lease(name, corev1.NamespaceNodeLease).
		WithSpec(coordinationv1ac.LeaseSpec().WithHolderIdentity(host).WithRenewTime(metav1.NewMicroTime(time.Now())))
}
