package bootstrap

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// clientCertificateRefresh 远短于 ESO 的每日签发与 3 天证书寿命，新证书最迟一分钟内生效。
const clientCertificateRefresh = time.Minute

// clientCertificate 持有 LB 管理 API 的最新客户端证书。ESO 每天从 Bao 签发新证书写入
// Secret；每次 TLS 握手经 GetClientCertificate 取当前值，因此轮换无需重启 manager。
// 已建立的连接继续使用握手时的证书，直到重连。
type clientCertificate struct {
	reader  client.Reader
	secret  types.NamespacedName
	current atomic.Pointer[tls.Certificate]
}

func newClientCertificate(secret *corev1.Secret, reader client.Reader) (*clientCertificate, error) {
	c := &clientCertificate{reader: reader, secret: client.ObjectKeyFromObject(secret)}
	return c, c.load(secret)
}

func (c *clientCertificate) load(secret *corev1.Secret) error {
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil {
		return fmt.Errorf("LoadBalancer client certificate: %w", err)
	}
	c.current.Store(&certificate)
	return nil
}

func (c *clientCertificate) refresh(ctx context.Context) error {
	secret := &corev1.Secret{}
	// 直读 API server：Secret 不进入共享 cache。
	if err := c.reader.Get(ctx, c.secret, secret); err != nil {
		return fmt.Errorf("read LoadBalancer credentials: %w", err)
	}
	return c.load(secret)
}

func (c *clientCertificate) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return c.current.Load(), nil
}

// Start 周期刷新；失败时保留上一张证书并记录，Secret 恢复后自动换上。
func (c *clientCertificate) Start(ctx context.Context) error {
	ticker := time.NewTicker(clientCertificateRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.refresh(ctx); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "无法刷新 LoadBalancer 客户端证书，继续使用当前证书")
			}
		}
	}
}

// NeedLeaderElection 为 false：备用副本接任 leader 时证书已是最新。
func (c *clientCertificate) NeedLeaderElection() bool { return false }
