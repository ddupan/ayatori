package kubernetes

import (
	"context"
	"fmt"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	TenantFinalizer   = "database.ayatori.ddupan.top/tenant-protection"
	DatabaseFinalizer = "database.ayatori.ddupan.top/database-protection"
	readyCondition    = "Ready"
)

// BindingResources 读取领域所需事实，并把用例结果呈现为 CR、finalizer 与 Conditions。
// 重新读取后校验快照版本，保留不属于本用例的字段；不决定绑定资格或恢复顺序。
type BindingResources struct {
	Client client.Client
	Reader client.Reader
}

var _ application.BindingResources = (*BindingResources)(nil)

func (r *BindingResources) Tenant(ctx context.Context, namespace, name string) (*application.BindingTenant, error) {
	object := &databasev1alpha1.PostgreSQLTenant{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return bindingTenant(object), nil
}

func (r *BindingResources) Database(ctx context.Context, name string) (*application.BindingDatabase, error) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return bindingDatabase(object), nil
}

func (r *BindingResources) Instance(ctx context.Context, name string) (*binding.Instance, error) {
	object := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return &binding.Instance{
		Identity: binding.Identity{Name: object.Name, UID: string(object.UID)},
		Deleting: !object.DeletionTimestamp.IsZero(), Ready: currentReady(object.Generation, object.Status.Conditions),
	}, nil
}

func (r *BindingResources) BeginBinding(ctx context.Context, tenant *application.BindingTenant,
	checkpoint *application.BindingStatus) (*application.BindingTenant, error) {
	object, err := r.tenantAtVersion(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if controllerutil.AddFinalizer(object, TenantFinalizer) {
		if err := r.Client.Update(ctx, object); err != nil {
			return nil, err
		}
	}
	if checkpoint != nil {
		if err := r.presentStatus(ctx, object, *checkpoint); err != nil {
			return nil, err
		}
	}
	return bindingTenant(object), nil
}

func (r *BindingResources) CreateDatabase(ctx context.Context, target binding.Target,
	tenant binding.TenantIdentity) (*application.BindingDatabase, error) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	object.Name = target.Name
	object.Spec = databasev1alpha1.PostgreSQLDatabaseSpec{
		InstanceRef: databasev1alpha1.InstanceReference{Name: databasev1alpha1.ObjectName(target.Provision.Instance)},
		Database:    databasev1alpha1.PostgreSQLIdentifier(target.Provision.Database),
		LoginRole:   databasev1alpha1.PostgreSQLIdentifier(target.Provision.LoginRole),
		Source:      "Provision", ReclaimPolicy: databasev1alpha1.ReclaimRetain, TenantRef: tenantReference(tenant),
	}
	controllerutil.AddFinalizer(object, DatabaseFinalizer)
	if err := r.Client.Create(ctx, object); err != nil {
		return nil, err
	}
	return bindingDatabase(object), nil
}

func (r *BindingResources) RecordInstance(ctx context.Context, database *application.BindingDatabase,
	instanceUID string) (*application.BindingDatabase, error) {
	object, err := r.databaseAtVersion(ctx, database)
	if err != nil {
		return nil, err
	}
	object.Status.InstanceUID = types.UID(instanceUID)
	if err := r.Client.Status().Update(ctx, object); err != nil {
		return nil, err
	}
	return bindingDatabase(object), nil
}

func (r *BindingResources) BindDatabase(ctx context.Context, database *application.BindingDatabase,
	tenant binding.TenantIdentity) (*application.BindingDatabase, error) {
	object, err := r.databaseAtVersion(ctx, database)
	if err != nil {
		return nil, err
	}
	wanted := tenantReference(tenant)
	changed := controllerutil.AddFinalizer(object, DatabaseFinalizer)
	if object.Spec.TenantRef == nil || *object.Spec.TenantRef != *wanted {
		object.Spec.TenantRef = wanted
		changed = true
	}
	if changed {
		if err := r.Client.Update(ctx, object); err != nil {
			return nil, err
		}
	}
	return bindingDatabase(object), nil
}

func (r *BindingResources) Present(ctx context.Context, result application.BindingResult) error {
	if result.Tenant == nil {
		return nil
	}
	object, err := r.tenantAtVersion(ctx, result.Tenant)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.presentStatus(ctx, object, result.Status)
}

func (r *BindingResources) presentStatus(ctx context.Context, object *databasev1alpha1.PostgreSQLTenant,
	status application.BindingStatus) error {
	previous := object.Status.DeepCopy()
	object.Status.Phase = status.Phase
	object.Status.ObservedGeneration = object.Generation
	if status.Database != nil {
		object.Status.DatabaseRef = &databasev1alpha1.BoundDatabaseReference{
			Name: databasev1alpha1.ObjectName(status.Database.Name), UID: types.UID(status.Database.UID),
		}
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: readyCondition, Status: metav1.ConditionFalse, Reason: status.Reason, Message: status.Message,
		ObservedGeneration: object.Generation,
	})
	if equality.Semantic.DeepEqual(*previous, object.Status) {
		return nil
	}
	return r.Client.Status().Update(ctx, object)
}

func (r *BindingResources) tenantAtVersion(ctx context.Context, tenant *application.BindingTenant) (*databasev1alpha1.PostgreSQLTenant, error) {
	object := &databasev1alpha1.PostgreSQLTenant{}
	key := types.NamespacedName{Namespace: tenant.Identity.Namespace, Name: tenant.Identity.Name}
	if err := r.Reader.Get(ctx, key, object); err != nil {
		return nil, err
	}
	if string(object.UID) != tenant.Identity.UID || object.ResourceVersion != tenant.Revision {
		return nil, bindingVersionConflict("postgresqltenants", object.Name)
	}
	return object, nil
}

func (r *BindingResources) databaseAtVersion(ctx context.Context, database *application.BindingDatabase) (*databasev1alpha1.PostgreSQLDatabase, error) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: database.Identity.Name}, object); err != nil {
		return nil, err
	}
	if string(object.UID) != database.Identity.UID || object.ResourceVersion != database.Revision {
		return nil, bindingVersionConflict("postgresqldatabases", object.Name)
	}
	return object, nil
}

func bindingVersionConflict(resource, name string) error {
	return apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource(resource).GroupResource(), name,
		fmt.Errorf("绑定快照已过期，请重新读取后判断"))
}

func currentReady(generation int64, conditions []metav1.Condition) bool {
	condition := meta.FindStatusCondition(conditions, readyCondition)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}
