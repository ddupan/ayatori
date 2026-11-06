//go:build integration

package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coordinationv1ac "k8s.io/client-go/applyconfigurations/coordination/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/agent"
	"go.ddupan.top/ayatori/internal/infra/clientcert"
	nodeagent "go.ddupan.top/ayatori/internal/infra/node"
	"go.ddupan.top/ayatori/internal/testenv"
)

// TestAgentIdentity：Agent 以 bootstrap token 加入（ADR-0021），之后只凭自己的证书工作；deploy/flux/nodes
// 与 deploy/flux/compute 的 RBAC 与准入策略让它只能写自己，且只能写 compute 角色的部分。
func TestAgentIdentity(t *testing.T) {
	ctx := t.Context()
	authority := testenv.NewClientCA(t)
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}}
	env.ControlPlane.GetAPIServer().Authn = authority
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(cfg)
	for _, path := range []string{
		filepath.Join("..", "..", "infra", "clientcert", "testdata", "rbac.yaml"),
		filepath.Join("..", "..", "..", "deploy", "flux", "nodes", "rbac.yaml"),
		filepath.Join("..", "..", "..", "deploy", "flux", "nodes", "admission.yaml"),
		filepath.Join("..", "..", "..", "deploy", "flux", "compute", "rbac.yaml"),
		filepath.Join("..", "..", "..", "deploy", "flux", "compute", "admission.yaml"),
	} {
		testenv.ApplyManifest(t, cfg, path)
	}
	go authority.SignApproved(ctx, admin, time.Hour)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)
	k, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	l := connect(t)
	defer func() { _, _ = l.Close() }()

	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	if err := os.Mkdir(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	const tokenID = "nodeaa"
	testenv.BootstrapToken(t, admin, cfg, creds, tokenID)
	running := runAgent(t, agent.Config{
		NodeName: node, CredentialsDir: creds, StateDir: filepath.Join(dir, "state"),
		LibvirtURI: uri, ResyncInterval: metav1.Duration{Duration: time.Second},
	})
	defer running.stop()

	approve, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := clientcert.ApproveBootstrap(approve, admin, tokenID, nodeagent.UserPrefix+node,
		[]string{nodeagent.NodesGroup, nodeagent.RoleGroupPrefix + agent.Role}, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	t.Run("registers its Node, role and lease with its own certificate", func(t *testing.T) {
		eventually(t, time.Minute, func() error {
			var n corev1.Node
			if err := k.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
				return err
			}
			if _, ok := n.Labels[nodeagent.RoleLabelPrefix+agent.Role]; !ok || !ready(n) {
				return fmt.Errorf("node not registered yet: %v %+v", n.Labels, n.Status.Conditions)
			}
			return k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: node + "-" + agent.Role}, &coordinationv1.Lease{})
		})
	})

	t.Run("writes the status of what is assigned to it", func(t *testing.T) {
		create(t, k, &computev1alpha1.LibvirtNetwork{Name: identNet, Spec: computev1alpha1.LibvirtNetworkSpec{
			XML: `<network><forward mode='bridge'/><bridge name='br-ident'/></network>`}})
		mine := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: identMine, Spec: computev1alpha1.LibvirtDomainSpec{
			NodeName: node, DomainXML: domainXML("")}}
		create(t, k, mine)
		vol := &computev1alpha1.LibvirtStorageVolume{Name: identMine, Spec: computev1alpha1.LibvirtStorageVolumeSpec{
			NodeName: node, Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: "missing", XML: "<volume><capacity>1</capacity></volume>"}}}
		create(t, k, vol)
		eventually(t, time.Minute, func() error {
			// pool 不存在，创建失败也要回写原因。
			if err := k.Get(ctx, client.ObjectKeyFromObject(vol), vol); err != nil {
				return err
			}
			if vol.Status.Message == "" {
				return fmt.Errorf("volume status = %+v", vol.Status)
			}
			var n computev1alpha1.LibvirtNetwork
			if err := k.Get(ctx, client.ObjectKey{Name: identNet}, &n); err != nil {
				return err
			}
			if err := k.Get(ctx, client.ObjectKeyFromObject(mine), mine); err != nil {
				return err
			}
			if len(n.Status.Nodes) != 1 || mine.Status.NodeName != node {
				return fmt.Errorf("network %+v, domain %+v", n.Status, mine.Status)
			}
			return nil
		})
	})

	// 以 node-a 的证书越权：全部被拒绝。
	agentConfig, err := clientcert.RESTConfig(creds)
	if err != nil {
		t.Fatal(err)
	}
	as, err := client.New(agentConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	owner := client.FieldOwner("intruder")
	theirs := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "ident-theirs", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: other, DomainXML: domainXML("")}}
	create(t, k, theirs)
	theirVolume := &computev1alpha1.LibvirtStorageVolume{Name: "ident-theirs", Spec: computev1alpha1.LibvirtStorageVolumeSpec{
		NodeName: other, Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: "missing", XML: "<volume><capacity>1</capacity></volume>"}}}
	create(t, k, theirVolume)

	denied := map[string]func() error{
		"another Node": func() error {
			return as.Apply(ctx, corev1ac.Node(other).WithLabels(map[string]string{"x": "y"}), owner)
		},
		"the lease named after the node only": func() error {
			return as.Apply(ctx, lease(node), owner)
		},
		"another node's role lease": func() error {
			return as.Apply(ctx, lease(other+"-"+agent.Role), owner)
		},
		"another role's label": func() error {
			return as.Apply(ctx, corev1ac.Node(node).WithLabels(map[string]string{nodeagent.RoleLabelPrefix + "loadbalancer": ""}), owner)
		},
		"another role's condition": func() error {
			return as.Status().Apply(ctx, corev1ac.Node(node).WithStatus(corev1ac.NodeStatus().WithConditions(
				corev1ac.NodeCondition().WithType(nodeagent.ConditionType("loadbalancer")).WithStatus(corev1.ConditionTrue))), owner)
		},
		"the status of a domain assigned to another node": func() error {
			d := theirs.DeepCopy()
			d.Status.NodeName = node
			return as.Status().Update(ctx, d)
		},
		"the status of a volume assigned to another node": func() error {
			v := theirVolume.DeepCopy()
			v.Status.Path = "/x"
			return as.Status().Update(ctx, v)
		},
		"another node as the running node of its own domain": func() error {
			var d computev1alpha1.LibvirtDomain
			if err := k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceDefault, Name: identMine}, &d); err != nil {
				return err
			}
			d.Status.NodeName = other
			return as.Status().Update(ctx, &d)
		},
		"another node's entry in a network status": func() error {
			var n computev1alpha1.LibvirtNetwork
			if err := k.Get(ctx, client.ObjectKey{Name: identNet}, &n); err != nil {
				return err
			}
			n.Status.Nodes = append(n.Status.Nodes, computev1alpha1.NodeObjectStatus{NodeName: other, Active: true})
			return as.Status().Update(ctx, &n)
		},
		"a domain spec": func() error {
			var d computev1alpha1.LibvirtDomain
			if err := k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceDefault, Name: identMine}, &d); err != nil {
				return err
			}
			d.Spec.RunState = computev1alpha1.RunStateStopped
			return as.Update(ctx, &d)
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
		t.Run("cannot write "+name, func(t *testing.T) {
			err := write()
			if err == nil {
				t.Fatal("allowed")
			}
			if !strings.Contains(err.Error(), "may only") && !strings.Contains(err.Error(), "not assigned") &&
				!strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

const (
	identNet  = "ident-net"
	identMine = "ident-mine"
	other     = "node-b"
)

func lease(name string) *coordinationv1ac.LeaseApplyConfiguration {
	return coordinationv1ac.Lease(name, corev1.NamespaceNodeLease).
		WithSpec(coordinationv1ac.LeaseSpec().WithHolderIdentity(node))
}
