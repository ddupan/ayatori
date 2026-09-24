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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// Connector 不读取 Secret、不决定连接何时替换；池本身由 pgxpool 实现。
type Connector struct {
	RootCert string
}

type database struct {
	pool *pgxpool.Pool
}

func (*database) String() string     { return "[redacted PostgreSQL database]" }
func (d *database) GoString() string { return d.String() }
func (d *database) Close() {
	d.pool.Close()
}

func (d *database) Version(ctx context.Context) (string, error) {
	var version string
	if err := d.pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "", safeError(err, application.ErrObservation)
	}
	return version, nil
}

func (c Connector) Connect(ctx context.Context, endpoint instance.Endpoint, credentials application.Credentials) (application.Database, error) {
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	if credentials.Username() == "" || credentials.Password() == "" {
		return nil, application.ErrCredentialsInvalid
	}
	endpointValues := endpoint.Values()
	query := url.Values{
		"sslmode":          {string(endpointValues.TLSMode)},
		"connect_timeout":  {"5"},
		"application_name": {"ayatori-database-management"},
	}
	if c.RootCert != "" {
		query.Set("sslrootcert", c.RootCert)
	}
	connectionURL := url.URL{
		Scheme:   "postgresql",
		Host:     net.JoinHostPort(endpointValues.Host, strconv.Itoa(endpointValues.Port)),
		Path:     "/" + endpointValues.ManagementDatabase,
		User:     url.UserPassword(credentials.Username(), credentials.Password()),
		RawQuery: query.Encode(),
	}
	config, err := pgxpool.ParseConfig(connectionURL.String())
	if err != nil {
		return nil, application.ErrConnection
	}
	// pgx 不实现 libpq hostaddr；复用其 LookupFunc 扩展点，TLS 验证身份仍采用 host。
	config.ConnConfig.LookupFunc = func(context.Context, string) ([]string, error) {
		return []string{endpointValues.HostAddr}, nil
	}
	config.ConnConfig.Fallbacks = nil
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, safeError(err, application.ErrConnection)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, safeError(err, application.ErrConnection)
	}
	return &database{pool: pool}, nil
}

func safeError(err, fallback error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && (pgerr.Code == "28P01" || pgerr.Code == "28000") {
		return application.ErrAuthentication
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return application.ErrAuthentication
	}
	return fallback
}
