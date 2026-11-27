package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"k8s.io/client-go/rest"

	"go.ddupan.top/ayatori/internal/ayatoriclient"
)

// 认证方法。它们是平级的：profile 不指定时按 Token 的顺序选用（ADR-0027）。
const (
	AuthOIDC   = "oidc"
	AuthSPIFFE = "spiffe"
)

// AuthMethods 是 `config add --auth` 可以指定的方法。
var AuthMethods = []string{AuthOIDC, AuthSPIFFE}

// Token 返回连接 apiserver 用的 bearer token。profile 指定了方法就只用它；否则依次：
//  1. 已保存的登录（人显式 `ayatori login`，优先于机器的环境身份；过期时报错而不换身份）；
//  2. 机器身份（ayatoriclient.MachineToken）。
//
// audience 都是环境的 audience（发现信息里的 clientID），apiserver 对每个 issuer 只认它。
func (s Store) Token(ctx context.Context, name string, p Profile) (string, error) {
	switch p.Auth {
	case AuthOIDC:
		return s.IDToken(ctx, name, p)
	case AuthSPIFFE:
		return ayatoriclient.SPIFFEToken(ctx, p.Profile)
	case "":
	default:
		return "", fmt.Errorf("environment %s: unknown auth method %q", name, p.Auth)
	}
	if _, err := os.Stat(s.tokenPath(name)); err == nil {
		return s.IDToken(ctx, name, p)
	}
	token, err := ayatoriclient.MachineToken(ctx, p.Profile)
	if errors.Is(err, ayatoriclient.ErrNoMachineIdentity) {
		return "", fmt.Errorf("%w to %s: no saved login and no machine identity; run `ayatori login`", ErrNotLoggedIn, name)
	}
	return token, err
}

// RESTConfig 以 Token 选出的凭据连接 profile 的 apiserver。
func (s Store) RESTConfig(ctx context.Context, name string, p Profile) (*rest.Config, error) {
	return ayatoriclient.RESTConfig(ctx, p.Profile, func(ctx context.Context) (string, error) {
		return s.Token(ctx, name, p)
	})
}
