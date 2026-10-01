package controller

import (
	"context"
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrCredentialsUnavailable 表示无法读取管理 Secret；不携带 API 错误详情。
var ErrCredentialsUnavailable = errors.New("management credentials unavailable")

// ManagementSecrets 读取固定 namespace 中的管理 Secret。Reader 必须是 manager.GetAPIReader()
// 或等价直连 reader，Secret 内容不进入共享 informer cache；Instance 不能选择跨 namespace 读取。
type ManagementSecrets struct {
	Reader    client.Reader
	Namespace string
}

func (s ManagementSecrets) Read(ctx context.Context, ref instance.CredentialReference) (instance.ManagementCredentials, error) {
	if err := ref.Validate(); err != nil {
		return instance.ManagementCredentials{}, instance.ErrCredentialsInvalid
	}
	keys := ref.Values()
	secret := &corev1.Secret{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: keys.Name}, secret); err != nil || secret.DeletionTimestamp != nil {
		return instance.ManagementCredentials{}, ErrCredentialsUnavailable
	}
	return instance.NewManagementCredentials(string(secret.Data[keys.UsernameKey]), string(secret.Data[keys.PasswordKey]))
}
