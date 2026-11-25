package cli

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Kubeconfig 生成 break-glass 用的 kubeconfig（ADR-0027）：集群与 CA 取自 profile，凭据由 command
// （ayatori 自己）作为 exec credential plugin 按需取得，kubeconfig 里不含任何 token。profile 名写进
// 参数，`ayatori config use` 切换环境不影响已导出的 kubeconfig。
func Kubeconfig(name string, p Profile, command string) ([]byte, error) {
	c := clientcmdapi.NewConfig()
	c.Clusters[name] = &clientcmdapi.Cluster{Server: p.Server, CertificateAuthorityData: []byte(p.CertificateAuthority)}
	c.AuthInfos[name] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		APIVersion:      clientauthv1.SchemeGroupVersion.String(),
		Command:         command,
		Args:            []string{"config", "credential", "--profile", name},
		InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
	}}
	c.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	c.CurrentContext = name
	return clientcmd.Write(*c)
}

// ExecCredential 是 exec credential plugin 的输出：当前环境仍然有效的 ID token（需要时先刷新）。
func (s Store) ExecCredential(ctx context.Context, name string, p Profile) ([]byte, error) {
	token, err := s.IDToken(ctx, name, p)
	if err != nil {
		return nil, err
	}
	exp := metav1.NewTime(expiry(token))
	return json.Marshal(clientauthv1.ExecCredential{
		APIVersion: clientauthv1.SchemeGroupVersion.String(), Kind: "ExecCredential",
		Status: &clientauthv1.ExecCredentialStatus{Token: token, ExpirationTimestamp: &exp},
	})
}
