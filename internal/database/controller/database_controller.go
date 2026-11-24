package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	databasev1alpha1 "go.ddupan.top/ayatori/api/database/v1alpha1"
	"go.ddupan.top/ayatori/internal/database/domain/binding"
	"go.ddupan.top/ayatori/internal/database/domain/credential"
	"go.ddupan.top/ayatori/internal/database/domain/database"
	"go.ddupan.top/ayatori/internal/database/domain/instance"
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
	UpdateCredential(ctx context.Context, location credential.Location, confirmed int64, value credential.ApplicationCredential) (int64, error)
	// ReadLatestCredential 只用于导入登记前读取管理员声明位置的最新版本。
	ReadLatestCredential(ctx context.Context, location credential.Location) (credential.ApplicationCredential, int64, error)
	// DestroyCredential 只在 Delete 回收时永久删除固定位置的全部版本。
	DestroyCredential(ctx context.Context, location credential.Location) error
}

// PostgreSQLBackend 只返回 catalog 事实和 database 包定义的结果分类。
// 每次调用都传入本轮读取的管理凭据，adapter 据此复用或替换连接池。
type PostgreSQLBackend interface {
	InspectResources(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) (database.ResourceFacts, error)
	CreateDatabase(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) (uint32, error)
	RevokePublicConnect(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) error
	CreateLoginRole(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database, credential.ApplicationCredential) (uint32, error)
	TransferOwnership(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) error
	InspectExtensions(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) (database.ExtensionFacts, error)
	InstallExtensions(ctx context.Context, target instance.ObservationTarget, credentials instance.ManagementCredentials, db *database.Database, names []string) error
	VerifyLogin(context.Context, instance.ObservationTarget, *database.Database, credential.ApplicationCredential) error
	DropDatabase(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) error
	DropRole(context.Context, instance.ObservationTarget, instance.ManagementCredentials, *database.Database) error
}

// DatabaseReconciler 处理 PostgreSQLDatabase 的删除保护与回收，并供应资源：先准备凭据，
// PostgreSQL 为 nil 时只做凭据准备，Credentials 为 nil 时只处理生命周期。
// 规则由 Database 聚合判断，这里只读写 API 和执行外部操作。
type DatabaseReconciler struct {
	Client      client.Client
	Reader      client.Reader
	Credentials CredentialBackend
	// Secrets 与 PostgreSQL 只在启用资源供应时注入。
	Secrets    ManagementSecrets
	PostgreSQL PostgreSQLBackend
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
	switch db.PlanLifecycle() {
	case database.Hold:
		return ctrl.Result{RequeueAfter: dependencyRetry}, r.save(ctx, state, db, true)
	case database.Forget:
		if controllerutil.RemoveFinalizer(state.object, DatabaseFinalizer) {
			return ctrl.Result{}, client.IgnoreNotFound(r.Client.Update(ctx, state.object))
		}
		return ctrl.Result{}, nil
	case database.Reclaim:
		return ctrl.Result{RequeueAfter: dependencyRetry}, r.reclaim(ctx, state, db)
	}
	if r.Credentials == nil {
		return ctrl.Result{}, nil
	}
	if db.Verifies() {
		if r.PostgreSQL == nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: dependencyRetry}, r.importDatabase(ctx, state, db)
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
		return r.save(ctx, state, db, false)
	case database.CreateCredential:
		_, existing := r.Credentials.ReadCredential(ctx, db.CredentialLocation(), 0)
		if generated, ok := db.GenerateCredential(existing); ok {
			version, err := r.Credentials.CreateCredential(ctx, db.CredentialLocation(), generated)
			db.CredentialCreated(version, err)
		}
		return r.save(ctx, state, db, false)
	case database.VerifyCredential:
		value, err = r.Credentials.ReadCredential(ctx, db.CredentialLocation(), db.CredentialVersion())
		db.VerifyCredential(value, err)
		if r.rewriteCredential(ctx, db) {
			return r.save(ctx, state, db, false)
		}
	}
	if r.PostgreSQL == nil || db.PlanResources() == database.Wait {
		return r.save(ctx, state, db, false)
	}
	target := state.target
	management, err := r.Secrets.Read(ctx, target.Definition().AdminCredential())
	var facts database.ResourceFacts
	if err == nil {
		facts, err = r.PostgreSQL.InspectResources(ctx, target, management, db)
	}
	switch db.ObserveResources(facts, err) {
	case database.CreateDatabase:
		db.DatabaseCreated(r.PostgreSQL.CreateDatabase(ctx, target, management, db))
	case database.RevokePublicConnect:
		db.Converged("撤销 PUBLIC CONNECT", r.PostgreSQL.RevokePublicConnect(ctx, target, management, db))
	case database.CreateRole:
		// PlanResources 只在本轮凭据验证成功后放行，value 此时一定是已确认凭据。
		db.RoleCreated(r.PostgreSQL.CreateLoginRole(ctx, target, management, db, value))
	case database.TransferOwnership:
		db.Converged("移交数据库 owner", r.PostgreSQL.TransferOwnership(ctx, target, management, db))
	case database.InspectExtensions:
		extensions, err := r.PostgreSQL.InspectExtensions(ctx, target, management, db)
		switch db.ObserveExtensions(extensions, err) {
		case database.InstallExtensions:
			db.ExtensionsInstalled(r.PostgreSQL.InstallExtensions(ctx, target, management, db, db.MissingExtensions()))
		case database.VerifyLogin:
			db.LoginVerified(r.PostgreSQL.VerifyLogin(ctx, target, db, value))
		}
	}
	return r.save(ctx, state, db, false)
}

