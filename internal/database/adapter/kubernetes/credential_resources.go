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

// CredentialResources 只映射资源事实与状态写入，供应资格和执行顺序由用例决定。
type CredentialResources struct {
	Client client.Client
	Reader client.Reader
}

var _ application.CredentialResources = (*CredentialResources)(nil)

func (r *CredentialResources) Load(ctx context.Context, name string) (*application.CredentialRecord, error) {
	database := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, database); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	record := &application.CredentialRecord{
		Database: *bindingDatabase(database), DatabaseProtected: controllerutil.ContainsFinalizer(database, DatabaseFinalizer),
		Status: credentialStatus(database),
	}
	if database.Spec.Source != "Provision" {
		return record, nil
	}
	if ref := database.Spec.TenantRef; ref != nil {
		tenant := &databasev1alpha1.PostgreSQLTenant{}
		err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: string(ref.Name)}, tenant)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err == nil {
			record.Tenant = bindingTenant(tenant)
			record.TenantProtected = controllerutil.ContainsFinalizer(tenant, TenantFinalizer)
		}
	}
	instance := &databasev1alpha1.PostgreSQLInstance{}
	err := r.Reader.Get(ctx, types.NamespacedName{Name: string(database.Spec.InstanceRef.Name)}, instance)
	if err != nil {
		return record, client.IgnoreNotFound(err)
	}
	observed, err := instanceRecord(instance)
	if err != nil {
		return nil, err
	}
	record.Instance = &application.CredentialInstance{
		Identity: binding.Identity{Name: instance.Name, UID: string(instance.UID)},
		Deleting: !instance.DeletionTimestamp.IsZero(), Ready: currentReady(instance.Generation, instance.Status.Conditions),
		Generation: instance.Generation, Endpoint: observed.Target.Definition().Endpoint(),
	}
	return record, nil
}

func credentialStatus(database *databasev1alpha1.PostgreSQLDatabase) application.CredentialStatus {
	status := application.CredentialStatus{Version: database.Status.CredentialVersion}
	if ref := database.Status.CredentialRef; ref != nil {
		status.Location = &application.CredentialLocation{Mount: ref.Mount, Path: ref.Path}
	}
	if condition := meta.FindStatusCondition(database.Status.Conditions, application.CredentialsReady); condition != nil {
		status.Ready = condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == database.Generation
		status.Reason, status.Message = condition.Reason, condition.Message
	}
	return status
}

func (r *CredentialResources) Save(ctx context.Context, record *application.CredentialRecord, status application.CredentialStatus) (*application.CredentialRecord, error) {
	bindingResources := &BindingResources{Client: r.Client, Reader: r.Reader}
	database, err := bindingResources.databaseAtVersion(ctx, &record.Database)
	if err != nil {
		return nil, err
	}
	previous := database.Status.DeepCopy()
	if status.Location != nil {
		database.Status.CredentialRef = &databasev1alpha1.CredentialReference{Mount: status.Location.Mount, Path: status.Location.Path}
	}
	database.Status.CredentialVersion = status.Version
	conditionStatus := metav1.ConditionFalse
	if status.Ready {
		conditionStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&database.Status.Conditions, metav1.Condition{
		Type: application.CredentialsReady, Status: conditionStatus, Reason: status.Reason, Message: status.Message,
		ObservedGeneration: database.Generation,
	})
	meta.SetStatusCondition(&database.Status.Conditions, metav1.Condition{
		Type: readyCondition, Status: metav1.ConditionFalse, Reason: "ProvisioningIncomplete",
		Message: "凭据准备不代表 PostgreSQL 供应、应用登录或 Tenant 交付已经完成", ObservedGeneration: database.Generation,
	})
	database.Status.ObservedGeneration = database.Generation
	if !equality.Semantic.DeepEqual(*previous, database.Status) {
		if err := r.Client.Status().Update(ctx, database); err != nil {
			return nil, err
		}
	}
	updated := *record
	updated.Database = *bindingDatabase(database)
	updated.Status = credentialStatus(database)
	// 旧 CRD 会裁剪未知 status 字段。不能把 HTTP 成功当作位置/版本已保存后继续写后端。
	if !equality.Semantic.DeepEqual(updated.Status.Location, status.Location) || updated.Status.Version != status.Version {
		return nil, fmt.Errorf("凭据位置或版本未被 API 保留；请先升级 Database CRD，未继续外部操作")
	}
	return &updated, nil
}

func (r *CredentialResources) CheckCurrent(ctx context.Context, record *application.CredentialRecord) error {
	current, err := r.Load(ctx, record.Database.Identity.Name)
	if err != nil {
		return err
	}
	if current == nil || current.Database.Identity != record.Database.Identity || current.Database.Revision != record.Database.Revision ||
		!sameCredentialDependencies(current, record) {
		return apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("postgresqldatabases").GroupResource(), record.Database.Identity.Name,
			fmt.Errorf("凭据准备的资源快照已变化；停止本轮操作并重新观察"))
	}
	return nil
}

func sameCredentialDependencies(current, previous *application.CredentialRecord) bool {
	if current.Tenant == nil || previous.Tenant == nil || current.Instance == nil || previous.Instance == nil {
		return false
	}
	// Tenant Ready 的诊断变化、Instance 对同一 generation 的观测刷新不改变写入目标。
	// 仍检查申请 spec generation、完整绑定身份、删除状态、保护和当前 Instance Ready。
	return current.Tenant.Generation == previous.Tenant.Generation &&
		equality.Semantic.DeepEqual(current.Tenant.Tenant, previous.Tenant.Tenant) &&
		current.TenantProtected == previous.TenantProtected && current.DatabaseProtected == previous.DatabaseProtected &&
		current.Instance.Instance == previous.Instance.Instance && current.Instance.Generation == previous.Instance.Generation &&
		current.Instance.Endpoint == previous.Instance.Endpoint
}
