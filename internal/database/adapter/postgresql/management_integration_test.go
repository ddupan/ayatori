//go:build integration

package postgresql_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const nativeManager = "native_manager"

func useNativeManager(t *testing.T, f *credentialFixture) {
	t.Helper()
	f.queryPostgres(t, "CREATE ROLE native_manager LOGIN CREATEDB CREATEROLE PASSWORD '"+fixturePassword+"'")
	f.createSecret(t, controllerNamespace)
	f.updateSecret(t, func(secret *corev1.Secret) { secret.Data[managementUsernameKey] = []byte(nativeManager) })
}

func assessManagement(t *testing.T, f *credentialFixture) instance.Snapshot {
	t.Helper()
	observation, err := f.service.ObserveManagement(f.ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := instance.Reconstitute(f.target, instance.Snapshot{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := aggregate.BeginValidation(); err != nil {
		t.Fatal(err)
	}
	capabilities, err := observation.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	if err := aggregate.AssessManagement(capabilities); err != nil {
		t.Fatal(err)
	}
	return aggregate.Snapshot()
}

func TestNativeManagementPrivileges(t *testing.T) {
	f := newCredentialFixture(t)
	useNativeManager(t, f)
	if snapshot := assessManagement(t, f); snapshot.Readiness != instance.Ready {
		t.Fatal("原生非 superuser 管理账号未通过检查")
	}
	before := f.backendIDs(t)
	for _, attribute := range []string{"NOCREATEROLE", "NOCREATEDB"} {
		f.queryPostgres(t, "ALTER ROLE native_manager "+attribute)
		if snapshot := assessManagement(t, f); snapshot.Failure != instance.InsufficientPrivileges {
			t.Fatal("已有连接忽略了管理权限撤回")
		}
		f.queryPostgres(t, "ALTER ROLE native_manager CREATEROLE CREATEDB")
		if snapshot := assessManagement(t, f); snapshot.Readiness != instance.Ready {
			t.Fatal("管理权限恢复后无法重新就绪")
		}
	}
	if f.backendIDs(t) != before {
		t.Fatal("权限检查不应要求重建连接才生效")
	}
	f.queryPostgres(t, "ALTER ROLE native_manager SET default_transaction_read_only = on")
	f.service.Forget(f.target.Identity().Name())
	if snapshot := assessManagement(t, f); snapshot.Failure != instance.DependencyUnavailable {
		t.Fatal("只读会话不应标记可供应")
	}
	f.queryPostgres(t, "ALTER ROLE native_manager RESET default_transaction_read_only")
	f.service.Forget(f.target.Identity().Name())
	if snapshot := assessManagement(t, f); snapshot.Readiness != instance.Ready {
		t.Fatal("恢复可写会话后没有就绪")
	}
	f.updateSecret(t, func(secret *corev1.Secret) { secret.Data[managementUsernameKey] = []byte(fixtureUser) })
	if snapshot := assessManagement(t, f); snapshot.Failure != instance.InsufficientPrivileges {
		t.Fatal("不应以 superuser 绕过非特权账号合同")
	}
	reads := 0
	f.gate.beforeRead = func() {
		reads++
		if reads == 2 {
			f.updateSecret(t, func(secret *corev1.Secret) { secret.Data[managementPasswordKey] = []byte(rotatedPassword) })
		}
	}
	observation, err := f.service.ObserveManagement(f.ctx, f.target)
	if !errors.Is(err, application.ErrCredentialsChanged) || observation.Target().Validate() == nil {
		t.Fatal("管理观察期间凭据轮换应丢弃全部能力结果")
	}
	if f.backendIDs(t) != "" {
		t.Fatal("中途轮换后不应保留旧管理连接")
	}
}

// 以实际非 superuser 会话验证能力矩阵的依据，不用超级用户执行 SQL 模拟管理账号。
// 这些固定名称只存在于本测试独占容器，生产观察本身不会创建探针对象。
func TestNativeManagementSupplyContract(t *testing.T) {
	f := newCredentialFixture(t)
	useNativeManager(t, f)
	config, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal("无法装配隔离测试连接")
	}
	config.Host, config.Port = fixtureAddress, uint16(f.port)
	config.Database, config.User, config.Password = fixtureUser, nativeManager, fixturePassword
	config.TLSConfig, config.Fallbacks = nil, nil
	connection, err := pgx.ConnectConfig(f.ctx, config)
	if err != nil {
		t.Fatal("非 superuser 测试连接失败")
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	execute := func(statement string) {
		t.Helper()
		if _, err := connection.Exec(f.ctx, statement); err != nil {
			t.Fatalf("原生管理能力合同未满足，步骤 %q", statement)
		}
	}
	execute("CREATE ROLE managed_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION")
	execute("GRANT managed_owner TO native_manager WITH SET TRUE")
	execute("CREATE DATABASE managed_database OWNER managed_owner")
	execute("SET ROLE managed_owner")
	execute("REVOKE CONNECT ON DATABASE managed_database FROM PUBLIC")
	execute("GRANT CONNECT ON DATABASE managed_database TO managed_owner")
	execute("RESET ROLE")
	config.Database = "managed_database"
	tenantConnection, err := pgx.ConnectConfig(f.ctx, config)
	if err != nil {
		t.Fatal("管理账号无法访问其受管数据库")
	}
	defer func() { _ = tenantConnection.Close(context.Background()) }()
	if _, err := tenantConnection.Exec(f.ctx, "SET ROLE managed_owner; CREATE EXTENSION hstore"); err != nil {
		t.Fatal("owner 无法安装 trusted 扩展")
	}
	var installed bool
	if err := tenantConnection.QueryRow(f.ctx, "SELECT EXISTS (SELECT FROM pg_catalog.pg_extension WHERE extname = 'hstore')").Scan(&installed); err != nil || !installed {
		t.Fatal("扩展安装后实际回读失败")
	}
	if _, err := tenantConnection.Exec(f.ctx, "CREATE EXTENSION file_fdw"); err == nil {
		t.Fatal("非 trusted 扩展不应被 Ready 隐式授权")
	}
	if err := tenantConnection.Close(f.ctx); err != nil {
		t.Fatal("关闭目标数据库连接失败")
	}
	execute("SET ROLE managed_owner")
	execute("DROP DATABASE managed_database")
	execute("RESET ROLE")
	execute("DROP ROLE managed_owner")
}
