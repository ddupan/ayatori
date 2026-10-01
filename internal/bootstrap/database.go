package bootstrap

import (
	"context"
	"flag"
	"fmt"
	"os"

	bao "github.com/openbao/openbao/api/v2"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	databasebao "git.ddupan.top/panxiao81/ayatori/internal/database/adapter/openbao"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	ctrl "sigs.k8s.io/controller-runtime"
)

type databaseOptions struct {
	secretNamespace    string
	rootCert           string
	credentialMount    string
	credentialPrefix   string
	provisionResources bool
}

func (o *databaseOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.secretNamespace, "database-secret-namespace", os.Getenv("POD_NAMESPACE"),
		"固定管理 Secret namespace；为空时不启用 Instance 观测")
	flags.StringVar(&o.rootCert, "database-root-cert", "", "PostgreSQL 管理连接信任的公开 CA bundle 路径")
	flags.StringVar(&o.credentialMount, "database-credential-mount", "", "应用凭据 KV v2 mount；为空时不启用凭据准备")
	flags.StringVar(&o.credentialPrefix, "database-credential-prefix", "applications", "应用凭据路径前缀；已有固定位置不随配置变化迁移")
	flags.BoolVar(&o.provisionResources, "database-provision-resources", false, "启用 PostgreSQL 角色和数据库创建；需要 Instance 管理凭据及 OpenBao 凭据准备")
}

func (o databaseOptions) configureManager(options *ctrl.Options) {
	if o.secretNamespace != "" {
		options.Cache = databasecontroller.InstanceCacheOptions(o.secretNamespace)
	}
}

// registerDatabaseControllers 封装 Database 的内部装配，并返回在 manager 停止后执行的清理。
func registerDatabaseControllers(ctx context.Context, manager ctrl.Manager, options databaseOptions, baoClient *bao.Client) (func(), error) {
	closeDatabaseConnections := func() {}
	var backend *application.InstanceService
	if options.secretNamespace != "" {
		service, err := setupInstanceObservation(manager, options.secretNamespace, options.rootCert)
		if err != nil {
			return nil, fmt.Errorf("set up Instance observation: %w", err)
		}
		closeDatabaseConnections = service.Close
		backend = service
	}
	if err := wireBindingController(manager.GetClient(), manager.GetAPIReader()).SetupWithManager(ctx, manager); err != nil {
		closeDatabaseConnections()
		return nil, fmt.Errorf("set up Database binding controller: %w", err)
	}
	if err := registerDatabaseSupplyController(manager, options, baoClient, backend); err != nil {
		closeDatabaseConnections()
		return nil, fmt.Errorf("register Database supply controller: %w", err)
	}
	return closeDatabaseConnections, nil
}

func registerDatabaseSupplyController(manager ctrl.Manager, options databaseOptions, baoClient *bao.Client, backend *application.InstanceService) error {
	if options.provisionResources && (backend == nil || options.credentialMount == "") {
		return fmt.Errorf("database resource provisioning requires management Secret namespace and credential mount")
	}
	if options.credentialMount == "" {
		return nil
	}
	if baoClient == nil {
		return fmt.Errorf("database credential preparation requires OpenBao authentication configuration")
	}
	store, err := databasebao.NewCredentials(baoClient, options.credentialMount, options.credentialPrefix)
	if err != nil {
		return err
	}
	var postgres databasecontroller.PostgreSQLBackend
	if options.provisionResources {
		postgres = backend
	}
	return wireDatabaseController(manager.GetClient(), manager.GetAPIReader(), store, postgres).SetupWithManager(manager)
}

func setupInstanceObservation(manager ctrl.Manager, namespace, rootCert string) (*application.InstanceService, error) {
	credentials, err := kubernetes.NewSecretCredentials(manager.GetAPIReader(), namespace)
	if err != nil {
		return nil, err
	}
	service, err := application.NewInstanceService(credentials, postgresql.Connector{RootCert: rootCert})
	if err != nil {
		return nil, err
	}
	reconciler := wireInstanceController(manager.GetClient(), manager.GetAPIReader(), service, namespace)
	if err := reconciler.SetupWithManager(manager); err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}
