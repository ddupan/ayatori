// ayatori-api 提供 apiserver 上的聚合子资源（ADR-0027）：apiserver 认证、授权并审计请求后经聚合层转来，
// 它以自己的身份读 API 与 OpenBao。不存储任何对象，可以运行在任何 apiserver 能连到的主机上。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	subresourcesv1alpha1 "go.ddupan.top/ayatori/api/subresources/v1alpha1"
	databasebao "go.ddupan.top/ayatori/internal/database/adapter/openbao"
	"go.ddupan.top/ayatori/internal/infra/openbao"
	"go.ddupan.top/ayatori/internal/subresource"
)

type options struct {
	listen, tlsCert, tlsKey     string
	baoAddress, baoCA           string
	baoMount, baoRole           string
	identity                    openbao.KubernetesIdentity
	credentialMount, credPrefix string
}

func main() {
	var o options
	flag.StringVar(&o.listen, "listen", ":8443", "HTTPS 监听地址")
	flag.StringVar(&o.tlsCert, "tls-cert", "", "服务证书 PEM，须含 APIService 所引用 Service 的 <service>.<namespace>.svc")
	flag.StringVar(&o.tlsKey, "tls-key", "", "服务证书私钥 PEM")
	flag.StringVar(&o.baoAddress, "openbao-address", "", "OpenBao HTTPS 地址")
	flag.StringVar(&o.baoCA, "openbao-ca-cert", "", "OpenBao 公开 CA PEM 路径；默认使用系统信任根")
	flag.StringVar(&o.baoMount, "openbao-auth-mount", "kubernetes", "OpenBao Kubernetes auth mount")
	flag.StringVar(&o.baoRole, "openbao-auth-role", "", "OpenBao 登录 role：只能读 Ayatori 交付的凭据")
	flag.StringVar(&o.identity.Namespace, "openbao-service-account-namespace", "", "TokenRequest 的 ServiceAccount namespace")
	flag.StringVar(&o.identity.ServiceAccount, "openbao-service-account-name", "", "TokenRequest 的 ServiceAccount 名称")
	flag.StringVar(&o.identity.Audience, "openbao-token-audience", "openbao", "SA JWT audience，须匹配 OpenBao role")
	flag.StringVar(&o.credentialMount, "database-credential-mount", "", "应用凭据 KV v2 mount，与 manager 相同")
	flag.StringVar(&o.credPrefix, "database-credential-prefix", "applications", "应用凭据路径前缀，与 manager 相同")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, logger); err != nil {
		logger.Error("ayatori-api exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, logger *slog.Logger) error {
	if o.tlsCert == "" || o.tlsKey == "" || o.baoAddress == "" || o.baoRole == "" || o.credentialMount == "" {
		return errors.New("--tls-cert, --tls-key, --openbao-address, --openbao-auth-role and --database-credential-mount are required")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, databasev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	// 客户端证书经 ADR-0021 签发，kubeconfig 由 --kubeconfig 或 KUBECONFIG 给出。
	config, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	frontProxy, err := subresource.LoadFrontProxy(ctx, kube)
	if err != nil {
		return err
	}
	baoClient, err := openbao.NewClient(o.baoAddress, o.baoCA)
	if err != nil {
		return err
	}
	session, err := openbao.NewKubernetesSession(baoClient, kube, o.baoMount, o.baoRole, o.identity)
	if err != nil {
		return err
	}
	credentials, err := databasebao.NewCredentials(baoClient, o.credentialMount, o.credPrefix)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              o.listen,
		Handler:           subresource.Server{Kube: kube, Credentials: credentials, FrontProxy: frontProxy, Logger: logger}.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: frontProxy.TLSConfig(func() (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
			return &c, err
		}),
	}
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- session.Start(ctx) }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServeTLS("", "") }()
	logger.Info("serving", "address", o.listen, "group", subresourcesv1alpha1.Group)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown) //nolint:contextcheck // 收到信号后 ctx 已取消
	case err := <-serveDone:
		return err
	case err := <-sessionDone:
		return err
	}
}
