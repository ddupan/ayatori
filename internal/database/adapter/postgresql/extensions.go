package postgresql

import (
	"context"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"github.com/jackc/pgx/v5"
)

// 在目标库内读取服务器可用与本库已安装的扩展；使用 pg_catalog 限定名称，不依赖 search_path。
const inspectExtensionsSQL = `
SELECT ARRAY(SELECT name::text FROM pg_catalog.pg_available_extensions ORDER BY name),
       ARRAY(SELECT extname::text FROM pg_catalog.pg_extension ORDER BY extname)`

// InspectExtensions 以管理账号连接目标库；管理账号在移交 owner 后仍保留 CONNECT。
func (i *Instances) InspectExtensions(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database) (database.ExtensionFacts, error) {
	connection, err := i.dial(ctx, target.Definition().Endpoint(), credentials.Username(), credentials.Password(), db.Name())
	if err != nil {
		return database.ExtensionFacts{}, database.ErrUnavailable
	}
	defer func() { _ = connection.Close(context.WithoutCancel(ctx)) }()
	var facts database.ExtensionFacts
	if err := connection.QueryRow(ctx, inspectExtensionsSQL).Scan(&facts.Available, &facts.Installed); err != nil {
		return database.ExtensionFacts{}, database.ErrUnavailable
	}
	return facts, nil
}

// InstallExtensions 以 owner 身份安装扩展，扩展对象归应用角色；非 trusted 扩展会被
// PostgreSQL 拒绝并按可重试处理。每个扩展单独提交，重复执行由 IF NOT EXISTS 保证幂等。
func (i *Instances) InstallExtensions(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database, names []string) error {
	connection, err := i.dial(ctx, target.Definition().Endpoint(), credentials.Username(), credentials.Password(), db.Name())
	if err != nil {
		return database.ErrUnavailable
	}
	defer func() { _ = connection.Close(context.WithoutCancel(ctx)) }()
	for _, name := range names {
		// 事务只用于限定 SET LOCAL ROLE 的范围。
		err := pgx.BeginFunc(ctx, connection, func(transaction pgx.Tx) error {
			if _, err := transaction.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{db.LoginRole()}.Sanitize()); err != nil {
				return err
			}
			_, err := transaction.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+pgx.Identifier{name}.Sanitize())
			return err
		})
		if err != nil {
			return database.ErrUnavailable
		}
	}
	return nil
}

// VerifyLogin 用已确认的应用凭据实际登录目标库，证明交付的凭据可用。
func (i *Instances) VerifyLogin(ctx context.Context, target instance.ObservationTarget, db *database.Database, value credential.ApplicationCredential) error {
	connection, err := i.dial(ctx, target.Definition().Endpoint(), db.LoginRole(), value.Password(), db.Name())
	if err != nil {
		return database.ErrUnavailable
	}
	defer func() { _ = connection.Close(context.WithoutCancel(ctx)) }()
	if _, err := connection.Exec(ctx, "SELECT 1"); err != nil {
		return database.ErrUnavailable
	}
	return nil
}
