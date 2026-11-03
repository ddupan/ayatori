//go:build integration

package testenv

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
)

// ClientCA 既是 apiserver 的 client CA，也在测试中代替 kube-controller-manager 的 csrsigning
// 签发证书，相当于 k0s 的 CA。设为 envtest 的 Authn，并用 Configure 打开 bootstrap token 认证。
type ClientCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	dir  string
}

func NewClientCA(t *testing.T) *ClientCA {
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
	return &ClientCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (c *ClientCA) sign(pub any, subject pkix.Name, lifetime time.Duration) []byte {
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
func (c *ClientCA) Configure(workDir string, args *envtest.Arguments) error {
	c.dir = workDir
	args.Set("client-ca-file", filepath.Join(workDir, "client-ca.crt"))
	args.Set("enable-bootstrap-token-auth", "true")
	return nil
}
func (c *ClientCA) Start() error {
	return os.WriteFile(filepath.Join(c.dir, "client-ca.crt"), c.pem, 0o600)
}
func (c *ClientCA) Stop() error { return nil }
func (c *ClientCA) AddUser(user envtest.User, base *rest.Config) (*rest.Config, error) {
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

// SignApproved 签发已批准的 kube-apiserver-client CSR，代替 csrsigning，直到 ctx 结束。
func (c *ClientCA) SignApproved(ctx context.Context, admin kubernetes.Interface, lifetime time.Duration) {
	for ctx.Err() == nil {
		list, err := admin.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range list.Items {
				csr := &list.Items[i]
				if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName || len(csr.Status.Certificate) > 0 || !Approved(csr) {
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

// Approved 报告 CSR 是否已被批准。
func Approved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved {
			return true
		}
	}
	return false
}

// ApplyManifest 创建一个多文档清单中的全部对象。
func ApplyManifest(t *testing.T, cfg *rest.Config, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
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

// BootstrapToken 创建 bootstrap token（额外组 clientcert.BootstrapGroup），并在身份目录 dir 写入
// bootstrap-kubeconfig，相当于部署方的首次签发准备。
func BootstrapToken(t *testing.T, admin kubernetes.Interface, cfg *rest.Config, dir, tokenID string) {
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
