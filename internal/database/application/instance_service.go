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
// Metadata 只查询版本与可用扩展，不能产生领域 Ready。
type Database interface {
	ResourceDatabase
	InspectMetadata(context.Context) (DatabaseMetadata, error)
	InspectManagement(context.Context) (DatabaseMetadata, error)
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
// 不缓存能力观察，不把连接存活等同于 Ready。凭据每轮重新读取，而非只在引用变化时读取。
// pgxpool 本身支持并发；mu 只保护连接登记，Secret 读取、建连、查询与关闭都在锁外执行。
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

// ObserveVersion 是完整 metadata 读取的便捷入口，不再维护另一条连接或查询路径。
func (s *InstanceService) ObserveVersion(ctx context.Context, target instance.ObservationTarget) (string, error) {
	observation, err := s.ObserveMetadata(ctx, target)
	return observation.Version(), err
}

// ObserveMetadata 返回当前目标和凭据下的版本与扩展；任何失败均丢弃全部结果。
// 调用者仍需使用 CR resourceVersion 保存前提防止 spec 并发修改；本方法不建立跨系统事务。
func (s *InstanceService) ObserveMetadata(ctx context.Context, target instance.ObservationTarget) (InstanceObservation, error) {
	return s.observe(ctx, target, false)
}

// ObserveManagement 复用同一凭据刷新与回读边界，但每轮重新检查原生管理能力。
func (s *InstanceService) ObserveManagement(ctx context.Context, target instance.ObservationTarget) (InstanceObservation, error) {
	return s.observe(ctx, target, true)
}

func (s *InstanceService) observe(ctx context.Context, target instance.ObservationTarget, management bool) (InstanceObservation, error) {
	current, err := s.connection(ctx, target)
	if err != nil {
		return InstanceObservation{}, err
	}
	var metadata DatabaseMetadata
	if management {
		metadata, err = current.database.InspectManagement(ctx)
	} else {
		metadata, err = current.database.InspectMetadata(ctx)
		metadata.Management = instance.ManagementChecks{}
	}
	if err != nil {
		s.release(current)
		return InstanceObservation{}, err
	}
	if metadata.Version == "" {
		s.release(current)
		return InstanceObservation{}, ErrObservation
	}
	if err := s.checkCredentials(ctx, current); err != nil {
		return InstanceObservation{}, err
	}
	return InstanceObservation{target: target, version: metadata.Version,
		extensions: instance.ObserveExtensionSupport(metadata.AvailableExtensions), management: metadata.Management}, nil
}

// connection 返回当前目标与有效凭据对应的连接池；不缓存观察结果或管理权限。
func (s *InstanceService) connection(ctx context.Context, target instance.ObservationTarget) (*entry, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 先读取有效凭据。读取失败时不得继续使用登记中的旧连接。
	name := target.Identity().Name()
	credentials, err := s.source.Read(ctx, target.Definition().AdminCredential())
	if err == nil && (credentials.username == "" || credentials.password == "") {
		err = ErrCredentialsInvalid
	}
	if err != nil {
		s.release(s.lookup(name))
		return nil, credentialError(err)
	}
	current, closed := s.registered(name)
	if closed {
		return nil, ErrClosed
	}
	// 连接身份与有效值均未变化时复用 pgxpool；generation 本身不要求换池。
	if current != nil && current.serves(target, credentials) {
		return current, nil
	}
	s.release(current)
	database, err := s.connector.Connect(ctx, target.Definition().Endpoint(), credentials)
	if err != nil {
		return nil, err
	}
	created := &entry{target: target, credentials: credentials, database: database}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		database.Close()
		return nil, ErrClosed
	}
	// 并发建连时只保留一个池；落后者关闭自己新建的池并复用已登记的。
	if existing := s.entries[name]; existing != nil && existing.serves(target, credentials) {
		s.mu.Unlock()
		database.Close()
		return existing, nil
	}
	replaced := s.entries[name]
	s.entries[name] = created
	s.mu.Unlock()
	if replaced != nil {
		replaced.database.Close()
	}
	return created, nil
}

func (e *entry) serves(target instance.ObservationTarget, credentials Credentials) bool {
	return e.target.Identity() == target.Identity() && e.target.Definition() == target.Definition() && e.credentials == credentials
}

func (s *InstanceService) lookup(name string) *entry {
	current, _ := s.registered(name)
	return current
}

func (s *InstanceService) registered(name string) (*entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[name], s.closed
}

// checkCredentials 在查询后再读一次凭据，避免把轮换前取得的结果交给新凭据的调用链。
func (s *InstanceService) checkCredentials(ctx context.Context, current *entry) error {
	latest, err := s.source.Read(ctx, current.target.Definition().AdminCredential())
	if err != nil {
		s.release(current)
		return credentialError(err)
	}
	if latest != current.credentials {
		s.release(current)
		return ErrCredentialsChanged
	}
	return nil
}

func credentialError(err error) error {
	if errors.Is(err, ErrCredentialsInvalid) {
		return ErrCredentialsInvalid
	}
	return ErrCredentialsUnavailable
}

// Forget 只释放本地连接；不删除数据库，不替代 Instance finalizer。
func (s *InstanceService) Forget(name string) {
	s.release(s.lookup(name))
}

// release 只注销仍登记着的同一个池，并在锁外关闭；pgxpool.Close 会等待在途查询归还连接。
func (s *InstanceService) release(current *entry) {
	if current == nil {
		return
	}
	name := current.target.Identity().Name()
	s.mu.Lock()
	registered := s.entries[name] == current
	if registered {
		delete(s.entries, name)
	}
	s.mu.Unlock()
	if registered {
		current.database.Close()
	}
}

// Close 在 manager 停止 worker 后调用；之后的建连请求返回 ErrClosed。
func (s *InstanceService) Close() {
	s.mu.Lock()
	s.closed = true
	entries := s.entries
	s.entries = make(map[string]*entry)
	s.mu.Unlock()
	for _, current := range entries {
		current.database.Close()
	}
}
