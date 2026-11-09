//go:build integration

package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	rack    = "rack"
	// localPool 是测试用的本地 dir pool。
	localPool = "local"
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
	cancel   func()
	done     chan struct{}
	registry *prometheus.Registry
}

func (r runningAgent) stop() { r.cancel(); <-r.done }

func runAgent(t *testing.T, config agent.Config) runningAgent {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	registry := prometheus.NewRegistry()
	a, err := agent.New(config, registry, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	a.Containers = containers
	go func() { a.Run(ctx); close(done) }()
	return runningAgent{cancel, done, registry}
}

// metric 返回名为 name、标签恰好为 labels 的 gauge 或 counter 的值；不存在时 ok 为 false。
func (r runningAgent) metric(t *testing.T, name string, labels map[string]string) (value float64, ok bool) {
	t.Helper()
	families, err := r.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if maps.Equal(got, labels) {
				return m.GetGauge().GetValue() + m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// families 列出名字以 prefix 开头的序列，用于记录 test 驱动实际给出了哪些数据。
func (r runningAgent) families(t *testing.T, prefix string) []string {
	t.Helper()
	families, err := r.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), prefix) {
			for _, m := range f.GetMetric() {
				out = append(out, fmt.Sprintf("%s%v", f.GetName(), m.GetLabel()))
			}
		}
	}
	return out
}

// expectMetric 等到 gauge 满足 ok；ok 收到 found=false 表示序列不存在。
func (s *suite) expectMetric(t *testing.T, name string, labels map[string]string, ok func(value float64, found bool) bool) {
	t.Helper()
	eventually(t, 10*time.Second, func() error {
		if v, found := s.agent.metric(t, name, labels); !ok(v, found) {
			return fmt.Errorf("%s%v = %v (found %v)", name, labels, v, found)
		}
		return nil
	})
}

func is(want float64) func(float64, bool) bool {
	return func(v float64, found bool) bool { return found && v == want }
}

func absent(_ float64, found bool) bool { return !found }

const (
	shouldRunMetric   = "ayatori_compute_agent_domain_should_run"
	runningMetric     = "ayatori_compute_agent_domain_running"
	lastSuccessMetric = "ayatori_compute_agent_object_last_success_timestamp_seconds"
)

// domainLabels 是实例指标的标签：与对象相同，但没有 kind。
func domainLabels(name string) map[string]string {
	l := objectLabels("", corev1.NamespaceDefault, name)
	delete(l, "kind")
	return l
}

func objectLabels(kind, namespace, name string) map[string]string {
	return map[string]string{"kind": kind, "namespace": namespace, "name": name}
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
	t.Run("creates volumes and reclaims them by policy", s.managesVolumes)
	t.Run("creates volumes by size and only lets size grow", s.managesVolumeSize)
	t.Run("keeps a replica of each image and creates volumes from it", func(sub *testing.T) { s.managesImages(sub, t) })
	t.Run("runs containers and restarts them when they stop", s.managesContainers)
	s.vm2 = &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "vm2", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(""), Interfaces: []computev1alpha1.DomainInterface{{Network: lan}}}}
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
			NodeLabels:     map[string]string{rack: "a"},
		},
	}
}

// registerNode 等 Agent 注册 Node 并写上自己决定的 label：发现的，加配置中的 rack；network/pool 的
// nodeSelector 据 rack 选择节点。
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
		want := map[string]string{
			corev1.LabelHostname: node, corev1.LabelArchStable: goruntime.GOARCH, corev1.LabelOSStable: goruntime.GOOS,
			rack: "a",
		}
		for k, v := range want {
			if n.Labels[k] != v {
				return fmt.Errorf("label %s: want %q, got %v", k, v, n.Labels)
			}
		}
		t.Logf("node labels: %v", n.Labels)
		return nil
	})
	eventually(t, 30*time.Second, func() error {
		return s.k.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: node + "-" + agent.Role}, &coordinationv1.Lease{})
	})
}

