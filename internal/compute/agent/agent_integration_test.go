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
)

const node = "node-a"

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

func TestAgent(t *testing.T) {
	ctx := t.Context()
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
	l := connect(t)
	defer func() { _, _ = l.Close() }()

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
	config := agent.Config{
		NodeName: node, Kubeconfig: kubeconfig, StateDir: filepath.Join(dir, "state"),
		LibvirtURI:     uri,
		ResyncInterval: metav1.Duration{Duration: time.Second},
	}

	running := runAgent(t, config)
	// Agent 注册 Node；管理员再打上 label，network/pool 的 nodeSelector 据此选择节点。
	eventually(t, 30*time.Second, func() error {
		var n corev1.Node
		if err := k.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
			return err
		}
		if n.Labels[agent.ManagedByLabel] == "" || len(n.Status.Conditions) == 0 || n.Status.Conditions[0].Status != corev1.ConditionTrue {
			return fmt.Errorf("node not ready yet: %+v", n.Status.Conditions)
		}
		n.Labels["rack"] = "a"
		return k.Update(ctx, &n)
	})
	eventually(t, 30*time.Second, func() error {
		return k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: node}, &coordinationv1.Lease{})
	})

	create(t, k, &computev1alpha1.LibvirtNetwork{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: computev1alpha1.LibvirtNetworkSpec{
		XML: `<network><name>ignored</name><forward mode='bridge'/><bridge name='br-lan'/></network>`, NodeSelector: map[string]string{"rack": "a"}}})
	create(t, k, &computev1alpha1.LibvirtNetwork{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}, Spec: computev1alpha1.LibvirtNetworkSpec{
		XML: `<network><forward mode='bridge'/><bridge name='br-b'/></network>`, NodeSelector: map[string]string{"rack": "b"}}})
	create(t, k, &computev1alpha1.LibvirtStoragePool{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Spec: computev1alpha1.LibvirtStoragePoolSpec{
		XML: `<pool type='dir'><target><path>/images</path></target></pool>`}})

	eventually(t, 30*time.Second, func() error {
		pool, err := l.LookupStoragePoolByName("local")
		if err != nil {
			return err
		}
		defer pool.Free()
		vol, err := pool.StorageVolCreateXML(`<volume><name>disk1</name><capacity>1048576</capacity></volume>`, 0)
		if err == nil {
			vol.Free()
		}
		return err
	})
	vm1 := &computev1alpha1.LibvirtDomain{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "vm1"}, Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(65536, `<disk type='volume' device='disk'><source pool='local' volume='disk1'/><target dev='vda'/></disk><interface type='network'><source network='lan'/><target dev='vnet7'/></interface>`)}}
	create(t, k, vm1)

	t.Run("defines objects selected for this node and starts the domain", func(t *testing.T) {
		eventually(t, 30*time.Second, func() error {
			if err := k.Get(ctx, client.ObjectKeyFromObject(vm1), vm1); err != nil {
				return err
			}
			if vm1.Status.State != "running" || vm1.Status.NodeName != node {
				return fmt.Errorf("domain status = %+v", vm1.Status)
			}
			return nil
		})
		dom, err := l.LookupDomainByName("default_vm1")
		if err != nil {
			t.Fatal(err)
		}
		defer dom.Free()
		xml, _ := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
		for _, want := range []string{"<uuid>" + string(vm1.UID) + "</uuid>", "<source file='/images/disk1'", "<target dev='ay" + strings.ReplaceAll(string(vm1.UID), "-", "")[:8] + "n0'"} {
			if !strings.Contains(xml, want) {
				t.Errorf("domain XML lacks %s:\n%s", want, xml)
			}
		}
		if autostart, _ := dom.GetAutostart(); autostart {
			t.Error("domain autostart must be off")
		}
		var n computev1alpha1.LibvirtNetwork
		eventually(t, 10*time.Second, func() error {
			if err := k.Get(ctx, client.ObjectKey{Name: "lan"}, &n); err != nil {
				return err
			}
			if len(n.Status.Nodes) != 1 || n.Status.Nodes[0].NodeName != node || !n.Status.Nodes[0].Active {
				return fmt.Errorf("network status = %+v", n.Status)
			}
			return nil
		})
		if _, err := l.LookupNetworkByName("elsewhere"); !libvirt.IsNotFound(err) {
			t.Errorf("network for another rack was defined here: %v", err)
		}
	})

	t.Run("restores configuration changed outside Ayatori", func(t *testing.T) {
		dom, err := l.LookupDomainByName("default_vm1")
		if err != nil {
			t.Fatal(err)
		}
		defer dom.Free()
		xml, _ := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
		if d, err := l.DomainDefineXML(strings.Replace(xml, "<memory unit='KiB'>65536</memory>", "<memory unit='KiB'>131072</memory>", 1)); err != nil {
			t.Fatal(err)
		} else {
			d.Free()
		}
		eventually(t, 15*time.Second, func() error {
			xml, err := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
			if err != nil || !strings.Contains(xml, "<memory unit='KiB'>65536</memory>") {
				return fmt.Errorf("memory not restored (err=%v)", err)
			}
			return nil
		})
	})

	t.Run("does not restart an instance shut down on purpose", func(t *testing.T) {
		dom, err := l.LookupDomainByName("default_vm1")
		if err != nil {
			t.Fatal(err)
		}
		defer dom.Free()
		if err := dom.Shutdown(); err != nil {
			t.Fatal(err)
		}
		eventually(t, 15*time.Second, func() error {
			if err := k.Get(ctx, client.ObjectKeyFromObject(vm1), vm1); err != nil {
				return err
			}
			if vm1.Status.Reason != "shutdown" {
				return fmt.Errorf("domain status = %+v", vm1.Status)
			}
			return nil
		})
		time.Sleep(3 * time.Second) // 几轮 reconcile
		if state, _, _ := dom.GetState(); state != golibvirt.DOMAIN_SHUTOFF {
			t.Fatalf("domain restarted after an intentional shutdown: state %d", state)
		}
		// 运行时开机（ADR-0016 的控制台操作）之后，意图回到"运行"。
		if err := dom.Create(); err != nil {
			t.Fatal(err)
		}
		eventually(t, 15*time.Second, func() error {
			if err := k.Get(ctx, client.ObjectKeyFromObject(vm1), vm1); err != nil {
				return err
			}
			if vm1.Status.State != "running" {
				return fmt.Errorf("domain status = %+v", vm1.Status)
			}
			return nil
		})
	})

	vm2 := &computev1alpha1.LibvirtDomain{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "vm2"}, Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(65536, `<interface type='network'><source network='lan'/></interface>`)}}
	create(t, k, vm2)
	eventually(t, 30*time.Second, func() error {
		if err := k.Get(ctx, client.ObjectKeyFromObject(vm2), vm2); err != nil {
			return err
		}
		if vm2.Status.State != "running" {
			return fmt.Errorf("vm2 status = %+v", vm2.Status)
		}
		return nil
	})

	t.Run("converges from local state when the control plane is unreachable", func(t *testing.T) {
		// 宿主机重启：关闭所有连接，test 驱动的状态随之清空。
		running.stop()
		_, _ = l.Close()
		l = connect(t)
		if _, err := l.LookupNetworkByName("lan"); !libvirt.IsNotFound(err) {
			t.Fatalf("libvirt state survived the simulated reboot: %v", err)
		}
		offline := config
		offline.Kubeconfig = unreachable
		running = runAgent(t, offline)
		eventually(t, 30*time.Second, func() error {
			n, err := l.LookupNetworkByName("lan")
			if err != nil {
				return err
			}
			defer n.Free()
			if active, _ := n.IsActive(); !active {
				return fmt.Errorf("network lan not active")
			}
			p, err := l.LookupStoragePoolByName("local")
			if err != nil {
				return err
			}
			p.Free()
			dom, err := l.LookupDomainByName("default_vm2")
			if err != nil {
				return err
			}
			defer dom.Free()
			if state, _, _ := dom.GetState(); state != golibvirt.DOMAIN_RUNNING {
				return fmt.Errorf("vm2 not running: state %d", state)
			}
			return nil
		})
		// vm1 的卷随宿主机"丢失"了：无法下发，但不会因此被删除。
		if _, err := l.LookupDomainByName("default_vm1"); !libvirt.IsNotFound(err) {
			t.Fatalf("vm1 should not be defined without its volume: %v", err)
		}
	})

	t.Run("removes objects only once the API confirms deletion", func(t *testing.T) {
		// 控制面仍不可用：删除 API 中的对象，Agent 看不到，什么也不会发生。
		if err := k.Delete(ctx, vm2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		if _, err := l.LookupDomainByName("default_vm2"); err != nil {
			t.Fatalf("vm2 removed while the API was unreachable: %v", err)
		}
		// 控制面恢复：首次同步后发现 vm2 已不存在，清理。
		running.stop()
		running = runAgent(t, config)
		eventually(t, 30*time.Second, func() error {
			if _, err := l.LookupDomainByName("default_vm2"); !libvirt.IsNotFound(err) {
				return fmt.Errorf("vm2 still defined: %v", err)
			}
			return nil
		})
		// 在线时删除：watch 到删除后清理。
		if err := k.Delete(ctx, &computev1alpha1.LibvirtNetwork{ObjectMeta: metav1.ObjectMeta{Name: "lan"}}); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, func() error {
			if _, err := l.LookupNetworkByName("lan"); !libvirt.IsNotFound(err) {
				return fmt.Errorf("network lan still defined: %v", err)
			}
			return nil
		})
	})
	running.stop()
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
