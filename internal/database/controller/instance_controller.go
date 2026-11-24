package controller

import (
	"context"
	"errors"
	"time"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	"go.ddupan.top/ayatori/internal/database/domain/instance"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// InstanceBackend 观察管理账号能力，并在 Instance 消失或删除时释放本地连接。
type InstanceBackend interface {
	InspectManagement(context.Context, instance.ObservationTarget, instance.ManagementCredentials) (instance.ManagementFacts, error)
	Forget(name string)
}

// InstanceReconciler 每轮从无证据的 Instance 聚合开始验证管理能力，并呈现为 status。
// 持久化 Ready 和连接存活不能替代本轮检查；删除时只检查引用、移除登记保护。
type InstanceReconciler struct {
	Client     client.Client
	Reader     client.Reader
	Secrets    ManagementSecrets
	PostgreSQL InstanceBackend
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances/finalizers,verbs=update
// Secret 权限单独声明为 namespace Role，不放入生成的 ClusterRole。

func (r *InstanceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, request.NamespacedName, object); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.PostgreSQL.Forget(request.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	target, err := instanceTarget(object)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !object.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.deleting(ctx, object, target)
	}
	if controllerutil.AddFinalizer(object, InstanceFinalizer) {
		if err := r.Client.Update(ctx, object); err != nil {
			return ctrl.Result{}, err
		}
	}
	aggregate, err := instance.Reconstitute(target, instance.Snapshot{}, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := aggregate.BeginValidation(); err != nil {
		return ctrl.Result{}, err
	}
	// 查询超时后仍用 worker context 保存安全失败结果；manager 停止时不强行写入。
	observeContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snapshot, reason, message, err := r.observe(observeContext, target, aggregate)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.present(ctx, object, snapshot, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

func (r *InstanceReconciler) observe(ctx context.Context, target instance.ObservationTarget, aggregate *instance.Instance) (instance.Snapshot, string, string, error) {
	credentials, err := r.Secrets.Read(ctx, target.Definition().AdminCredential())
	var facts instance.ManagementFacts
	if err == nil {
		facts, err = r.PostgreSQL.InspectManagement(ctx, target, credentials)
	} else {
		r.PostgreSQL.Forget(target.Identity().Name())
	}
	if err != nil {
		snapshot := aggregate.Snapshot()
		snapshot.Readiness, snapshot.ObservedRevision = instance.NotReady, target.Revision().Value()
		reason, message := observationFailure(err)
		return snapshot, reason, message, nil
	}
	observation, err := instance.NewCapabilityObservation(target, facts.Version, facts.Checks())
	if err != nil {
		return instance.Snapshot{}, "", "", err
	}
	if err := aggregate.AssessManagement(observation); err != nil {
		return instance.Snapshot{}, "", "", err
	}
	snapshot := aggregate.Snapshot()
	snapshot.ReportedVersion = facts.Version
	reason, message := managementResult(snapshot.Failure)
	return snapshot, reason, message, nil
}

func (r *InstanceReconciler) deleting(ctx context.Context, object *databasev1alpha1.PostgreSQLInstance, target instance.ObservationTarget) error {
	r.PostgreSQL.Forget(object.Name)
	aggregate, err := instance.Reconstitute(target, instance.Snapshot{}, true)
	if err != nil {
		return err
	}
	if err := aggregate.BeginDeletion(); err != nil {
		return err
	}
	reference, referenceErr := r.reference(ctx, object.Name)
	reason, message := "Deleting", "引用已解除，仅移除登记保护；不删除 PostgreSQL 或凭据"
	switch {
	case referenceErr != nil:
		reason, message = instanceDependencyUnavailable, "无法确认 Database/Tenant 引用已解除；保留 Instance 删除保护并重试"
	case reference != "":
		reason, message = "InstanceInUse", "仍被 "+reference+" 引用；先处理该资源，不会级联删除外部数据库"
	}
	if err := r.present(ctx, object, aggregate.Snapshot(), reason, message); err != nil {
		return err
	}
	if referenceErr == nil && reference == "" && controllerutil.RemoveFinalizer(object, InstanceFinalizer) {
		return client.IgnoreNotFound(r.Client.Update(ctx, object))
	}
	return nil
}

// reference 直读 API 返回一个可定位的阻塞引用；包含 Released、删除中的 Database 和尚未绑定的申请。
// 不按旧 Instance UID 忽略引用，也不依赖 informer 索引的及时性。
func (r *InstanceReconciler) reference(ctx context.Context, name string) (string, error) {
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

// present 用本轮读取的 resourceVersion 写 status；在途 API 修改由冲突拒绝，下一轮重新观察。
func (r *InstanceReconciler) present(ctx context.Context, object *databasev1alpha1.PostgreSQLInstance, snapshot instance.Snapshot, reason, message string) error {
	previous := object.Status.DeepCopy()
	object.Status.Phase = string(snapshot.Phase)
	object.Status.ObservedGeneration = object.Generation
	object.Status.PostgreSQLVersion = snapshot.ReportedVersion
	ready := metav1.ConditionFalse
	if snapshot.Readiness == instance.Ready {
		ready = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: readyCondition, Status: ready, ObservedGeneration: object.Generation, Reason: reason, Message: message,
	})
	if equality.Semantic.DeepEqual(*previous, object.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Client.Status().Update(ctx, object))
}

const (
	instanceDependencyUnavailable = "DependencyUnavailable"
	instanceAuthenticationFailed  = "AuthenticationFailed"
)

func observationFailure(err error) (string, string) {
	switch {
	case errors.Is(err, instance.ErrAuthentication):
		return instanceAuthenticationFailed, "管理连接认证或 TLS 校验失败；检查管理 Secret 和 CA/证书配置"
	case errors.Is(err, instance.ErrCredentialsInvalid):
		return "InvalidCredentials", "管理 Secret 的用户名或密码字段缺失；检查引用字段映射"
	case errors.Is(err, ErrCredentialsUnavailable):
		return instanceDependencyUnavailable, "无法读取管理 Secret；检查其是否存在及 controller namespace 内的读取权限"
	default:
		return instanceDependencyUnavailable, "管理连接或能力查询失败；检查 PostgreSQL 可达性、catalog 读取权限和超时"
	}
}

func managementResult(failure instance.Failure) (string, string) {
	switch failure {
	case instance.NoFailure:
		return "ManagementReady", "当前管理能力检查通过；具体资源授权和扩展安装仍需执行时验证"
	case instance.InsufficientPrivileges:
		return "InsufficientPrivileges", "原生管理要求非 superuser 且具备 CREATEDB/CREATEROLE；不会自动修改账号权限"
	case instance.DependencyUnavailable:
		return instanceDependencyUnavailable, "当前 PostgreSQL 不可写或所需管理能力暂不可用"
	case instance.AuthenticationFailed:
		return instanceAuthenticationFailed, "当前管理能力检查未通过认证"
	default:
		return "ObservationIncomplete", "管理能力检查尚有缺项，不能仅凭 metadata 查询成功标记 Ready"
	}
}
