//go:build integration

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

package postgresql_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql/registry"
)

// registryFixture 只连接本测试创建的容器，不接受外部数据库地址。
type registryFixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	store *registry.Store
}

func newRegistryFixture(t *testing.T) *registryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	_, port := postgresFixture(t, ctx)
	endpoint := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(fixtureUser, fixturePassword),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:     "/postgres",
		RawQuery: "sslmode=disable",
	}
	pool, err := pgxpool.New(ctx, endpoint.String())
	if err != nil {
		t.Fatal("cannot configure registry fixture connection")
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("cannot connect to registry fixture")
	}
	return &registryFixture{ctx: ctx, pool: pool, store: registry.NewStore(pool)}
}

func registryOwner(suffix string) registry.Ownership {
	return registry.Ownership{
		InstanceUID:     "instance-uid",
		TenantUID:       "tenant-uid-" + suffix,
		TenantNamespace: "applications",
		TenantName:      "tenant-" + suffix,
		DatabaseName:    "database_" + suffix,
		RoleName:        "role_" + suffix,
		CredentialPath:  "credentials/" + suffix,
	}
}

func (f *registryFixture) bootstrap(t *testing.T) {
	t.Helper()
	if err := f.store.Bootstrap(f.ctx); err != nil {
		t.Fatalf("bootstrap registry: %v", err)
	}
}

func (f *registryFixture) claim(t *testing.T, owner registry.Ownership, expected registry.ClaimResult) {
	t.Helper()
	result, err := f.store.Claim(f.ctx, owner)
	if err != nil || result != expected {
		t.Fatalf("claim: result=%q, error=%v; want %q", result, err, expected)
	}
}

func TestRegistryOwnershipLifecycle(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	f.bootstrap(t)
	owner := registryOwner("lifecycle")
	f.claim(t, owner, registry.ClaimCreated)
	f.claim(t, owner, registry.ClaimOwned)
	record, err := f.store.Get(f.ctx, owner.InstanceUID, owner.TenantUID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Ownership != owner || !record.Managed || record.CreatedAt.IsZero() || record.RetainedAt != nil {
		t.Fatalf("unexpected ownership record: %+v", record)
	}

	// 错误的资源归属不能删除或 Retain 原记录。
	wrongOwner := owner
	wrongOwner.RoleName = "another_role"
	if err := f.store.Delete(f.ctx, wrongOwner); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("delete mismatched owner: %v", err)
	}
	if err := f.store.MarkRetained(f.ctx, wrongOwner); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("retain mismatched owner: %v", err)
	}
	for range 2 {
		if err := f.store.Delete(f.ctx, owner); err != nil {
			t.Fatalf("delete retry: %v", err)
		}
	}
	if _, err := f.store.Get(f.ctx, owner.InstanceUID, owner.TenantUID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("read deleted record: %v", err)
	}
}

func TestRegistryRetainedRecordCannotBeReclaimed(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	owner := registryOwner("retained")
	f.claim(t, owner, registry.ClaimCreated)
	if err := f.store.MarkRetained(f.ctx, owner); err != nil {
		t.Fatal(err)
	}
	retained, err := f.store.Get(f.ctx, owner.InstanceUID, owner.TenantUID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Managed || retained.RetainedAt == nil {
		t.Fatalf("missing retained tombstone: %+v", retained)
	}
	if err := f.store.MarkRetained(f.ctx, owner); err != nil {
		t.Fatalf("retain retry: %v", err)
	}
	retried, err := f.store.Get(f.ctx, owner.InstanceUID, owner.TenantUID)
	if err != nil || !retried.UpdatedAt.Equal(retained.UpdatedAt) {
		t.Fatalf("retain retry changed tombstone: %+v, %v", retried, err)
	}
	if err := f.store.Delete(f.ctx, owner); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("delete retained record: %v", err)
	}
	if _, err := f.store.Claim(f.ctx, owner); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("reclaim retained record: %v", err)
	}
	owner.TenantUID = "replacement-uid"
	if _, err := f.store.Claim(f.ctx, owner); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("replacement tenant reclaimed tombstone: %v", err)
	}
}

