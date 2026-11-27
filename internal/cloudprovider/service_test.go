package cloudprovider

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
)

const (
	ayatoriNamespace = "homelab"
	vip              = "10.62.0.7"
)

func TestLoadBalancerName(t *testing.T) {
	short := &corev1.Service{Namespace: "gitea", Name: "ssh"}
	if got := loadBalancerName(short); got != "gitea-ssh" {
		t.Fatalf("got %q", got)
	}
	long := &corev1.Service{Namespace: strings.Repeat("n", 40), Name: strings.Repeat("s", 40)}
	other := long.DeepCopy()
	other.Name += "x"
	a, b := loadBalancerName(long), loadBalancerName(other)
	if len(a) > 63 || a == b || a != loadBalancerName(long.DeepCopy()) {
		t.Fatalf("truncated names must be stable, distinct and fit a DNS label: %q %q", a, b)
	}
}

// 本集群与 Ayatori 用同一个 apiserver：Service、Node 在 workload namespace，Ayatori 对象在 ayatoriNamespace。
func TestServiceLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("运行 make test 启动真实 API server")
	}
	crds, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = networkv1alpha1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, name := range []string{ayatoriNamespace, "apps"} {
		must(t, c.Create(ctx, &corev1.Namespace{Name: name}))
	}
	addNode(t, c, "node-b", "192.0.2.12")

	stop := start(t, cfg, scheme)

	// 没有 class 的 Service 不处理（仍归 servicelb）。
	plain := service("plain", nil, corev1.ServiceExternalTrafficPolicyCluster, 80)
	must(t, c.Create(ctx, plain))

	// 名字已被声明的对象占用：不接管。
	declared := &networkv1alpha1.LoadBalancer{Namespace: ayatoriNamespace, Name: "apps-taken",
		Spec: networkv1alpha1.LoadBalancerSpec{Ports: []networkv1alpha1.LoadBalancerPort{{Port: 1, BackendRef: networkv1alpha1.BackendRef{Name: "x"}}}}}
	must(t, c.Create(ctx, declared))
	taken := service("taken", new(LoadBalancerClass), corev1.ServiceExternalTrafficPolicyCluster, 80)
	must(t, c.Create(ctx, taken))

	svc := service("web", new(LoadBalancerClass), corev1.ServiceExternalTrafficPolicyLocal, 80, 443)
	svc.Spec.LoadBalancerIP = vip
	must(t, c.Create(ctx, svc))

	lb := &networkv1alpha1.LoadBalancer{}
	eventually(t, "LoadBalancer created", func() bool {
		return c.Get(ctx, client.ObjectKey{Namespace: ayatoriNamespace, Name: "apps-web"}, lb) == nil
	})
	if lb.Spec.Address != vip || len(lb.Spec.Ports) != 2 || lb.Spec.Ports[1].BackendRef.Name != "apps-web-443" {
		t.Fatalf("unexpected spec %+v", lb.Spec)
	}
	must(t, c.Get(ctx, client.ObjectKeyFromObject(svc), svc))
	backend := &networkv1alpha1.Backend{}
	must(t, c.Get(ctx, client.ObjectKey{Namespace: ayatoriNamespace, Name: "apps-web-80"}, backend))
	if len(backend.Spec.Endpoints) != 1 || backend.Spec.Endpoints[0] != (networkv1alpha1.Endpoint{Address: "192.0.2.12", Port: svc.Spec.Ports[0].NodePort}) {
		t.Fatalf("unexpected endpoints %+v", backend.Spec.Endpoints)
	}
	hc := backend.Spec.HealthCheck
	if hc.Type != "HTTP" || hc.Port == nil || *hc.Port != svc.Spec.HealthCheckNodePort || hc.Interval.Duration != 5*time.Second {
		t.Fatalf("Local Service must check healthCheckNodePort, got %+v", hc)
	}

	// VIP 分配与落地之后才公布 ingress；之前不写。
	if len(svc.Status.LoadBalancer.Ingress) != 0 {
		t.Fatal("ingress published before Programmed")
	}
	// 地址被占用（池外同理）：LoadBalancer controller 拒绝，原因同步到 Service。
	meta.SetStatusCondition(&lb.Status.Conditions, metav1.Condition{Type: programmedType, Status: metav1.ConditionFalse,
		Reason: "AddressConflict", Message: "VIP 10.62.0.7 已被其他 LoadBalancer 占用"})
	must(t, c.Status().Update(ctx, lb))
	eventually(t, "rejection visible on Service", func() bool {
		_ = c.Get(ctx, client.ObjectKeyFromObject(svc), svc)
		cond := meta.FindStatusCondition(svc.Status.Conditions, serviceConditionType)
		return cond != nil && cond.Reason == "AddressConflict" && len(svc.Status.LoadBalancer.Ingress) == 0
	})
	program(t, c, lb, vip, "apps-web.homelab.lb.lab.ddupan.top")
	eventually(t, "ingress published", func() bool {
		_ = c.Get(ctx, client.ObjectKeyFromObject(svc), svc)
		in := svc.Status.LoadBalancer.Ingress
		return len(in) == 1 && in[0].IP == vip && in[0].Hostname == "apps-web.homelab.lb.lab.ddupan.top" &&
			meta.IsStatusConditionTrue(svc.Status.Conditions, serviceConditionType)
	})

	// 节点增加：Backend 跟随。
	addNode(t, c, "node-a", "192.0.2.11")
	eventually(t, "node added to Backend", func() bool {
		_ = c.Get(ctx, client.ObjectKeyFromObject(backend), backend)
		return len(backend.Spec.Endpoints) == 2 && backend.Spec.Endpoints[0].Address == "192.0.2.11"
	})

	// controller 重启：认领已有对象，不重复创建。
	stop()
	stop = start(t, cfg, scheme)

	// 端口减少：LoadBalancer 先不再引用，再删多余的 Backend。
	must(t, c.Get(ctx, client.ObjectKeyFromObject(svc), svc))
	svc.Spec.Ports = svc.Spec.Ports[:1]
	must(t, c.Update(ctx, svc))
	eventually(t, "stale Backend removed", func() bool {
		err := c.Get(ctx, client.ObjectKey{Namespace: ayatoriNamespace, Name: "apps-web-443"}, &networkv1alpha1.Backend{})
		return apierrors.IsNotFound(err)
	})
	must(t, c.Get(ctx, client.ObjectKeyFromObject(lb), lb))
	if len(lb.Spec.Ports) != 1 {
		t.Fatalf("LoadBalancer still has %d ports", len(lb.Spec.Ports))
	}

	// 删除：等 LoadBalancer（有自己的 finalizer）真正消失后才放行 Service。
	controllerutil.AddFinalizer(lb, "network.ayatori.ddupan.top/test")
	must(t, c.Update(ctx, lb))
	must(t, c.Delete(ctx, svc))
	eventually(t, "LoadBalancer deleting", func() bool {
		_ = c.Get(ctx, client.ObjectKeyFromObject(lb), lb)
		return !lb.DeletionTimestamp.IsZero()
	})
	must(t, c.Get(ctx, client.ObjectKeyFromObject(svc), svc))
	controllerutil.RemoveFinalizer(lb, "network.ayatori.ddupan.top/test")
	must(t, c.Update(ctx, lb))
	eventually(t, "Service released", func() bool {
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(svc), svc))
	})
	eventually(t, "Backends removed", func() bool {
		list := &networkv1alpha1.BackendList{}
		_ = c.List(ctx, list, client.InNamespace(ayatoriNamespace))
		return len(list.Items) == 0
	})

	must(t, c.Get(ctx, client.ObjectKeyFromObject(plain), plain))
	must(t, c.Get(ctx, client.ObjectKeyFromObject(taken), taken))
	must(t, c.Get(ctx, client.ObjectKeyFromObject(declared), declared))
	if len(plain.Finalizers) != 0 || declared.Labels[labelManagedBy] != "" || len(declared.Spec.Ports) != 1 {
		t.Fatal("touched objects it does not own")
	}
	// 删除未接管的 Service：finalizer 照常移除，声明的 LB 保留。
	must(t, c.Delete(ctx, taken))
	eventually(t, "unowned Service released", func() bool {
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(taken), taken))
	})
	must(t, c.Get(ctx, client.ObjectKeyFromObject(declared), declared))
	stop()
}

