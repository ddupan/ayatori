package postgresql

import (
	"context"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// 只读取当前执行角色的属性，不能从可继承的角色成员关系推导 CREATEDB/CREATEROLE。
// 所有事实来自同一条语句；不创建探针数据库，不修改管理账号或持久 schema。
const inspectManagementStatement = `
SELECT
    pg_catalog.current_setting('server_version'),
    ARRAY(SELECT name::text FROM pg_catalog.pg_available_extensions ORDER BY name),
    role.rolsuper,
    role.rolcreaterole,
    role.rolcreatedb,
    pg_catalog.pg_is_in_recovery() OR
        pg_catalog.current_setting('transaction_read_only')::boolean
FROM pg_catalog.pg_roles AS role
WHERE role.rolname = current_user`

func (d *connection) InspectManagement(ctx context.Context) (application.DatabaseMetadata, error) {
	var metadata application.DatabaseMetadata
	var superuser, createRole, createDatabase, readOnly bool
	err := d.pool.QueryRow(ctx, inspectManagementStatement).Scan(
		&metadata.Version, &metadata.AvailableExtensions,
		&superuser, &createRole, &createDatabase, &readOnly,
	)
	if err != nil {
		return application.DatabaseMetadata{}, safeError(err, application.ErrObservation)
	}
	checks := instance.ManagementChecks{
		Connection: instance.CheckPassed,
		Metadata:   instance.CheckPassed,
		Roles:      nativePrivilege(createRole && !superuser),
		Databases:  nativePrivilege(createDatabase && !superuser),
		// CREATEROLE 可管理自己新建角色的 membership；供应时必须显式取得 SET 权限，
		// 再以 owner 操作数据库 ACL。这里不授权操作任意导入角色或他人数据库。
		Grants: nativePrivilege(createRole && createDatabase && !superuser),
		// 新建数据库 owner 可安装 trusted 扩展。具体扩展仍需逐请求执行和回读，
		// 非 trusted 扩展不能因出现在 available 列表就视为可安装。
		Extensions: nativePrivilege(createRole && createDatabase && !superuser),
	}
	if readOnly {
		checks.Databases = instance.CheckUnavailable
	}
	metadata.Management = checks
	return metadata, nil
}

func nativePrivilege(allowed bool) instance.CheckResult {
	if allowed {
		return instance.CheckPassed
	}
	return instance.CheckInsufficientPrivileges
}