func (s *suite) createObjects(t *testing.T) {
	create(t, s.k, &computev1alpha1.LibvirtNetwork{Name: lan, Spec: computev1alpha1.LibvirtNetworkSpec{
		XML: `<network><name>ignored</name><forward mode='bridge'/><bridge name='br-lan'/></network>`, NodeSelector: map[string]string{rack: "a"}}})
	create(t, s.k, &computev1alpha1.LibvirtNetwork{Name: "elsewhere", Spec: computev1alpha1.LibvirtNetworkSpec{
		XML: `<network><forward mode='bridge'/><bridge name='br-b'/></network>`, NodeSelector: map[string]string{rack: "b"}}})
	create(t, s.k, &computev1alpha1.LibvirtStoragePool{Name: localPool, Spec: computev1alpha1.LibvirtStoragePoolSpec{
		XML: `<pool type='dir'><target><path>/images</path></target></pool>`}})
	create(t, s.k, &computev1alpha1.LibvirtStorageVolume{Name: "disk1", Spec: computev1alpha1.LibvirtStorageVolumeSpec{
		NodeName: node, Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: localPool, XML: `<volume><capacity>1048576</capacity></volume>`}}})
	s.vm1 = &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "vm1", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(""),
		Disks:      []computev1alpha1.DomainDisk{{Volume: "disk1"}},
		Interfaces: []computev1alpha1.DomainInterface{{Network: lan, MAC: "52:54:00:00:00:01"}}}}
	create(t, s.k, s.vm1)
}

