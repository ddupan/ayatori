//go:build integration

package agent_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	golibvirt "libvirt.org/go/libvirt"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/agent"
	nodeagent "go.ddupan.top/ayatori/internal/infra/node"
)

const (
	node    = "node-a"
	lan     = "lan"
	running = "running"
)

// test:///default 在进程内由所有连接共享；最后一个连接关闭后状态清空，用来模拟宿主机重启。
const uri = "test:///default"

func connect(t *testing.T) *golibvirt.Connect {
	t.Helper()
	conn, err := golibvirt.NewConnect(uri)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

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

// free 释放 libvirt 对象的引用。
func free(f func() error) { _ = f() }

type runningAgent struct {
	cancel func()
	done   chan struct{}
}

func (r runningAgent) stop() { r.cancel(); <-r.done }

func runAgent(t *testing.T, config agent.Config) runningAgent {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	a, err := agent.New(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	return runningAgent{cancel, done}
}

// suite 是各场景共享的环境。场景按顺序依次运行，后一个建立在前一个的结果之上。
type suite struct {
	k           client.Client
	l           *golibvirt.Connect
	config      agent.Config
	unreachable string
	agent       runningAgent
	vm1, vm2    *computev1alpha1.LibvirtDomain
}

func TestAgent(t *testing.T) {
	s := setup(t)
	defer func() { _, _ = s.l.Close() }()
	s.agent = runAgent(t, s.config)
	s.registerNode(t)
	s.createObjects(t)
	t.Run("defines objects selected for this node and starts the domain", s.definesSelected)
	t.Run("restores configuration changed outside Ayatori", s.restoresDrift)
	t.Run("does not restart an instance shut down on purpose", s.keepsIntentionalShutdown)
	s.vm2 = &computev1alpha1.LibvirtDomain{Namespace: "default", Name: "vm2", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(65536, `<interface type='network'><source network='lan'/></interface>`)}}
	create(t, s.k, s.vm2)
	s.waitState(t, s.vm2, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running })
	t.Run("converges from local state when the control plane is unreachable", s.convergesOffline)
	t.Run("removes objects only once the API confirms deletion", s.removesAfterConfirmation)
	s.agent.stop()
}

func setup(t *testing.T) *suite {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)
	k, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, env.KubeConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	// 指向一个不存在的 apiserver：用于验证控制面不可用时 Agent 仍按本地状态收敛。
	unreachable := filepath.Join(dir, "unreachable")
	broken := strings.Replace(string(env.KubeConfig), cfg.Host, "https://127.0.0.1:1", 1)
	if err := os.WriteFile(unreachable, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	return &suite{
		k: k, l: connect(t), unreachable: unreachable,
		config: agent.Config{
			NodeName: node, Kubeconfig: kubeconfig, StateDir: filepath.Join(dir, "state"),
			LibvirtURI:     uri,
			ResyncInterval: metav1.Duration{Duration: time.Second},
		},
	}
}

// registerNode 等 Agent 注册 Node；管理员再打上 label，network/pool 的 nodeSelector 据此选择节点。
func (s *suite) registerNode(t *testing.T) {
	ctx := t.Context()
	eventually(t, 30*time.Second, func() error {
		var n corev1.Node
		if err := s.k.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
			return err
		}
		if _, ok := n.Labels[nodeagent.RoleLabelPrefix+agent.Role]; !ok || !ready(n) {
			return fmt.Errorf("node not ready yet: %+v", n.Status.Conditions)
		}
		n.Labels["rack"] = "a"
		return s.k.Update(ctx, &n)
	})
	eventually(t, 30*time.Second, func() error {
		return s.k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: node + "-" + agent.Role}, &coordinationv1.Lease{})
	})
}

