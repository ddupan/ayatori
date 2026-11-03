//go:build integration

package clientcert_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
)

// ca 既是 apiserver 的 client CA，也在测试中代替 kube-controller-manager 的 csrsigning 签发证书，
// 相当于 k0s 的 CA。
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	dir  string
}

func newCA(t *testing.T) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &ca{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (c *ca) sign(pub any, subject pkix.Name, lifetime time.Duration) []byte {
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{SerialNumber: serial, Subject: subject,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(lifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, pub, c.key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// envtest.Authn
func (c *ca) Configure(workDir string, args *envtest.Arguments) error {
	c.dir = workDir
	args.Set("client-ca-file", filepath.Join(workDir, "client-ca.crt"))
	return nil
}
func (c *ca) Start() error { return os.WriteFile(filepath.Join(c.dir, "client-ca.crt"), c.pem, 0o600) }
func (c *ca) Stop() error  { return nil }
func (c *ca) AddUser(user envtest.User, base *rest.Config) (*rest.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	config := rest.CopyConfig(base)
	config.CertData = c.sign(&key.PublicKey, pkix.Name{CommonName: user.Name, Organization: user.Groups}, 24*time.Hour)
	config.KeyData = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return config, nil
}

// signApproved 签发已批准的 kube-apiserver-client CSR，代替 csrsigning。
func (c *ca) signApproved(ctx context.Context, admin kubernetes.Interface, lifetime time.Duration) {
	for ctx.Err() == nil {
		list, err := admin.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range list.Items {
				csr := &list.Items[i]
				if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName || len(csr.Status.Certificate) > 0 || !approved(csr) {
					continue
				}
				block, _ := pem.Decode(csr.Spec.Request)
				req, err := x509.ParseCertificateRequest(block.Bytes)
				if err != nil {
					continue
				}
				csr.Status.Certificate = c.sign(req.PublicKey, req.Subject, lifetime)
				_, _ = admin.CertificatesV1().CertificateSigningRequests().UpdateStatus(ctx, csr, metav1.UpdateOptions{})
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func approved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %v", timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestClientCertificates(t *testing.T) {
	// client-go 默认每 5 分钟重新加载证书文件；测试中的证书只有几十秒。
	transport.CertCallbackRefreshDuration = time.Second
	ctrl.SetLogger(logr.Discard())
	ctx := t.Context()
	authority := newCA(t)
	env := &envtest.Environment{}
	api := env.ControlPlane.GetAPIServer()
	api.Authn = authority
	api.Configure().Set("enable-bootstrap-token-auth", "true")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(cfg)
	applyRBAC(t, cfg)

	// 证书 40 秒到期：证书管理器在 70–90% 时续签，测试在一分钟内看到续签和过期后的重新加载。
	go authority.signApproved(ctx, admin, 40*time.Second)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&clientcert.RenewalApprover{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	dir := t.TempDir()
	const tokenID, user = "abcdef", "test-component"
	writeBootstrap(t, admin, cfg, dir, tokenID)
	stop, err := clientcert.Start(ctx, clientcert.Identity{Dir: dir, CommonName: user, UID: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	t.Run("bootstrap token alone gets no certificate", func(t *testing.T) {
		var csr *certificatesv1.CertificateSigningRequest
		eventually(t, 30*time.Second, func() error {
			csr = requestedBy(t, admin, "system:bootstrap:"+tokenID)
			if csr == nil {
				return fmt.Errorf("no CSR from the bootstrap token yet")
			}
			return nil
		})
		time.Sleep(2 * time.Second)
		if csr = requestedBy(t, admin, "system:bootstrap:"+tokenID); approved(csr) {
			t.Fatal("the renewal approver approved a bootstrap request")
		}
	})

	t.Run("deployer rejects a request for another identity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := clientcert.ApproveBootstrap(ctx, admin, tokenID, "someone-else", nil); err == nil {
			t.Fatal("approved a CSR for another identity")
		}
	})

	var first *x509.Certificate
	t.Run("deployer approves the first certificate", func(t *testing.T) {
		// 被拒绝后证书管理器会以同一 token 重新申请。
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := clientcert.ApproveBootstrap(ctx, admin, tokenID, user, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, func() error {
			var err error
			first, err = current(dir)
			return err
		})
		whoami(t, dir, user)
	})

	t.Run("renews itself and clients reload the file", func(t *testing.T) {
		config, err := clientcert.RESTConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		long := kubernetes.NewForConfigOrDie(config)
		eventually(t, time.Minute, func() error {
			cert, err := current(dir)
			if err != nil {
				return err
			}
			if cert.SerialNumber.Cmp(first.SerialNumber) == 0 {
				return fmt.Errorf("not renewed yet")
			}
			return nil
		})
		// 等第一张证书过期：同一个客户端仍能访问，说明它已换用续签后的证书。
		time.Sleep(time.Until(first.NotAfter) + 2*time.Second)
		if _, err := long.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("client built before renewal stopped working: %v", err)
		}
	})
}

// applyRBAC 应用部署 role 中同一份 RBAC 清单。
func applyRBAC(t *testing.T, cfg *rest.Config) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "ansible", "roles", "control_plane", "files", "client-certificates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatal(err)
		}
		if err := c.Create(t.Context(), &obj); err != nil {
			t.Fatal(err)
		}
	}
}

func writeBootstrap(t *testing.T, admin kubernetes.Interface, cfg *rest.Config, dir, tokenID string) {
	t.Helper()
	secret := &corev1.Secret{
		Namespace: metav1.NamespaceSystem, Name: "bootstrap-token-" + tokenID,
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id": tokenID, "token-secret": "0123456789abcdef",
			"expiration":                     time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"auth-extra-groups":              clientcert.BootstrapGroup,
		},
	}
	if _, err := admin.CoreV1().Secrets(metav1.NamespaceSystem).Create(t.Context(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"ayatori": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"bootstrap": {Token: tokenID + ".0123456789abcdef"}},
		Contexts:       map[string]*clientcmdapi.Context{"default": {Cluster: "ayatori", AuthInfo: "bootstrap"}},
		CurrentContext: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, clientcert.BootstrapKubeconfig), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func requestedBy(t *testing.T, admin kubernetes.Interface, user string) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	list, err := admin.CertificatesV1().CertificateSigningRequests().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Username == user {
			return &list.Items[i]
		}
	}
	return nil
}

func current(dir string) (*x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.pem"))
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

func whoami(t *testing.T, dir, user string) {
	t.Helper()
	config, err := clientcert.RESTConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	review, err := kubernetes.NewForConfigOrDie(config).AuthenticationV1().SelfSubjectReviews().
		Create(t.Context(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info := review.Status.UserInfo
	if info.Username != user || !slices.Contains(info.Groups, clientcert.Group) {
		t.Fatalf("authenticated as %s %v", info.Username, info.Groups)
	}
}
