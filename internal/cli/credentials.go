package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// 认证方法。它们是平级的：profile 不指定时按 Token 的顺序选用（ADR-0027）。
const (
	AuthOIDC   = "oidc"
	AuthSPIFFE = "spiffe"
)

// AuthMethods 是 `config add --auth` 可以指定的方法。
var AuthMethods = []string{AuthOIDC, AuthSPIFFE}

// defaultSPIFFESocket 是 homelab 节点上 spire-agent 的 Workload API；SPIFFE_ENDPOINT_SOCKET 优先。
const defaultSPIFFESocket = "/run/spire/agent-sockets/spire-agent.sock"

// Token 返回连接 apiserver 用的 bearer token。profile 指定了方法就只用它；否则依次：
//  1. 已保存的登录（人显式 `ayatori login`，优先于机器的环境身份；过期时报错而不换身份）；
//  2. SPIFFE Workload API 的 JWT-SVID（节点、自动化）。
//
// audience 都是环境的 audience（发现信息里的 clientID），apiserver 对每个 issuer 只认它。
func (s Store) Token(ctx context.Context, name string, p Profile) (string, error) {
	switch p.Auth {
	case AuthOIDC:
		return s.IDToken(ctx, name, p)
	case AuthSPIFFE:
		return spiffeToken(ctx, spiffeSocket(), p.ClientID)
	case "":
	default:
		return "", fmt.Errorf("environment %s: unknown auth method %q", name, p.Auth)
	}
	if _, err := os.Stat(s.tokenPath(name)); err == nil {
		return s.IDToken(ctx, name, p)
	}
	if addr := spiffeSocket(); addr != "" {
		return spiffeToken(ctx, addr, p.ClientID)
	}
	return "", fmt.Errorf("%w to %s: no saved login and no SPIFFE Workload API; run `ayatori login`", ErrNotLoggedIn, name)
}

// spiffeSocket 返回 Workload API 地址：SPIFFE_ENDPOINT_SOCKET，或存在的默认 socket；都没有时为空。
func spiffeSocket() string {
	if addr := os.Getenv("SPIFFE_ENDPOINT_SOCKET"); addr != "" {
		return addr
	}
	if _, err := os.Stat(defaultSPIFFESocket); err == nil {
		return "unix://" + defaultSPIFFESocket
	}
	return ""
}

func spiffeToken(ctx context.Context, addr, audience string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("no SPIFFE Workload API: set SPIFFE_ENDPOINT_SOCKET or run spire-agent at %s", defaultSPIFFESocket)
	}
	svid, err := workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience}, workloadapi.WithAddr(addr))
	if err != nil {
		return "", fmt.Errorf("fetch JWT-SVID from %s: %w", addr, err)
	}
	return svid.Marshal(), nil
}
