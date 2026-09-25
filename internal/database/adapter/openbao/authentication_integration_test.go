//go:build integration

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openbao_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
)

const (
	authNamespace = "bao-controller"
	authAudience  = "openbao"
)

// Bao 容器通过 Docker bridge 访问这个仅转发 TokenReview 的临时入口。
// 上游仍是带 CA 验证的真实 envtest API；不模拟 JWT 签名、audience 或 RBAC 判定。
func tokenReviewEndpoint(t *testing.T, config *rest.Config) (string, string) {
	t.Helper()
	upstream, err := url.Parse(config.Host)
	if err != nil {
		t.Fatal("invalid envtest address")
	}
	transport, err := rest.TransportFor(rest.AnonymousClientConfig(config))
	if err != nil {
		t.Fatal("cannot construct TokenReview transport")
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) }
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	if err := server.Listener.Close(); err != nil {
		t.Fatal("cannot replace fixture listener")
	}
	server.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal("cannot expose TokenReview fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "network", "inspect", "bridge", "--format",
		`{{(index .IPAM.Config 0).Gateway}}`).Output()
	if err != nil {
		t.Fatal("cannot locate fixture Docker bridge")
	}
	gateway := strings.TrimSpace(string(output))
	if net.ParseIP(gateway) == nil {
		t.Fatal("invalid fixture bridge gateway")
	}
	certificate, caPEM := tokenReviewCertificate(t, net.ParseIP(gateway))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	port := server.Listener.Addr().(*net.TCPAddr).Port
	return "https://" + net.JoinHostPort(gateway, strconv.Itoa(port)), caPEM
}

func tokenReviewCertificate(t *testing.T, address net.IP) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("cannot create fixture TLS key")
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{address},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal("cannot create fixture TLS certificate")
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return certificate, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestKubernetesSessionWithRealTokenReview(t *testing.T) {
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		t.Fatal("cannot start authentication API fixture", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error("cannot stop authentication API fixture")
		}
	})
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal("cannot construct fixture API client")
	}
	ctx := t.Context()
	for _, namespace := range []string{authNamespace, "bao-unrelated"} {
		if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{Name: namespace}, metav1.CreateOptions{}); err != nil {
			t.Fatal("cannot create fixture namespace")
		}
		if _, err := clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{Name: authRole}, metav1.CreateOptions{}); err != nil {
			t.Fatal("cannot create fixture service account")
		}
	}
	if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		Name:     "bao-fixture-reviewer",
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "system:auth-delegator"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: authNamespace, Name: authRole}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal("cannot authorize fixture TokenReview")
	}
	requestToken := func(namespace string, audiences []string) string {
		t.Helper()
		response, err := clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, authRole,
			&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: audiences}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal("cannot issue fixture service account token")
		}
		return response.Status.Token
	}
	root := baoFixture(t)
	if err := root.Sys().EnableAuthWithOptionsWithContext(ctx, "kubernetes", &bao.EnableAuthOptions{Type: "kubernetes"}); err != nil {
		t.Fatal("cannot enable fixture Kubernetes auth")
	}
	reviewerToken := requestToken(authNamespace, nil)
	reviewURL, reviewCA := tokenReviewEndpoint(t, config)
	if _, err := root.Logical().WriteWithContext(ctx, "auth/kubernetes/config", map[string]any{
		"kubernetes_host":      reviewURL,
		"kubernetes_ca_cert":   reviewCA,
		"token_reviewer_jwt":   reviewerToken,
		"disable_local_ca_jwt": true,
	}); err != nil {
		t.Fatal("cannot configure fixture TokenReview:", strings.NewReplacer(reviewerToken, "[REDACTED]", fixtureToken, "[REDACTED]").Replace(err.Error()))
	}
	if err := root.Sys().PutPolicyWithContext(ctx, authRole, `path "secret/data/applications/*" { capabilities = ["create", "update", "read"] }`); err != nil {
		t.Fatal("cannot configure fixture credential policy")
	}
	if _, err := root.Logical().WriteWithContext(ctx, "auth/kubernetes/role/controller", map[string]any{
		"bound_service_account_names":      []string{authRole},
		"bound_service_account_namespaces": []string{authNamespace},
		"audience":                         authAudience,
		"token_policies":                   []string{authRole},
		"token_ttl":                        "3s", "token_max_ttl": "15s",
	}); err != nil {
		t.Fatal("cannot configure fixture auth role")
	}
	path := filepath.Join(t.TempDir(), "token")
	writeAuthToken(t, path, requestToken(authNamespace, []string{authAudience}))
	client := fixtureClient(t, root.Address())
	session, err := openbao.NewKubernetesSession(client, "kubernetes", authRole, path)
	if err != nil {
		t.Fatal(err)
	}
	cancel := startSession(t, session)
	waitForAuthentication(t, session.Ready)
	store := fixtureStore(t, client)
	if err := store.Create(ctx, credentialPath, fixtureCredential(t)); err != nil {
		t.Fatal("Kubernetes identity cannot create scoped credential", err)
	}
	initialToken := client.Token()
	// 超过初始 TTL 后同一 token 仍可用，证明发生真实 renew-self，而非只登录一次。
	start := time.Now()
	waitForAuthentication(t, func() bool { return time.Since(start) > 4*time.Second })
	if client.Token() != initialToken {
		t.Fatal("token was replaced before renewal could be verified")
	}
	if _, err := client.Auth().Token().LookupSelfWithContext(ctx); err != nil {
		t.Fatal("short-lived token was not renewed")
	}
	for _, invalid := range []struct {
		namespace string
		audience  string
	}{
		{"bao-unrelated", authAudience}, {authNamespace, "wrong-audience"},
	} {
		writeAuthToken(t, path, requestToken(invalid.namespace, []string{invalid.audience}))
		if err := root.Auth().Token().RevokeOrphanWithContext(ctx, client.Token()); err != nil {
			t.Fatal("cannot revoke fixture token")
		}
		waitForAuthentication(t, func() bool { return !session.Ready() && client.Token() == "" })
		// 用相同真实登录入口直接确认 namespace/audience 拒绝，不依赖定时轮询推断。
		if _, err := root.Logical().WriteWithContext(ctx, "auth/kubernetes/login", map[string]any{
			"role": authRole, "jwt": requestToken(invalid.namespace, []string{invalid.audience}),
		}); err == nil {
			t.Fatal("invalid Kubernetes identity was accepted")
		}
		writeAuthToken(t, path, requestToken(authNamespace, []string{authAudience}))
		waitForAuthentication(t, session.Ready)
		if _, err := store.Read(ctx, credentialPath); err != nil {
			t.Fatal("credential access did not recover", err)
		}
	}
	cancel()
	waitForAuthentication(t, func() bool { return !session.Ready() && client.Token() == "" })
}