// importDatabase 只读验证导入或重新开放的资源：凭据、catalog 身份与 owner、扩展、实际登录；
// 导入在全部通过后首次登记确认身份，之后每轮按确认身份重新验证。不改密码、owner、权限或扩展。
func (r *DatabaseReconciler) importDatabase(ctx context.Context, state *databaseState, db *database.Database) error {
	if db.PlanImport() == database.Wait {
		return r.save(ctx, state, db, true)
	}
	var value credential.ApplicationCredential
	var version int64
	var credentialErr error
	if version = db.CredentialVersion(); version > 0 {
		value, credentialErr = r.Credentials.ReadCredential(ctx, db.CredentialLocation(), version)
	} else {
		value, version, credentialErr = r.Credentials.ReadLatestCredential(ctx, db.ImportLocation())
	}
	target := state.target
	management, err := r.Secrets.Read(ctx, target.Definition().AdminCredential())
	var facts database.ResourceFacts
	if err == nil {
		facts, err = r.PostgreSQL.InspectResources(ctx, target, management, db)
	}
	action := db.ObserveImport(value, version, credentialErr, facts, err)
	if r.rewriteCredential(ctx, db) {
		return r.save(ctx, state, db, true)
	}
	if action == database.InspectExtensions {
		extensions, err := r.PostgreSQL.InspectExtensions(ctx, target, management, db)
		if db.ObserveExtensions(extensions, err) == database.VerifyLogin {
			db.LoginVerified(r.PostgreSQL.VerifyLogin(ctx, target, db, value))
		}
	}
	return r.save(ctx, state, db, true)
}

