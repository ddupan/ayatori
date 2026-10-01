package postgresql

import (
	"context"
	"errors"
	"regexp"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/provisioning"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var resourceIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

const inspectResourcesSQL = `
SELECT COALESCE(r.oid, 0), COALESCE(d.oid, 0),
 COALESCE(r.rolcanlogin AND NOT (r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls)
 AND NOT EXISTS (SELECT FROM pg_catalog.pg_auth_members m WHERE m.member = r.oid), false),
 COALESCE(d.datdba, 0), COALESCE(d.datallowconn, false),
 EXISTS (SELECT FROM pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a
         WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT')
FROM (SELECT 1) seed
LEFT JOIN pg_catalog.pg_roles r ON r.rolname = $1
LEFT JOIN pg_catalog.pg_database d ON d.datname = $2`

type resourceReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func inspectResources(ctx context.Context, reader resourceReader, name, role string) (provisioning.Observation, error) {
	var result provisioning.Observation
	if !resourceIdentifier.MatchString(name) || !resourceIdentifier.MatchString(role) {
		return result, application.ErrResourceConflict
	}
	err := reader.QueryRow(ctx, inspectResourcesSQL, role, name).Scan(
		&result.RoleOID, &result.DatabaseOID, &result.RoleSafe, &result.OwnerOID, &result.AllowConnections, &result.PublicConnect,
	)
	if err != nil {
		return provisioning.Observation{}, application.ErrResourceUnavailable
	}
	return result, nil
}

func (d *database) InspectResources(ctx context.Context, name, role string) (provisioning.Observation, error) {
	return inspectResources(ctx, d.pool, name, role)
}

// CREATE ROLE 与 membership 在一个原生事务提交；不修改任何已有角色。
// simple protocol 使用 pgx 的参数转义，避免自行拼接密码字面量；错误不带 SQL 或驱动响应。
func (d *database) CreateLoginRole(ctx context.Context, value credential.ApplicationCredential) (uint32, error) {
	if value.Validate() != nil {
		return 0, application.ErrResourceConflict
	}
	data := value.SecretData()
	role := data["username"].(string)
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, application.ErrResourceUnavailable
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	statement := "CREATE ROLE " + pgx.Identifier{role}.Sanitize() + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD $1"
	if _, err := transaction.Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol, data["password"]); err != nil {
		return 0, rolledBackError(err)
	}
	if _, err := transaction.Exec(ctx, "GRANT "+pgx.Identifier{role}.Sanitize()+" TO CURRENT_USER WITH SET TRUE, INHERIT FALSE"); err != nil {
		return 0, rolledBackError(err)
	}
	var oid uint32
	if err := transaction.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$1", role).Scan(&oid); err != nil {
		return 0, rolledBackError(err)
	}
	// 只有提交本身的结果可能未知；提交前的失败都随事务回滚，确定未创建。
	if err := transaction.Commit(ctx); err != nil {
		return 0, application.ErrResourceUncertain
	}
	observed, err := d.InspectResources(ctx, data["database"].(string), role)
	if err != nil || observed.RoleOID != oid || !observed.RoleSafe {
		return 0, application.ErrResourceUncertain
	}
	return oid, nil
}

func (d *database) CreateOwnedDatabase(ctx context.Context, name, role string, roleOID uint32) (uint32, error) {
	observed, err := d.InspectResources(ctx, name, role)
	if err != nil {
		return 0, err
	}
	if roleOID == 0 || observed.RoleOID != roleOID || !observed.RoleSafe || observed.DatabaseOID != 0 {
		return 0, application.ErrResourceConflict
	}
	// 不能放入事务。先关闭连接入口，避免默认 PUBLIC CONNECT 暴露未收紧的数据库。
	statement := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " OWNER " + pgx.Identifier{role}.Sanitize() + " ALLOW_CONNECTIONS false"
	if _, err := d.pool.Exec(ctx, statement); err != nil {
		return 0, creationError(err)
	}
	observed, err = d.InspectResources(ctx, name, role)
	if err != nil || observed.DatabaseOID == 0 || observed.OwnerOID != roleOID || observed.RoleOID != roleOID {
		return 0, application.ErrResourceUncertain
	}
	return observed.DatabaseOID, nil
}

func (d *database) ConfigureAccess(ctx context.Context, name, role string, state provisioning.State) error {
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return application.ErrResourceUnavailable
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	observed, err := inspectResources(ctx, transaction, name, role)
	if err != nil {
		return err
	}
	if state.RoleOID == 0 || state.DatabaseOID == 0 || state.Check(observed) != nil {
		return application.ErrResourceConflict
	}
	if _, err := transaction.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return application.ErrResourceUnavailable
	}
	for _, statement := range []string{
		"REVOKE CONNECT ON DATABASE " + pgx.Identifier{name}.Sanitize() + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{name}.Sanitize() + " TO " + pgx.Identifier{role}.Sanitize(),
		"ALTER DATABASE " + pgx.Identifier{name}.Sanitize() + " ALLOW_CONNECTIONS true",
	} {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return application.ErrResourceUnavailable
		}
	}
	// SET LOCAL 不污染池内连接；ACL 失败可在已确认对象上幂等重试，不改密码或 owner。
	if err := transaction.Commit(ctx); err != nil {
		return application.ErrResourceUnavailable
	}
	observed, err = d.InspectResources(ctx, name, role)
	if err != nil {
		return err
	}
	if state.Check(observed) != nil {
		return application.ErrResourceConflict
	}
	if observed.PublicConnect || !observed.AllowConnections {
		return application.ErrResourceUnavailable
	}
	return nil
}

// rolledBackError 用于提交前失败：事务未提交，可等待恢复后重试；同名对象仍需人工核对。
func rolledBackError(err error) error {
	if errors.Is(creationError(err), application.ErrResourceConflict) {
		return application.ErrResourceConflict
	}
	return application.ErrResourceUnavailable
}

func creationError(err error) error {
	if serverError, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch serverError.Code {
		case "42710", "42P04", "23505":
			return application.ErrResourceConflict
		case "42501", "25006", "28P01", "28000":
			return application.ErrResourceUnavailable
		}
	}
	if pgconn.SafeToRetry(err) {
		return application.ErrResourceUnavailable
	}
	return application.ErrResourceUncertain
}
