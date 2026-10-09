package bootstrap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/gobgp"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy"
	lbcontroller "git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/loadbalancer"
)

type loadBalancerOptions struct {
	addressPool    string
	hosts          []loadbalancer.Host
	credentials    string
	connectTimeout time.Duration
	resolvers      string
}

func (o *loadBalancerOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.addressPool, "loadbalancer-address-pool", "",
		"LoadBalancer VIP 池（IPv4 CIDR），不得与本 API server 的 Service CIDR 重叠；为空时不启用 LoadBalancer")
	flags.Func("loadbalancer-host", "LB 主机，格式 <名称>,<Data Plane API URL>,<gobgpd gRPC 地址>；可重复；为空时只分配 VIP",
		func(value string) error {
			parts := strings.Split(value, ",")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
				return fmt.Errorf("want <name>,<data plane API URL>,<gobgpd address>")
			}
			o.hosts = append(o.hosts, loadbalancer.Host{Name: parts[0], ProxyURL: parts[1], SpeakerAddress: parts[2]})
			return nil
		})
	flags.StringVar(&o.credentials, "loadbalancer-credentials", "",
		"LB 主机凭据 Secret（<namespace>/<name>）：username、password 为 Data Plane API Basic 凭据，tls.crt、tls.key、ca.crt 为两个 API 的 mTLS")
	flags.DurationVar(&o.connectTimeout, "loadbalancer-connect-timeout", 5*time.Second, "代理连接后端的超时")
	flags.StringVar(&o.resolvers, "loadbalancer-resolvers", "", "DNS endpoint 使用的 HAProxy resolvers section 名称")
}

// registerLoadBalancerControllers 返回在 manager 停止后释放 gRPC 连接的清理函数。
func registerLoadBalancerControllers(ctx context.Context, manager ctrl.Manager, options loadBalancerOptions) (func(), error) {
	noop := func() {}
	if options.addressPool == "" {
		return noop, nil
	}
	prefix, err := netip.ParsePrefix(options.addressPool)
	if err != nil || !prefix.Addr().Is4() {
		return nil, fmt.Errorf("loadbalancer address pool %q must be an IPv4 CIDR", options.addressPool)
	}
	reconciler := &lbcontroller.LoadBalancerReconciler{Client: manager.GetClient(), Pool: address.Pool{Prefix: prefix.Masked()}, Hosts: options.hosts}
	cleanup := noop
	if len(options.hosts) > 0 {
		proxy, routes, err := loadBalancerBackends(ctx, manager, options)
		if err != nil {
			return nil, err
		}
		reconciler.Proxy, reconciler.Routes, cleanup = proxy, routes, routes.Close
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		cleanup()
		return nil, fmt.Errorf("set up LoadBalancer controller: %w", err)
	}
	if err := (&lbcontroller.AddressReconciler{Client: manager.GetClient()}).SetupWithManager(manager); err != nil {
		cleanup()
		return nil, fmt.Errorf("set up LoadBalancer address controller: %w", err)
	}
	return cleanup, nil
}

// loadBalancerBackends 在启动时读取一次凭据 Secret。
// ponytail: 证书轮换需要重启 manager；需要热轮换时改用 tls.Config 的 GetClientCertificate 回调按 Secret 刷新。
func loadBalancerBackends(ctx context.Context, manager ctrl.Manager, options loadBalancerOptions) (*haproxy.Hosts, *gobgp.Speakers, error) {
	namespace, name, ok := strings.Cut(options.credentials, "/")
	if !ok || namespace == "" || name == "" {
		return nil, nil, errors.New("LB hosts require --loadbalancer-credentials=<namespace>/<name>")
	}
	secret := &corev1.Secret{}
	// manager 尚未启动，直接读 API server，Secret 也不进入共享 cache。
	if err := manager.GetAPIReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, nil, fmt.Errorf("read LoadBalancer credentials: %w", err)
	}
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return nil, nil, fmt.Errorf("LoadBalancer client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data["ca.crt"]) {
		return nil, nil, errors.New("LoadBalancer credentials need a PEM ca.crt")
	}
	config := &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots, MinVersion: tls.VersionTLS13}
	proxy := &haproxy.Hosts{
		HTTP:           &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 30 * time.Second},
		Username:       string(secret.Data["username"]),
		Password:       string(secret.Data["password"]),
		ConnectTimeout: options.connectTimeout,
		Resolvers:      options.resolvers,
	}
	return proxy, &gobgp.Speakers{Credentials: credentials.NewTLS(config)}, nil
}
