package kubernetes

import (
	"context"
	"fmt"
	"reflect"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/provisioning"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const resourcesReadyCondition = "ResourcesReady"

type ProvisioningResources struct {
	Client client.Client
	Reader client.Reader
}

func (r *ProvisioningResources) Load(ctx context.Context, name string) (*application.ProvisioningRecord, error) {
	// 同一组 API 事实复用映射；供应用例不持有凭据准备用例的快照或 repository。
	facts, err := (&CredentialResources{Client: r.Client, Reader: r.Reader}).Load(ctx, name)
	if err != nil || facts == nil {
		return nil, err
	}
	object := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, object); err != nil {
		return nil, err
	}
	if object.ResourceVersion != facts.Revision {
		return nil, provisioningSnapshotConflict(name)
	}
	record := &application.ProvisioningRecord{Target: facts.Target, Revision: facts.Revision, TenantGeneration: facts.TenantGeneration,
		InstanceGeneration: facts.InstanceGeneration, Credentials: facts.Status, State: provisioningStatus(object)}
	if facts.Instance == nil {
		return record, nil
	}
	instance := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: facts.Database.Instance}, instance); err != nil {
		return nil, err
	}
	if string(instance.UID) != facts.Instance.Identity.UID || instance.Generation != facts.InstanceGeneration ||
		(!instance.DeletionTimestamp.IsZero()) != facts.Instance.Deleting || currentReady(instance.Generation, instance.Status.Conditions) != facts.Instance.Ready {
		return nil, provisioningSnapshotConflict(name)
	}
	mapped, err := instanceRecord(instance)
	if err != nil {
		return nil, err
	}
	record.InstanceTarget = mapped.Target
	return record, nil
}

func provisioningStatus(object *databasev1alpha1.PostgreSQLDatabase) provisioning.State {
	state := provisioning.State{RoleOID: uint32(object.Status.RoleOID), DatabaseOID: uint32(object.Status.DatabaseOID), Phase: provisioning.Pending}
	if condition := meta.FindStatusCondition(object.Status.Conditions, resourcesReadyCondition); condition != nil {
		state.Phase, state.Message = provisioning.Phase(condition.Reason), condition.Message
	}
	return state
}

func (r *ProvisioningResources) Save(ctx context.Context, record *application.ProvisioningRecord, state provisioning.State) (*application.ProvisioningRecord, error) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: record.Database.Identity.Name}, object); err != nil {
		return nil, err
	}
	if object.ResourceVersion != record.Revision || string(object.UID) != record.Database.Identity.UID {
		return nil, provisioningSnapshotConflict(object.Name)
	}
	previous := object.Status.DeepCopy()
	object.Status.RoleOID, object.Status.DatabaseOID = int64(state.RoleOID), int64(state.DatabaseOID)
	status := metav1.ConditionFalse
	if state.Phase == provisioning.Available {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{Type: resourcesReadyCondition, Status: status,
		Reason: string(state.Phase), Message: state.Message, ObservedGeneration: object.Generation})
	if !equality.Semantic.DeepEqual(*previous, object.Status) {
		if err := r.Client.Status().Update(ctx, object); err != nil {
			return nil, err
		}
	}
	if object.Status.RoleOID != int64(state.RoleOID) || object.Status.DatabaseOID != int64(state.DatabaseOID) {
		return nil, fmt.Errorf("资源 OID 未被 API 保留；请先升级 Database CRD，未继续供应")
	}
	updated := *record
	updated.Revision, updated.State = object.ResourceVersion, provisioningStatus(object)
	return &updated, nil
}

func (r *ProvisioningResources) CheckCurrent(ctx context.Context, record *application.ProvisioningRecord) error {
	current, err := r.Load(ctx, record.Database.Identity.Name)
	if err != nil {
		return err
	}
	if current == nil || !reflect.DeepEqual(current, record) {
		return provisioningSnapshotConflict(record.Database.Identity.Name)
	}
	return nil
}

func provisioningSnapshotConflict(name string) error {
	return apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("postgresqldatabases").GroupResource(), name,
		fmt.Errorf("供应快照发生变化；停止本轮并重新观察"))
}
