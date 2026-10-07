//go:build integration

package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestBootstrapWithRealAPIServer(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	user, err := environment.AddUser(envtest.User{Name: "bootstrap-fixture", Groups: []string{"system:masters"}}, config)
	if err != nil {
		t.Fatal(err)
	}
	data, err := user.KubeConfig()
	if err != nil {
		t.Fatal("cannot generate isolated fixture kubeconfig")
	}
	// 仅写临时 envtest 身份，不读取现场 kubeconfig；t.TempDir 会自动清理。
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	t.Setenv("POD_NAMESPACE", "")
	createLoadBalancerCredentials(t, config)
	options := parseTestOptions(t, "--metrics-bind-address=0", "--health-probe-bind-address=0", "--webhook-port=-1",
		"--loadbalancer-address-pool=10.62.0.0/24", "--loadbalancer-host=lb1,https://127.0.0.1:1,127.0.0.1:1",
		"--loadbalancer-credentials=default/loadbalancer-hosts")
	manager, err := newManager(options)
	if err != nil {
		t.Fatal(err)
	}
	baoClient, err := setupOpenBaoAuthentication(manager, options.openBao)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// 空 API 中没有供应目标；启用凭据准备以验证完整装配路径确实注册 controller，不访问外部 Bao。
	fixtureConfig := bao.NewConfig()
	fixtureConfig.Address = "http://127.0.0.1:1"
	fixtureClient, err := bao.NewClient(fixtureConfig)
	if err != nil {
		t.Fatal(err)
	}
	if baoClient == nil {
		baoClient = fixtureClient
	}
	options.database.credentialMount = "secret"
	closeDatabaseConnections, err := registerDatabaseControllers(ctx, manager, options.database, baoClient)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDatabaseConnections()
	closeLoadBalancerConnections, err := registerLoadBalancerControllers(ctx, manager, options.lb)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLoadBalancerConnections()
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("manager did not stop before component closeDatabaseConnections")
		}
	}()
	if !manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("assembled controller cache did not synchronize")
	}
}

// createLoadBalancerCredentials 写入只用于本测试的自签证书；装配路径须能解析 mTLS 凭据。
func createLoadBalancerCredentials(t *testing.T, config *rest.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ayatori-test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{Name: "loadbalancer-hosts", Namespace: "default", Data: map[string][]byte{
		"username": []byte("ayatori"), "password": []byte("AYATORI-TEST-ONLY"),
		"tls.crt": certificate, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), "ca.crt": certificate,
	}}
	if _, err := clientset.CoreV1().Secrets("default").Create(t.Context(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
