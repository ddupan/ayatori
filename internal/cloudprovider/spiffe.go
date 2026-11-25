package cloudprovider

import (
	"fmt"
	"net/http"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"k8s.io/client-go/rest"
)

// WithJWTSVID 让 config 的每个请求带上 audience 为环境 audience 的 JWT-SVID（ADR-0027）。
// Ayatori 的 apiserver 已信任 SPIRE 的 OIDC discovery，用户名就是 SPIFFE ID，授权由 Ayatori 中的
// RBAC 决定。
//
// ponytail: 每个请求向本机 spire-agent 取一次（agent 自己缓存 JWT-SVID）；请求量大到 socket 往返
// 可见时再在进程内按过期时间缓存。
func WithJWTSVID(config *rest.Config, source *workloadapi.JWTSource, audience string) {
	config.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripper(func(request *http.Request) (*http.Response, error) {
			svid, err := source.FetchJWTSVID(request.Context(), jwtsvid.Params{Audience: audience})
			if err != nil {
				return nil, fmt.Errorf("fetch JWT-SVID: %w", err)
			}
			request = request.Clone(request.Context())
			request.Header.Set("Authorization", "Bearer "+svid.Marshal())
			return next.RoundTrip(request)
		})
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