func TestRegistryUniqueReservations(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	owner := registryOwner("original")
	f.claim(t, owner, registry.ClaimCreated)
	tests := []struct {
		name   string
		change func(*registry.Ownership)
	}{
		{name: "tenant UID", change: func(other *registry.Ownership) { other.TenantUID = owner.TenantUID }},
		{name: "tenant name", change: func(other *registry.Ownership) { other.TenantName = owner.TenantName }},
		{name: "database", change: func(other *registry.Ownership) { other.DatabaseName = owner.DatabaseName }},
		{name: "role", change: func(other *registry.Ownership) { other.RoleName = owner.RoleName }},
		{name: "credential path", change: func(other *registry.Ownership) { other.CredentialPath = owner.CredentialPath }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := registryOwner("other")
			tt.change(&other)
			if _, err := f.store.Claim(f.ctx, other); !errors.Is(err, registry.ErrConflict) {
				t.Fatalf("conflicting reservation: %v", err)
			}
		})
	}
}

func TestRegistryConcurrentBootstrapAndClaims(t *testing.T) {
	f := newRegistryFixture(t)
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			<-start
			results <- f.store.Bootstrap(f.ctx)
		})
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent bootstrap: %v", err)
		}
	}

	owner := registryOwner("concurrent")
	type claimOutcome struct {
		result registry.ClaimResult
		err    error
	}
	claims := make(chan claimOutcome, workers)
	start = make(chan struct{})
	for range workers {
		group.Go(func() {
			<-start
			result, err := f.store.Claim(f.ctx, owner)
			claims <- claimOutcome{result: result, err: err}
		})
	}
	close(start)
	group.Wait()
	close(claims)
	created := 0
	for outcome := range claims {
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		switch outcome.result {
		case registry.ClaimCreated:
			created++
		case registry.ClaimOwned:
		default:
			t.Fatalf("unexpected claim result: %q", outcome.result)
		}
	}
	if created != 1 {
		t.Fatalf("created %d records for the same owner; want 1", created)
	}
}

func TestRegistryRestartAndCanceledRequestRecovery(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	owner := registryOwner("restart")
	f.claim(t, owner, registry.ClaimCreated)
	config := f.pool.Config()
	f.pool.Close()
	pool, err := pgxpool.NewWithConfig(f.ctx, config)
	if err != nil {
		t.Fatal("cannot reopen fixture connection")
	}
	t.Cleanup(pool.Close)
	f.store = registry.NewStore(pool)
	f.bootstrap(t)
	// 模拟客户端丢失上次成功结果后，以新连接重试；归属证据必须来自数据库。
	f.claim(t, owner, registry.ClaimOwned)
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.Claim(canceled, registryOwner("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled claim: %v", err)
	}
	f.claim(t, registryOwner("canceled"), registry.ClaimCreated)
}

func TestRegistryBootstrapRejectsUnknownSchema(t *testing.T) {
	f := newRegistryFixture(t)
	// 人工创建的同名 schema 不能被初始化过程接管或覆盖。
	if _, err := f.pool.Exec(f.ctx, "CREATE SCHEMA postgresql_tenant_operator"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Bootstrap(f.ctx); err == nil {
		t.Fatal("bootstrap accepted an unmanaged schema")
	}
	var version int
	if err := f.pool.QueryRow(f.ctx, "SELECT version FROM public.postgresql_tenant_operator_schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("failed migration advanced version to %d", version)
	}
	// 仅在本测试拥有的临时数据库中清除空冲突 schema，然后重试失败的迁移。
	if _, err := f.pool.Exec(f.ctx, "DROP SCHEMA postgresql_tenant_operator"); err != nil {
		t.Fatal(err)
	}
	f.bootstrap(t)
	if _, err := f.pool.Exec(f.ctx, "UPDATE public.postgresql_tenant_operator_schema_version SET version = 100"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Bootstrap(f.ctx); err == nil {
		t.Fatal("bootstrap accepted a future migration version")
	}
	if err := f.pool.QueryRow(f.ctx, "SELECT version FROM public.postgresql_tenant_operator_schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 100 {
		t.Fatalf("bootstrap rewrote future version to %d", version)
	}
}

// 数据库执行真实 COMMIT 后才注入客户端错误，模拟客户端无法确认提交结果。
// 这不是网络故障测试，但能确定性覆盖已提交、调用者却收到失败的恢复分支。
type lostCommitReply struct {
	registry.Beginner
	err error
}

func (db lostCommitReply) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.Beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return uncertainCommit{Tx: tx, err: db.err}, nil
}

type uncertainCommit struct {
	pgx.Tx
	err error
}

func (tx uncertainCommit) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	return tx.err
}

