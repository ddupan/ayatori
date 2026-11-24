package cli

import (
	"context"
	"fmt"
	"runtime/debug"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// manager 启动时在这里公布自身版本，登录的用户可读（deploy/flux/control-plane/version.yaml）。
const (
	versionNamespace = "kube-public"
	versionConfigMap = "ayatori-version"
)

// ServerVersions 是环境的版本：Ayatori 为 manager 公布的版本，Kubernetes 为 apiserver 的 /version。
type ServerVersions struct {
	Ayatori    string
	Kubernetes string
}

// ServerVersion 读取环境的版本。manager 尚未公布版本（早于公布版本的 release）时 Ayatori 为空。
func ServerVersion(ctx context.Context, config *rest.Config) (ServerVersions, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return ServerVersions{}, err
	}
	info, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return ServerVersions{}, err
	}
	v := ServerVersions{Kubernetes: info.GitVersion}
	cm, err := clientset.CoreV1().ConfigMaps(versionNamespace).Get(ctx, versionConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return v, nil
	}
	if err != nil {
		return ServerVersions{}, err
	}
	v.Ayatori = cm.Data["version"]
	return v, nil
}

// ClientGoVersion 是构建 CLI 所用的 k8s.io/client-go 版本（取自构建信息）。
func ClientGoVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "k8s.io/client-go" {
				return dep.Version
			}
		}
	}
	return "unknown"
}

// SkewWarning 按 ADR-0027 比较 CLI 与环境的 Ayatori 版本：major 相同、minor 相差至多 1（可新可旧）。
// 超出时返回警告，CLI 照常执行。任一版本无法解析（本地构建的 dev、环境未公布版本）时不比较。
func SkewWarning(cli, server string) string {
	c, err := version.ParseSemantic(cli)
	if err != nil {
		return ""
	}
	s, err := version.ParseSemantic(server)
	if err != nil {
		return ""
	}
	if d := int(c.Minor()) - int(s.Minor()); c.Major() == s.Major() && d >= -1 && d <= 1 {
		return ""
	}
	return fmt.Sprintf("warning: ayatori %s is more than one minor version away from the server (%s); "+
		"install the CLI from the server's release", cli, server)
}