func (s *suite) createObjects(t *testing.T) {
	create(t, s.k, &computev1alpha1.LibvirtNetwork{Name: lan, Spec: computev1alpha1.LibvirtNetworkSpec{
		XML: `<network><name>ignored</name><forward mode='bridge'/><bridge name='br-lan'/></network>`, NodeSelector: map[string]string{"rack": "a"}}})
	create(t, s.k, &computev1alpha1.LibvirtNetwork{Name: "elsewhere", Spec: computev1alpha1.LibvirtNetworkSpec{
		XML: `<network><forward mode='bridge'/><bridge name='br-b'/></network>`, NodeSelector: map[string]string{"rack": "b"}}})
	create(t, s.k, &computev1alpha1.LibvirtStoragePool{Name: "local", Spec: computev1alpha1.LibvirtStoragePoolSpec{
		XML: `<pool type='dir'><target><path>/images</path></target></pool>`}})
	eventually(t, 30*time.Second, func() error {
		pool, err := s.l.LookupStoragePoolByName("local")
		if err != nil {
			return err
		}
		defer free(pool.Free)
		vol, err := pool.StorageVolCreateXML(`<volume><name>disk1</name><capacity>1048576</capacity></volume>`, 0)
		if err == nil {
			free(vol.Free)
		}
		return err
	})
	s.vm1 = &computev1alpha1.LibvirtDomain{Namespace: "default", Name: "vm1", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(65536, `<disk type='volume' device='disk'><source pool='local' volume='disk1'/><target dev='vda'/></disk><interface type='network'><source network='lan'/><target dev='vnet7'/></interface>`)}}
	create(t, s.k, s.vm1)
}

func (s *suite) definesSelected(t *testing.T) {
	s.waitState(t, s.vm1, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running && st.NodeName == node })
	dom := s.domain(t, "default_vm1")
	defer free(dom.Free)
	xml, _ := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
	for _, want := range []string{"<uuid>" + string(s.vm1.UID) + "</uuid>", "<source file='/images/disk1'", "<target dev='ay" + strings.ReplaceAll(string(s.vm1.UID), "-", "")[:8] + "n0'"} {
		if !strings.Contains(xml, want) {
			t.Errorf("domain XML lacks %s:\n%s", want, xml)
		}
	}
	if autostart, _ := dom.GetAutostart(); autostart {
		t.Error("domain autostart must be off")
	}
	var n computev1alpha1.LibvirtNetwork
	eventually(t, 10*time.Second, func() error {
		if err := s.k.Get(t.Context(), client.ObjectKey{Name: lan}, &n); err != nil {
			return err
		}
		if len(n.Status.Nodes) != 1 || n.Status.Nodes[0].NodeName != node || !n.Status.Nodes[0].Active {
			return fmt.Errorf("network status = %+v", n.Status)
		}
		return nil
	})
	if _, err := s.l.LookupNetworkByName("elsewhere"); !libvirt.IsNotFound(err) {
		t.Errorf("network for another rack was defined here: %v", err)
	}
}

func (s *suite) restoresDrift(t *testing.T) {
	dom := s.domain(t, "default_vm1")
	defer free(dom.Free)
	xml, _ := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
	d, err := s.l.DomainDefineXML(strings.Replace(xml, "<memory unit='KiB'>65536</memory>", "<memory unit='KiB'>131072</memory>", 1))
	if err != nil {
		t.Fatal(err)
	}
	free(d.Free)
	eventually(t, 15*time.Second, func() error {
		xml, err := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
		if err != nil || !strings.Contains(xml, "<memory unit='KiB'>65536</memory>") {
			return fmt.Errorf("memory not restored (err=%v)", err)
		}
		return nil
	})
}

func (s *suite) keepsIntentionalShutdown(t *testing.T) {
	dom := s.domain(t, "default_vm1")
	defer free(dom.Free)
	if err := dom.Shutdown(); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, s.vm1, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.Reason == "shutdown" })
	time.Sleep(3 * time.Second) // 几轮 reconcile
	if state, _, _ := dom.GetState(); state != golibvirt.DOMAIN_SHUTOFF {
		t.Fatalf("domain restarted after an intentional shutdown: state %d", state)
	}
	// 运行时开机（ADR-0016 的控制台操作）之后，意图回到"运行"。
	if err := dom.Create(); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, s.vm1, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running })
}

