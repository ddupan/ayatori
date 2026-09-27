package bootstrap

import (
	"context"
	"flag"
	"fmt"
	"os"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	ctrl "sigs.k8s.io/controller-runtime"
)

type databaseOptions struct {
	secretNamespace string
	rootCert        string
}

func (o *databaseOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.secretNamespace, "database-secret-namespace", os.Getenv("POD_NAMESPACE"),
		"固定管理 Secret namespace；为空时不启用 Instance 观测")
	flags.StringVar(&o.rootCert, "database-root-cert", "", "PostgreSQL 管理连接信任的公开 CA bundle 路径")
}

func (o databaseOptions) configureManager(options *ctrl.Options) {
	if o.secretNamespace != "" {
		options.Cache = databasecontroller.InstanceCacheOptions(o.secretNamespace)
	}
}

// setupDatabase 封装 Database 的内部装配，并返回在 manager 停止后执行的清理。
func setupDatabase(ctx context.Context, manager ctrl.Manager, options databaseOptions) (func(), error) {
	cleanup := func() {}
	if options.secretNamespace != "" {
		service, err := setupInstanceObservation(manager, options.secretNamespace, options.rootCert)
		if err != nil {
			return nil, fmt.Errorf("set up Instance observation: %w", err)
		}
		cleanup = service.Close
	}
	if err := (&databasecontroller.BindingReconciler{}).SetupWithManager(ctx, manager); err != nil {
		cleanup()
		return nil, fmt.Errorf("set up Database binding controller: %w", err)
	}
	return cleanup, nil
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
	reconciler := &databasecontroller.InstanceReconciler{Observer: service, SecretNamespace: namespace}
	if err := reconciler.SetupWithManager(manager); err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}
