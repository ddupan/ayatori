/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package kubernetes 提供 Database 所需的 Kubernetes API 薄适配。
package kubernetes

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// SecretCredentials 直接读取 API server，不将 Secret 数据纳入共享 informer cache。
// namespace 在装配时固定，Instance 不能选择跨 namespace 读取。
type SecretCredentials struct {
	secrets typedcore.SecretInterface
}

func NewSecretCredentials(config *rest.Config, namespace string) (*SecretCredentials, error) {
	if config == nil || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("valid controller namespace and API configuration required")
	}
	client, err := typedcore.NewForConfig(config)
	if err != nil {
		return nil, application.ErrCredentialsUnavailable
	}
	return &SecretCredentials{secrets: client.Secrets(namespace)}, nil
}

func (r *SecretCredentials) Read(ctx context.Context, ref instance.CredentialReference) (application.Credentials, error) {
	if err := ref.Validate(); err != nil {
		return application.Credentials{}, application.ErrCredentialsInvalid
	}
	keys := ref.Values()
	secret, err := r.secrets.Get(ctx, keys.Name, metav1.GetOptions{})
	if err != nil {
		return application.Credentials{}, application.ErrCredentialsUnavailable
	}
	return decode(secret, keys)
}

func decode(secret *corev1.Secret, keys instance.CredentialReferenceValues) (application.Credentials, error) {
	if secret.DeletionTimestamp != nil {
		return application.Credentials{}, application.ErrCredentialsUnavailable
	}
	return application.NewCredentials(string(secret.Data[keys.UsernameKey]), string(secret.Data[keys.PasswordKey]))
}
