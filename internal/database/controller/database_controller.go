package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	credentialsReadyCondition = "CredentialsReady"
	resourcesReadyCondition   = "ResourcesReady"
	readyCondition            = "Ready"
)

// CredentialBackend 只表达凭据准备需要的操作，不提供覆盖或删除。
// version=0 的读取只用于观察位置是否为空，成功不能作为认领依据。
type CredentialBackend interface {
	ProvisionLocation(databaseUID string) (credential.Location, error)
	ReadCredential(ctx context.Context, location credential.Location, version int64) (credential.ApplicationCredential, error)
	CreateCredential(ctx context.Context, location credential.Location, value credential.ApplicationCredential) (int64, error)
}

// PostgreSQLBackend 只返回 catalog 事实和 database 包定义的结果分类。
type PostgreSQLBackend interface {
	InspectResources(context.Context, instance.ObservationTarget, *database.Database) (database.ResourceFacts, error)
	CreateDatabase(context.Context, instance.ObservationTarget, *database.Database) (uint32, error)
	RevokePublicConnect(context.Context, instance.ObservationTarget, *database.Database) error
	CreateLoginRole(context.Context, instance.ObservationTarget, *database.Database, credential.ApplicationCredential) (uint32, error)
	TransferOwnership(context.Context, instance.ObservationTarget, *database.Database) error
}

// DatabaseReconciler 是 PostgreSQLDatabase 的唯一 status 写入者：先准备凭据，
// PostgreSQL 为 nil 时只做凭据准备。规则由 Database 聚合判断，这里只读写 API 和执行外部操作。
type DatabaseReconciler struct {
	Client      client.Client
	Reader      client.Reader
	Credentials CredentialBackend
	PostgreSQL  PostgreSQLBackend
}

// databaseState 是本轮从 API 直接读取的事实。
type databaseState struct {
	object       *databasev1alpha1.PostgreSQLDatabase
	dependencies database.Dependencies
	target       instance.ObservationTarget
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	state, err := r.load(ctx, request.Name)
	if err != nil || state == nil {
		return ctrl.Result{}, err
	}
	db := database.Reconstitute(snapshot(state.object), state.dependencies)
	if !db.RequiresProvisioning() {
		return ctrl.Result{}, nil
	}
	if err := r.reconcile(ctx, state, db); err != nil {
		return ctrl.Result{}, err
	}
	// Bao 与 PostgreSQL 的变化没有 Kubernetes watch；状态写入本身会触发下一轮。
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

// reconcile 每轮最多执行一次外部写入，再记录结果。不使用持久检查点：结果丢失时，
// 下一轮会观察到未记录的同名对象或凭据而转为冲突；controller-runtime 对同一对象串行协调。
func (r *DatabaseReconciler) reconcile(ctx context.Context, state *databaseState, db *database.Database) error {
	location, err := r.Credentials.ProvisionLocation(db.UID())
	var value credential.ApplicationCredential
	switch db.PlanCredential(location, err) {
	case database.Save:
		return r.save(ctx, state, db)
	case database.CreateCredential:
		_, existing := r.Credentials.ReadCredential(ctx, db.CredentialLocation(), 0)
		if generated, ok := db.GenerateCredential(existing); ok {
			version, err := r.Credentials.CreateCredential(ctx, db.CredentialLocation(), generated)
			db.CredentialCreated(version, err)
		}
		return r.save(ctx, state, db)
	case database.VerifyCredential:
		value, err = r.Credentials.ReadCredential(ctx, db.CredentialLocation(), db.CredentialVersion())
		db.VerifyCredential(value, err)
	}
	if r.PostgreSQL == nil || db.PlanResources() == database.Wait {
		return r.save(ctx, state, db)
	}
	facts, err := r.PostgreSQL.InspectResources(ctx, state.target, db)
	switch db.ObserveResources(facts, err) {
	case database.CreateDatabase:
		db.DatabaseCreated(r.PostgreSQL.CreateDatabase(ctx, state.target, db))
	case database.RevokePublicConnect:
		db.Converged("撤销 PUBLIC CONNECT", r.PostgreSQL.RevokePublicConnect(ctx, state.target, db))
	case database.CreateRole:
		// PlanResources 只在本轮凭据验证成功后放行，value 此时一定是已确认凭据。
		db.RoleCreated(r.PostgreSQL.CreateLoginRole(ctx, state.target, db, value))
	case database.TransferOwnership:
		db.Converged("移交数据库 owner", r.PostgreSQL.TransferOwnership(ctx, state.target, db))
	}
	return r.save(ctx, state, db)
}

func (r *DatabaseReconciler) load(ctx context.Context, name string) (*databaseState, error) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	state := &databaseState{object: object}
	if object.Spec.Source != "Provision" {
		return state, nil
	}
	if ref := object.Spec.TenantRef; ref != nil {
		tenant := &databasev1alpha1.PostgreSQLTenant{}
		err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: string(ref.Name)}, tenant)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err == nil {
			facts := kubernetes.BindingTenant(tenant)
			state.dependencies.Tenant = &facts
			state.dependencies.TenantProtected = controllerutil.ContainsFinalizer(tenant, kubernetes.TenantFinalizer)
		}
	}
	owner := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: string(object.Spec.InstanceRef.Name)}, owner); err != nil {
		return state, client.IgnoreNotFound(err)
	}
	target, err := kubernetes.InstanceTarget(owner)
	if err != nil {
		return nil, err
	}
	state.target = target
	state.dependencies.Instance = &database.Instance{
		Identity: binding.Identity{Name: owner.Name, UID: string(owner.UID)},
		Deleting: !owner.DeletionTimestamp.IsZero(), Ready: kubernetes.CurrentReady(owner.Generation, owner.Status.Conditions),
		Endpoint: target.Definition().Endpoint(),
	}
	return state, nil
}

