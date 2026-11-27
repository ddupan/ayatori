// cloud-provider 运行在 workload 集群中，把 loadBalancerClass 为 ayatori.ddupan.top/loadbalancer 的
// Service 翻译为 Ayatori 的 LoadBalancer 与 Backend（ADR-0028）。由消费方部署，只在 --ayatori-namespace 中
// 读写。连接 Ayatori 有两种方式：--ayatori-server 与 --ayatori-ca-file，以机器身份认证（ayatoriclient）；
// 或 --ayatori-kubeconfig，凭据由 kubeconfig 决定（客户端证书、exec plugin 等）。
package main

import (
	"context"
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	networkv1alpha1 "go.ddupan.top/ayatori/api/network/v1alpha1"
	"go.ddupan.top/ayatori/internal/ayatoriclient"
	"go.ddupan.top/ayatori/internal/cloudprovider"
)

func main() {
	server := flag.String("ayatori-server", "", "Ayatori API 地址，如 https://ayatori-prod.lab.ddupan.top:6443")
	caFile := flag.String("ayatori-ca-file", "", "Ayatori apiserver 的 CA（kube-public/ayatori-login 公布的 CA）")
	kubeconfig := flag.String("ayatori-kubeconfig", "",
		"连接 Ayatori 的 kubeconfig；给出时不用 --ayatori-server 与 --ayatori-ca-file")
	namespace := flag.String("ayatori-namespace", "", "Ayatori 中代表本集群的 namespace")
	metrics := flag.String("metrics-bind-address", ":8080", "指标端口；0 关闭")
	probe := flag.String("health-probe-bind-address", ":8081", "健康检查端口")
	leaderElect := flag.Bool("leader-elect", true, "在本集群以 Lease 选主")
	options := zap.Options{}
	options.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options)))
	log := ctrl.Log.WithName("cloud-provider")
	if *namespace == "" || (*kubeconfig == "") == (*server == "" || *caFile == "") {
		log.Error(nil, "--ayatori-namespace is required, with either --ayatori-kubeconfig "+
			"or --ayatori-server and --ayatori-ca-file")
		os.Exit(2)
	}
	if err := run(*kubeconfig, *server, *caFile, *namespace, *metrics, *probe, *leaderElect); err != nil {
		log.Error(err, "cloud-provider exited")
		os.Exit(1)
	}
}

func run(kubeconfig, server, caFile, namespace, metrics, probe string, leaderElect bool) error {
	ctx := ctrl.SetupSignalHandler()
	ayatoriConfig, err := ayatoriRESTConfig(ctx, kubeconfig, server, caFile)
	if err != nil {
		return err
	}

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

// ayatoriRESTConfig：给了 kubeconfig 就照它连接；否则以部署方给出的 CA 读取环境的发现信息（audience 即其
// clientID），以机器身份认证。
func ayatoriRESTConfig(ctx context.Context, kubeconfig, server, caFile string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	ca, err := os.ReadFile(caFile) //nolint:gosec // 部署方给出的路径
	if err != nil {
		return nil, err
	}
	trust, err := ayatoriclient.TrustCA(ca)
	if err != nil {
		return nil, err
	}
	profile, err := ayatoriclient.Discover(ctx, server, trust)
	if err != nil {
		return nil, err
	}
	return ayatoriclient.RESTConfig(ctx, profile, func(ctx context.Context) (string, error) {
		return ayatoriclient.MachineToken(ctx, profile)
	})
}