func (s *suite) definesSelected(t *testing.T) {
	s.waitState(t, s.vm1, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running && st.NodeName == node })
	dom := s.domain(t, "default_vm1")
	defer free(dom.Free)
	xml, _ := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
	for _, want := range []string{"<uuid>" + string(s.vm1.UID) + "</uuid>", "<source file='/images/disk1'", "<mac address='52:54:00:00:00:01'/>", "<target dev='ay" + strings.ReplaceAll(string(s.vm1.UID), "-", "")[:8] + "n0'"} {
		if !strings.Contains(xml, want) {
			t.Errorf("domain XML lacks %s:\n%s", want, xml)
		}
	}
	if autostart, _ := dom.GetAutostart(); autostart {
		t.Error("domain autostart must be off")
	}

	// Ayatori 拥有的元素写在 domainXML 中：拒绝下发并说明原因，不静默覆盖。
	owned := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "owned", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(`<interface type='network'><source network='lan'/></interface>`)}}
	create(t, s.k, owned)
	eventually(t, 30*time.Second, func() error {
		if err := s.k.Get(t.Context(), client.ObjectKeyFromObject(owned), owned); err != nil {
			return err
		}
		if !strings.Contains(owned.Status.Message, "must not contain <interface>") {
			return fmt.Errorf("status = %+v", owned.Status)
		}
		return nil
	})
	if _, err := s.l.LookupDomainByName("default_owned"); !libvirt.IsNotFound(err) {
		t.Errorf("domain with Ayatori-owned elements in domainXML was defined: %v", err)
	}
	if err := s.k.Delete(t.Context(), owned); err != nil {
		t.Fatal(err)
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
	s.expectMetric(t, "ayatori_compute_agent_libvirt_up", map[string]string{}, is(1))
	s.expectMetric(t, shouldRunMetric, domainLabels("vm1"), is(1))
	s.expectMetric(t, runningMetric, domainLabels("vm1"), is(1))
	s.expectMetric(t, lastSuccessMetric,
		objectLabels("LibvirtNetwork", "", lan),
		func(v float64, found bool) bool { return found && time.Since(time.Unix(int64(v), 0)) < time.Minute })
	s.expectMetric(t, "ayatori_compute_agent_pool_bytes", map[string]string{"name": localPool, "type": "capacity"},
		func(v float64, found bool) bool { return found && v > 0 })
	// 资源用量来自 GetAllDomainStats。
	s.expectMetric(t, "ayatori_compute_agent_domain_cpu_seconds_total", domainLabels("vm1"),
		func(_ float64, found bool) bool { return found })
	t.Logf("domain metrics: %v", s.agent.families(t, "ayatori_compute_agent_domain_"))
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
	// 有意关机不是"应运行却没有运行"，不应触发告警。
	s.expectMetric(t, shouldRunMetric, domainLabels("vm1"), is(0))
	s.expectMetric(t, runningMetric, domainLabels("vm1"), is(0))
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

func (s *suite) managesVolumes(t *testing.T) {
	ctx := t.Context()
	volume := func(name, node string, policy computev1alpha1.ReclaimPolicy) *computev1alpha1.LibvirtStorageVolume {
		return &computev1alpha1.LibvirtStorageVolume{Name: name, Spec: computev1alpha1.LibvirtStorageVolumeSpec{
			NodeName: node, ReclaimPolicy: policy, Libvirt: &computev1alpha1.LibvirtVolumeSource{
				Pool: localPool, XML: `<volume><name>ignored</name><capacity unit='MiB'>1</capacity></volume>`}}}
	}
	exists := func(name string) bool {
		pool, err := s.l.LookupStoragePoolByName(localPool)
		if err != nil {
			t.Fatal(err)
		}
		defer free(pool.Free)
		_ = pool.Refresh(0)
		vol, err := pool.LookupStorageVolByName(name)
		if err == nil {
			free(vol.Free)
		}
		return err == nil
	}
	waitPath := func(v *computev1alpha1.LibvirtStorageVolume) {
		t.Helper()
		eventually(t, 15*time.Second, func() error {
			if err := s.k.Get(ctx, client.ObjectKeyFromObject(v), v); err != nil {
				return err
			}
			if !strings.HasSuffix(v.Status.Path, "/"+v.Name) || v.Status.Capacity != 1<<20 {
				return fmt.Errorf("volume status = %+v", v.Status)
			}
			return nil
		})
	}

	disk := volume("disk2", node, computev1alpha1.ReclaimDelete)
	create(t, s.k, disk)
	create(t, s.k, volume("theirs", "node-b", computev1alpha1.ReclaimDelete))
	waitPath(disk)
	if exists("theirs") {
		t.Error("volume for another node was created here")
	}
	for _, patch := range []string{`{"spec":{"libvirt":{"pool":"elsewhere"}}}`, `{"spec":{"libvirt":null}}`, `{"spec":{"nodeName":"elsewhere"}}`} {
		if err := s.k.Patch(ctx, disk, client.RawPatch(types.MergePatchType, []byte(patch))); err == nil {
			t.Errorf("%s must be rejected: storage backend and node are immutable", patch)
		}
	}

	// 被实例引用的卷：删除对象后，等实例清理了才删卷。
	vm := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "vm3", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(""), Disks: []computev1alpha1.DomainDisk{{Volume: "disk2"}}}}
	create(t, s.k, vm)
	s.waitState(t, vm, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running })
	if err := s.k.Delete(ctx, disk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // 几轮 reconcile
	if !exists("disk2") {
		t.Fatal("volume deleted while a domain still references it")
	}
	if err := s.k.Delete(ctx, vm); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, func() error {
		if exists("disk2") {
			return fmt.Errorf("disk2 not deleted after its domain was removed")
		}
		return nil
	})

	// pool、卷和引用卷的实例同时删除：先清理实例，再删卷，最后才 undefine pool；pool 不能先于卷消失，
	// 否则卷无法删除，文件残留。
	const scratchPool = "scratch"
	create(t, s.k, &computev1alpha1.LibvirtStoragePool{Name: scratchPool, Spec: computev1alpha1.LibvirtStoragePoolSpec{
		XML: `<pool type='dir'><target><path>/scratch</path></target></pool>`}})
	pending := volume("pending", node, computev1alpha1.ReclaimDelete)
	pending.Spec.Libvirt.Pool = scratchPool
	create(t, s.k, pending)
	waitPath(pending)
	vm4 := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "vm4", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(""), Disks: []computev1alpha1.DomainDisk{{Volume: "pending"}}}}
	create(t, s.k, vm4)
	s.waitState(t, vm4, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running })
	scratchVolume := func() (bool, error) {
		pool, err := s.l.LookupStoragePoolByName(scratchPool)
		if err != nil {
			return false, err
		}
		defer free(pool.Free)
		vol, err := pool.LookupStorageVolByName("pending")
		if libvirt.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		free(vol.Free)
		return true, nil
	}
	for _, obj := range []client.Object{&computev1alpha1.LibvirtStoragePool{Name: scratchPool}, pending} {
		if err := s.k.Delete(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3 * time.Second)
	if found, err := scratchVolume(); err != nil || !found {
		t.Fatalf("pool removed or volume deleted while a domain still references the volume: found=%v err=%v", found, err)
	}
	if err := s.k.Delete(ctx, vm4); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, func() error {
		pool, err := s.l.LookupStoragePoolByName(scratchPool)
		if err == nil {
			free(pool.Free)
			return errors.New("pool scratch still defined")
		}
		if !libvirt.IsNotFound(err) {
			return err
		}
		return nil
	})
	// Retain：删除对象保留卷，同名对象再次创建时接管而不是重建。
	kept := volume("kept", node, computev1alpha1.ReclaimRetain)
	create(t, s.k, kept)
	waitPath(kept)
	if err := s.k.Delete(ctx, kept); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if !exists("kept") {
		t.Fatal("retained volume was deleted")
	}
	kept = volume("kept", node, computev1alpha1.ReclaimDelete)
	kept.Spec.Libvirt.XML = `<volume><capacity unit='MiB'>2</capacity></volume>` // 已存在就接管，不按新 XML 重建
	create(t, s.k, kept)
	waitPath(kept)
	// 清理，免得后面模拟宿主机重启时重新创建。
	if err := s.k.Delete(ctx, kept); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, func() error {
		if exists("kept") {
			return fmt.Errorf("kept not deleted")
		}
		return nil
	})
}

