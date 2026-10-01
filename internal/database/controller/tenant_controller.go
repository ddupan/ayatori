package controller

import (
	"context"
	"time"

	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const dependencyRetry = 30 * time.Second

// TenantReconciler 建立 Tenant 与 Database 的排他双向绑定：先固定申请，再创建或选择
// Database 并写资源侧绑定，回读确认后才写申请侧 status。绑定资格规则在 domain/binding。
// 读取直连 API；写入使用本轮读取的 resourceVersion，不覆盖并发修改。
type TenantReconciler struct {
	Client client.Client
	Reader client.Reader
}

// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqltenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqltenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqltenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqldatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=database.ayatori.ddupan.top,resources=postgresqlinstances,verbs=get;list;watch

func (r *TenantReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	object := &databasev1alpha1.PostgreSQLTenant{}
	if err := r.Reader.Get(ctx, request.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant := bindingTenant(object)
	if tenant.Deleting {
		return r.wait(ctx, object, binding.Deleting, "DeletionPending", "删除清理尚未接入；保留 finalizer 和 Database 绑定，未执行后端删除")
	}
	target, err := tenant.Request.Resolve(tenant.Identity)
	if err != nil {
		return r.wait(ctx, object, tenant.Phase, "InvalidRequest", err.Error())
	}
	// 持久固定申请，再创建/绑定资源；不是预先宣告双向绑定成功。
	if controllerutil.AddFinalizer(object, TenantFinalizer) {
		if err := r.Client.Update(ctx, object); err != nil {
			return ctrl.Result{}, err
		}
	}
	if tenant.Phase != binding.Binding && tenant.Phase != binding.Bound {
		if err := r.present(ctx, object, binding.Binding, "BindingPending", "申请目标已固定，等待资源侧绑定", nil); err != nil {
			return ctrl.Result{}, err
		}
	}
	tenant = bindingTenant(object)
	database, issue, err := r.resolveDatabase(ctx, tenant, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if issue == nil {
		facts := bindingDatabase(database)
		issue = facts.CanBind(tenant)
	}
	if issue != nil {
		return r.wait(ctx, object, tenant.Phase, issue.Reason, issue.Message)
	}
	if err := r.bindDatabase(ctx, database, tenant.Identity); err != nil {
		return ctrl.Result{}, err
	}
	return r.confirm(ctx, object, database)
}

// resolveDatabase 返回可绑定的 Database 对象；无法继续时返回 Issue 而不是错误。
func (r *TenantReconciler) resolveDatabase(ctx context.Context, tenant binding.Tenant, target binding.Target) (
	*databasev1alpha1.PostgreSQLDatabase, *binding.Issue, error,
) {
	database, err := r.database(ctx, target.Name)
	if err != nil {
		return nil, nil, err
	}
	if database == nil && target.Provision == nil {
		return nil, &binding.Issue{Reason: binding.DependencyUnavailable, Message: "指定的 Database 尚不存在，等待资源出现"}, nil
	}
	instanceName := ""
	var observed *binding.Database
	if database != nil {
		facts := bindingDatabase(database)
		instanceName, observed = facts.Instance, &facts
	}
	if target.Provision != nil {
		instanceName = target.Provision.Instance
		if observed != nil && !observed.MatchesProvision(target, tenant.Identity) {
			return nil, &binding.Issue{Reason: binding.Conflict,
				Message: "动态 Database 名称已存在，但目标或 Tenant UID 不匹配；请核实记录，未自动认领"}, nil
		}
	}
	owner := &databasev1alpha1.PostgreSQLInstance{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: instanceName}, owner); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, nil, err
		}
		return nil, &binding.Issue{Reason: binding.DependencyUnavailable, Message: "引用的 Instance 尚不存在"}, nil
	}
	instance := binding.Instance{
		Identity: binding.Identity{Name: owner.Name, UID: string(owner.UID)},
		Deleting: !owner.DeletionTimestamp.IsZero(), Ready: currentReady(owner.Generation, owner.Status.Conditions),
	}
	if issue := instance.Check(observed); issue != nil {
		return nil, issue, nil
	}
	if database == nil {
		database = newProvisionedDatabase(target, tenant.Identity)
		if err := r.Client.Create(ctx, database); err != nil {
			return nil, nil, err
		}
	}
	// 只有首次动态记录可以补入实例身份；导入必须先有资源观察。
	if database.Status.InstanceUID == "" && target.Provision != nil {
		database.Status.InstanceUID = owner.UID
		if err := r.Client.Status().Update(ctx, database); err != nil {
			return nil, nil, err
		}
	}
	if database.Status.InstanceUID == "" {
		return nil, &binding.Issue{Reason: binding.DependencyUnavailable, Message: "Database 尚未完成实例身份验证"}, nil
	}
	return database, nil, nil
}

