//go:build integration

package subresource_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	subresourcesv1alpha1 "go.ddupan.top/ayatori/api/subresources/v1alpha1"
	"go.ddupan.top/ayatori/internal/cli"
	databasebao "go.ddupan.top/ayatori/internal/database/adapter/openbao"
	"go.ddupan.top/ayatori/internal/subresource"
	"go.ddupan.top/ayatori/internal/testenv"
)

const (
	team, ready, pending = "team-a", "web", "pending"
	app, system          = "app", "ayatori-system"       // 数据库与登录名；ayatori-api 所在 namespace
	baoToken             = "AYATORI-TEST-ONLY-bao-token" // hack/integration-test.sh 的 dev server
)

// 真实 apiserver（envtest，配好 front-proxy）经 APIService 把子资源转给 ayatori-api，后端是真实 OpenBao：
// RBAC 在 apiserver，ayatori-api 只认 front-proxy 证书，凭据按 Tenant status 公布的位置与版本读取。
func TestDatabaseCredentialsThroughTheAggregator(t *testing.T) {
	dir := t.TempDir()
	frontCA, frontCAKey := newCA(t, "front-proxy-ca")
	writeCert(t, filepath.Join(dir, "front-proxy-ca.crt"), frontCA.Raw)
	writeKeyPair(t, filepath.Join(dir, "front-proxy-client"), leaf(t, frontCA, frontCAKey, "front-proxy-client", nil, true))
	servingCA, servingCAKey := newCA(t, "ayatori-api-ca")
	serving := leaf(t, servingCA, servingCAKey, "ayatori-api", []string{"ayatori-api." + system + ".svc"}, false)

	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("requestheader-client-ca-file", filepath.Join(dir, "front-proxy-ca.crt"))
	args.Set("requestheader-allowed-names", "front-proxy-client")
	args.Set("requestheader-username-headers", "X-Remote-User")
	args.Set("requestheader-group-headers", "X-Remote-Group")
	args.Set("requestheader-extra-headers-prefix", "X-Remote-Extra-")
	args.Set("proxy-client-cert-file", filepath.Join(dir, "front-proxy-client.crt"))
	args.Set("proxy-client-key-file", filepath.Join(dir, "front-proxy-client.key"))
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := t.Context()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme,
		databasev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	as := func(user string) *rest.Config {
		c := rest.CopyConfig(config)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		return c
	}
	// 部署的 RBAC（deploy/flux/control-plane/api.yaml）：ayatori-api 以自己的证书用户读取，用户经
	// ayatori-database-credentials 获得子资源。
	create(t, admin, &corev1.Namespace{Name: system}, &corev1.Namespace{Name: team})
	applyManifest(t, admin, filepath.Join("..", "..", "deploy", "flux", "control-plane", "api.yaml"))
	apiUser, err := client.New(as("ayatori-api"), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	// OpenBao：Tenant app 的凭据在 secret/applications/<scenario>，版本 1。
	_, port := testenv.Container(t, "openbao", 8200)
	root, err := bao.NewClient(&bao.Config{Address: "http://127.0.0.1:" + strconv.Itoa(port)})
	if err != nil {
		t.Fatal(err)
	}
	root.SetToken(baoToken)
	_ = root.Sys().MountWithContext(ctx, "secret", &bao.MountInput{Type: "kv", Options: map[string]string{"version": "2"}})
	path := "applications/s" + strconv.FormatInt(testenv.Scenario(), 10)
	if _, err := root.KVv2("secret").Put(ctx, path, map[string]any{"username": app, "password": "s3cret",
		"database": app, "host": "pg.example", "port": "5432", "sslmode": "verify-full"}); err != nil {
		t.Fatal(err)
	}
	credentials, err := databasebao.NewCredentials(root, "secret", "applications")
	if err != nil {
		t.Fatal(err)
	}

	// ayatori-api 在本机，APIService 经 ExternalName Service 找到它（与 Dev 的部署方式相同）。
	frontProxy, err := subresource.LoadFrontProxy(ctx, apiUser)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:           subresource.Server{Kube: apiUser, Credentials: credentials, FrontProxy: frontProxy, Logger: slog.New(slog.DiscardHandler)}.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         frontProxy.TLSConfig(func() (*tls.Certificate, error) { return &serving, nil }),
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	apiPort := int32(listener.Addr().(*net.TCPAddr).Port) //nolint:gosec // 本机端口
	register(t, admin, apiPort, servingCA.Raw)

	// alice 在 team-a 被授予 ayatori-database-credentials，bob 没有。
	create(t, admin, &rbacv1.RoleBinding{Name: "alice", Namespace: team,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ayatori-database-credentials"},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "alice"}}})
	tenant(t, admin, ready, &databasev1alpha1.CredentialReference{Mount: "secret", Path: path}, 1)
	tenant(t, admin, pending, nil, 0)

	t.Run("a user granted the subresource reads the published credentials", func(t *testing.T) {
		c, err := cli.DatabaseCredentials(ctx, as("alice"), team, ready)
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != "DatabaseCredentials" || c.Name != ready || c.Version != 1 || c.Host != "pg.example" ||
			c.Port != "5432" || c.SSLMode != "verify-full" || c.Database != app || c.Username != app || c.Password != "s3cret" {
			t.Errorf("credentials %+v", c)
		}
	})

	t.Run("the apiserver refuses a user without the subresource", func(t *testing.T) {
		_, err := cli.DatabaseCredentials(ctx, as("bob"), team, ready)
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `User "bob" cannot get resource "postgresqltenants/credentials"`) {
			t.Errorf("bob: %v, want the apiserver's forbidden reason", err)
		}
	})

	t.Run("credentials not yet published are a conflict, a missing tenant is not found", func(t *testing.T) {
		if _, err := cli.DatabaseCredentials(ctx, as("alice"), team, pending); !apierrors.IsConflict(err) {
			t.Errorf("pending: %v, want conflict", err)
		}
		if _, err := cli.DatabaseCredentials(ctx, as("alice"), team, "missing"); !apierrors.IsNotFound(err) ||
			!strings.Contains(err.Error(), `"missing" not found`) {
			t.Errorf("missing: %v, want ayatori-api's not found", err)
		}
	})

	t.Run("a version the store does not hold is a conflict, not another version", func(t *testing.T) {
		tenant(t, admin, "stale", &databasev1alpha1.CredentialReference{Mount: "secret", Path: path}, 2)
		if _, err := cli.DatabaseCredentials(ctx, as("alice"), team, "stale"); !apierrors.IsConflict(err) {
			t.Errorf("stale: %v, want conflict", err)
		}
	})

	t.Run("ayatori-api accepts no connection without the front-proxy certificate", func(t *testing.T) {
		pool := x509.NewCertPool()
		pool.AddCert(servingCA)
		direct := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool,
			ServerName: "ayatori-api." + system + ".svc", MinVersion: tls.VersionTLS12}}}
		resp, err := direct.Get("https://" + listener.Addr().String() + "/apis/" + subresourcesv1alpha1.Group + "/" +
			subresourcesv1alpha1.Version + "/namespaces/" + team + "/postgresqltenants/" + ready + "/credentials")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("direct request answered %s", resp.Status)
		}
	})
}

