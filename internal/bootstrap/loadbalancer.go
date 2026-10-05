package bootstrap

import (
	"flag"
	"fmt"
	"net/netip"

	lbcontroller "git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
	ctrl "sigs.k8s.io/controller-runtime"
)

type loadBalancerOptions struct {
	addressPool string
}

func (o *loadBalancerOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.addressPool, "loadbalancer-address-pool", "",
		"LoadBalancer VIP 池（IPv4 CIDR），不得与本 API server 的 Service CIDR 重叠；为空时不启用 LoadBalancer")
}

func registerLoadBalancerControllers(manager ctrl.Manager, options loadBalancerOptions) error {
	if options.addressPool == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(options.addressPool)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("loadbalancer address pool %q must be an IPv4 CIDR", options.addressPool)
	}
	pool := address.Pool{Prefix: prefix.Masked()}
	if err := (&lbcontroller.LoadBalancerReconciler{Client: manager.GetClient(), Pool: pool}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancer controller: %w", err)
	}
	if err := (&lbcontroller.AddressReconciler{Client: manager.GetClient()}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up LoadBalancer address controller: %w", err)
	}
	return nil
}
