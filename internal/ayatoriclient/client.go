// Package ayatoriclient 是连接 Ayatori API 的共享部分：发现环境、取得凭据、生成 rest.Config（ADR-0027）。
// CLI 在它之上加配置文件与人的登录；cloud-provider 等无人值守的调用方直接使用它。
package ayatoriclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

// Profile 是一个环境：连哪个 apiserver、信任哪个 CA、它的 issuer 与 audience（clientID）。
type Profile struct {
	Server string `json:"server"`
	// CertificateAuthority 是已确认的集群 CA（PEM）。
	CertificateAuthority string `json:"certificateAuthority"`
	Issuer               string `json:"issuer"`
	ClientID             string `json:"clientID"`
}

// TokenFunc 取得一个 bearer token（ID token 或 JWT-SVID）。
type TokenFunc func(context.Context) (string, error)

// RESTConfig 以 token 连接 profile 的 apiserver。先取一次 token，拿不到就报错；之后按 JWT 的 exp 缓存，
// 过期前 refreshEarly 换新，长期运行的 controller 也能用。ctx 是换新 token 时使用的 context。
func RESTConfig(ctx context.Context, p Profile, token TokenFunc) (*rest.Config, error) {
	source := tokenSource{ctx: ctx, token: token}
	first, err := source.Token()
	if err != nil {
		return nil, err
	}
	config := &rest.Config{Host: p.Server, CAData: []byte(p.CertificateAuthority), UserAgent: "ayatori"}
	config.Wrap(transport.TokenSourceWrapTransport(oauth2.ReuseTokenSourceWithExpiry(first, source, refreshEarly)))
	return config, nil
}

// refreshEarly 要短于最短的 token 有效期（测试 issuer 30 秒），否则每个请求都会换新。
const refreshEarly = 10 * time.Second

type tokenSource struct {
	ctx   context.Context //nolint:containedctx // oauth2.TokenSource 不带 context
	token TokenFunc
}

func (s tokenSource) Token() (*oauth2.Token, error) {
	token, err := s.token(s.ctx)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{AccessToken: token, TokenType: "Bearer", Expiry: Expiry(token)}, nil
}

// Expiry 读 JWT 的 exp，不验证签名：真正的验证在 apiserver。读不出来就当作已过期。
func Expiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