func (s *suite) convergesOffline(t *testing.T) {
	// 宿主机重启：关闭所有连接，test 驱动的状态随之清空。
	s.agent.stop()
	containers.reboot()
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
		p, err := s.l.LookupStoragePoolByName(localPool)
		if err != nil {
			return err
		}
		free(p.Free)
		// vm1 的卷由本地状态中的卷对象重新创建（test 驱动重启后卷是空的），随后 vm1 也能下发。
		for _, name := range []string{"default_vm1", "default_vm2"} {
			dom, err := s.l.LookupDomainByName(name)
			if err != nil {
				return err
			}
			state, _, _ := dom.GetState()
			free(dom.Free)
			if state != golibvirt.DOMAIN_RUNNING {
				return fmt.Errorf("%s not running: state %d", name, state)
			}
		}
		// 容器同样按本地状态拉起；rootfs 卷由本地状态中的卷对象重建后才能下发。
		if c, _ := containers.Container("default_" + ctKeep); c.State != stateRunning {
			return fmt.Errorf("container not running: %+v", c)
		}
		return nil
	})
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
	// 删除后不再导出它的指标，免得留下"应运行"的陈旧序列。
	s.expectMetric(t, shouldRunMetric, domainLabels("vm2"), absent)
	s.expectMetric(t, lastSuccessMetric, objectLabels("LibvirtDomain", corev1.NamespaceDefault, "vm2"), absent)
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

// managesVolumeSize 用 test 驱动验证 API 约束与按 size 创建；真实的离线、在线扩容在 Dev 环境验证。
func (s *suite) managesVolumeSize(t *testing.T) {
	ctx := t.Context()
	const name = "sized"
	size := resource.MustParse("1Mi")
	v := &computev1alpha1.LibvirtStorageVolume{Name: name, Spec: computev1alpha1.LibvirtStorageVolumeSpec{NodeName: node,
		Size: &size, Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: localPool}}}
	create(t, s.k, v)
	s.waitVolume(t, name, func(st computev1alpha1.LibvirtStorageVolumeStatus) bool { return st.Capacity == 1<<20 })

	invalid := []func(*computev1alpha1.LibvirtStorageVolume){
		func(v *computev1alpha1.LibvirtStorageVolume) { q := resource.MustParse("512Ki"); v.Spec.Size = &q },
		func(v *computev1alpha1.LibvirtStorageVolume) { v.Spec.Size = nil },
	}
	for i, change := range invalid {
		bad := v.DeepCopy()
		change(bad)
		if err := s.k.Update(ctx, bad); err == nil {
			t.Fatalf("invalid update %d accepted", i)
		}
	}
	zero := resource.MustParse("0")
	for i, spec := range []computev1alpha1.LibvirtStorageVolumeSpec{
		{NodeName: node, Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: localPool}},
		{NodeName: node, Size: &zero, Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: localPool}},
	} {
		bad := &computev1alpha1.LibvirtStorageVolume{Name: "invalid-size", Spec: spec}
		if err := s.k.Create(ctx, bad); err == nil {
			t.Fatalf("invalid volume %d accepted", i)
		}
	}
	if err := s.k.Delete(ctx, v); err != nil {
		t.Fatal(err)
	}
}

