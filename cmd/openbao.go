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

package main

import (
	"errors"
	"flag"
	"net/http"
	"net/url"
	"strings"

	bao "github.com/openbao/openbao/api/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
)

type openBaoOptions struct {
	address  string
	caCert   string
	mount    string
	role     string
	identity openbao.KubernetesIdentity
}

func (o *openBaoOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.address, "openbao-address", "", "OpenBao HTTPS 地址；为空时不启用认证会话")
	flags.StringVar(&o.caCert, "openbao-ca-cert", "", "OpenBao 公开 CA PEM 路径；默认使用系统信任根")
	flags.StringVar(&o.mount, "openbao-auth-mount", "kubernetes", "OpenBao Kubernetes auth mount")
	flags.StringVar(&o.role, "openbao-auth-role", "", "OpenBao 登录 role")
	flags.StringVar(&o.identity.Namespace, "openbao-service-account-namespace", "",
		"TokenRequest 的固定 ServiceAccount namespace")
	flags.StringVar(&o.identity.ServiceAccount, "openbao-service-account-name", "", "TokenRequest 的固定 ServiceAccount 名称")
	flags.StringVar(&o.identity.Audience, "openbao-token-audience", "openbao", "SA JWT audience，须匹配 OpenBao role")
}

func (o openBaoOptions) client() (*bao.Client, error) {
	address, err := url.Parse(o.address)
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil ||
		address.RawQuery != "" || address.ForceQuery || address.Fragment != "" ||
		(address.Path != "" && address.Path != "/") {
		return nil, errors.New("OpenBao address must be an absolute HTTPS URL without credentials, query, fragment or path")
	}
	// NewConfig 不读取 BAO_TOKEN/BAO_SKIP_VERIFY 等环境配置，不允许旁路 Kubernetes 身份或 TLS。
	config := bao.NewConfig()
	config.Address = strings.TrimSuffix(o.address, "/")
	if config.Error != nil || config.ConfigureTLS(&bao.TLSConfig{CACert: o.caCert}) != nil {
		return nil, errors.New("cannot configure OpenBao TLS trust")
	}
	client, err := bao.NewClient(config)
	if err != nil {
		return nil, errors.New("cannot construct OpenBao client")
	}
	return client, nil
}

func setupOpenBaoAuthentication(manager ctrl.Manager, options openBaoOptions) error {
	if options.address == "" {
		return nil
	}
	client, err := options.client()
	if err != nil {
		return err
	}
	// 复用 manager 已装配的 Kubernetes client，不重复加载配置或创建客户端。
	session, err := openbao.NewKubernetesSession(
		client, manager.GetClient(), options.mount, options.role, options.identity,
	)
	if err != nil {
		return err
	}
	if err := manager.Add(session); err != nil {
		return err
	}
	return manager.AddReadyzCheck("openbao-auth", func(_ *http.Request) error {
		if !session.Ready() {
			return errors.New("OpenBao Kubernetes authentication unavailable")
		}
		return nil
	})
}