func newProvisionedDatabase(target binding.Target, tenant binding.TenantIdentity) *databasev1alpha1.PostgreSQLDatabase {
	database := &databasev1alpha1.PostgreSQLDatabase{}
	database.Name = target.Name
	database.Spec = databasev1alpha1.PostgreSQLDatabaseSpec{
		InstanceRef: databasev1alpha1.InstanceReference{Name: databasev1alpha1.ObjectName(target.Provision.Instance)},
		Database:    databasev1alpha1.PostgreSQLIdentifier(target.Provision.Database),
		LoginRole:   databasev1alpha1.PostgreSQLIdentifier(target.Provision.LoginRole),
		Source:      "Provision", ReclaimPolicy: databasev1alpha1.ReclaimRetain, TenantRef: tenantReference(tenant),
	}
	controllerutil.AddFinalizer(database, DatabaseFinalizer)
	return database
}

// bindDatabase 写资源侧绑定与删除保护；resourceVersion 冲突说明有并发申请，交给下一轮判断。
func (r *TenantReconciler) bindDatabase(ctx context.Context, database *databasev1alpha1.PostgreSQLDatabase, tenant binding.TenantIdentity) error {
	wanted := tenantReference(tenant)
	changed := controllerutil.AddFinalizer(database, DatabaseFinalizer)
	if database.Spec.TenantRef == nil || *database.Spec.TenantRef != *wanted {
		database.Spec.TenantRef = wanted
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Client.Update(ctx, database)
}

// confirm 回读双方后才写申请侧绑定；单边写入不代表绑定完成。
func (r *TenantReconciler) confirm(ctx context.Context, object *databasev1alpha1.PostgreSQLTenant, database *databasev1alpha1.PostgreSQLDatabase) (ctrl.Result, error) {
	latest := &databasev1alpha1.PostgreSQLTenant{}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(object), latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	current := bindingTenant(latest)
	if current.Identity != bindingTenant(object).Identity || current.Deleting {
		return ctrl.Result{}, nil
	}
	if latest.Generation != object.Generation {
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	observed, err := r.database(ctx, database.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if observed == nil || observed.UID != database.UID || observed.Spec.TenantRef == nil {
		return r.wait(ctx, latest, current.Phase, binding.Conflict, "资源侧身份或绑定已变化，未完成申请侧绑定")
	}
	facts := bindingDatabase(observed)
	if issue := facts.CanBind(current); issue != nil {
		return r.wait(ctx, latest, current.Phase, issue.Reason, issue.Message)
	}
	if err := r.present(ctx, latest, binding.Bound, "BindingComplete",
		"双向绑定已记录；尚未执行供应、应用登录验证或凭据交付，不能 Ready", &facts.Identity); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

func (r *TenantReconciler) database(ctx context.Context, name string) (*databasev1alpha1.PostgreSQLDatabase, error) {
	database := &databasev1alpha1.PostgreSQLDatabase{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, database); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return database, nil
}

// wait 呈现不能继续的原因，并按低频重试等待依赖或人工处理。
func (r *TenantReconciler) wait(ctx context.Context, object *databasev1alpha1.PostgreSQLTenant, phase, reason, message string) (ctrl.Result, error) {
	if err := r.present(ctx, object, phase, reason, message, nil); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

func (r *TenantReconciler) present(ctx context.Context, object *databasev1alpha1.PostgreSQLTenant, phase, reason, message string, database *binding.Identity) error {
	previous := object.Status.DeepCopy()
	object.Status.Phase = phase
	object.Status.ObservedGeneration = object.Generation
	if database != nil {
		object.Status.DatabaseRef = &databasev1alpha1.BoundDatabaseReference{
			Name: databasev1alpha1.ObjectName(database.Name), UID: types.UID(database.UID),
		}
	}
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{
		Type: readyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		ObservedGeneration: object.Generation,
	})
	if equality.Semantic.DeepEqual(*previous, object.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Client.Status().Update(ctx, object))
}
