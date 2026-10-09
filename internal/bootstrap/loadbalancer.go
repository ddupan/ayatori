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
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/powerdns"
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
	dnsURL         string
	dnsZone        string
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
		"LB 凭据 Secret（<namespace>/<name>）：username、password 为 Data Plane API Basic 凭据，tls.crt、tls.key、ca.crt 为两个 API 的 mTLS，powerdns-api-key 为 PowerDNS API key")
	flags.DurationVar(&o.connectTimeout, "loadbalancer-connect-timeout", 5*time.Second, "代理连接后端的超时")
	flags.StringVar(&o.resolvers, "loadbalancer-resolvers", "", "DNS endpoint 使用的 HAProxy resolvers section 名称")
	flags.StringVar(&o.dnsURL, "loadbalancer-dns-url", "", "PowerDNS API 地址；与 --loadbalancer-dns-zone 同时设置时维护 Hostname")
	flags.StringVar(&o.dnsZone, "loadbalancer-dns-zone", "", "Ayatori 独占的 LoadBalancer zone，如 lb.lab.ddupan.top")
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
	if (options.dnsURL == "") != (options.dnsZone == "") {
		return nil, errors.New("--loadbalancer-dns-url and --loadbalancer-dns-zone must be set together")
	}
	cleanup := noop
	if len(options.hosts) > 0 || options.dnsZone != "" {
		secret, err := loadBalancerCredentials(ctx, manager, options.credentials)
		if err != nil {
			return nil, err
		}
		if len(options.hosts) > 0 {
			proxy, routes, err := loadBalancerBackends(secret, options)
			if err != nil {
				return nil, err
			}
			reconciler.Proxy, reconciler.Routes, cleanup = proxy, routes, routes.Close
		}
		if options.dnsZone != "" {
			reconciler.DNS = powerdns.New(options.dnsURL, string(secret.Data["powerdns-api-key"]), options.dnsZone, &http.Client{Timeout: 30 * time.Second})
			reconciler.DNSZone = options.dnsZone
		}
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

// loadBalancerCredentials 在启动时读取一次凭据 Secret。
// ponytail: 轮换需要重启 manager；需要热轮换时改用 tls.Config 的 GetClientCertificate 回调按 Secret 刷新。
func loadBalancerCredentials(ctx context.Context, manager ctrl.Manager, reference string) (*corev1.Secret, error) {
	namespace, name, ok := strings.Cut(reference, "/")
	if !ok || namespace == "" || name == "" {
		return nil, errors.New("LB hosts or DNS require --loadbalancer-credentials=<namespace>/<name>")
	}
	secret := &corev1.Secret{}
	// manager 尚未启动，直接读 API server，Secret 也不进入共享 cache。
	if err := manager.GetAPIReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("read LoadBalancer credentials: %w", err)
	}
	return secret, nil
}

func loadBalancerBackends(secret *corev1.Secret, options loadBalancerOptions) (*haproxy.Hosts, *gobgp.Speakers, error) {
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
