package kubernetes

import (
	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"k8s.io/apimachinery/pkg/types"
)

func bindingTenant(object *databasev1alpha1.PostgreSQLTenant) *application.BindingTenant {
	return &application.BindingTenant{Tenant: BindingTenant(object), Revision: object.ResourceVersion, Generation: object.Generation}
}

// BindingTenant 把 Tenant CR 映射为绑定规则所需事实。
func BindingTenant(object *databasev1alpha1.PostgreSQLTenant) binding.Tenant {
	tenant := binding.Tenant{
		Identity: binding.TenantIdentity{Namespace: object.Namespace, Name: object.Name, UID: string(object.UID)},
		Phase:    object.Status.Phase, Deleting: !object.DeletionTimestamp.IsZero(),
	}
	if request := object.Spec.Provision; request != nil {
		tenant.Request.Provision = &binding.ProvisionRequest{
			Instance: string(request.InstanceRef.Name), Database: string(request.Database), LoginRole: string(request.LoginRole),
		}
	}
	if object.Spec.DatabaseRef != nil {
		tenant.Request.ExistingDatabase = string(object.Spec.DatabaseRef.Name)
	}
	if ref := object.Status.DatabaseRef; ref != nil {
		tenant.Database = &binding.Identity{Name: string(ref.Name), UID: string(ref.UID)}
	}
	return tenant
}

func bindingDatabase(object *databasev1alpha1.PostgreSQLDatabase) *application.BindingDatabase {
	return &application.BindingDatabase{Database: BindingDatabase(object), Revision: object.ResourceVersion}
}

// BindingDatabase 把 Database CR 映射为绑定规则所需事实。
func BindingDatabase(object *databasev1alpha1.PostgreSQLDatabase) binding.Database {
	database := binding.Database{
		Identity: binding.Identity{Name: object.Name, UID: string(object.UID)},
		Instance: string(object.Spec.InstanceRef.Name), InstanceUID: string(object.Status.InstanceUID),
		Name: string(object.Spec.Database), LoginRole: string(object.Spec.LoginRole), Source: object.Spec.Source,
		Phase: object.Status.Phase, Deleting: !object.DeletionTimestamp.IsZero(),
		Ready: CurrentReady(object.Generation, object.Status.Conditions),
	}
	if ref := object.Spec.TenantRef; ref != nil {
		database.Tenant = &binding.TenantIdentity{Namespace: ref.Namespace, Name: string(ref.Name), UID: string(ref.UID)}
	}
	return database
}

func tenantReference(tenant binding.TenantIdentity) *databasev1alpha1.TenantReference {
	return &databasev1alpha1.TenantReference{
		Namespace: tenant.Namespace, Name: databasev1alpha1.ObjectName(tenant.Name), UID: types.UID(tenant.UID),
	}
}