// register 建 ExternalName Service 与 APIService，等聚合层确认可用。
func register(t *testing.T, admin client.Client, port int32, caBundle []byte) {
	t.Helper()
	name := subresourcesv1alpha1.Version + "." + subresourcesv1alpha1.Group
	apiService := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"group": subresourcesv1alpha1.Group, "version": subresourcesv1alpha1.Version,
			"groupPriorityMinimum": int64(1000), "versionPriority": int64(15),
			"caBundle": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caBundle})),
			"service":  map[string]any{"namespace": system, "name": "ayatori-api", "port": int64(port)}},
	}}
	create(t, admin,
		&corev1.Service{Name: "ayatori-api", Namespace: system,
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "127.0.0.1",
				Ports: []corev1.ServicePort{{Port: port}}}},
		apiService)
	deadline := time.Now().Add(time.Minute)
	for {
		if err := admin.Get(t.Context(), client.ObjectKey{Name: name}, apiService); err != nil {
			t.Fatal(err)
		}
		conditions, _, _ := unstructured.NestedSlice(apiService.Object, "status", "conditions")
		for _, c := range conditions {
			if c, ok := c.(map[string]any); ok && c["type"] == "Available" && c["status"] == "True" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("APIService not available: %v", conditions)
		}
		time.Sleep(time.Second)
	}
}

// applyManifest 创建清单中的全部对象。
func applyManifest(t *testing.T, admin client.Client, path string) {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // 仓库内的清单
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var o unstructured.Unstructured
		if err := decoder.Decode(&o.Object); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if len(o.Object) > 0 {
			create(t, admin, &o)
		}
	}
}

func tenant(t *testing.T, admin client.Client, name string, ref *databasev1alpha1.CredentialReference, version int64) {
	t.Helper()
	tn := &databasev1alpha1.PostgreSQLTenant{Name: name, Namespace: team,
		Spec: databasev1alpha1.PostgreSQLTenantSpec{DatabaseRef: &databasev1alpha1.DatabaseReference{Name: "db"}}}
	create(t, admin, tn)
	if ref == nil {
		return
	}
	tn.Status.CredentialRef, tn.Status.CredentialVersion = ref, version
	if err := admin.Status().Update(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
}

func create(t *testing.T, admin client.Client, objects ...client.Object) {
	t.Helper()
	for _, o := range objects {
		if err := admin.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func newCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

// leaf 签发客户端（client）或服务（dnsNames）证书。
func leaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, dnsNames []string, isClient bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.ExtKeyUsageServerAuth
	if isClient {
		usage = x509.ExtKeyUsageClientAuth
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: dnsNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writeCert(t *testing.T, path string, der []byte) {
	t.Helper()
	write(t, path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// writeKeyPair 写 <base>.crt 与 <base>.key。
func writeKeyPair(t *testing.T, base string, cert tls.Certificate) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	writeCert(t, base+".crt", cert.Certificate[0])
	write(t, base+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
