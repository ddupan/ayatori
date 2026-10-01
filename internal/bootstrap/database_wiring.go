package bootstrap

import (
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// 本文件是 Database 的显式依赖图：adapter → 用例 → controller。
// 构造只在启动时执行；用例和 controller 都不持有容器或动态查找依赖。
func wireBindingController(writer client.Client, reader client.Reader) *databasecontroller.BindingReconciler {
	resources := &kubernetes.BindingResources{Client: writer, Reader: reader}
	service := &application.BindingService{Resources: resources}
	return databasecontroller.NewBindingReconciler(writer, service, resources)
}

// wireInstanceController 中的 reader 必须直连 API server：Secret 内容不进入共享 cache。
func wireInstanceController(writer client.Client, reader client.Reader, instances databasecontroller.InstanceBackend, namespace string) *databasecontroller.InstanceReconciler {
	return &databasecontroller.InstanceReconciler{Client: writer, Reader: reader, PostgreSQL: instances,
		Secrets: databasecontroller.ManagementSecrets{Reader: reader, Namespace: namespace}}
}

// wireDatabaseController 装配 Database 的唯一 status 写入者；postgres 为 nil 时只准备凭据。
func wireDatabaseController(writer client.Client, reader client.Reader, credentials databasecontroller.CredentialBackend,
	postgres databasecontroller.PostgreSQLBackend, namespace string) *databasecontroller.DatabaseReconciler {
	reconciler := &databasecontroller.DatabaseReconciler{Client: writer, Reader: reader, Credentials: credentials}
	if postgres != nil {
		reconciler.PostgreSQL = postgres
		reconciler.Secrets = databasecontroller.ManagementSecrets{Reader: reader, Namespace: namespace}
	}
	return reconciler
}
