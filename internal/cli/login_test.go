package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.ddupan.top/ayatori/internal/ayatoriclient"
)

const (
	clientID      = "ayatori-test"
	discoveryName = "ayatori-login"
)

// 对真实 apiserver（envtest，以结构化认证配置接受测试 issuer 的 token）走一遍：config add 的
// 发现与 CA 固定、device code 与浏览器登录、whoami、refresh token 轮换、logout。
func TestLoginAgainstAPIServer(t *testing.T) {
	issuer, spire := newIssuer(t), newIssuer(t)
	server, admin := startAPIServer(t, issuer, spire)
	publishDiscovery(t, admin, issuer.URL, server.caPEM)
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, issuer.Client())

	t.Run("a CA the user does not trust stops discovery", func(t *testing.T) {
		_, err := ayatoriclient.Discover(ctx, server.url, func(ayatoriclient.CA) error { return errors.New("no") })
		if err == nil || err.Error() != "no" {
			t.Fatalf("Discover = %v, want the trust error", err)
		}
	})

	t.Run("unattended callers trust only the configured CA", func(t *testing.T) {
		trust, err := ayatoriclient.TrustCA(server.caPEM)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ayatoriclient.Discover(ctx, server.url, trust); err != nil {
			t.Errorf("Discover with the server's CA = %v", err)
		}
		other, err := ayatoriclient.TrustCA(issuer.caPEM)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ayatoriclient.Discover(ctx, server.url, other); err == nil {
			t.Error("Discover trusted a CA other than the configured one")
		}
	})

	var p Profile
	t.Run("discovery pins the published CA after its fingerprint is confirmed", func(t *testing.T) {
		var seen ayatoriclient.CA
		var err error
		p.Profile, err = ayatoriclient.Discover(ctx, server.url+"/", func(ca ayatoriclient.CA) error { seen = ca; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if want := spkiFingerprint(t, server.caPEM); seen.Fingerprint != want {
			t.Errorf("fingerprint %s, want %s", seen.Fingerprint, want)
		}
		if p.Server != server.url || p.Issuer != issuer.URL || p.ClientID != clientID || p.CertificateAuthority != string(server.caPEM) {
			t.Errorf("profile %+v", p)
		}
	})

	store := Store{Dir: t.TempDir()}
	t.Run("device code login, then whoami as the OIDC user", func(t *testing.T) {
		var out bytes.Buffer
		if err := store.Login(ctx, "test", p, LoginOptions{Device: true, Out: &out}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), issuer.URL+"/verify?user_code=") {
			t.Errorf("no verification URL in %q", out.String())
		}
		assertUser(t, ctx, store, p)
		info, err := os.Stat(filepath.Join(store.Dir, "tokens", "test.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("token file %v %v, want mode 0600", info, err)
		}
	})

	t.Run("an expiring ID token is refreshed and the rotated refresh token kept", func(t *testing.T) {
		// 测试 issuer 的 ID token 只有 30 秒，每次取用都要刷新；旧 refresh token 用过即失效，
		// 第二次刷新成功说明轮换后的 refresh token 已保存。
		before := issuer.refreshes()
		assertUser(t, ctx, store, p)
		assertUser(t, ctx, store, p)
		if got := issuer.refreshes() - before; got != 2 {
			t.Errorf("%d refreshes, want 2", got)
		}
	})

	t.Run("browser login with PKCE on the loopback callback", func(t *testing.T) {
		browser := Store{Dir: t.TempDir()}
		opened := make(chan error, 1)
		open := func(url string) error {
			go func() {
				// 浏览器：issuer 立即重定向回本机回环地址，客户端跟随重定向完成回调。
				resp, err := issuer.Client().Get(url)
				if err == nil {
					_ = resp.Body.Close()
				}
				opened <- err
			}()
			return nil
		}
		if err := browser.Login(ctx, "test", p, LoginOptions{Out: &bytes.Buffer{}, Open: open}); err != nil {
			t.Fatal(err)
		}
		if err := <-opened; err != nil {
			t.Fatal(err)
		}
		assertUser(t, ctx, browser, p)
	})

	t.Run("the kubeconfig's exec credential authenticates as the logged-in user", func(t *testing.T) {
		assertKubeconfig(t, ctx, store, p, server)
	})

	t.Run("SPIFFE identity without a saved login; a human login comes first", func(t *testing.T) {
		assertWorkloadIdentity(t, ctx, store, p, spire)
	})

	t.Run("logout forgets the login", func(t *testing.T) {
		if err := store.Logout("test"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.IDToken(ctx, "test", p); !errors.Is(err, ErrNotLoggedIn) {
			t.Errorf("IDToken after logout = %v, want ErrNotLoggedIn", err)
		}
	})
}

// assertKubeconfig：kubeconfig 指向固定的 CA 并以 exec 调 ayatori；exec 的输出能以登录的用户连接。
func assertKubeconfig(t *testing.T, ctx context.Context, store Store, p Profile, server apiServer) {
	t.Helper()
	data, err := Kubeconfig("test", p, "/usr/local/bin/ayatori")
	if err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != server.url || string(config.CAData) != string(server.caPEM) || config.BearerToken != "" ||
		config.ExecProvider == nil || config.ExecProvider.Command != "/usr/local/bin/ayatori" ||
		!slices.Equal(config.ExecProvider.Args, []string{"config", "credential", "--profile", "test"}) {
		t.Fatalf("kubeconfig:\n%s", data)
	}
	// client-go 执行 exec 插件后用的就是这份输出：按 v1 ExecCredential 解析，拿 token 连接。
	out, err := store.ExecCredential(ctx, "test", p)
	if err != nil {
		t.Fatal(err)
	}
	var cred clientauthv1.ExecCredential
	if err := json.Unmarshal(out, &cred); err != nil {
		t.Fatal(err)
	}
	if cred.Kind != "ExecCredential" || cred.APIVersion != clientauthv1.SchemeGroupVersion.String() ||
		cred.Status.ExpirationTimestamp == nil || !cred.Status.ExpirationTimestamp.After(time.Now()) {
		t.Fatalf("credential kind %q, apiVersion %q, expiry %v", cred.Kind, cred.APIVersion, cred.Status.ExpirationTimestamp)
	}
	config.ExecProvider, config.BearerToken = nil, cred.Status.Token
	user, err := WhoAmI(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != humanUser {
		t.Errorf("user %+v", user)
	}
}

func assertUser(t *testing.T, ctx context.Context, s Store, p Profile) {
	t.Helper()
	config, err := s.RESTConfig(ctx, "test", p)
	if err != nil {
		t.Fatal(err)
	}
	user, err := WhoAmI(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != humanUser || !slices.Contains(user.Groups, "oidc:platform-admins") {
		t.Errorf("user %+v", user)
	}
}

type apiServer struct {
	url   string
	caPEM []byte
}

func startAPIServer(t *testing.T, issuer, spire *testIssuer) (apiServer, *kubernetes.Clientset) {
	t.Helper()
	// 与 homelab_ayatori 写入的 authentication.yaml 相同的形状，issuer 换成测试 issuer，spire 代替 SPIRE
	// 的 OIDC discovery（用户名即 SPIFFE ID）；/healthz 是 envtest 不带凭据的就绪检查。
	config := fmt.Sprintf(`apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
jwt:
- issuer:
    url: %q
    audiences: [%s]
    certificateAuthority: %q
  claimMappings:
    username: {claim: preferred_username, prefix: "oidc:"}
    groups: {claim: groups, prefix: "oidc:"}
- issuer:
    url: %q
    audiences: [%s]
    certificateAuthority: %q
  claimMappings:
    username: {claim: sub, prefix: ""}
anonymous:
  enabled: true
  conditions:
  - path: %s
  - path: /healthz
`, issuer.URL, clientID, issuer.caPEM, spire.URL, clientID, spire.caPEM, ayatoriclient.DiscoveryPath)
	path := filepath.Join(t.TempDir(), "authentication.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().Configure().Set("authentication-config", path)
	rest, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	clientset, err := kubernetes.NewForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	return apiServer{url: strings.TrimRight(rest.Host, "/"), caPEM: rest.CAData}, clientset
}

// publishDiscovery 写入 homelab_ayatori bootstrap.yaml 中的同样对象。
func publishDiscovery(t *testing.T, admin *kubernetes.Clientset, issuer string, ca []byte) {
	t.Helper()
	ctx := t.Context()
	meta := metav1.ObjectMeta{Name: discoveryName, Namespace: "kube-public"}
	// kube-public 由 apiserver 启动后创建，可能稍晚出现。
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := admin.CoreV1().ConfigMaps("kube-public").Create(ctx, &corev1.ConfigMap{ObjectMeta: meta,
			Data: map[string]string{"issuer": issuer, "clientID": clientID, "ca.crt": string(ca)}}, metav1.CreateOptions{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := admin.RbacV1().Roles("kube-public").Create(ctx, &rbacv1.Role{ObjectMeta: meta, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{discoveryName}, Verbs: []string{"get"},
	}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RbacV1().RoleBindings("kube-public").Create(ctx, &rbacv1.RoleBinding{ObjectMeta: meta,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: discoveryName},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "system:unauthenticated"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// 用户的 OIDC 组在 apiserver 里能 create SelfSubjectReview（system:basic-user 已允许），无需另外授权。
}

func spkiFingerprint(t *testing.T, caPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(caPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// testIssuer 是最小的 OIDC 提供方：discovery、JWKS、device code（第一次轮询 pending）、授权码 +
// PKCE（立即重定向，相当于用户已登录并同意）、轮换的 refresh token。
type testIssuer struct {
	*httptest.Server
	caPEM  []byte
	signer jose.Signer
	jwks   jose.JSONWebKeySet

	mu        sync.Mutex
	polls     map[string]int    // device code → 轮询次数
	codes     map[string]string // 授权码 → code_challenge
	refresh   map[string]bool   // 仍有效的 refresh token
	refreshed int
}

func newIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	i := &testIssuer{signer: signer, polls: map[string]int{}, codes: map[string]string{}, refresh: map[string]bool{},
		jwks: jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"issuer": i.URL, "jwks_uri": i.URL + "/jwks",
			"authorization_endpoint": i.URL + "/authorize", "token_endpoint": i.URL + "/token",
			"device_authorization_endpoint": i.URL + "/device", "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, i.jwks) })
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != clientID {
			http.Error(w, "unknown client", http.StatusBadRequest)
			return
		}
		code := rand.Text()
		i.mu.Lock()
		i.polls[code] = 0
		i.mu.Unlock()
		writeJSON(w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": i.URL + "/verify",
			"verification_uri_complete": i.URL + "/verify?user_code=ABCD-EFGH", "expires_in": 60, "interval": 1})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != clientID || q.Get("code_challenge_method") != "S256" ||
			!strings.HasPrefix(q.Get("redirect_uri"), "http://localhost:") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		code := rand.Text()
		i.mu.Lock()
		i.codes[code] = q.Get("code_challenge")
		i.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("POST /token", i.token)
	i.Server = httptest.NewTLSServer(mux)
	t.Cleanup(i.Close)
	i.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.Certificate().Raw})
	return i
}

func (i *testIssuer) token(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("client_id") != clientID {
		oauthError(w, "invalid_client")
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	switch r.FormValue("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		n, ok := i.polls[r.FormValue("device_code")]
		if !ok {
			oauthError(w, "invalid_grant")
			return
		}
		i.polls[r.FormValue("device_code")] = n + 1
		if n == 0 {
			oauthError(w, "authorization_pending")
			return
		}
		delete(i.polls, r.FormValue("device_code"))
	case "authorization_code":
		challenge, ok := i.codes[r.FormValue("code")]
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			oauthError(w, "invalid_grant")
			return
		}
		delete(i.codes, r.FormValue("code"))
	case "refresh_token":
		if !i.refresh[r.FormValue("refresh_token")] {
			oauthError(w, "invalid_grant")
			return
		}
		delete(i.refresh, r.FormValue("refresh_token")) // 轮换：旧的立即失效
		i.refreshed++
	default:
		oauthError(w, "unsupported_grant_type")
		return
	}
	now := time.Now()
	id, err := jwt.Signed(i.signer).Claims(jwt.Claims{Issuer: i.URL, Subject: "u-1", Audience: jwt.Audience{clientID},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(30 * time.Second))}).
		Claims(map[string]any{"preferred_username": "alice", "groups": []string{"platform-admins"}}).Serialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	refresh := rand.Text()
	i.refresh[refresh] = true
	writeJSON(w, map[string]any{"access_token": rand.Text(), "token_type": "Bearer", "expires_in": 30,
		"id_token": id, "refresh_token": refresh})
}

func (i *testIssuer) refreshes() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.refreshed
}

func oauthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
