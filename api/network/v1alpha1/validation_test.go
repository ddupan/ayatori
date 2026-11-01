package v1alpha1_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	testNamespace = "network-api"
	fixedVIP      = "10.62.0.10"
)

func environment(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS 未设置；运行 make test 执行真实 API server 测试")
	}
	scheme := runtime.NewScheme()
	if err := networkv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crdPath, err := filepath.Abs("../../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	config, err := env.Start()
	if err != nil {
		t.Fatalf("启动 envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("停止 envtest: %v", err)
		}
	})
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	namespace.Name = testNamespace
	if err := c.Create(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}
	return c
}

func loadBalancer(name string, mutate func(*networkv1alpha1.LoadBalancer)) *networkv1alpha1.LoadBalancer {
	lb := &networkv1alpha1.LoadBalancer{Spec: networkv1alpha1.LoadBalancerSpec{
		Ports: []networkv1alpha1.LoadBalancerPort{{Port: 5432, Endpoints: []networkv1alpha1.Endpoint{{Address: "192.168.10.127", Port: 5432}}}},
	}}
	lb.Namespace, lb.Name = testNamespace, name
	if mutate != nil {
		mutate(lb)
	}
	return lb
}

func TestLoadBalancerAPI(t *testing.T) {
	c := environment(t)

	t.Run("默认值", func(t *testing.T) {
		lb := loadBalancer("defaults", nil)
		if err := c.Create(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		port := lb.Spec.Ports[0]
		check := port.HealthCheck
		if port.ProxyProtocol != "None" || port.IdleTimeout.Duration != time.Hour || port.DrainTimeout.Duration != 30*time.Second ||
			check.Type != "TCP" || check.Interval.Duration != 5*time.Second || check.Timeout.Duration != 3*time.Second ||
			check.HealthyThreshold != 2 || check.UnhealthyThreshold != 3 {
			t.Fatalf("默认值未生效: %+v", port)
		}
	})

	invalid := map[string]func(*networkv1alpha1.LoadBalancer){
		"非规范 IPv4 地址": func(lb *networkv1alpha1.LoadBalancer) { lb.Spec.Address = "10.62.0.010" },
		"IPv6 地址":     func(lb *networkv1alpha1.LoadBalancer) { lb.Spec.Address = "fd00::1" },
		"非法 endpoint": func(lb *networkv1alpha1.LoadBalancer) { lb.Spec.Ports[0].Endpoints[0].Address = "bad_name" },
		"重复 endpoint": func(lb *networkv1alpha1.LoadBalancer) {
			lb.Spec.Ports[0].Endpoints = append(lb.Spec.Ports[0].Endpoints, lb.Spec.Ports[0].Endpoints[0])
		},
		"HTTP 缺少 path": func(lb *networkv1alpha1.LoadBalancer) { lb.Spec.Ports[0].HealthCheck.Type = "HTTP" },
		"TCP 带 path":   func(lb *networkv1alpha1.LoadBalancer) { lb.Spec.Ports[0].HealthCheck.Path = "/primary" },
		"timeout 超过 interval": func(lb *networkv1alpha1.LoadBalancer) {
			lb.Spec.Ports[0].HealthCheck.Interval.Duration = 2 * time.Second
			lb.Spec.Ports[0].HealthCheck.Timeout.Duration = 3 * time.Second
		},
	}
	t.Run("名称必须是 DNS label", func(t *testing.T) {
		if err := c.Create(t.Context(), loadBalancer("with.dot", nil)); !apierrors.IsInvalid(err) {
			t.Fatalf("Create = %v, want Invalid", err)
		}
	})

	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := c.Create(t.Context(), loadBalancer("invalid", mutate)); !apierrors.IsInvalid(err) {
				t.Fatalf("Create = %v, want Invalid", err)
			}
		})
	}

	t.Run("Patroni 主库检查与 DNS endpoint", func(t *testing.T) {
		lb := loadBalancer("patroni", func(lb *networkv1alpha1.LoadBalancer) {
			lb.Spec.Ports[0].Endpoints = append(lb.Spec.Ports[0].Endpoints, networkv1alpha1.Endpoint{Address: "pg-lxc150.lab.ddupan.top", Port: 5432})
			lb.Spec.Ports[0].HealthCheck = networkv1alpha1.HealthCheck{Type: "HTTP", Port: ptr.To[int32](8008), Path: "/primary", ExpectedStatuses: []int32{200}}
		})
		if err := c.Create(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("地址不可变", func(t *testing.T) {
		lb := loadBalancer("immutable", func(lb *networkv1alpha1.LoadBalancer) { lb.Spec.Address = fixedVIP })
		if err := c.Create(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		lb.Spec.Address = "10.62.0.11"
		if err := c.Update(t.Context(), lb); !apierrors.IsInvalid(err) {
			t.Fatalf("修改 spec.address = %v, want Invalid", err)
		}
		lb.Spec.Address = fixedVIP
		lb.Status.Address = fixedVIP
		if err := c.Status().Update(t.Context(), lb); err != nil {
			t.Fatal(err)
		}
		lb.Status.Address = "10.62.0.11"
		if err := c.Status().Update(t.Context(), lb); !apierrors.IsInvalid(err) {
			t.Fatalf("修改 status.address = %v, want Invalid", err)
		}
	})
}

func TestLoadBalancerHostAPI(t *testing.T) {
	c := environment(t)
	host := func(name, url string) *networkv1alpha1.LoadBalancerHost {
		h := &networkv1alpha1.LoadBalancerHost{Spec: networkv1alpha1.LoadBalancerHostSpec{DataPlaneURL: url}}
		h.Name = name
		return h
	}
	if err := c.Create(t.Context(), host("lb-dev-1", "https://lb-dev-1.lab.ddupan.top:5555")); err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]*networkv1alpha1.LoadBalancerHost{
		"Data Plane API 必须是 HTTPS": host("plain", "http://lb-dev-1.lab.ddupan.top:5555"),
		"不是 URL":                   host("garbage", "lb-dev-1:5555"),
		"名称必须是 DNS label":          host("with.dot", "https://lb-dev-1.lab.ddupan.top:5555"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := c.Create(t.Context(), invalid); !apierrors.IsInvalid(err) {
				t.Fatalf("Create = %v, want Invalid", err)
			}
		})
	}
}
