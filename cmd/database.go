package main

import (
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	ctrl "sigs.k8s.io/controller-runtime"
)

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
