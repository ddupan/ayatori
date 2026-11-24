package postgresql

import (
	"context"
	"errors"

	"go.ddupan.top/ayatori/internal/database/domain/instance"
)

// 只读取当前执行角色的属性；是否满足管理要求由 instance 领域判断。
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

var errObservation = errors.New("management observation failed")

// InspectManagement 失败时释放该池，下一轮按当前凭据重新建连。
func (i *Instances) InspectManagement(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials) (instance.ManagementFacts, error) {
	current, err := i.pool(ctx, target, credentials)
	if err != nil {
		return instance.ManagementFacts{}, err
	}
	var facts instance.ManagementFacts
	err = current.pool.QueryRow(ctx, inspectManagementStatement).Scan(
		&facts.Version, &facts.AvailableExtensions, &facts.Superuser, &facts.CreateRole, &facts.CreateDatabase, &facts.ReadOnly,
	)
	if err != nil || facts.Version == "" {
		i.release(current)
		return instance.ManagementFacts{}, safeError(err, errObservation)
	}
	return facts, nil
}
