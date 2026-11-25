// cloud-provider 运行在 workload 集群中，把 loadBalancerClass 为 ayatori.ddupan.top/loadbalancer 的
// Service 翻译为 Ayatori 的 LoadBalancer 与 Backend（ADR-0028）。由消费方部署；对 Ayatori 以 SPIFFE
// JWT-SVID 认证，只在 --ayatori-namespace 中读写。
package main

import (
	"flag"
	"os"

	"github.com/spiffe/go-spiffe/v2/workloadapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
	"go.ddupan.top/ayatori/internal/cloudprovider"
)

func main() {
	server := flag.String("ayatori-server", "", "Ayatori API 地址，如 https://ayatori-prod.lab.ddupan.top:6443")
	caFile := flag.String("ayatori-ca-file", "", "Ayatori apiserver 的 CA（kube-public/ayatori-login 公布的 CA）")
	audience := flag.String("ayatori-audience", "", "环境的 audience，即 ayatori-login 的 clientID，如 ayatori-prod")
	namespace := flag.String("ayatori-namespace", "", "Ayatori 中代表本集群的 namespace")
	metrics := flag.String("metrics-bind-address", ":8080", "指标端口；0 关闭")
	probe := flag.String("health-probe-bind-address", ":8081", "健康检查端口")
	leaderElect := flag.Bool("leader-elect", true, "在本集群以 Lease 选主")
	options := zap.Options{}
	options.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options)))
	log := ctrl.Log.WithName("cloud-provider")
	if *server == "" || *caFile == "" || *audience == "" || *namespace == "" {
		log.Error(nil, "--ayatori-server, --ayatori-ca-file, --ayatori-audience and --ayatori-namespace are required")
		os.Exit(2)
	}
	if err := run(*server, *caFile, *audience, *namespace, *metrics, *probe, *leaderElect); err != nil {
		log.Error(err, "cloud-provider exited")
		os.Exit(1)
	}
}

func run(server, caFile, audience, namespace, metrics, probe string, leaderElect bool) error {
	ctx := ctrl.SetupSignalHandler()
	// Workload API 地址取 SPIFFE_ENDPOINT_SOCKET（SPIFFE CSI driver 挂载的 socket）。
	source, err := workloadapi.NewJWTSource(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := networkv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metrics},
		HealthProbeBindAddress: probe,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "ayatori-cloud-provider",
	})
	if err != nil {
		return err
	}
	ayatoriConfig := &rest.Config{Host: server, CAFile: caFile}
	cloudprovider.WithJWTSVID(ayatoriConfig, source, audience)
	ayatori, err := cluster.New(ayatoriConfig, func(o *cluster.Options) {
		o.Scheme = scheme
		o.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
	})
	if err != nil {
		return err
	}
	if err := manager.Add(ayatori); err != nil {
		return err
	}
	reconciler := &cloudprovider.ServiceReconciler{
		Client: manager.GetClient(), Ayatori: ayatori.GetClient(), Namespace: namespace,
	}
	if err := reconciler.SetupWithManager(manager, ayatori); err != nil {
		return err
	}
	if err := manager.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return manager.Start(ctx)
}