// save 记录本轮结果。OID 与凭据版本是已经发生的外部事实，resourceVersion 冲突时
// 重新读取后再写，而不是丢弃；controller 是这些 status 字段的唯一写入者。
func (r *DatabaseReconciler) save(ctx context.Context, state *databaseState, db *database.Database) error {
	desired := db.Snapshot()
	object := state.object
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		previous := object.Status.DeepCopy()
		presentCredential(object, desired.Credential)
		if r.PostgreSQL != nil {
			presentResources(object, desired.Resources)
		}
		meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
			Type: readyCondition, Status: metav1.ConditionFalse, Reason: "ProvisioningIncomplete",
			Message: "凭据准备与角色建库不代表扩展、应用登录或 Tenant 交付已经完成", ObservedGeneration: object.Generation,
		})
		object.Status.ObservedGeneration = object.Generation
		if equality.Semantic.DeepEqual(*previous, object.Status) {
			return nil
		}
		if err := r.Client.Status().Update(ctx, object); err != nil {
			if apierrors.IsConflict(err) {
				object = &databasev1alpha1.PostgreSQLDatabase{}
				if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(state.object), object); err != nil {
					return err
				}
				return apierrors.NewConflict(databasev1alpha1.GroupVersion.WithResource("postgresqldatabases").GroupResource(),
					object.Name, errors.New("status 已被并发更新，重新读取后重试"))
			}
			return err
		}
		// 旧 CRD 会裁剪未知 status 字段。不能把 HTTP 成功当作确认已保存后继续写后端。
		saved := snapshot(object)
		if !equality.Semantic.DeepEqual(saved.Credential.Location, desired.Credential.Location) ||
			saved.Credential.Version != desired.Credential.Version ||
			(r.PostgreSQL != nil && (saved.Resources.RoleOID != desired.Resources.RoleOID || saved.Resources.DatabaseOID != desired.Resources.DatabaseOID)) {
			return fmt.Errorf("凭据位置、版本或资源 OID 未被 API 保留；请先升级 Database CRD，未继续外部操作")
		}
		return nil
	})
}

