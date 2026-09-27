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

func wireCredentialController(writer client.Client, reader client.Reader, store application.CredentialStore) *databasecontroller.CredentialReconciler {
	resources := &kubernetes.CredentialResources{Client: writer, Reader: reader}
	service := &application.CredentialPreparation{Resources: resources, Store: store}
	return databasecontroller.NewCredentialReconciler(writer, service)
}

func wireInstanceController(writer client.Client, reader client.Reader, observer application.InstanceObserver, namespace string) *databasecontroller.InstanceReconciler {
	resources := &kubernetes.InstanceResources{Client: writer, Reader: reader}
	service := &application.InstanceReconciliation{Resources: resources, Observer: observer}
	return databasecontroller.NewInstanceReconciler(writer, service, resources, namespace)
}
