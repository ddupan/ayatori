package postgresql

import (
	"context"
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 只返回 catalog 事实；是否构成冲突、下一步做什么由 Database 聚合判断。
// has_database_privilege 不计入 INHERIT FALSE 的成员关系，反映管理账号自身能否连接。
const inspectResourcesSQL = `
SELECT COALESCE(r.oid, 0), COALESCE(d.oid, 0), COALESCE(d.datdba, 0), m.oid,
 EXISTS (SELECT FROM pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a
         WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT'),
 COALESCE(pg_catalog.has_database_privilege(m.oid, d.oid, 'CONNECT'), false)
FROM pg_catalog.pg_roles m
LEFT JOIN pg_catalog.pg_roles r ON r.rolname = $1
LEFT JOIN pg_catalog.pg_database d ON d.datname = $2
WHERE m.rolname = current_user`

// on 取得管理连接；连接失败按未执行处理，不认为外部对象有变化。
func (i *Instances) on(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials) (*entry, error) {
	current, err := i.pool(ctx, target, credentials)
	if err != nil {
		return nil, database.ErrUnavailable
	}
	return current, nil
}

func (i *Instances) InspectResources(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) (database.ResourceFacts, error) {
	d, err := i.on(ctx, target, credentials)
	if err != nil {
		return database.ResourceFacts{}, err
	}
	var facts database.ResourceFacts
	err = d.pool.QueryRow(ctx, inspectResourcesSQL, db.LoginRole(), db.Name()).Scan(
		&facts.RoleOID, &facts.DatabaseOID, &facts.OwnerOID, &facts.ManagerOID, &facts.PublicConnect, &facts.ManagerConnect,
	)
	if err != nil {
		return database.ResourceFacts{}, database.ErrUnavailable
	}
	return facts, nil
}

// CreateDatabase 以管理账号为临时 owner 建库；空库在撤销 PUBLIC CONNECT 前没有可泄露的数据。
func (i *Instances) CreateDatabase(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) (uint32, error) {
	d, err := i.on(ctx, target, credentials)
	if err != nil {
		return 0, err
	}
	if _, err := d.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db.Name()}.Sanitize()); err != nil {
		return 0, creationError(err)
	}
	var oid uint32
	if err := d.pool.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_database WHERE datname=$1", db.Name()).Scan(&oid); err != nil {
		return 0, database.ErrUncertain
	}
	return oid, nil
}

func (i *Instances) RevokePublicConnect(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) error {
	d, err := i.on(ctx, target, credentials)
	if err != nil {
		return err
	}
	if _, err := d.pool.Exec(ctx, "REVOKE CONNECT ON DATABASE "+pgx.Identifier{db.Name()}.Sanitize()+" FROM PUBLIC"); err != nil {
		return database.ErrUnavailable
	}
	return nil
}

// CreateLoginRole 最后创建应用角色：此时 PUBLIC CONNECT 已撤销，角色在移交 owner 前无法连接。
// simple protocol 使用 pgx 的参数转义，避免自行拼接密码字面量；错误不带 SQL 或驱动响应。
func (i *Instances) CreateLoginRole(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database, value credential.ApplicationCredential) (uint32, error) {
	d, err := i.on(ctx, target, credentials)
	if err != nil {
		return 0, err
	}
	statement := "CREATE ROLE " + pgx.Identifier{db.LoginRole()}.Sanitize() +
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD $1"
	if _, err := d.pool.Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol, value.Password()); err != nil {
		return 0, creationError(err)
	}
	var oid uint32
	if err := d.pool.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$1", db.LoginRole()).Scan(&oid); err != nil {
		return 0, database.ErrUncertain
	}
	return oid, nil
}

// TransferOwnership 把数据库交给应用角色，并保留管理账号的 CONNECT。每条语句都可重复执行。
// ALTER OWNER 会把原 owner 的 ACL 条目转给新 owner，因此之后以新 owner 身份重新授予管理账号。
func (i *Instances) TransferOwnership(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) error {
	d, err := i.on(ctx, target, credentials)
	if err != nil {
		return err
	}
	name, role := pgx.Identifier{db.Name()}.Sanitize(), pgx.Identifier{db.LoginRole()}.Sanitize()
	for _, statement := range []string{
		"GRANT " + role + " TO CURRENT_USER WITH SET TRUE, INHERIT FALSE",
		"ALTER DATABASE " + name + " OWNER TO " + role,
	} {
		if _, err := d.pool.Exec(ctx, statement); err != nil {
			return database.ErrUnavailable
		}
	}
	// 事务只用于限定 SET LOCAL ROLE 的范围，避免污染池内连接。
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return database.ErrUnavailable
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		return database.ErrUnavailable
	}
	if _, err := transaction.Exec(ctx, "GRANT CONNECT ON DATABASE "+name+" TO SESSION_USER"); err != nil {
		return database.ErrUnavailable
	}
	if err := transaction.Commit(ctx); err != nil {
		return database.ErrUnavailable
	}
	return nil
}

// creationError 只把同名冲突标为需要人工处理；其余失败由下一轮观察实际状态决定。
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
