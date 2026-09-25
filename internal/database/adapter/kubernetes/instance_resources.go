package kubernetes

import (
	"context"
	"errors"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const InstanceFinalizer = "database.ayatori.ddupan.top/instance-protection"

type InstanceResources struct {
	Client client.Client
	Reader client.Reader
}

func (r *InstanceResources) LoadInstance(ctx context.Context, name string) (*application.InstanceRecord, error) {
	object := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return instanceRecord(object)
}

func (r *InstanceResources) ProtectInstance(ctx context.Context, record *application.InstanceRecord) (*application.InstanceRecord, error) {
	object, err := r.instanceAtVersion(ctx, record)
	if err != nil {
		return nil, err
	}
	if controllerutil.AddFinalizer(object, InstanceFinalizer) {
		if err := r.Client.Update(ctx, object); err != nil {
			return nil, err
		}
	}
	return instanceRecord(object)
}

func (r *InstanceResources) InstanceReferences(ctx context.Context, name string) (string, error) {
	// 删除判断必须直读 API；包含 Released、删除中的 Database 和尚未绑定的申请。
	// 不按旧 Instance UID 忽略引用，也不依赖 informer 索引的及时性。
	databases := &databasev1alpha1.PostgreSQLDatabaseList{}
	if err := r.Reader.List(ctx, databases); err != nil {
		return "", err
	}
	for _, database := range databases.Items {
		if string(database.Spec.InstanceRef.Name) == name {
			return "Database/" + database.Name, nil
		}
	}
	tenants := &databasev1alpha1.PostgreSQLTenantList{}
	if err := r.Reader.List(ctx, tenants); err != nil {
		return "", err
	}
	for _, tenant := range tenants.Items {
		if tenant.Spec.Provision != nil && string(tenant.Spec.Provision.InstanceRef.Name) == name {
			return "Tenant/" + tenant.Namespace + "/" + tenant.Name, nil
		}
	}
	return "", nil
}

func (r *InstanceResources) PresentInstance(ctx context.Context, result application.InstanceResult) error {
	if result.Record == nil {
		return nil
	}
	object, err := r.instanceAtVersion(ctx, result.Record)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	previous := object.Status.DeepCopy()
	object.Status.Phase = string(result.Snapshot.Phase)
	object.Status.ObservedGeneration = object.Generation
	object.Status.PostgreSQLVersion = result.Snapshot.ReportedVersion
	ready := metav1.ConditionFalse
	if result.Snapshot.Readiness == instance.Ready {
		ready = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: ready, ObservedGeneration: object.Generation,
		Reason: result.Reason, Message: result.Message,
	})
	if !equality.Semantic.DeepEqual(*previous, object.Status) {
		if err := r.Client.Status().Update(ctx, object); err != nil {
			return err
		}
	}
	if result.RemoveProtection && controllerutil.RemoveFinalizer(object, InstanceFinalizer) {
		return r.Client.Update(ctx, object)
	}
	return nil
}

func (r *InstanceResources) instanceAtVersion(ctx context.Context, record *application.InstanceRecord) (*databasev1alpha1.PostgreSQLInstance, error) {
	object := &databasev1alpha1.PostgreSQLInstance{}
	name := record.Target.Identity().Name()
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
		return nil, err
	}
	if string(object.UID) != record.Target.Identity().UID() || object.ResourceVersion != record.Revision {
		return nil, apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("postgresqlinstances").GroupResource(),
			name, errors.New("Instance 快照已过期，请重新观察"))
	}
	return object, nil
}
