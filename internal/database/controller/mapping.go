package controller

import (
	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CR 与领域对象之间的映射；Reconciler 是唯一的 Kubernetes adapter。
const (
	TenantFinalizer   = "database.ayatori.ddupan.top/tenant-protection"
	DatabaseFinalizer = "database.ayatori.ddupan.top/database-protection"
	InstanceFinalizer = "database.ayatori.ddupan.top/instance-protection"
)

// currentReady 只承认当前 generation 的 Ready=True 观察。
func currentReady(generation int64, conditions []metav1.Condition) bool {
	condition := meta.FindStatusCondition(conditions, readyCondition)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}

// bindingTenant 把 Tenant CR 映射为绑定规则所需事实。
func bindingTenant(object *databasev1alpha1.PostgreSQLTenant) binding.Tenant {
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

// bindingDatabase 把 Database CR 映射为绑定规则所需事实。
func bindingDatabase(object *databasev1alpha1.PostgreSQLDatabase) binding.Database {
	database := binding.Database{
		Identity: binding.Identity{Name: object.Name, UID: string(object.UID)},
		Instance: string(object.Spec.InstanceRef.Name), InstanceUID: string(object.Status.InstanceUID),
		Name: string(object.Spec.Database), LoginRole: string(object.Spec.LoginRole), Source: object.Spec.Source,
		Phase: object.Status.Phase, Deleting: !object.DeletionTimestamp.IsZero(),
		Ready: currentReady(object.Generation, object.Status.Conditions),
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

// instanceTarget 把 Instance CR 映射为观察与管理连接目标。
func instanceTarget(object *databasev1alpha1.PostgreSQLInstance) (instance.ObservationTarget, error) {
	identity, err := instance.NewIdentity(string(object.UID), object.Name)
	if err != nil {
		return instance.ObservationTarget{}, err
	}
	revision, err := instance.NewRevision(object.Generation)
	if err != nil {
		return instance.ObservationTarget{}, err
	}
	spec := object.Spec
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host: spec.Endpoint.Host, HostAddr: spec.Endpoint.HostAddr,
		Port: int(spec.Endpoint.Port), ManagementDatabase: string(spec.Endpoint.Database),
		TLSMode: instance.TLSMode(spec.Endpoint.SSLMode),
	})
	if err != nil {
		return instance.ObservationTarget{}, err
	}
	credential, err := instance.NewCredentialReference(instance.CredentialReferenceValues{
		Name:        string(spec.AdminCredentialRef.Name),
		UsernameKey: spec.AdminCredentialRef.UsernameKey, PasswordKey: spec.AdminCredentialRef.PasswordKey,
	})
	if err != nil {
		return instance.ObservationTarget{}, err
	}
	definition, err := instance.NewDefinition(endpoint, credential)
	if err != nil {
		return instance.ObservationTarget{}, err
	}
	return instance.NewObservationTarget(identity, revision, definition)
}
