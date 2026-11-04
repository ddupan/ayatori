//go:build integration

package agent_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/agent"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
	"git.ddupan.top/panxiao81/ayatori/internal/testenv"
)

const (
	// 与 HAProxy adapter 的 contract test 同一 fixture（hack/integration-test.sh 的 haproxy 服务）。
	dataPlaneUser     = "admin"
	dataPlanePassword = "AYATORI-TEST-ONLY"
	// 模拟 controller 持有的 finalizer：删除时对象保留到 agent 清理完成。
	lbFinalizer = "network.ayatori.ddupan.top/loadbalancer"
	orphanUID   = "99999999-0000-0000-0000-000000000000"
)

// TestAgentConverges：agent watch LoadBalancer 与 Backend，经本机 Data Plane API 收敛 HAProxy，并在
// LoadBalancerHost 中报告与哪份期望一致（ADR-0022）；删除时清理，已不存在的 LoadBalancer 的配置也清理。
func TestAgentConverges(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	id, port := testenv.Container(t, "haproxy", 5555)
	if out, err := exec.CommandContext(t.Context(), "docker", "exec", id, "/fixture/reset.sh").CombinedOutput(); err != nil {
		t.Fatalf("cannot reset HAProxy fixture: %v: %s", err, out)
	}
	proxy := loadbalancer.Host{Name: "fixture", ProxyURL: "http://127.0.0.1:" + strconv.Itoa(port)}
	hosts := &haproxy.Hosts{HTTP: http.DefaultClient, Username: dataPlaneUser, Password: dataPlanePassword, ConnectTimeout: 5 * time.Second}

	root := filepath.Join("..", "..", "..")
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join(root, "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
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

	// 已有一个不存在的 LoadBalancer 的配置：agent 同步后应删除。
	orphan := loadbalancer.Reconstitute(loadbalancer.Snapshot{Owner: address.Owner{UID: orphanUID},
		Address: netip.MustParseAddr("10.62.0.9"), Ports: []loadbalancer.Port{{Port: 7100, Backend: "gone", MissingBackend: true}}})
	if err := hosts.Apply(t.Context(), proxy, orphan); err != nil {
		t.Fatal(err)
	}

	backend := &networkv1alpha1.Backend{Spec: networkv1alpha1.BackendSpec{
		Endpoints: []networkv1alpha1.Endpoint{{Address: "127.0.0.1", Port: 5555}},
	}}
	backend.Namespace, backend.Name = testNamespace, "pg"
	if err := k.Create(t.Context(), backend); err != nil {
		t.Fatal(err)
	}
	lb := &networkv1alpha1.LoadBalancer{Spec: networkv1alpha1.LoadBalancerSpec{
		Ports: []networkv1alpha1.LoadBalancerPort{{Port: 7000, BackendRef: networkv1alpha1.BackendRef{Name: "pg"}}},
	}}
	lb.Namespace, lb.Name, lb.Finalizers = testNamespace, "pg", []string{lbFinalizer}
	if err := k.Create(t.Context(), lb); err != nil {
		t.Fatal(err)
	}
	// VIP 由中央 controller 分配；agent 只读 status.address。
	lb.Status.Address = "10.62.0.5"
	if err := k.Status().Update(t.Context(), lb); err != nil {
		t.Fatal(err)
	}

	startConvergingAgent(t, cfg, port)
	f := convergence{k: k, hosts: hosts, proxy: proxy}

	want := f.digest(t, lb)
	waitForLong(t, "收敛并报告与期望一致", f.reported(t, lb.Name, func(s *networkv1alpha1.HostLoadBalancerStatus) bool {
		return s != nil && s.Digest == want && s.Present && s.Reload == reloadSucceeded && s.UID == string(lb.UID)
	}))
	waitForLong(t, "已不存在的 LoadBalancer 的配置被删除", f.owners(t, string(lb.UID)))

	t.Run("Backend 变化后重新收敛", func(t *testing.T) {
		backend.Spec.Endpoints = []networkv1alpha1.Endpoint{{Address: "127.0.0.2", Port: 5555}}
		if err := k.Update(t.Context(), backend); err != nil {
			t.Fatal(err)
		}
		changed := f.digest(t, lb)
		if changed == want {
			t.Fatal("Backend 变化应改变摘要")
		}
		waitForLong(t, "报告新的摘要", f.reported(t, lb.Name, func(s *networkv1alpha1.HostLoadBalancerStatus) bool {
			return s != nil && s.Digest == changed && s.Reload == reloadSucceeded
		}))
	})
	t.Run("删除时清理，对象消失后移除报告", func(t *testing.T) { f.deletes(t, lb) })
	t.Run("删除 LoadBalancerHost 时清除本机全部配置并报告 Purged", func(t *testing.T) { f.purges(t, lb.Spec) })
	t.Run("API 不可达期间不清理任何配置", func(t *testing.T) { f.keepsWhileAPIDown(t, env, lb.Spec) })
}

// convergence 从 API 与本机代理两侧观察 agent 的收敛。
type convergence struct {
	k     client.Client
	hosts *haproxy.Hosts
	proxy loadbalancer.Host
}

// digest 以 controller 的算法计算期望摘要。
func (f convergence) digest(t *testing.T, lb *networkv1alpha1.LoadBalancer) string {
	t.Helper()
	object := &networkv1alpha1.LoadBalancer{}
	if err := f.k.Get(t.Context(), client.ObjectKeyFromObject(lb), object); err != nil {
		t.Fatal(err)
	}
	desired, err := controller.Desired(t.Context(), f.k, object)
	if err != nil {
		t.Fatal(err)
	}
	return desired.Digest()
}

// reported 对 LoadBalancerHost 中名为 name 的报告调用 check；没有该项时传入 nil。
func (f convergence) reported(t *testing.T, name string, check func(*networkv1alpha1.HostLoadBalancerStatus) bool) func() bool {
	return func() bool {
		host := &networkv1alpha1.LoadBalancerHost{}
		if err := f.k.Get(t.Context(), client.ObjectKey{Name: lbHost}, host); err != nil {
			return false
		}
		i := slices.IndexFunc(host.Status.LoadBalancers, func(s networkv1alpha1.HostLoadBalancerStatus) bool { return s.Name == name })
		if i < 0 {
			return check(nil)
		}
		return check(&host.Status.LoadBalancers[i])
	}
}

// owners 在本机代理上恰好有 want 这些 LoadBalancer 的配置时满足。
func (f convergence) owners(t *testing.T, want ...string) func() bool {
	return func() bool {
		got, err := f.hosts.Owners(t.Context(), f.proxy)
		return err == nil && slices.Equal(got, want)
	}
}

func (f convergence) deletes(t *testing.T, lb *networkv1alpha1.LoadBalancer) {
	if err := f.k.Delete(t.Context(), lb); err != nil {
		t.Fatal(err)
	}
	waitForLong(t, "删除中：本机配置已清除", f.reported(t, lb.Name, func(s *networkv1alpha1.HostLoadBalancerStatus) bool {
		return s != nil && !s.Present && s.Reload == reloadSucceeded
	}))
	waitForLong(t, "本机已没有任何配置", f.owners(t))
	if err := f.k.Get(t.Context(), client.ObjectKeyFromObject(lb), lb); err != nil {
		t.Fatal(err)
	}
	lb.Finalizers = nil
	if err := f.k.Update(t.Context(), lb); err != nil {
		t.Fatal(err)
	}
	waitForLong(t, "对象删除后不再报告", f.reported(t, lb.Name, func(s *networkv1alpha1.HostLoadBalancerStatus) bool { return s == nil }))
}

func (f convergence) purges(t *testing.T, spec networkv1alpha1.LoadBalancerSpec) {
	doomed := &networkv1alpha1.LoadBalancer{Spec: spec}
	doomed.Namespace, doomed.Name = testNamespace, "doomed"
	if err := f.k.Create(t.Context(), doomed); err != nil {
		t.Fatal(err)
	}
	doomed.Status.Address = "10.62.0.7"
	if err := f.k.Status().Update(t.Context(), doomed); err != nil {
		t.Fatal(err)
	}
	waitForLong(t, "收敛", f.owners(t, string(doomed.UID)))
	// controller 的 finalizer 让对象停在删除中，直到 agent 报告 Purged。
	host := &networkv1alpha1.LoadBalancerHost{}
	if err := f.k.Get(t.Context(), client.ObjectKey{Name: lbHost}, host); err != nil {
		t.Fatal(err)
	}
	host.Finalizers = []string{"network.ayatori.ddupan.top/loadbalancerhost"}
	if err := f.k.Update(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if err := f.k.Delete(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	waitForLong(t, "本机配置全部清除", f.owners(t))
	waitForLong(t, "报告 Purged", func() bool {
		if err := f.k.Get(t.Context(), client.ObjectKey{Name: lbHost}, host); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(host.Status.Conditions, controller.PurgedType)
	})
	host.Finalizers = nil
	if err := f.k.Update(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	// agent 仍在运行就会重新注册（退役须先停 agent），之后照常收敛。
	waitForLong(t, "重新注册后照常收敛", f.owners(t, string(doomed.UID)))
	if err := f.k.Delete(t.Context(), doomed); err != nil {
		t.Fatal(err)
	}
}

func (f convergence) keepsWhileAPIDown(t *testing.T, env *envtest.Environment, spec networkv1alpha1.LoadBalancerSpec) {
	kept := &networkv1alpha1.LoadBalancer{Spec: spec}
	kept.Namespace, kept.Name = testNamespace, "kept"
	if err := f.k.Create(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	kept.Status.Address = "10.62.0.6"
	if err := f.k.Status().Update(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	waitForLong(t, "收敛", f.owners(t, string(kept.UID)))
	// 有 watch 连接时 envtest 等不到 apiserver 正常退出，会在超时后强制结束；以能否访问为准。
	_ = env.Stop()
	if err := f.k.Get(t.Context(), client.ObjectKeyFromObject(kept), kept); err == nil {
		t.Fatal("API 仍可访问")
	}
	// 几轮收敛之后，配置仍在：缓存只随 API 的事件变化。
	time.Sleep(3 * agent.ConvergeInterval)
	if got, err := f.hosts.Owners(t.Context(), f.proxy); err != nil || !slices.Equal(got, []string{string(kept.UID)}) {
		t.Fatalf("Owners() = %v, %v", got, err)
	}
}

// startConvergingAgent 以管理员 kubeconfig 启动在本机收敛的 agent：身份与准入由 TestAgentJoins 验证。
func startConvergingAgent(t *testing.T, cfg *rest.Config, dataPlanePort int) {
	t.Helper()
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"c": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"admin": {ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}},
		Contexts:       map[string]*clientcmdapi.Context{"c": {Cluster: "c", AuthInfo: "admin"}},
		CurrentContext: "c",
	}, kubeconfig); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte(dataPlanePassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	startRouter(t, port)
	ctx, cancel := context.WithCancel(t.Context())
	a, err := agent.New(ctx, agent.Config{
		ASN: 65100, RouterID: routerID, VIPPool: vipPool, HAProxySocket: filepath.Join(dir, "absent.sock"),
		Neighbors: []agent.Neighbor{{Address: loopback, ASN: 65002, Port: uint32(port)}},
		Interval:  metav1.Duration{Duration: 200 * time.Millisecond},
		NodeName:  lbHost, Kubeconfig: kubeconfig,
		DataPlane: &agent.DataPlane{URL: "http://127.0.0.1:" + strconv.Itoa(dataPlanePort), Username: dataPlaneUser, PasswordFile: password,
			ConnectTimeout: metav1.Duration{Duration: 5 * time.Second}},
	}, newFakeVIPs(), prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}