// reclaim 按 Delete 策略逐步清理已释放的资源：数据库 → 角色 → 凭据 → 删除 Database。
// 每轮最多一次外部写入；对象已不存在则跳过，同名但身份不符则停止并保留 finalizer。
func (r *DatabaseReconciler) reclaim(ctx context.Context, state *databaseState, db *database.Database) error {
	if r.PostgreSQL == nil || r.Credentials == nil || state.dependencies.Instance == nil {
		db.Reclaimed("Delete 回收", database.ErrUnavailable)
		return r.save(ctx, state, db, true)
	}
	target := state.target
	management, err := r.Secrets.Read(ctx, target.Definition().AdminCredential())
	var facts database.ResourceFacts
	if err == nil {
		facts, err = r.PostgreSQL.InspectResources(ctx, target, management, db)
	}
	switch db.ObserveReclaim(facts, err) {
	case database.DropDatabase:
		db.Reclaimed("删除数据库", r.PostgreSQL.DropDatabase(ctx, target, management, db))
	case database.DropRole:
		db.Reclaimed("删除登录角色", r.PostgreSQL.DropRole(ctx, target, management, db))
	case database.DestroyCredential:
		// 凭据位置记录保留到 Database 删除为止，因此销毁成功后在本轮完成回收，不再重复判断。
		err := r.Credentials.DestroyCredential(ctx, db.CredentialLocation())
		db.Reclaimed("删除应用凭据", err)
		if err == nil {
			return r.finalize(ctx, state)
		}
	case database.Finalize:
		return r.finalize(ctx, state)
	}
	return r.save(ctx, state, db, true)
}

// finalize 在外部资源清理完成后删除 Database；已在删除中时移除 finalizer。
func (r *DatabaseReconciler) finalize(ctx context.Context, state *databaseState) error {
	object := state.object
	if object.DeletionTimestamp.IsZero() {
		return client.IgnoreNotFound(r.Client.Delete(ctx, object))
	}
	if controllerutil.RemoveFinalizer(object, DatabaseFinalizer) {
		return client.IgnoreNotFound(r.Client.Update(ctx, object))
	}
	return nil
}

func (r *DatabaseReconciler) load(ctx context.Context, name string) (*databaseState, error) {
	object := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	state := &databaseState{object: object}
	if ref := object.Spec.TenantRef; ref != nil {
		tenant := &databasev1alpha1.PostgreSQLTenant{}
		err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: string(ref.Name)}, tenant)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err == nil {
			facts := bindingTenant(tenant)
			state.dependencies.Tenant = &facts
			state.dependencies.Extensions = instance.NewExtensionSet(tenant.Spec.Extensions)
			state.dependencies.TenantProtected = controllerutil.ContainsFinalizer(tenant, TenantFinalizer)
		}
	}
	owner := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: string(object.Spec.InstanceRef.Name)}, owner); err != nil {
		return state, client.IgnoreNotFound(err)
	}
	target, err := instanceTarget(owner)
	if err != nil {
		return nil, err
	}
	state.target = target
	state.dependencies.Instance = &database.Instance{
		Identity: binding.Identity{Name: owner.Name, UID: string(owner.UID)},
		Deleting: !owner.DeletionTimestamp.IsZero(), Ready: currentReady(owner.Generation, owner.Status.Conditions),
		Endpoint: target.Definition().Endpoint(),
	}
	return state, nil
}

