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

// Package openbao 管理控制面共享的 OpenBao 连接与认证，不依赖任何产品领域。
package openbao

import (
	"errors"
	"net/url"
	"strings"
	"time"

	bao "github.com/openbao/openbao/api/v2"
)

// NewClient 创建供读写适配器共享的官方 SDK client；身份由 KubernetesSession 管理。
func NewClient(addressURL, caCert string) (*bao.Client, error) {
	address, err := url.Parse(addressURL)
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil ||
		address.RawQuery != "" || address.ForceQuery || address.Fragment != "" ||
		(address.Path != "" && address.Path != "/") {
		return nil, errors.New("OpenBao address must be an absolute HTTPS URL without credentials, query, fragment or path")
	}
	// NewConfig 不读取 BAO_TOKEN/BAO_SKIP_VERIFY 等环境配置，不允许旁路 Kubernetes 身份或 TLS。
	config := bao.NewConfig()
	config.Address = strings.TrimSuffix(addressURL, "/")
	// 公共读写 client 不自动重试：写入结果不确定时交由具体用例决定恢复行为。
	config.MaxRetries = 0
	config.Timeout = 15 * time.Second
	if config.Error != nil || config.ConfigureTLS(&bao.TLSConfig{CACert: caCert}) != nil {
		return nil, errors.New("cannot configure OpenBao TLS trust")
	}
	client, err := bao.NewClient(config)
	if err != nil {
		return nil, errors.New("cannot construct OpenBao client")
	}
	return client, nil
}