func snapshot(object *databasev1alpha1.PostgreSQLDatabase) database.Snapshot {
	result := database.Snapshot{
		Binding:   kubernetes.BindingDatabase(object),
		Protected: controllerutil.ContainsFinalizer(object, kubernetes.DatabaseFinalizer),
		Credential: database.CredentialState{
			Version: object.Status.CredentialVersion,
		},
		Resources: database.ResourceState{
			RoleOID: uint32(object.Status.RoleOID), DatabaseOID: uint32(object.Status.DatabaseOID), Phase: database.ResourcePending,
		},
	}
	if ref := object.Status.CredentialRef; ref != nil {
		result.Credential.Location = &credential.Location{Mount: ref.Mount, Path: ref.Path}
	}
	if condition := meta.FindStatusCondition(object.Status.Conditions, credentialsReadyCondition); condition != nil {
		result.Credential.Phase, result.Credential.Message = credentialPhase(condition.Reason), condition.Message
	}
	if condition := meta.FindStatusCondition(object.Status.Conditions, resourcesReadyCondition); condition != nil {
		result.Resources.Phase, result.Resources.Message = database.ResourcePhase(condition.Reason), condition.Message
	}
	return result
}

func presentCredential(object *databasev1alpha1.PostgreSQLDatabase, state database.CredentialState) {
	if state.Location != nil {
		object.Status.CredentialRef = &databasev1alpha1.CredentialReference{Mount: state.Location.Mount, Path: state.Location.Path}
	}
	object.Status.CredentialVersion = state.Version
	status := metav1.ConditionFalse
	if state.Phase == database.CredentialPrepared {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: credentialsReadyCondition, Status: status, Reason: credentialReasons[state.Phase], Message: state.Message,
		ObservedGeneration: object.Generation,
	})
}

func presentResources(object *databasev1alpha1.PostgreSQLDatabase, state database.ResourceState) {
	object.Status.RoleOID, object.Status.DatabaseOID = int64(state.RoleOID), int64(state.DatabaseOID)
	status := metav1.ConditionFalse
	if state.Phase == database.ResourceAvailable {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: resourcesReadyCondition, Status: status, Reason: string(state.Phase), Message: state.Message,
		ObservedGeneration: object.Generation,
	})
}

// 持久化 API 的 Reason 保持兼容，领域内部只使用准备阶段。
var credentialReasons = map[database.CredentialPhase]string{
	database.CredentialPending:       "PreparationPending",
	database.CredentialPinned:        "LocationPinned",
	database.CredentialPrepared:      "CredentialPrepared",
	database.CredentialConflict:      "Conflict",
	database.CredentialUnavailable:   "DependencyUnavailable",
	database.CredentialStopped:       "PreparationStopped",
	database.CredentialInvalidTarget: "InvalidTarget",
}

func credentialPhase(reason string) database.CredentialPhase {
	for phase, value := range credentialReasons {
		if value == reason {
			return phase
		}
	}
	return database.CredentialPending
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases,verbs=get;list;watch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases/status,verbs=get;update;patch

func (r *DatabaseReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil || r.Reader == nil || r.Credentials == nil {
		return errors.New("database controller requires injected clients and credential backend")
	}
	return ctrl.NewControllerManagedBy(manager).Named("database").
		For(&databasev1alpha1.PostgreSQLDatabase{}).
		Watches(&databasev1alpha1.PostgreSQLTenant{}, handler.EnqueueRequestsFromMapFunc(databaseRequestsForTenant)).
		Watches(&databasev1alpha1.PostgreSQLInstance{}, handler.EnqueueRequestsFromMapFunc(r.databaseRequestsForInstance)).
		Complete(r)
}

func databaseRequestsForTenant(_ context.Context, object client.Object) []ctrl.Request {
	tenant := object.(*databasev1alpha1.PostgreSQLTenant)
	if tenant.Status.DatabaseRef == nil {
		return nil
	}
	return []ctrl.Request{{Name: string(tenant.Status.DatabaseRef.Name)}}
}

func (r *DatabaseReconciler) databaseRequestsForInstance(ctx context.Context, object client.Object) []ctrl.Request {
	databases := &databasev1alpha1.PostgreSQLDatabaseList{}
	if err := r.Client.List(ctx, databases); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "无法映射 Instance 事件；等待低频重试")
		return nil
	}
	var requests []ctrl.Request
	for _, item := range databases.Items {
		if string(item.Spec.InstanceRef.Name) == object.GetName() {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&item)})
		}
	}
	return requests
}