// save 记录本轮结果。OID 与凭据版本是已经发生的外部事实，resourceVersion 冲突时
// 重新读取后再写，而不是丢弃；controller 是这些 status 字段的唯一写入者。
func (r *DatabaseReconciler) save(ctx context.Context, state *databaseState, db *database.Database, lifecycle bool) error {
	desired := db.Snapshot()
	object := state.object
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		previous := object.Status.DeepCopy()
		if r.Credentials != nil {
			presentCredential(object, desired.Credential)
		}
		if r.PostgreSQL != nil || lifecycle {
			presentResources(object, desired.Resources)
		}
		// 导入登记写入实例身份；导入或重新开放验证通过后写 Available。Released 由 TenantReconciler
		// 在有绑定时写入；管理员清除 spec.tenantRef 后才允许回到 Available，因此不会覆盖释放记录。
		if object.Status.InstanceUID == "" && desired.Binding.InstanceUID != "" {
			object.Status.InstanceUID = types.UID(desired.Binding.InstanceUID)
		}
		if desired.Binding.Phase == database.Available && object.Spec.TenantRef == nil &&
			(object.Status.Phase == "" || object.Status.Phase == database.Released) {
			object.Status.Phase = database.Available
		}
		ready := metav1.Condition{
			Type: readyCondition, Status: metav1.ConditionFalse, Reason: "ProvisioningIncomplete",
			Message: "资源、扩展或应用登录尚未全部验证", ObservedGeneration: object.Generation,
		}
		if r.PostgreSQL != nil && db.Ready() {
			ready.Status, ready.Reason = metav1.ConditionTrue, "DatabaseReady"
			ready.Message = "数据库资源与扩展已收敛，应用凭据本轮登录成功；Tenant 凭据交付另行判断"
		}
		meta.SetStatusCondition(&object.Status.Conditions, ready)
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
		Binding:       bindingDatabase(object),
		ReclaimPolicy: database.ReclaimPolicy(object.Spec.ReclaimPolicy),
		Protected:     controllerutil.ContainsFinalizer(object, DatabaseFinalizer),
		Credential: database.CredentialState{
			Version: object.Status.CredentialVersion, Phase: database.CredentialPhase(object.Status.CredentialPhase),
		},
		Resources: database.ResourceState{
			RoleOID: uint32(object.Status.RoleOID), DatabaseOID: uint32(object.Status.DatabaseOID),
			Phase: database.ResourcePhase(object.Status.ResourcePhase),
		},
	}
	if ref := object.Status.CredentialRef; ref != nil {
		result.Credential.Location = &credential.Location{Mount: ref.Mount, Path: ref.Path}
	}
	if ref := object.Spec.CredentialRef; ref != nil {
		result.ImportLocation = &credential.Location{Mount: ref.Mount, Path: ref.Path}
	}
	if result.Credential.Phase == "" {
		result.Credential.Phase = database.CredentialPending
	}
	if result.Resources.Phase == "" {
		result.Resources.Phase = database.ResourcePending
	}
	// 阶段只来自显式字段；Condition 只是呈现，这里仅取回诊断消息以保留冲突的首次原因。
	if condition := meta.FindStatusCondition(object.Status.Conditions, credentialsReadyCondition); condition != nil {
		result.Credential.Message = condition.Message
	}
	if condition := meta.FindStatusCondition(object.Status.Conditions, resourcesReadyCondition); condition != nil {
		result.Resources.Message = condition.Message
	}
	return result
}

func presentCredential(object *databasev1alpha1.PostgreSQLDatabase, state database.CredentialState) {
	if state.Location != nil {
		object.Status.CredentialRef = &databasev1alpha1.CredentialReference{Mount: state.Location.Mount, Path: state.Location.Path}
	}
	object.Status.CredentialVersion = state.Version
	object.Status.CredentialPhase = string(state.Phase)
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
	object.Status.ResourcePhase = string(state.Phase)
	status := metav1.ConditionFalse
	if state.Phase == database.ResourceAvailable {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: resourcesReadyCondition, Status: status, Reason: string(state.Phase), Message: state.Message,
		ObservedGeneration: object.Generation,
	})
}

// Condition Reason 只用于呈现，保持既有拼写；controller 不从 Reason 还原阶段。
var credentialReasons = map[database.CredentialPhase]string{
	database.CredentialPending:       "PreparationPending",
	database.CredentialPinned:        "LocationPinned",
	database.CredentialPrepared:      "CredentialPrepared",
	database.CredentialConflict:      "Conflict",
	database.CredentialUnavailable:   "DependencyUnavailable",
	database.CredentialStopped:       "PreparationStopped",
	database.CredentialInvalidTarget: "InvalidTarget",
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases/status,verbs=get;update;patch

func (r *DatabaseReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil || r.Reader == nil {
		return errors.New("database controller requires injected clients")
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

// rewriteCredential 执行本轮判定的连接信息改写；返回 true 表示本轮已做外部写入，应直接保存结果。
func (r *DatabaseReconciler) rewriteCredential(ctx context.Context, db *database.Database) bool {
	value, ok := db.CredentialRewrite()
	if !ok {
		return false
	}
	db.CredentialRewritten(r.Credentials.UpdateCredential(ctx, db.CredentialLocation(), db.CredentialVersion(), value))
	return true
}
