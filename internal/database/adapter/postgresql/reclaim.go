package postgresql

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.ddupan.top/ayatori/internal/database/domain/database"
	"go.ddupan.top/ayatori/internal/database/domain/instance"
)

// DropDatabase 依次阻止新登录、关闭连接入口、终止应用角色在本库的会话并删除数据库；
// 每条语句都可重复执行。以 owner 身份执行：移交后 owner 是应用角色，管理账号经 SET ROLE 获得
// 其权限，终止的也只是该角色自己的会话，不需要 pg_signal_backend。
func (i *Instances) DropDatabase(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) error {
	endpoint := target.Definition().Endpoint()
	connection, err := i.dial(ctx, endpoint, credentials.Username(), credentials.Password(), endpoint.Values().ManagementDatabase)
	if err != nil {
		return database.ErrUnavailable
	}
	defer func() { _ = connection.Close(context.WithoutCancel(ctx)) }()
	name, role := pgx.Identifier{db.Name()}.Sanitize(), pgx.Identifier{db.LoginRole()}.Sanitize()
	var owner string
	if err := connection.QueryRow(ctx, "SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname=$1", db.Name()).Scan(&owner); err != nil {
		return database.ErrUnavailable
	}
	type statement struct {
		sql  string
		args []any
	}
	statements := []statement{{sql: "ALTER ROLE " + role + " NOLOGIN"}}
	// 未完成 owner 移交时数据库仍归管理账号，直接以当前身份执行。
	if owner == db.LoginRole() {
		statements = append(statements, statement{sql: "SET ROLE " + role})
	}
	statements = append(statements,
		statement{sql: "ALTER DATABASE " + name + " ALLOW_CONNECTIONS false"},
		statement{sql: "SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE datname = $1 AND usename = $2",
			args: []any{db.Name(), db.LoginRole()}},
		// DROP DATABASE 不能放入事务；会话级 SET ROLE 只作用于这个短连接。
		statement{sql: "DROP DATABASE " + name},
	)
	for _, step := range statements {
		if _, err := connection.Exec(ctx, step.sql, step.args...); err != nil {
			return database.ErrUnavailable
		}
	}
	return nil
}

// DropRole 不使用 CASCADE 或 REASSIGN：角色仍拥有其他对象时交给人工处理。
func (i *Instances) DropRole(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) error {
	current, err := i.on(ctx, target, credentials)
	if err != nil {
		return err
	}
	if _, err := current.pool.Exec(ctx, "DROP ROLE "+pgx.Identifier{db.LoginRole()}.Sanitize()); err != nil {
		if serverError, ok := errors.AsType[*pgconn.PgError](err); ok && serverError.Code == "2BP01" {
			return database.ErrConflict
		}
		return database.ErrUnavailable
	}
	return nil
}
