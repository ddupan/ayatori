package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/int128/oauth2cli"
	"golang.org/x/oauth2"

	"go.ddupan.top/ayatori/internal/ayatoriclient"
)

// ErrNotLoggedIn：没有 token，或已过期且无法刷新。
var ErrNotLoggedIn = errors.New("not logged in")

// scopes：groups 给 RBAC 用，offline_access 换来 refresh token。
var scopes = []string{"openid", "profile", "groups", "offline_access"}

// callbackAddrs 对应 iam-login 为 ayatori-* 客户端登记的 redirect URI http://localhost:8000 与
// :18000（homelab-infra apps/iam-login/terraform/clients.tf），按顺序尝试。
var callbackAddrs = []string{"127.0.0.1:8000", "127.0.0.1:18000"}

type tokens struct {
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

// LoginOptions：Device 为 RFC 8628 device code，否则为浏览器 + 本机回环（授权码 + PKCE）。
type LoginOptions struct {
	Device bool
	Out    io.Writer
	// Open 打开浏览器；失败时用户照着 Out 里的地址手动打开。
	Open func(url string) error
}

// Login 向 profile 的 issuer 登录，并保存 ID token 与 refresh token。
func (s Store) Login(ctx context.Context, name string, p Profile, opts LoginOptions) error {
	config, err := oauthConfig(ctx, p)
	if err != nil {
		return err
	}
	var token *oauth2.Token
	if opts.Device {
		token, err = deviceLogin(ctx, config, opts)
	} else {
		token, err = browserLogin(ctx, config, opts)
	}
	if err != nil {
		return err
	}
	unlock, err := s.lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	return s.saveTokens(name, token)
}

// Logout 删除本机保存的 token。iam-login 的会话与已签发的 token 不受影响，到期失效。
func (s Store) Logout(name string) error {
	err := os.Remove(s.tokenPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// IDToken 返回仍然有效的 ID token，快过期时用 refresh token 换新的。iam-login 对公共客户端轮换
// refresh token，旧的立即失效，因此刷新在文件锁内完成：同时运行的多个 ayatori 只有一个去换。
func (s Store) IDToken(ctx context.Context, name string, p Profile) (string, error) {
	unlock, err := s.lock(name)
	if err != nil {
		return "", err
	}
	defer unlock()
	data, err := os.ReadFile(s.tokenPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w to %s: run `ayatori login`", ErrNotLoggedIn, name)
	}
	if err != nil {
		return "", err
	}
	var t tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return "", err
	}
	if ayatoriclient.Expiry(t.IDToken).After(time.Now().Add(time.Minute)) {
		return t.IDToken, nil
	}
	if t.RefreshToken == "" {
		return "", fmt.Errorf("%w to %s: session expired, run `ayatori login`", ErrNotLoggedIn, name)
	}
	config, err := oauthConfig(ctx, p)
	if err != nil {
		return "", err
	}
	// 没有 access token 的 Token 视为过期，TokenSource 直接用 refresh token 去换。
	token, err := config.TokenSource(ctx, &oauth2.Token{RefreshToken: t.RefreshToken}).Token()
	if err != nil {
		return "", fmt.Errorf("%w to %s: refresh failed, run `ayatori login`: %w", ErrNotLoggedIn, name, err)
	}
	if err := s.saveTokens(name, token); err != nil {
		return "", err
	}
	return token.Extra("id_token").(string), nil
}

func (s Store) saveTokens(name string, token *oauth2.Token) error {
	id, _ := token.Extra("id_token").(string)
	if id == "" {
		return errors.New("issuer returned no ID token")
	}
	data, err := json.Marshal(tokens{IDToken: id, RefreshToken: token.RefreshToken}) //nolint:gosec // 正是要保存它，文件只有本人可读
	if err != nil {
		return err
	}
	return writeFile(s.tokenPath(name), data)
}

func (s Store) tokenPath(name string) string { return filepath.Join(s.Dir, "tokens", name+".json") }

func (s Store) lock(name string) (func(), error) {
	path := filepath.Join(s.Dir, "tokens", name+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // 配置目录下的锁文件
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// oauthConfig 从 issuer 的 OIDC discovery 取端点。HTTP client 取自 ctx（oauth2.HTTPClient），
// 测试以此信任测试 issuer 的证书。
func oauthConfig(ctx context.Context, p Profile) (*oauth2.Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(p.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC discovery for %s: %s", p.Issuer, resp.Status)
	}
	var meta struct {
		Issuer                      string `json:"issuer"`
		AuthorizationEndpoint       string `json:"authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, err
	}
	if meta.Issuer != p.Issuer {
		return nil, fmt.Errorf("OIDC discovery for %s names issuer %s", p.Issuer, meta.Issuer)
	}
	return &oauth2.Config{ClientID: p.ClientID, Scopes: scopes, Endpoint: oauth2.Endpoint{
		AuthURL: meta.AuthorizationEndpoint, TokenURL: meta.TokenEndpoint,
		DeviceAuthURL: meta.DeviceAuthorizationEndpoint, AuthStyle: oauth2.AuthStyleInParams,
	}}, nil
}

func httpClient(ctx context.Context) *http.Client {
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		return c
	}
	return http.DefaultClient
}

func deviceLogin(ctx context.Context, config *oauth2.Config, opts LoginOptions) (*oauth2.Token, error) {
	if config.Endpoint.DeviceAuthURL == "" {
		return nil, errors.New("issuer does not support device code login")
	}
	auth, err := config.DeviceAuth(ctx)
	if err != nil {
		return nil, err
	}
	if auth.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(opts.Out, "Open %s\n", auth.VerificationURIComplete)
	} else {
		_, _ = fmt.Fprintf(opts.Out, "Open %s and enter %s\n", auth.VerificationURI, auth.UserCode)
	}
	return config.DeviceAccessToken(ctx, auth)
}

// browserLogin 由 oauth2cli（kubelogin 所用）在本机回环端口完成授权码 + PKCE。redirect URI 为
// http://localhost:<端口>，与 iam-login 登记的一致。
func browserLogin(ctx context.Context, config *oauth2.Config, opts LoginOptions) (*oauth2.Token, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan string, 1)
	go func() {
		select {
		case url := <-ready:
			_, _ = fmt.Fprintf(opts.Out, "Open %s\n", url)
			if opts.Open != nil {
				_ = opts.Open(url) // 打不开浏览器时，用户照上面的地址手动打开
			}
		case <-ctx.Done():
		}
	}()
	verifier := oauth2.GenerateVerifier()
	return oauth2cli.GetToken(ctx, oauth2cli.Config{
		OAuth2Config:           *config,
		AuthCodeOptions:        []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)},
		TokenRequestOptions:    []oauth2.AuthCodeOption{oauth2.VerifierOption(verifier)},
		LocalServerBindAddress: callbackAddrs,
		LocalServerReadyChan:   ready,
	})
}
