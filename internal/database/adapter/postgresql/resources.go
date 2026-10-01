package postgresql

import (
	"context"
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 只返回 catalog 事实；是否构成冲突由 Database 聚合判断。
const inspectResourcesSQL = `
SELECT COALESCE(r.oid, 0), COALESCE(d.oid, 0), COALESCE(d.datdba, 0), COALESCE(d.datallowconn, false),
 EXISTS (SELECT FROM pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a
         WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT')
FROM (SELECT 1) seed
LEFT JOIN pg_catalog.pg_roles r ON r.rolname = $1
LEFT JOIN pg_catalog.pg_database d ON d.datname = $2`

type resourceReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func inspectResources(ctx context.Context, reader resourceReader, target *database.Database) (database.ResourceFacts, error) {
	var facts database.ResourceFacts
	err := reader.QueryRow(ctx, inspectResourcesSQL, target.LoginRole(), target.Name()).Scan(
		&facts.RoleOID, &facts.DatabaseOID, &facts.OwnerOID, &facts.AllowConnections, &facts.PublicConnect,
	)
	if err != nil {
		return database.ResourceFacts{}, database.ErrUnavailable
	}
	return facts, nil
}

func (d *connection) InspectResources(ctx context.Context, target *database.Database) (database.ResourceFacts, error) {
	return inspectResources(ctx, d.pool, target)
}

// CREATE ROLE 与 membership 在一个原生事务提交；不修改任何已有角色。
// simple protocol 使用 pgx 的参数转义，避免自行拼接密码字面量；错误不带 SQL 或驱动响应。
func (d *connection) CreateLoginRole(ctx context.Context, target *database.Database, value credential.ApplicationCredential) (uint32, error) {
	role := pgx.Identifier{target.LoginRole()}.Sanitize()
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, database.ErrUnavailable
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	statement := "CREATE ROLE " + role + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD $1"
	if _, err := transaction.Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol, value.Password()); err != nil {
		return 0, rolledBackError(err)
	}
	if _, err := transaction.Exec(ctx, "GRANT "+role+" TO CURRENT_USER WITH SET TRUE, INHERIT FALSE"); err != nil {
		return 0, rolledBackError(err)
	}
	var oid uint32
	if err := transaction.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$1", target.LoginRole()).Scan(&oid); err != nil {
		return 0, rolledBackError(err)
	}
	// 只有提交本身的结果可能未知；提交前的失败都随事务回滚，确定未创建。
	if err := transaction.Commit(ctx); err != nil {
		return 0, database.ErrUncertain
	}
	return oid, nil
}

func (d *connection) CreateOwnedDatabase(ctx context.Context, target *database.Database) (uint32, error) {
	// 不能放入事务。先关闭连接入口，避免默认 PUBLIC CONNECT 暴露未收紧的数据库。
	statement := "CREATE DATABASE " + pgx.Identifier{target.Name()}.Sanitize() + " OWNER " +
		pgx.Identifier{target.LoginRole()}.Sanitize() + " ALLOW_CONNECTIONS false"
	if _, err := d.pool.Exec(ctx, statement); err != nil {
		return 0, creationError(err)
	}
	// 回读 owner，防止角色在观察与创建之间被同名重建。
	facts, err := d.InspectResources(ctx, target)
	if err != nil || facts.DatabaseOID == 0 || facts.RoleOID != target.RoleOID() || facts.OwnerOID != target.RoleOID() {
		return 0, database.ErrUncertain
	}
	return facts.DatabaseOID, nil
}

func (d *connection) ConfigureAccess(ctx context.Context, target *database.Database) error {
	name, role := pgx.Identifier{target.Name()}.Sanitize(), pgx.Identifier{target.LoginRole()}.Sanitize()
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return database.ErrUnavailable
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	// 在同一事务内核对身份，再以名称切换角色，避免对同名重建的对象授权。
	facts, err := inspectResources(ctx, transaction, target)
	if err != nil {
		return err
	}
	if target.CheckResources(facts) != nil {
		return database.ErrConflict
	}
	if _, err := transaction.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		return database.ErrUnavailable
	}
	for _, statement := range []string{
		"REVOKE CONNECT ON DATABASE " + name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + name + " TO " + role,
		"ALTER DATABASE " + name + " ALLOW_CONNECTIONS true",
	} {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return database.ErrUnavailable
		}
	}
	// SET LOCAL 不污染池内连接；ACL 失败可在已确认对象上幂等重试，不改密码或 owner。
	if err := transaction.Commit(ctx); err != nil {
		return database.ErrUnavailable
	}
	facts, err = d.InspectResources(ctx, target)
	if err != nil {
		return err
	}
	if facts.PublicConnect || !facts.AllowConnections {
		return database.ErrUnavailable
	}
	return nil
}

// rolledBackError 用于提交前失败：事务未提交，可等待恢复后重试；同名对象仍需人工核对。
func rolledBackError(err error) error {
	if errors.Is(creationError(err), database.ErrConflict) {
		return database.ErrConflict
	}
	return database.ErrUnavailable
}

func creationError(err error) error {
	if serverError, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch serverError.Code {
		case "42710", "42P04", "23505":
			return database.ErrConflict
		case "42501", "25006", "28P01", "28000":
			return database.ErrUnavailable
		}
	}
	if pgconn.SafeToRetry(err) {
		return database.ErrUnavailable
	}
	return database.ErrUncertain
}