func TestRegistryRetriesAfterLostCommitReply(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	replyLost := errors.New("test-only lost commit reply")
	uncertain := registry.NewStore(lostCommitReply{Beginner: f.pool, err: replyLost})
	owner := registryOwner("uncertain")
	if _, err := uncertain.Claim(f.ctx, owner); !errors.Is(err, replyLost) {
		t.Fatalf("claim did not report lost reply: %v", err)
	}
	f.claim(t, owner, registry.ClaimOwned)
	if err := uncertain.MarkRetained(f.ctx, owner); !errors.Is(err, replyLost) {
		t.Fatalf("retain did not report lost reply: %v", err)
	}
	if err := f.store.MarkRetained(f.ctx, owner); err != nil {
		t.Fatalf("retry uncertain retain: %v", err)
	}
	if err := f.store.Delete(f.ctx, owner); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("uncertain retain lost tombstone protection: %v", err)
	}
	deletable := registryOwner("uncertain_delete")
	f.claim(t, deletable, registry.ClaimCreated)
	if err := uncertain.Delete(f.ctx, deletable); !errors.Is(err, replyLost) {
		t.Fatalf("delete did not report lost reply: %v", err)
	}
	if err := f.store.Delete(f.ctx, deletable); err != nil {
		t.Fatalf("retry uncertain delete: %v", err)
	}
}

func TestRegistryConcurrentConflictingClaims(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	first := registryOwner("first")
	second := registryOwner("second")
	second.DatabaseName = first.DatabaseName
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, owner := range []registry.Ownership{first, second} {
		group.Go(func() {
			<-start
			_, err := f.store.Claim(f.ctx, owner)
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	created, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, registry.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent claim error: %v", err)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("concurrent reservation: created=%d conflicts=%d; want one of each", created, conflicts)
	}
}

func TestRegistryConcurrentRetainAndDelete(t *testing.T) {
	f := newRegistryFixture(t)
	f.bootstrap(t)
	owner := registryOwner("delete_race")
	f.claim(t, owner, registry.ClaimCreated)
	start := make(chan struct{})
	retainResult := make(chan error, 1)
	deleteResult := make(chan error, 1)
	go func() {
		<-start
		retainResult <- f.store.MarkRetained(f.ctx, owner)
	}()
	go func() {
		<-start
		deleteResult <- f.store.Delete(f.ctx, owner)
	}()
	close(start)
	retainErr, deleteErr := <-retainResult, <-deleteResult
	record, readErr := f.store.Get(f.ctx, owner.InstanceUID, owner.TenantUID)
	switch {
	case retainErr == nil:
		// Retain 先取得行锁时，Delete 必须拒绝删除墓碑。
		if !errors.Is(deleteErr, registry.ErrConflict) || readErr != nil || record.Managed || record.RetainedAt == nil {
			t.Fatalf("retain won but tombstone was not protected: delete=%v read=%v record=%+v", deleteErr, readErr, record)
		}
	case errors.Is(retainErr, registry.ErrNotFound):
		// Delete 先提交时，Retain 必须报告记录已不存在，不能重建墓碑。
		if deleteErr != nil || !errors.Is(readErr, registry.ErrNotFound) {
			t.Fatalf("delete won but record remains: delete=%v read=%v", deleteErr, readErr)
		}
	default:
		t.Fatalf("unexpected retain/delete race: retain=%v delete=%v", retainErr, deleteErr)
	}
}
