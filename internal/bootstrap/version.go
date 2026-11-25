package bootstrap

import (
	"context"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VersionNamespace 与 VersionConfigMap 是 manager 公布自身版本的位置；登录的用户都可读
// （deploy/flux/control-plane/version.yaml），CLI 据此检查版本偏差（ADR-0027）。
const (
	VersionNamespace = "kube-public"
	VersionConfigMap = "ayatori-version"
)

// versionPublisher 在启动时公布实际运行的 manager 版本，与 kube-apiserver 的 /version 一样报告二进制
// 自身，而不是部署声明的 release。每个副本都写同一个值，不需要选主。
type versionPublisher struct {
	client  client.Client
	version string
}

// Start 重试到写入成功：升级时 Chef 先换镜像，新版本的 RBAC 稍后才由 Flux 应用。写不进去只影响 CLI 的
// 版本提示，不影响控制循环，不让 manager 因此退出。
func (p versionPublisher) Start(ctx context.Context) error {
	backoff := wait.Backoff{Duration: time.Second, Factor: 2, Cap: time.Minute, Steps: math.MaxInt32}
	_ = wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if err := p.publish(ctx); err != nil {
			setupLog.Error(err, "Cannot publish the manager version yet", "namespace", VersionNamespace, "name", VersionConfigMap)
			return false, nil
		}
		return true, nil
	})
	return nil
}

func (p versionPublisher) publish(ctx context.Context) error {
	cm := corev1apply.ConfigMap(VersionConfigMap, VersionNamespace).WithData(map[string]string{"version": p.version})
	return p.client.Apply(ctx, cm, client.FieldOwner("ayatori-manager"), client.ForceOwnership)
}

func (versionPublisher) NeedLeaderElection() bool { return false }
