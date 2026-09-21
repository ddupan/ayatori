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

// Package registry persists controller ownership evidence in the PostgreSQL
// management database. It deliberately does not persist reconciliation phases;
// those belong to the Kubernetes resource status.
package registry

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

const migrationVersionTable = "public.postgresql_tenant_operator_schema_version"

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	// ErrNotFound indicates that the registry has no matching ownership record.
	ErrNotFound = errors.New("registry ownership record not found")
	// ErrConflict indicates that a requested name or path belongs to another Tenant UID.
	ErrConflict = errors.New("registry ownership conflict")
)

// Beginner is implemented by pgx.Conn and pgxpool.Pool.
type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Store manages ownership records in one PostgreSQLInstance management database.
type Store struct {
	db Beginner
}

// NewStore creates a registry store backed by a PostgreSQL connection or pool.
func NewStore(db Beginner) *Store {
	return &Store{db: db}
}

// Ownership identifies every external resource reserved for one Tenant UID.
type Ownership struct {
	InstanceUID     string
	TenantUID       string
	TenantNamespace string
	TenantName      string
	DatabaseName    string
	RoleName        string
	CredentialPath  string
}

// Record is the persisted ownership state.
type Record struct {
	Ownership
	Managed    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
	RetainedAt *time.Time
}

// ClaimResult reports whether Claim inserted a new record or found the same claim.
type ClaimResult string

const (
	ClaimCreated ClaimResult = "Created"
	ClaimOwned   ClaimResult = "Owned"
)

// Bootstrap applies all pending versioned registry migrations idempotently.
func (s *Store) Bootstrap(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("bootstrap registry: nil database")
	}

	return s.withMigrationConnection(ctx, func(conn *pgx.Conn) error {
		migrations, err := fs.Sub(migrationFiles, "migrations")
		if err != nil {
			return fmt.Errorf("open embedded registry migrations: %w", err)
		}
		migrator, err := migrate.NewMigrator(ctx, conn, migrationVersionTable)
		if err != nil {
			return fmt.Errorf("initialize registry migrator: %w", err)
		}
		if err := migrator.LoadMigrations(migrations); err != nil {
			return fmt.Errorf("load registry migrations: %w", err)
		}
		if err := migrator.Migrate(ctx); err != nil {
			return fmt.Errorf("apply registry migrations: %w", err)
		}
		return nil
	})
}

// Claim reserves all names for an owner. Repeating an identical claim is idempotent.
func (s *Store) Claim(ctx context.Context, owner Ownership) (ClaimResult, error) {
	if s == nil || s.db == nil {
		return "", errors.New("claim registry ownership: nil database")
	}
	if err := owner.validate(); err != nil {
		return "", err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin registry claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = scanRecord(tx.QueryRow(ctx, claimStatement,
		owner.InstanceUID,
		owner.TenantUID,
		owner.TenantNamespace,
		owner.TenantName,
		owner.DatabaseName,
		owner.RoleName,
		owner.CredentialPath,
	))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("insert registry claim: %w", err)
	}
	if errors.Is(err, ErrNotFound) {
		record, err := getByTenantUID(ctx, tx, owner.InstanceUID, owner.TenantUID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", fmt.Errorf("%w: database, role, tenant identity, or credential path is already reserved", ErrConflict)
			}
			return "", err
		}
		if !record.equal(owner) || !record.Managed {
			return "", fmt.Errorf("%w: database, role, tenant identity, or credential path is already reserved", ErrConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit registry claim: %w", err)
		}
		return ClaimOwned, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit registry claim: %w", err)
	}
	return ClaimCreated, nil
}

func (s *Store) withMigrationConnection(ctx context.Context, run func(*pgx.Conn) error) error {
	switch db := s.db.(type) {
	case *pgx.Conn:
		return run(db)
	case *pgxpool.Pool:
		conn, err := db.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("acquire registry migration connection: %w", err)
		}
		defer conn.Release()
		return run(conn.Conn())
	default:
		return fmt.Errorf("bootstrap registry: database type %T cannot provide a migration connection", s.db)
	}
}

// Get returns the ownership record for an Instance UID and Tenant UID.
func (s *Store) Get(ctx context.Context, instanceUID, tenantUID string) (Record, error) {
	if s == nil || s.db == nil {
		return Record{}, errors.New("get registry record: nil database")
	}
	if instanceUID == "" || tenantUID == "" {
		return Record{}, errors.New("get registry record: instance UID and tenant UID are required")
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("begin registry read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	record, err := getByTenantUID(ctx, tx, instanceUID, tenantUID)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Record{}, fmt.Errorf("commit registry read: %w", err)
	}
	return record, nil
}

// MarkRetained changes a matching managed ownership record into an unmanaged tombstone.
// Repeating the operation for the same tombstone is safe.
func (s *Store) MarkRetained(ctx context.Context, owner Ownership) error {
	return s.changeOwnership(ctx, owner, "mark registry record retained", func(ctx context.Context, tx pgx.Tx, record Record) error {
		if !record.Managed {
			return nil
		}
		tag, err := tx.Exec(ctx, markRetainedStatement, owner.InstanceUID, owner.TenantUID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

// Delete removes a matching managed record after its external resources have been deleted.
// An already absent record is treated as a successful retry; a retained record is never deleted.
func (s *Store) Delete(ctx context.Context, owner Ownership) error {
	return s.changeOwnership(ctx, owner, "delete registry record", func(ctx context.Context, tx pgx.Tx, record Record) error {
		if !record.Managed {
			return ErrConflict
		}
		tag, err := tx.Exec(ctx, deleteStatement, owner.InstanceUID, owner.TenantUID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

func (s *Store) changeOwnership(
	ctx context.Context,
	owner Ownership,
	operation string,
	change func(context.Context, pgx.Tx, Record) error,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%s: nil database", operation)
	}
	if err := owner.validate(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", operation, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	record, err := getByTenantUIDForUpdate(ctx, tx, owner.InstanceUID, owner.TenantUID)
	if errors.Is(err, ErrNotFound) && operation == "delete registry record" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !record.equal(owner) {
		return fmt.Errorf("%s: %w", operation, ErrConflict)
	}
	if err := change(ctx, tx, record); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", operation, err)
	}
	return nil
}

func getByTenantUIDForUpdate(ctx context.Context, tx pgx.Tx, instanceUID, tenantUID string) (Record, error) {
	return scanRecord(tx.QueryRow(ctx, getByTenantUIDForUpdateStatement, instanceUID, tenantUID))
}

func getByTenantUID(ctx context.Context, tx pgx.Tx, instanceUID, tenantUID string) (Record, error) {
	return scanRecord(tx.QueryRow(ctx, getByTenantUIDStatement, instanceUID, tenantUID))
}

type rowScanner interface {
	Scan(...any) error
}

func scanRecord(row rowScanner) (Record, error) {
	var record Record
	err := row.Scan(
		&record.InstanceUID,
		&record.TenantUID,
		&record.TenantNamespace,
		&record.TenantName,
		&record.DatabaseName,
		&record.RoleName,
		&record.CredentialPath,
		&record.Managed,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.RetainedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read registry record: %w", err)
	}
	return record, nil
}

func (o Ownership) validate() error {
	if o.InstanceUID == "" || o.TenantUID == "" || o.TenantNamespace == "" || o.TenantName == "" ||
		o.DatabaseName == "" || o.RoleName == "" || o.CredentialPath == "" {
		return errors.New("claim registry ownership: all ownership fields are required")
	}
	return nil
}

func (o Ownership) equal(other Ownership) bool {
	return o == other
}