func start(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme) func() {
	t.Helper()
	manager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ayatori, err := cluster.New(cfg, func(o *cluster.Options) {
		o.Scheme = scheme
		o.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{ayatoriNamespace: {}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	must(t, manager.Add(ayatori))
	r := &ServiceReconciler{Client: manager.GetClient(), Ayatori: ayatori.GetClient(), Namespace: ayatoriNamespace}
	must(t, r.SetupWithManager(manager, ayatori))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- manager.Start(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func service(name string, class *string, policy corev1.ServiceExternalTrafficPolicy, ports ...int32) *corev1.Service {
	svc := &corev1.Service{Namespace: "apps", Name: name,
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: class, ExternalTrafficPolicy: policy,
			Selector: map[string]string{"app": name}}}
	for _, port := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "p" + strconv.Itoa(int(port)), Port: port, Protocol: corev1.ProtocolTCP})
	}
	return svc
}

func addNode(t *testing.T, c client.Client, name, address string) {
	t.Helper()
	node := &corev1.Node{Name: name}
	must(t, c.Create(t.Context(), node))
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
	must(t, c.Status().Update(t.Context(), node))
}

// program 代替 Ayatori 的 LoadBalancer controller 写 status。
func program(t *testing.T, c client.Client, lb *networkv1alpha1.LoadBalancer, address, hostname string) {
	t.Helper()
	lb.Status.Address, lb.Status.Hostname = address, hostname
	meta.SetStatusCondition(&lb.Status.Conditions, metav1.Condition{Type: programmedType, Status: metav1.ConditionTrue, Reason: "AllHostsReady"})
	must(t, c.Status().Update(t.Context(), lb))
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if condition() {
			return
		}
	}
	t.Fatal("timed out waiting for " + what)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