// managesImages 用 test 驱动验证 API、状态与文件；真实 QEMU 上的换盘与 qcow2 复制在 Dev 环境验证。
func (s *suite) managesImages(t, parent *testing.T) {
	ctx := t.Context()
	iso := func(tag string) []byte {
		b := make([]byte, 32769+64+len(tag))
		copy(b[32769:], "CD001")
		copy(b[32769+64:], tag)
		return b
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	const cd, root, imgPool, badImage, fromRoot, fromCD, pathV1 = "cd", "root", "imgs", "bad", "from-root", "from-cd", "/v1"
	v1, v2, disk := iso("v1"), iso("v2"), []byte("raw disk")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(map[string][]byte{pathV1: v1, "/v2": v2, "/disk": disk}[r.URL.Path])
	}))
	defer srv.Close()
	dir := t.TempDir()
	create(t, s.k, &computev1alpha1.LibvirtStoragePool{Name: imgPool, Spec: computev1alpha1.LibvirtStoragePoolSpec{
		XML: "<pool type='dir'><target><path>" + dir + "</path></target></pool>"}})
	image := func(name string, kind computev1alpha1.ImageType, path, sha string) *computev1alpha1.Image {
		return &computev1alpha1.Image{Name: name, Spec: computev1alpha1.ImageSpec{Type: kind, Pool: imgPool,
			Source: computev1alpha1.ImageSource{HTTP: &computev1alpha1.ImageHTTPSource{URL: srv.URL + path, SHA256: sha}}}}
	}
	fileIs := func(want []byte) {
		t.Helper()
		if got, err := os.ReadFile(filepath.Join(dir, cd)); err != nil || string(got) != string(want) {
			t.Fatalf("replica: %d bytes, err %v; want %d bytes", len(got), err, len(want))
		}
	}

	create(t, s.k, image(cd, computev1alpha1.ImageISO, pathV1, sum(v1)))
	s.waitImage(t, cd, func(st computev1alpha1.ImageNodeStatus) bool { return st.Ready && st.SHA256 == sum(v1) })
	fileIs(v1)

	vm := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "vm-cd", Spec: computev1alpha1.LibvirtDomainSpec{
		NodeName: node, DomainXML: domainXML(""), Disks: []computev1alpha1.DomainDisk{{Image: cd, Bus: computev1alpha1.DiskBusSATA}}}}
	create(t, s.k, vm)
	s.waitState(t, vm, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running })
	dom := s.domain(t, "default_vm-cd")
	defer free(dom.Free)
	xml, err := dom.GetXMLDesc(golibvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"device='cdrom'", "<source file='" + filepath.Join(dir, cd) + "'", "<readonly/>"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("domain XML lacks %s:\n%s", want, xml)
		}
	}

	// 更新：副本被替换，已挂载的实例继续运行。
	var img computev1alpha1.Image
	if err := s.k.Get(ctx, client.ObjectKey{Name: cd}, &img); err != nil {
		t.Fatal(err)
	}
	img.Spec.Source.HTTP = &computev1alpha1.ImageHTTPSource{URL: srv.URL + "/v2", SHA256: sum(v2)}
	if err := s.k.Update(ctx, &img); err != nil {
		t.Fatal(err)
	}
	s.waitImage(t, cd, func(st computev1alpha1.ImageNodeStatus) bool { return st.Ready && st.SHA256 == sum(v2) })
	fileIs(v2)
	s.waitState(t, vm, func(st computev1alpha1.LibvirtDomainStatus) bool { return st.State == running })

	// 校验失败：报错、不就绪，不留下副本。
	create(t, s.k, image(badImage, computev1alpha1.ImageISO, pathV1, sum(v2)))
	s.waitImage(t, badImage, func(st computev1alpha1.ImageNodeStatus) bool {
		return !st.Ready && strings.Contains(st.Message, "checksum mismatch")
	})
	if _, err := os.Stat(filepath.Join(dir, badImage)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad image left a replica: %v", err)
	}

	volume := func(name, from string) *computev1alpha1.LibvirtStorageVolume {
		return &computev1alpha1.LibvirtStorageVolume{Name: name, Spec: computev1alpha1.LibvirtStorageVolumeSpec{NodeName: node,
			// test 驱动只支持同 pool 复制；跨 pool 在 Dev 环境验证。
			Libvirt: &computev1alpha1.LibvirtVolumeSource{Pool: imgPool, XML: "<volume><capacity>1048576</capacity></volume>"},
			Source:  &computev1alpha1.VolumeContentSource{Image: from}}}
	}
	// 依赖稍后出现：镜像还不存在时卷等待；类型不对时被拒绝。
	create(t, s.k, volume(fromRoot, root))
	s.waitVolume(t, fromRoot, func(st computev1alpha1.LibvirtStorageVolumeStatus) bool {
		return st.Path == "" && strings.Contains(st.Message, "no such image")
	})
	create(t, s.k, volume(fromCD, cd))
	s.waitVolume(t, fromCD, func(st computev1alpha1.LibvirtStorageVolumeStatus) bool {
		return st.Path == "" && strings.Contains(st.Message, "has type ISO")
	})
	// test 驱动的 refresh 不扫描目录，这里替它登记副本。
	pool, err := s.l.LookupStoragePoolByName(imgPool)
	if err != nil {
		t.Fatal(err)
	}
	defer free(pool.Free)
	vol, err := pool.StorageVolCreateXML("<volume><name>"+root+"</name><capacity>1024</capacity></volume>", 0)
	if err != nil {
		t.Fatal(err)
	}
	free(vol.Free)
	create(t, s.k, image(root, computev1alpha1.ImageDisk, "/disk", sum(disk)))
	s.waitVolume(t, fromRoot, func(st computev1alpha1.LibvirtStorageVolumeStatus) bool {
		return st.Path != "" && st.Image == root && st.SHA256 == sum(disk) && st.Message == ""
	})

	// 重启后按本地状态判断，不重新下载。"bad" 一直在重试，先删掉，计数才有意义。
	s.waitImage(t, root, func(st computev1alpha1.ImageNodeStatus) bool { return st.Ready })
	if err := s.k.Delete(ctx, &computev1alpha1.Image{Name: badImage}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	s.agent.stop()
	before := requests.Load()
	s.agent = runAgent(parent, s.config) // 子测试结束后 Agent 还要继续运行
	time.Sleep(3 * time.Second)
	s.waitImage(t, cd, func(st computev1alpha1.ImageNodeStatus) bool { return st.Ready && st.SHA256 == sum(v2) })
	s.waitVolume(t, fromRoot, func(st computev1alpha1.LibvirtStorageVolumeStatus) bool { return st.SHA256 == sum(disk) })
	if got := requests.Load(); got != before {
		t.Fatalf("restart downloaded again: %d requests, had %d", got, before)
	}

	// 仍有实例挂载时延后删除。
	if err := s.k.Delete(ctx, &computev1alpha1.Image{Name: cd}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	fileIs(v2)
	if err := s.k.Delete(ctx, vm); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, func() error {
		if _, err := os.Stat(filepath.Join(dir, cd)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replica still present: %v", err)
		}
		return nil
	})
	for _, name := range []string{fromRoot, fromCD} {
		if err := s.k.Delete(ctx, &computev1alpha1.LibvirtStorageVolume{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.k.Delete(ctx, &computev1alpha1.Image{Name: root}); err != nil {
		t.Fatal(err)
	}
	s.rejectsInvalidDisks(t, cd)
}

func (s *suite) waitImage(t *testing.T, name string, ok func(computev1alpha1.ImageNodeStatus) bool) {
	t.Helper()
	eventually(t, 30*time.Second, func() error {
		var img computev1alpha1.Image
		if err := s.k.Get(t.Context(), client.ObjectKey{Name: name}, &img); err != nil {
			return err
		}
		if len(img.Status.Nodes) != 1 || !ok(img.Status.Nodes[0]) {
			return fmt.Errorf("image %s status = %+v", name, img.Status)
		}
		return nil
	})
}

func (s *suite) waitVolume(t *testing.T, name string, ok func(computev1alpha1.LibvirtStorageVolumeStatus) bool) {
	t.Helper()
	eventually(t, 30*time.Second, func() error {
		var v computev1alpha1.LibvirtStorageVolume
		if err := s.k.Get(t.Context(), client.ObjectKey{Name: name}, &v); err != nil {
			return err
		}
		if !ok(v.Status) {
			return fmt.Errorf("volume %s status = %+v", name, v.Status)
		}
		return nil
	})
}

func (s *suite) rejectsInvalidDisks(t *testing.T, cd string) {
	ctx := t.Context()
	// 一块盘必须且只能引用卷或镜像；镜像是光驱，不能接 virtio。
	for _, disk := range []computev1alpha1.DomainDisk{
		{},
		{Volume: "a", Image: cd, Bus: computev1alpha1.DiskBusSATA},
		{Image: cd},
	} {
		bad := &computev1alpha1.LibvirtDomain{Namespace: corev1.NamespaceDefault, Name: "invalid", Spec: computev1alpha1.LibvirtDomainSpec{
			NodeName: node, DomainXML: domainXML(""), Disks: []computev1alpha1.DomainDisk{disk}}}
		if err := s.k.Create(ctx, bad); err == nil {
			t.Fatalf("disk %+v accepted", disk)
		}
	}
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

func domainXML(devices string) string {
	return fmt.Sprintf(`<domain type='test'><memory unit='KiB'>65536</memory><os><type>hvm</type></os><devices>%s</devices></domain>`, devices)
}

func create(t *testing.T, k client.Client, obj client.Object) {
	t.Helper()
	if err := k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}
