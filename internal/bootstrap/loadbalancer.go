package bootstrap

import (
	"context"
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

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/adapter/powerdns"
	lbcontroller "git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
)

type loadBalancerOptions struct {
	addressPool   string
	credentials   string
	agentStale    time.Duration
	dnsURL        string
	dnsZone       string
	agentConverge bool
}

func (o *loadBalancerOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.addressPool, "loadbalancer-address-pool", "",
		"LoadBalancer VIP 池（IPv4 CIDR），不得与本 API server 的 Service CIDR 重叠；为空时不启用 LoadBalancer")
	flags.StringVar(&o.credentials, "loadbalancer-credentials", "",
		"LB 凭据 Secret（<namespace>/<name>），只在维护 Hostname 时需要：powerdns-api-key 为 PowerDNS API key")
	flags.DurationVar(&o.agentStale, "loadbalancer-agent-stale-after", time.Minute,
		"LB agent 的 Lease 超过此值未续约即报告 AgentReady=Unknown（AgentStale）")
	flags.StringVar(&o.dnsURL, "loadbalancer-dns-url", "", "PowerDNS API 地址；与 --loadbalancer-dns-zone 同时设置时维护 Hostname")
	flags.StringVar(&o.dnsZone, "loadbalancer-dns-zone", "", "Ayatori 独占的 LoadBalancer zone，如 lb.lab.ddupan.top")
	// ponytail: 只为与仍传入它的部署兼容保留一个 minor，之后删除。
	flags.BoolVar(&o.agentConverge, "loadbalancer-agent-convergence", true,
		"已废弃：代理配置总是由各 LB 主机的 agent 在本机收敛（ADR-0022），不能设为 false")
}

func registerLoadBalancerControllers(ctx context.Context, manager ctrl.Manager, options loadBalancerOptions) error {
	if options.addressPool == "" {
		return nil
	}
	if !options.agentConverge {
		return errors.New("--loadbalancer-agent-convergence=false 不再支持：远程收敛已移除（ADR-0022）")
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
	var secret *corev1.Secret
	if options.credentials != "" {
		if secret, err = loadBalancerCredentials(ctx, manager, options.credentials); err != nil {
			return err
		}
	}
	// 不连接主机：代理配置由各主机的 agent 在本机收敛，这里只汇总报告（ADR-0022）。
	hosts := &lbcontroller.LoadBalancerHostReconciler{Client: manager.GetClient(), AgentStaleAfter: options.agentStale}
	if err := hosts.SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancerHost controller: %w", err)
	}
	if options.dnsZone != "" {
		reconciler.DNS = powerdns.New(options.dnsURL, string(secret.Data["powerdns-api-key"]), options.dnsZone, &http.Client{Timeout: 30 * time.Second})
		reconciler.DNSZone = options.dnsZone
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancer controller: %w", err)
	}
	if err := (&lbcontroller.AddressReconciler{Client: manager.GetClient()}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancer address controller: %w", err)
	}
	if err := (&lbcontroller.BackendReconciler{Client: manager.GetClient()}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up Backend controller: %w", err)
	}
	return nil
}

// loadBalancerCredentials 在启动时读取凭据 Secret。
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
