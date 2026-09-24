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

package application

import (
	"context"
	"errors"
	"sync"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

var (
	ErrConnection         = errors.New("management connection unavailable")
	ErrAuthentication     = errors.New("management authentication failed")
	ErrObservation        = errors.New("management observation failed")
	ErrCredentialsChanged = errors.New("management credentials changed during observation")
	ErrClosed             = errors.New("instance service closed")
)

// Database 与 Connector 复用原项目 internal/instance/service.go 的能力边界。
// 版本查询只是本切片的连通性观察，不能产生领域 Ready。
type Database interface {
	Version(context.Context) (string, error)
	Close()
}

type Connector interface {
	Connect(context.Context, instance.Endpoint, Credentials) (Database, error)
}

type entry struct {
	target      instance.ObservationTarget
	credentials Credentials
	database    Database
}

// InstanceService 由原 Service 迁移：连接复用与释放属于应用装配，不属于 SQL adapter。
// 保留原实现串行操作的约束，防止 Close 与查询并发；controller 停止 worker 后调用 Close。
// 不缓存能力观察，不把连接存活等同于 Ready。凭据每轮重新读取，而非只在引用变化时读取。
type InstanceService struct {
	mu        sync.Mutex
	source    CredentialReader
	connector Connector
	entries   map[string]*entry
	closed    bool
}

func NewInstanceService(source CredentialReader, connector Connector) (*InstanceService, error) {
	if source == nil || connector == nil {
		return nil, errors.New("credential source and connector required")
	}
	return &InstanceService{
		source:    source,
		connector: connector,
		entries:   make(map[string]*entry),
	}, nil
}

func (s *InstanceService) String() string   { return "[redacted instance service]" }
func (s *InstanceService) GoString() string { return s.String() }

// ObserveVersion 返回当前目标和凭据下的版本；任何失败均返回空结果。
// 调用者仍需使用 CR resourceVersion 保存前提防止 spec 并发修改；本方法不建立跨系统事务。
func (s *InstanceService) ObserveVersion(ctx context.Context, target instance.ObservationTarget) (string, error) {
	if err := target.Validate(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// 先读取有效凭据。读取失败时不得继续使用缓存中的旧连接。
	name := target.Identity().Name()
	credentials, err := s.source.Read(ctx, target.Definition().AdminCredential())
	if err != nil {
		s.release(name)
		return "", credentialError(err)
	}
	if credentials.username == "" || credentials.password == "" {
		s.release(name)
		return "", ErrCredentialsInvalid
	}

	// 连接身份与有效值均未变化时复用 pgxpool；generation 本身不要求换池。
	current := s.entries[name]
	if current != nil && (current.target.Identity() != target.Identity() ||
		current.target.Definition() != target.Definition() || current.credentials != credentials) {
		s.release(name)
		current = nil
	}
	if current == nil {
		database, err := s.connector.Connect(ctx, target.Definition().Endpoint(), credentials)
		if err != nil {
			return "", err
		}
		current = &entry{
			target:      target,
			credentials: credentials,
			database:    database,
		}
		s.entries[name] = current
	}

	version, err := current.database.Version(ctx)
	if err != nil {
		s.release(name)
		return "", err
	}

	// 回读后再检查凭据，避免把轮换前取得的结果交给新凭据的调用链。
	latest, err := s.source.Read(ctx, target.Definition().AdminCredential())
	if err != nil {
		s.release(name)
		return "", credentialError(err)
	}
	if latest != credentials {
		s.release(name)
		return "", ErrCredentialsChanged
	}
	return version, nil
}

func credentialError(err error) error {
	if errors.Is(err, ErrCredentialsInvalid) {
		return ErrCredentialsInvalid
	}
	return ErrCredentialsUnavailable
}

// Forget 只释放本地连接；不删除数据库或 registry，不替代 Instance finalizer。
func (s *InstanceService) Forget(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release(name)
}

func (s *InstanceService) release(name string) {
	if current := s.entries[name]; current != nil {
		current.database.Close()
	}
	delete(s.entries, name)
}

func (s *InstanceService) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for name := range s.entries {
		s.release(name)
	}
}
