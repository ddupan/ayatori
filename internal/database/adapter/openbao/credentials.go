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

// Package openbao 通过官方 SDK 适配应用凭据，不保存资源归属或重建供应状态。
package openbao

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	bao "github.com/openbao/openbao/api/v2"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
)

var (
	ErrInvalidLocation = errors.New("credential location is outside the configured scope")
	ErrUnavailable     = errors.New("credential backend unavailable")
	ErrNotFound        = errors.New("application credential not found")
	ErrConflict        = errors.New("credential creation requires manual conflict resolution")
	ErrUncertain       = errors.New("credential creation outcome is uncertain; manual resolution required")
)

var pathSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Credentials 使用独立的 SDK client；认证与短期 token 生命周期由部署装配负责。
// 本适配器既不自动认领已有值，也不提供覆盖、轮换或删除操作。
type Credentials struct {
	kv       *bao.KVv2
	basePath string
}

// NewCredentials 不登录、不读取环境 token。调用方必须提供专用的已认证 client。
// client 由公共 infra 禁用自动重试，防止第一次结果丢失后被 CAS 错误掩盖。
func NewCredentials(client *bao.Client, mount, basePath string) (*Credentials, error) {
	if client == nil || !validPath(mount) || !validPath(basePath) {
		return nil, ErrInvalidLocation
	}
	return &Credentials{kv: client.KVv2(mount), basePath: basePath}, nil
}

func validPath(value string) bool {
	for segment := range strings.SplitSeq(value, "/") {
		if !pathSegment.MatchString(segment) || segment == "data" || segment == "metadata" {
			return false
		}
	}
	return true
}

// ProvisionPath 只按 Database UID 定位；调用方须先持久化位置，再执行外部写入。
func (c *Credentials) ProvisionPath(databaseUID string) (string, error) {
	if !pathSegment.MatchString(databaseUID) {
		return "", ErrInvalidLocation
	}
	return c.basePath + "/" + databaseUID, nil
}

func (c *Credentials) accepts(path string) bool {
	return validPath(path) && strings.HasPrefix(path, c.basePath+"/")
}

// Read 只读取调用方已确认关联的路径；成功读取不构成对既有凭据的自动认领。
func (c *Credentials) Read(ctx context.Context, path string) (application.ApplicationCredential, error) {
	secret, err := c.read(ctx, path)
	if err != nil {
		return application.ApplicationCredential{}, err
	}
	return application.ParseApplicationCredential(secret.Data)
}

// ReadConfirmed 读取最新值并核对已持久化的确认版本，不回退读取历史版本。
// 确认后的删除或改写需要人工处理，不能因此重新生成密码。
func (c *Credentials) ReadConfirmed(ctx context.Context, path string, version int64) (application.ApplicationCredential, error) {
	if version < 1 {
		return application.ApplicationCredential{}, ErrConflict
	}
	secret, err := c.read(ctx, path)
	if errors.Is(err, ErrNotFound) {
		return application.ApplicationCredential{}, ErrConflict
	}
	if err != nil {
		return application.ApplicationCredential{}, err
	}
	if secret.VersionMetadata == nil || int64(secret.VersionMetadata.Version) != version {
		return application.ApplicationCredential{}, ErrConflict
	}
	credential, err := application.ParseApplicationCredential(secret.Data)
	if err != nil {
		return application.ApplicationCredential{}, ErrConflict
	}
	return credential, nil
}

func (c *Credentials) read(ctx context.Context, path string) (*bao.KVSecret, error) {
	if !c.accepts(path) {
		return nil, ErrInvalidLocation
	}
	secret, err := c.kv.Get(ctx, path)
	if errors.Is(err, bao.ErrSecretNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	if secret == nil || secret.Data == nil {
		return nil, ErrNotFound
	}
	return secret, nil
}

// Create 只创建从未存在过的路径，并验证回读七键与提交值完全一致。
// 任何不确定写入都不返回凭据；上层必须停止供应并持久化冲突，不能重新生成密码。
func (c *Credentials) Create(ctx context.Context, path string, credential application.ApplicationCredential) error {
	if !c.accepts(path) {
		return ErrInvalidLocation
	}
	if err := credential.Validate(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	data := credential.SecretData()
	created, err := c.kv.Put(ctx, path, data, bao.WithCheckAndSet(0))
	if err != nil {
		// 明确的认证/权限拒绝没有发生写入，可以等待依赖恢复。
		// SDK 的原始错误可能携带路径及响应体，不向外传播。
		if response, ok := errors.AsType[*bao.ResponseError](err); ok {
			switch response.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return ErrUnavailable
			case http.StatusBadRequest:
				if slices.Contains(response.Errors, "check-and-set parameter did not match the current version") {
					return ErrConflict
				}
			}
		}
		return ErrUncertain
	}
	if created == nil || created.VersionMetadata == nil || created.VersionMetadata.Version != 1 {
		return ErrUncertain
	}
	observed, err := c.kv.Get(ctx, path)
	if err != nil || observed == nil || observed.VersionMetadata == nil || observed.VersionMetadata.Version != 1 {
		return ErrUncertain
	}
	if !maps.Equal(data, observed.Data) {
		return ErrUncertain
	}
	return nil
}