func (s *suite) convergesOffline(t *testing.T) {
	// 宿主机重启：关闭所有连接，test 驱动的状态随之清空。
	s.agent.stop()
	_, _ = s.l.Close()
	s.l = connect(t)
	if _, err := s.l.LookupNetworkByName(lan); !libvirt.IsNotFound(err) {
		t.Fatalf("libvirt state survived the simulated reboot: %v", err)
	}
	offline := s.config
	offline.Kubeconfig = s.unreachable
	s.agent = runAgent(t, offline)
	eventually(t, 30*time.Second, func() error {
		n, err := s.l.LookupNetworkByName(lan)
		if err != nil {
			return err
		}
		defer free(n.Free)
		if active, _ := n.IsActive(); !active {
			return fmt.Errorf("network lan not active")
		}
		p, err := s.l.LookupStoragePoolByName("local")
		if err != nil {
			return err
		}
		free(p.Free)
		dom, err := s.l.LookupDomainByName("default_vm2")
		if err != nil {
			return err
		}
		defer free(dom.Free)
		if state, _, _ := dom.GetState(); state != golibvirt.DOMAIN_RUNNING {
			return fmt.Errorf("vm2 not running: state %d", state)
		}
		return nil
	})
	// vm1 的卷随宿主机"丢失"了：无法下发，但不会因此被删除。
	if _, err := s.l.LookupDomainByName("default_vm1"); !libvirt.IsNotFound(err) {
		t.Fatalf("vm1 should not be defined without its volume: %v", err)
	}
}

func (s *suite) removesAfterConfirmation(t *testing.T) {
	ctx := t.Context()
	// 控制面仍不可用：删除 API 中的对象，Agent 看不到，什么也不会发生。
	if err := s.k.Delete(ctx, s.vm2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if _, err := s.l.LookupDomainByName("default_vm2"); err != nil {
		t.Fatalf("vm2 removed while the API was unreachable: %v", err)
	}
	// 控制面恢复：首次同步后发现 vm2 已不存在，清理。
	s.agent.stop()
	s.agent = runAgent(t, s.config)
	eventually(t, 30*time.Second, func() error {
		if _, err := s.l.LookupDomainByName("default_vm2"); !libvirt.IsNotFound(err) {
			return fmt.Errorf("vm2 still defined: %v", err)
		}
		return nil
	})
	// 在线时删除：watch 到删除后清理。
	if err := s.k.Delete(ctx, &computev1alpha1.LibvirtNetwork{Name: lan}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, func() error {
		if _, err := s.l.LookupNetworkByName(lan); !libvirt.IsNotFound(err) {
			return fmt.Errorf("network lan still defined: %v", err)
		}
		return nil
	})
}

func (s *suite) domain(t *testing.T, name string) *golibvirt.Domain {
	t.Helper()
	dom, err := s.l.LookupDomainByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return dom
}

func (s *suite) waitState(t *testing.T, d *computev1alpha1.LibvirtDomain, ok func(computev1alpha1.LibvirtDomainStatus) bool) {
	t.Helper()
	eventually(t, 30*time.Second, func() error {
		if err := s.k.Get(t.Context(), client.ObjectKeyFromObject(d), d); err != nil {
			return err
		}
		if !ok(d.Status) {
			return fmt.Errorf("%s status = %+v", d.Name, d.Status)
		}
		return nil
	})
}

// ready 报告 Node 上 compute 角色的就绪 condition。
func ready(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == nodeagent.ConditionType(agent.Role) {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func domainXML(memoryKiB int, devices string) string {
	return fmt.Sprintf(`<domain type='test'><name>x</name><memory unit='KiB'>%d</memory><os><type>hvm</type></os><devices>%s</devices></domain>`, memoryKiB, devices)
}

func create(t *testing.T, k client.Client, obj client.Object) {
	t.Helper()
	if err := k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}
