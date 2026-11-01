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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/agentstatus"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/haproxy"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/powerdns"
	lbcontroller "git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
)

type loadBalancerOptions struct {
	addressPool    string
	credentials    string
	connectTimeout time.Duration
	agentStale     time.Duration
	resolvers      string
	dnsURL         string
	dnsZone        string
}

func (o *loadBalancerOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.addressPool, "loadbalancer-address-pool", "",
		"LoadBalancer VIP 池（IPv4 CIDR），不得与本 API server 的 Service CIDR 重叠；为空时不启用 LoadBalancer")
	flags.StringVar(&o.credentials, "loadbalancer-credentials", "",
		"LB 凭据 Secret（<namespace>/<name>），为空时只分配 VIP、不管理 LoadBalancerHost：username、password 为 Data Plane API Basic 凭据，tls.crt、tls.key、ca.crt 为 Data Plane API 的 mTLS，powerdns-api-key 为 PowerDNS API key")
	flags.DurationVar(&o.connectTimeout, "loadbalancer-connect-timeout", 5*time.Second, "代理连接后端的超时")
	flags.DurationVar(&o.agentStale, "loadbalancer-agent-stale-after", time.Minute,
		"LB agent 最近一次成功收敛距今超过此值即报告 AgentReady=False（收敛停滞）")
	flags.StringVar(&o.resolvers, "loadbalancer-resolvers", "", "DNS endpoint 使用的 HAProxy resolvers section 名称")
	flags.StringVar(&o.dnsURL, "loadbalancer-dns-url", "", "PowerDNS API 地址；与 --loadbalancer-dns-zone 同时设置时维护 Hostname")
	flags.StringVar(&o.dnsZone, "loadbalancer-dns-zone", "", "Ayatori 独占的 LoadBalancer zone，如 lb.lab.ddupan.top")
}

func registerLoadBalancerControllers(ctx context.Context, manager ctrl.Manager, options loadBalancerOptions) error {
	if options.addressPool == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(options.addressPool)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("loadbalancer address pool %q must be an IPv4 CIDR", options.addressPool)
	}
	reconciler := &lbcontroller.LoadBalancerReconciler{Client: manager.GetClient(), Pool: address.Pool{Prefix: prefix.Masked()}}
	if (options.dnsURL == "") != (options.dnsZone == "") {
		return errors.New("--loadbalancer-dns-url and --loadbalancer-dns-zone must be set together")
	}
	if options.dnsZone != "" && options.credentials == "" {
		return errors.New("LoadBalancer DNS requires --loadbalancer-credentials=<namespace>/<name>")
	}
	if options.credentials != "" {
		secret, err := loadBalancerCredentials(ctx, manager, options.credentials)
		if err != nil {
			return err
		}
		certificate, err := newClientCertificate(secret, manager.GetAPIReader())
		if err != nil {
			return err
		}
		if err := manager.Add(certificate); err != nil {
			return fmt.Errorf("set up LoadBalancer client certificate refresh: %w", err)
		}
		proxy, err := loadBalancerProxy(secret, certificate, options)
		if err != nil {
			return err
		}
		reconciler.Proxy = proxy
		hosts := &lbcontroller.LoadBalancerHostReconciler{
			Client: manager.GetClient(), Proxy: proxy, AgentStaleAfter: options.agentStale,
			// agent 状态接口只读、不含凭据，明文 HTTP（ADR-0015），不使用 Data Plane API 的 mTLS 客户端。
			// 超时由每轮的 hostTimeout context 限定。
			Agent: agentstatus.Client{HTTP: &http.Client{}},
		}
		if err := hosts.SetupWithManager(manager); err != nil {
			return fmt.Errorf("set up LoadBalancerHost controller: %w", err)
		}
		if options.dnsZone != "" {
			reconciler.DNS = powerdns.New(options.dnsURL, string(secret.Data["powerdns-api-key"]), options.dnsZone, &http.Client{Timeout: 30 * time.Second})
			reconciler.DNSZone = options.dnsZone
		}
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancer controller: %w", err)
	}
	if err := (&lbcontroller.AddressReconciler{Client: manager.GetClient()}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancer address controller: %w", err)
	}
	return nil
}

// loadBalancerCredentials 在启动时读取凭据 Secret；客户端证书此后由 clientCertificate 刷新。
func loadBalancerCredentials(ctx context.Context, manager ctrl.Manager, reference string) (*corev1.Secret, error) {
	namespace, name, ok := strings.Cut(reference, "/")
	if !ok || namespace == "" || name == "" {
		return nil, errors.New("--loadbalancer-credentials must be <namespace>/<name>")
	}
	secret := &corev1.Secret{}
	// manager 尚未启动，直接读 API server，Secret 也不进入共享 cache。
	if err := manager.GetAPIReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("read LoadBalancer credentials: %w", err)
	}
	return secret, nil
}

// loadBalancerProxy 中客户端证书每次握手取最新值。
// ponytail: 服务端 CA（ca.crt）、Basic 凭据只在启动时读取——CA 是寿命 10 年的内部根，轮换
// 是罕见的有意操作，届时重启 manager；热更新 CA 需要自行实现证书链校验，不值得。
func loadBalancerProxy(secret *corev1.Secret, certificate *clientCertificate, options loadBalancerOptions) (*haproxy.Hosts, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data["ca.crt"]) {
		return nil, errors.New("LoadBalancer credentials need a PEM ca.crt")
	}
	config := &tls.Config{GetClientCertificate: certificate.get, RootCAs: roots, MinVersion: tls.VersionTLS13}
	proxy := &haproxy.Hosts{
		HTTP:           &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 30 * time.Second},
		Username:       string(secret.Data["username"]),
		Password:       string(secret.Data["password"]),
		ConnectTimeout: options.connectTimeout,
		Resolvers:      options.resolvers,
	}
	return proxy, nil
}
