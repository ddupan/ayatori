package subresource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FrontProxy 认证 apiserver 经聚合层转来的请求（requestheader）：只接受 apiserver 在
// kube-system/extension-apiserver-authentication 公布的 CA 签发、名字在 allowed names 中的客户端证书，
// 用户取自它设置的请求头。没有这张证书的连接在 TLS 握手时即被拒绝。
type FrontProxy struct {
	CAs             *x509.CertPool
	AllowedNames    []string
	UsernameHeaders []string
	GroupHeaders    []string
}

// LoadFrontProxy 读 apiserver 公布的 requestheader 配置。
func LoadFrontProxy(ctx context.Context, c client.Reader) (FrontProxy, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "extension-apiserver-authentication"}, &cm); err != nil {
		return FrontProxy{}, err
	}
	f := FrontProxy{CAs: x509.NewCertPool()}
	if !f.CAs.AppendCertsFromPEM([]byte(cm.Data["requestheader-client-ca-file"])) {
		return FrontProxy{}, errors.New("apiserver publishes no requestheader client CA: is the front proxy configured?")
	}
	for key, into := range map[string]*[]string{
		"requestheader-allowed-names":    &f.AllowedNames,
		"requestheader-username-headers": &f.UsernameHeaders,
		"requestheader-group-headers":    &f.GroupHeaders,
	} {
		if v := cm.Data[key]; v != "" {
			if err := json.Unmarshal([]byte(v), into); err != nil {
				return FrontProxy{}, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	if len(f.UsernameHeaders) == 0 {
		return FrontProxy{}, errors.New("apiserver publishes no requestheader username headers")
	}
	return f, nil
}

// TLSConfig 要求并校验 front-proxy 客户端证书。服务证书每次握手从 certificate 取，轮换不需要重启。
func (f FrontProxy) TLSConfig(certificate func() (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      f.CAs,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certificate() },
	}
}

// User 返回 apiserver 认定的用户与组。证书已在握手时校验；这里再检查它的名字。
func (f FrontProxy) User(r *http.Request) (string, []string, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return "", nil, errors.New("no verified front-proxy client certificate")
	}
	if cn := r.TLS.VerifiedChains[0][0].Subject.CommonName; len(f.AllowedNames) > 0 && !slices.Contains(f.AllowedNames, cn) {
		return "", nil, fmt.Errorf("client certificate %q is not an allowed front proxy", cn)
	}
	var user string
	for _, h := range f.UsernameHeaders {
		if user = r.Header.Get(h); user != "" {
			break
		}
	}
	if user == "" {
		return "", nil, errors.New("no user in the front-proxy headers")
	}
	var groups []string
	for _, h := range f.GroupHeaders {
		groups = append(groups, r.Header.Values(h)...)
	}
	return user, groups, nil
}
