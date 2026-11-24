package bootstrap

import (
	"crypto/tls"
	"flag"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	executionv1alpha1 "go.ddupan.top/ayatori/api/execution/v1alpha1"
)

func parseTestOptions(t *testing.T, args ...string) options {
	t.Helper()
	flags := flag.NewFlagSet("bootstrap-test", flag.ContinueOnError)
	var options options
	options.bindFlags(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return options
}

func TestDefaultConfiguration(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	options := parseTestOptions(t)
	manager := options.manager.configuration()
	if manager.Metrics.BindAddress != "0" || !manager.Metrics.SecureServing || manager.Metrics.FilterProvider == nil {
		t.Fatal("metrics defaults or authentication changed")
	}
	if manager.HealthProbeBindAddress != ":8081" || manager.LeaderElection ||
		manager.LeaderElectionID != "a6325ed6.ddupan.top" || manager.LeaderElectionReleaseOnCancel {
		t.Fatal("probe or leader election defaults changed")
	}
	if options.manager.webhookOptions().Port != 9443 || !options.logging.Development {
		t.Fatal("webhook or logging defaults changed")
	}
	if options.database.secretNamespace != "" || options.database.credentialMount != "" || options.openBao.address != "" {
		t.Fatal("optional backends enabled by default")
	}
	if options.openBao.mount != "kubernetes" || options.openBao.identity.Audience != "openbao" {
		t.Fatal("OpenBao defaults changed")
	}
	options.database.configureManager(&manager)
	if len(manager.Cache.ByObject) != 0 {
		t.Fatal("disabled observation unexpectedly configured Secret cache")
	}
	for _, object := range []runtime.Object{
		&corev1.Secret{}, &corev1.ServiceAccount{}, &executionv1alpha1.Job{},
		&databasev1alpha1.PostgreSQLInstance{}, &databasev1alpha1.PostgreSQLDatabase{}, &databasev1alpha1.PostgreSQLTenant{},
	} {
		if _, _, err := manager.Scheme.ObjectKinds(object); err != nil {
			t.Fatalf("missing scheme registration for %T: %v", object, err)
		}
	}
}

func TestCredentialPreparationOptions(t *testing.T) {
	options := parseTestOptions(t, "--database-credential-mount=applications-kv", "--database-credential-prefix=database")
	if options.database.credentialMount != "applications-kv" || options.database.credentialPrefix != "database" {
		t.Fatal("凭据准备参数未传入领域装配")
	}
	if err := registerDatabaseSupplyController(nil, options.database, nil, nil); err == nil {
		t.Fatal("启用凭据准备必须有显式配置的认证 client")
	}
	if err := registerDatabaseSupplyController(nil, databaseOptions{provisionResources: true}, nil, nil); err == nil {
		t.Fatal("启用资源供应必须有管理连接及凭据准备")
	}
}

func TestManagerFlagOverrides(t *testing.T) {
	options := parseTestOptions(t,
		"--metrics-bind-address=:9090", "--metrics-secure=false", "--health-probe-bind-address=:9091",
		"--leader-elect", "--leader-election-namespace=ayatori-system", "--webhook-port=-1", "--metrics-cert-path=/fixture/metrics",
		"--metrics-cert-name=server.crt", "--metrics-cert-key=server.key", "--webhook-cert-path=/fixture/webhook",
		"--webhook-cert-name=hook.crt", "--webhook-cert-key=hook.key",
	)
	manager := options.manager.configuration()
	if manager.Metrics.BindAddress != ":9090" || manager.Metrics.SecureServing || manager.Metrics.FilterProvider != nil ||
		manager.HealthProbeBindAddress != ":9091" || !manager.LeaderElection ||
		manager.LeaderElectionNamespace != "ayatori-system" {
		t.Fatal("manager flags not applied")
	}
	if manager.Metrics.CertDir != "/fixture/metrics" || manager.Metrics.CertName != "server.crt" || manager.Metrics.KeyName != "server.key" {
		t.Fatal("metrics certificate flags not applied")
	}
	webhook := options.manager.webhookOptions()
	if webhook.Port != -1 || webhook.CertDir != "/fixture/webhook" || webhook.CertName != "hook.crt" || webhook.KeyName != "hook.key" {
		t.Fatal("webhook flags not applied")
	}
}

func TestHTTP2Policy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		args := []string{}
		if enabled {
			args = append(args, "--enable-http2")
		}
		options := parseTestOptions(t, args...)
		for _, callbacks := range [][]func(*tls.Config){options.manager.metricsOptions().TLSOpts, options.manager.webhookOptions().TLSOpts} {
			config := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
			for _, callback := range callbacks {
				callback(config)
			}
			if slices.Contains(config.NextProtos, "h2") != enabled || !slices.Contains(config.NextProtos, "http/1.1") {
				t.Fatal("HTTP/2 policy changed")
			}
		}
	}
}

func TestComponentFlagsAndNamespaceScope(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "from-environment")
	if parseTestOptions(t).database.secretNamespace != "from-environment" {
		t.Fatal("namespace environment default lost")
	}
	options := parseTestOptions(t,
		"--database-secret-namespace=controller", "--database-root-cert=/fixture/postgres-ca.pem",
		"--openbao-address=https://bao.example", "--openbao-ca-cert=/fixture/bao-ca.pem",
		"--openbao-auth-mount=cluster", "--openbao-auth-role=controller",
		"--openbao-service-account-namespace=identity", "--openbao-service-account-name=bao-login",
		"--openbao-token-audience=bao",
	)
	if options.database.secretNamespace != "controller" || options.database.rootCert != "/fixture/postgres-ca.pem" {
		t.Fatal("Database flags not applied")
	}
	bao := options.openBao
	if bao.address != "https://bao.example" || bao.caCert != "/fixture/bao-ca.pem" || bao.mount != "cluster" ||
		bao.role != "controller" || bao.identity.Namespace != "identity" || bao.identity.ServiceAccount != "bao-login" || bao.identity.Audience != "bao" {
		t.Fatal("OpenBao flags not applied")
	}
	manager := options.manager.configuration()
	options.database.configureManager(&manager)
	if len(manager.Cache.ByObject) != 1 {
		t.Fatal("Secret cache scope missing")
	}
	for object, config := range manager.Cache.ByObject {
		if _, ok := object.(*corev1.Secret); !ok {
			t.Fatal("unexpected cache object")
		}
		if _, ok := config.Namespaces["controller"]; !ok || len(config.Namespaces) != 1 {
			t.Fatal("Secret cache escaped explicit namespace")
		}
	}
}
