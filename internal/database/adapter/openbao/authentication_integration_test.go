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
	"strconv"
	"strings"
	"testing"
	"time"

	bao "github.com/openbao/openbao/api/v2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/openbao"
)

const (
	unrelatedAuthNamespace = "bao-unrelated"
	tokenRequestRole       = "request-openbao-token"
	authNamespace          = "bao-controller"
	authAudience           = "openbao"
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

type kubernetesAuthFixture struct {
	config           *rest.Config
	controllerConfig *rest.Config
	admin            *kubernetes.Clientset
	controller       *kubernetes.Clientset
	grant            func()
	requestToken     func(string, []string) string
}

func newKubernetesAuthFixture(t *testing.T) *kubernetesAuthFixture {
	t.Helper()
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
	for _, namespace := range []string{authNamespace, unrelatedAuthNamespace} {
		if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{Name: namespace}, metav1.CreateOptions{}); err != nil {
			t.Fatal("cannot create fixture namespace")
		}
		if _, err := clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{Name: authRole}, metav1.CreateOptions{}); err != nil {
			t.Fatal("cannot create fixture service account")
		}
	}
	// 集群外身份只有指定 SA 的 TokenRequest 权限，不使用 envtest 管理员凭据运行会话。
	user, err := environment.AddUser(envtest.User{Name: "systemd-controller"}, config)
	if err != nil {
		t.Fatal("cannot create external controller identity")
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		t.Fatal("cannot build external kubeconfig")
	}
	externalConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		t.Fatal("cannot load external kubeconfig")
	}
	if _, err := clientset.RbacV1().Roles(authNamespace).Create(ctx, &rbacv1.Role{
		Name: tokenRequestRole,
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"},
			ResourceNames: []string{authRole}, Verbs: []string{"create"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal("cannot create TokenRequest Role")
	}
	permission := &rbacv1.RoleBinding{
		Name:     tokenRequestRole,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: tokenRequestRole},
		Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "systemd-controller"}},
	}
	grant := func() {
		t.Helper()
		if _, err := clientset.RbacV1().RoleBindings(authNamespace).Create(ctx, permission.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatal("cannot grant TokenRequest permission")
		}
	}
	grant()
	externalClient, err := kubernetes.NewForConfig(externalConfig)
	if err != nil {
		t.Fatal("cannot construct restricted controller client")
	}
	for _, target := range []struct{ namespace, name string }{{unrelatedAuthNamespace, authRole}, {authNamespace, "another-account"}} {
		_, err := externalClient.CoreV1().ServiceAccounts(target.namespace).CreateToken(ctx, target.name,
			&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{authAudience}}}, metav1.CreateOptions{})
		if !apierrors.IsForbidden(err) {
			t.Fatal("TokenRequest escaped Role namespace/resourceNames restriction")
		}
	}
	if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		Name:     "bao-fixture-reviewer",
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:auth-delegator"},
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

	return &kubernetesAuthFixture{
		config: config, controllerConfig: externalConfig, admin: clientset, controller: externalClient,
		grant: grant, requestToken: requestToken,
	}
}

const authRole = "controller"

var testIdentity = openbao.KubernetesIdentity{Namespace: authNamespace, ServiceAccount: authRole, Audience: authAudience}

func waitForAuthentication(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatal("authentication condition timed out")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func startSession(t *testing.T, session interface{ Start(context.Context) error }) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- session.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("authentication did not stop")
		}
	})
	return cancel
}

// 此处验证公共认证会话与 Database 凭据适配器的跨层集成。
func TestKubernetesSessionWithRealTokenReview(t *testing.T) {
	api := newKubernetesAuthFixture(t)
	ctx := t.Context()
	root := baoFixture(t)
	if err := root.Sys().EnableAuthWithOptionsWithContext(ctx, "kubernetes", &bao.EnableAuthOptions{Type: "kubernetes"}); err != nil {
		t.Fatal("cannot enable fixture Kubernetes auth")
	}
	reviewerToken := api.requestToken(authNamespace, nil)
	reviewURL, reviewCA := tokenReviewEndpoint(t, api.config)
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
		"token_ttl":                        "3s", "token_max_ttl": "60s",
	}); err != nil {
		t.Fatal("cannot configure fixture auth role")
	}
	client := fixtureClient(t, root.Address())
	manager, err := ctrl.NewManager(api.controllerConfig, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal("cannot construct external controller manager")
	}
	session, err := openbao.NewKubernetesSession(client, manager.GetClient(), "kubernetes", authRole, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(session); err != nil {
		t.Fatal("cannot register authentication lifecycle")
	}
	cancel := startSession(t, manager)
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
		{unrelatedAuthNamespace, authAudience}, {authNamespace, "wrong-audience"},
	} {
		// 用相同真实登录入口直接确认 namespace/audience 拒绝，不依赖定时轮询推断。
		if _, err := root.Logical().WriteWithContext(ctx, "auth/kubernetes/login", map[string]any{
			"role": authRole, "jwt": api.requestToken(invalid.namespace, []string{invalid.audience}),
		}); err == nil {
			t.Fatal("invalid Kubernetes identity was accepted")
		}
	}
	if err := api.admin.RbacV1().RoleBindings(authNamespace).Delete(ctx, tokenRequestRole, metav1.DeleteOptions{}); err != nil {
		t.Fatal("cannot revoke TokenRequest permission")
	}
	if err := root.Auth().Token().RevokeOrphanWithContext(ctx, client.Token()); err != nil {
		t.Fatal("cannot revoke fixture OpenBao token")
	}
	waitForAuthentication(t, func() bool { return !session.Ready() && client.Token() == "" })
	// RBAC 授权器读 informer 缓存，删除 RoleBinding 后要等它观察到才会拒绝；负载高时立即断言会误报。
	waitForAuthentication(t, func() bool {
		denied, err := api.controller.CoreV1().ServiceAccounts(authNamespace).CreateToken(ctx, authRole,
			&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{authAudience}}}, metav1.CreateOptions{})
		return apierrors.IsForbidden(err) && (denied == nil || denied.Status.Token == "")
	})
	api.grant()
	waitForAuthentication(t, session.Ready)
	if client.Token() == initialToken {
		t.Fatal("reauthentication reused revoked OpenBao token")
	}
	if _, err := store.Read(ctx, credentialPath); err != nil {
		t.Fatal("credential access did not recover", err)
	}
	cancel()
	waitForAuthentication(t, func() bool { return !session.Ready() && client.Token() == "" })
}
