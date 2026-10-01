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

// Package postgresql 使用 pgxpool 提供 PostgreSQL 能力的薄适配。
package postgresql

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// ErrClosed 表示 manager 已停止，不再建立新连接。
var ErrClosed = errors.New("postgresql instances closed")

// Instances 按 Instance 名称复用管理连接池。不读取 Secret：调用方每轮传入当前有效凭据，
// 目标或凭据变化时替换池。pgxpool 本身支持并发；mu 只保护登记，建连、查询与关闭都在锁外。
type Instances struct {
	RootCert string

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
}

type entry struct {
	target      instance.ObservationTarget
	credentials instance.ManagementCredentials
	pool        *pgxpool.Pool
}

func (*Instances) String() string     { return "[redacted PostgreSQL instances]" }
func (i *Instances) GoString() string { return i.String() }

func (e *entry) serves(target instance.ObservationTarget, credentials instance.ManagementCredentials) bool {
	return e.target.Identity() == target.Identity() && e.target.Definition() == target.Definition() && e.credentials == credentials
}

// pool 返回目标与凭据对应的连接池；generation 本身不要求换池。
func (i *Instances) pool(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials) (*entry, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	name := target.Identity().Name()
	i.mu.Lock()
	current, closed := i.entries[name], i.closed
	i.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if current != nil && current.serves(target, credentials) {
		return current, nil
	}
	i.release(current)
	pool, err := i.connect(ctx, target.Definition().Endpoint(), credentials)
	if err != nil {
		return nil, err
	}
	created := &entry{target: target, credentials: credentials, pool: pool}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		pool.Close()
		return nil, ErrClosed
	}
	// 并发建连时只保留一个池；落后者关闭自己新建的池并复用已登记的。
	if existing := i.entries[name]; existing != nil && existing.serves(target, credentials) {
		i.mu.Unlock()
		pool.Close()
		return existing, nil
	}
	if i.entries == nil {
		i.entries = make(map[string]*entry)
	}
	replaced := i.entries[name]
	i.entries[name] = created
	i.mu.Unlock()
	if replaced != nil {
		replaced.pool.Close()
	}
	return created, nil
}

func (i *Instances) connect(ctx context.Context, endpoint instance.Endpoint, credentials instance.ManagementCredentials) (*pgxpool.Pool, error) {
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	values := endpoint.Values()
	query := url.Values{
		"sslmode":          {string(values.TLSMode)},
		"connect_timeout":  {"5"},
		"application_name": {"ayatori-database-management"},
	}
	if i.RootCert != "" {
		query.Set("sslrootcert", i.RootCert)
	}
	connectionURL := url.URL{
		Scheme:   "postgresql",
		Host:     net.JoinHostPort(values.Host, strconv.Itoa(values.Port)),
		Path:     "/" + values.ManagementDatabase,
		User:     url.UserPassword(credentials.Username(), credentials.Password()),
		RawQuery: query.Encode(),
	}
	config, err := pgxpool.ParseConfig(connectionURL.String())
	if err != nil {
		return nil, errConnection
	}
	// pgx 不实现 libpq hostaddr；复用其 LookupFunc 扩展点，TLS 验证身份仍采用 host。
	config.ConnConfig.LookupFunc = func(context.Context, string) ([]string, error) {
		return []string{values.HostAddr}, nil
	}
	config.ConnConfig.Fallbacks = nil
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, safeError(err, errConnection)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, safeError(err, errConnection)
	}
	return pool, nil
}

// release 只注销仍登记着的同一个池，并在锁外关闭；pgxpool.Close 会等待在途查询归还连接。
func (i *Instances) release(current *entry) {
	if current == nil {
		return
	}
	name := current.target.Identity().Name()
	i.mu.Lock()
	registered := i.entries[name] == current
	if registered {
		delete(i.entries, name)
	}
	i.mu.Unlock()
	if registered {
		current.pool.Close()
	}
}

// Forget 只释放本地连接；不删除数据库，不替代 Instance finalizer。
func (i *Instances) Forget(name string) {
	i.mu.Lock()
	current := i.entries[name]
	i.mu.Unlock()
	i.release(current)
}

// Close 在 manager 停止 worker 后调用；之后的建连请求返回 ErrClosed。
func (i *Instances) Close() {
	i.mu.Lock()
	i.closed = true
	entries := i.entries
	i.entries = nil
	i.mu.Unlock()
	for _, current := range entries {
		current.pool.Close()
	}
}

var errConnection = errors.New("management connection unavailable")

func safeError(err, fallback error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && (pgerr.Code == "28P01" || pgerr.Code == "28000") {
		return instance.ErrAuthentication
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return instance.ErrAuthentication
	}
	return fallback
}
