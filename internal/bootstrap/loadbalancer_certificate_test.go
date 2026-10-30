package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const pemCertificate = "CERTIFICATE"

type testCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{certificate: certificate, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: der})}
}

// issue 返回 PEM 证书与私钥；usage 决定是客户端还是服务端证书。
func (ca testCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func TestClientCertificateRotatesWithoutRestart(t *testing.T) {
	serverCA, clientCA := newTestCA(t, "server-ca"), newTestCA(t, "client-ca")

	// 与 LB 主机相同：只接受 clientCA 签发的客户端证书，并记录每次握手看到的 CN。
	var mu sync.Mutex
	var seen []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.TLS.PeerCertificates[0].Subject.CommonName)
		mu.Unlock()
	}))
	serverCert, serverKey := serverCA.issue(t, "lb1", x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPool := x509.NewCertPool()
	clientPool.AddCert(clientCA.certificate)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: clientPool, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	defer server.Close()

	secret := &corev1.Secret{}
	secret.Namespace, secret.Name = "ayatori-system", "loadbalancer-hosts"
	writeClient := func(name string) {
		crt, key := clientCA.issue(t, name, x509.ExtKeyUsageClientAuth)
		secret.Data = map[string][]byte{"tls.crt": crt, "tls.key": key, "ca.crt": serverCA.pem}
	}
	writeClient("v1")
	reader := fake.NewClientBuilder().WithObjects(secret.DeepCopy()).Build()

	certificate, err := newClientCertificate(secret, reader)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := loadBalancerProxy(secret, certificate, loadBalancerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	call := func() {
		t.Helper()
		// 新连接才会重新握手；已建立的连接沿用握手时的证书。
		proxy.HTTP.CloseIdleConnections()
		response, err := proxy.HTTP.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	update := func() {
		t.Helper()
		if err := reader.Update(t.Context(), secret.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}

	call()
	writeClient("v2")
	update()
	if err := certificate.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	call()

	// 损坏的 Secret 不得替换可用证书。
	secret.Data["tls.key"] = []byte("not a key")
	update()
	if err := certificate.refresh(t.Context()); err == nil {
		t.Fatal("损坏的证书应报告错误")
	}
	call()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != "v1" || seen[1] != "v2" || seen[2] != "v2" {
		t.Fatalf("握手使用的证书 = %v, want [v1 v2 v2]", seen)
	}
}
