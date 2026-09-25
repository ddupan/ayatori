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

package openbao

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kubernetesauth "github.com/openbao/openbao/api/auth/kubernetes/v2"
	bao "github.com/openbao/openbao/api/v2"
)

var ErrAuthenticationConfiguration = errors.New("invalid OpenBao Kubernetes authentication configuration")

// KubernetesSession 为专用 SDK client 维护短期登录，不持久化或对外返回 token。
// 登录失败或 lease 不再可用时清空 client token，依赖恢复后重新读取投射的 SA token。
// 续期调度由官方 LifetimeWatcher 负责，不实现自己的 lease 算法。
type KubernetesSession struct {
	client    *bao.Client
	mount     string
	role      string
	tokenPath string
	running   sync.Mutex
	ready     atomic.Bool
}

func NewKubernetesSession(client *bao.Client, mount, role, tokenPath string) (*KubernetesSession, error) {
	if client == nil || !validPath(mount) || !pathSegment.MatchString(role) || !filepath.IsAbs(tokenPath) {
		return nil, ErrAuthenticationConfiguration
	}
	client.ClearToken()
	client.SetMaxRetries(0)
	client.SetClientTimeout(15 * time.Second)
	return &KubernetesSession{client: client, mount: mount, role: role, tokenPath: tokenPath}, nil
}

// Ready 仅代表当前登录 lease 正由 SDK 管理，不保证下一次后端请求一定成功。
func (s *KubernetesSession) Ready() bool { return s.ready.Load() }

// Start 可交给 manager 管理；关闭时清空本地 token，不撤销共享后端数据。
// SDK 的 Stop 不取消已发出的续期 HTTP 请求，因此等待该请求结束后才退出，最长受 client timeout 限制。
func (s *KubernetesSession) Start(ctx context.Context) error {
	if !s.running.TryLock() {
		return errors.New("OpenBao authentication is already running")
	}
	defer s.running.Unlock()
	defer s.clear()
	for ctx.Err() == nil {
		s.clear()
		secret := s.login(ctx)
		if secret != nil {
			s.watch(ctx, secret)
		}
		s.clear()
		// 登录失败及不能继续续期均有限速，防止依赖故障时形成请求忙循环。
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

func (s *KubernetesSession) clear() {
	s.ready.Store(false)
	s.client.ClearToken()
}

func (s *KubernetesSession) login(ctx context.Context) *bao.Secret {
	// 只读取指定投射位置；SDK 的空 token 选项会回退默认路径，必须在此拒绝空文件。
	data, err := os.ReadFile(s.tokenPath)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return nil
	}
	// helper 会缓存 token，不能跨登录轮次复用。
	method, err := kubernetesauth.NewKubernetesAuth(s.role,
		kubernetesauth.WithMountPath(s.mount),
		kubernetesauth.WithServiceAccountToken(strings.TrimSpace(string(data))),
	)
	if err != nil {
		return nil
	}
	// 先检查短期 lease，再发布到共享 client，避免暴露不合规的登录结果。
	secret, err := method.Login(ctx, s.client)
	if err != nil || secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" || secret.Auth.LeaseDuration <= 0 {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	s.client.SetToken(secret.Auth.ClientToken)
	return secret
}

func (s *KubernetesSession) watch(ctx context.Context, secret *bao.Secret) {
	behavior := bao.RenewBehaviorErrorOnErrors
	if !secret.Auth.Renewable {
		behavior = bao.RenewBehaviorRenewDisabled
	}
	watcher, err := s.client.NewLifetimeWatcher(&bao.LifetimeWatcherInput{Secret: secret, RenewBehavior: behavior})
	if err != nil {
		return
	}
	s.ready.Store(true)
	go watcher.Start()
	for {
		select {
		case <-ctx.Done():
			s.clear()
			watcher.Stop()
			<-watcher.DoneCh()
			return
		case <-watcher.DoneCh():
			watcher.Stop()
			return
		case <-watcher.RenewCh():
			// 不记录 SDK Secret 或 token；无需复制 SDK 已处理的续期数据。
		}
	}
}
