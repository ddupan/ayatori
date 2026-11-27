package ayatoriclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// DiscoveryPath 是 apiserver 唯一允许匿名读取的路径（homelab_ayatori 写入 authentication.yaml）。
const DiscoveryPath = "/api/v1/namespaces/kube-public/configmaps/ayatori-login"

// CA 是服务端声明的集群 CA，在用户确认指纹之前不可信。
type CA struct {
	PEM         []byte
	Fingerprint string // sha256:<SubjectPublicKeyInfo 的十六进制摘要>，与 kubeadm 的 CA 指纹相同
	Subject     string
}

// Trust 决定是否信任服务端声明的 CA：命令行比对 --ca-fingerprint，或请用户确认。
type Trust func(CA) error

// TrustCA 只信任部署方给出的 CA（PEM）：无人值守的调用方不 TOFU。
func TrustCA(pinned []byte) (Trust, error) {
	want, err := parseCA(pinned)
	if err != nil {
		return nil, err
	}
	return func(ca CA) error {
		if ca.Fingerprint != want.Fingerprint {
			return fmt.Errorf("server CA %s is not the configured CA %s", ca.Fingerprint, want.Fingerprint)
		}
		return nil
	}, nil
}

// Discover 读取发现信息并固定 CA：第一次请求不校验证书（此时没有可信的 CA），只用来取得 CA；
// trust 通过后以该 CA 校验证书再读一次，issuer 与 client 以第二次的结果为准。
func Discover(ctx context.Context, server string, trust Trust) (Profile, error) {
	server = strings.TrimRight(server, "/")
	if !strings.HasPrefix(server, "https://") {
		return Profile{}, fmt.Errorf("server must be an https:// URL, got %q", server)
	}
	claimed, err := fetchLogin(ctx, server, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // 只取 CA，信任由 trust 决定
	if err != nil {
		return Profile{}, err
	}
	ca, err := parseCA([]byte(claimed.Data["ca.crt"]))
	if err != nil {
		return Profile{}, err
	}
	if err := trust(ca); err != nil {
		return Profile{}, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	verified, err := fetchLogin(ctx, server, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		return Profile{}, fmt.Errorf("server certificate is not signed by the trusted CA: %w", err)
	}
	p := Profile{Server: server, CertificateAuthority: string(ca.PEM),
		Issuer: verified.Data["issuer"], ClientID: verified.Data["clientID"]}
	if p.Issuer == "" || p.ClientID == "" {
		return Profile{}, errors.New("server published no issuer or clientID: is OIDC enabled for this environment?")
	}
	return p, nil
}

func fetchLogin(ctx context.Context, server string, tlsConfig *tls.Config) (*corev1.ConfigMap, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+DiscoveryPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read %s: %s (does this environment publish Ayatori login discovery?)", DiscoveryPath, resp.Status)
	}
	var cm corev1.ConfigMap
	return &cm, json.NewDecoder(resp.Body).Decode(&cm)
}

func parseCA(data []byte) (CA, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return CA{}, errors.New("server published no CA certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CA{}, err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return CA{PEM: pem.EncodeToMemory(block), Fingerprint: "sha256:" + hex.EncodeToString(sum[:]),
		Subject: cert.Subject.String()}, nil
}
