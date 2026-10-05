package bootstrap

import (
	"crypto/tls"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// 保留 kubeconfig 支持的官方认证插件，不自建身份加载流程。
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	executionv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/execution/v1alpha1"
	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
)

func newManager(options options) (ctrl.Manager, error) {
	config, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	configuration := options.manager.configuration()
	options.database.configureManager(&configuration)
	manager, err := ctrl.NewManager(config, configuration)
	if err != nil {
		return nil, fmt.Errorf("create controller manager: %w", err)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("set up health check: %w", err)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("set up readiness check: %w", err)
	}
	return manager, nil
}

func (o managerOptions) configuration() ctrl.Options {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(executionv1alpha1.AddToScheme(scheme))
	utilruntime.Must(databasev1alpha1.AddToScheme(scheme))
	utilruntime.Must(networkv1alpha1.AddToScheme(scheme))
	return ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 o.metricsOptions(),
		WebhookServer:           webhook.NewServer(o.webhookOptions()),
		HealthProbeBindAddress:  o.probeAddr,
		LeaderElection:          o.enableLeaderElection,
		LeaderElectionID:        "a6325ed6.ddupan.top",
		LeaderElectionNamespace: o.leaderElectionNS,
		// 保持默认不主动释放选主 Lease：manager 停止后还要完成组件清理。
	}
}

func (o managerOptions) tlsOptions() []func(*tls.Config) {
	if o.enableHTTP2 {
		return nil
	}
	// 默认禁用 HTTP/2，沿用 scaffold 对 Rapid Reset 等风险的防护。
	return []func(*tls.Config){func(config *tls.Config) {
		config.NextProtos = []string{"http/1.1"}
	}}
}

func (o managerOptions) metricsOptions() metricsserver.Options {
	options := metricsserver.Options{
		BindAddress:   o.metricsAddr,
		SecureServing: o.secureMetrics,
		TLSOpts:       o.tlsOptions(),
	}
	if o.secureMetrics {
		options.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if o.metricsCertPath != "" {
		options.CertDir = o.metricsCertPath
		options.CertName = o.metricsCertName
		options.KeyName = o.metricsCertKey
	}
	return options
}

func (o managerOptions) webhookOptions() webhook.Options {
	options := webhook.Options{Port: o.webhookPort, TLSOpts: o.tlsOptions()}
	if o.webhookCertPath != "" {
		options.CertDir = o.webhookCertPath
		options.CertName = o.webhookCertName
		options.KeyName = o.webhookCertKey
	}
	return options
}
