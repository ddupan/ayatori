// Package database 定义 Database 聚合：资源侧供应资格、应用凭据准备与 PostgreSQL 资源供应。
// 聚合只做判断与状态迁移，不访问 Kubernetes、凭据后端或 PostgreSQL；外部操作由 Reconciler 执行。
package database

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// Snapshot 是 Database 自身的持久状态，不含 resourceVersion 或 Conditions。
type Snapshot struct {
	Binding binding.Database
	// ImportLocation 是导入资源在 spec 中声明的已有凭据位置；动态供应为 nil。
	ImportLocation *credential.Location
	ReclaimPolicy  ReclaimPolicy
	Protected      bool
	Credential     CredentialState
	Resources      ResourceState
}

type Instance struct {
	binding.Instance
	Endpoint instance.Endpoint
}

// Dependencies 是本轮读取的 Tenant 与 Instance 事实，不属于 Database 的持久状态。
type Dependencies struct {
	Tenant          *binding.Tenant
	TenantProtected bool
	// Extensions 是 Tenant 申请的扩展；CRD 保证只能追加。
	Extensions instance.ExtensionSet
	Instance   *Instance
}

// Action 是聚合要求 Reconciler 执行的下一步。除 Wait 外，每轮最多执行一次外部写入。
type Action uint8

const (
	// Wait 表示聚合已记录需要呈现的状态；保存后等待事件或低频重试。
	Wait Action = iota
	// Save 表示只有状态变化（如固定凭据位置），保存后进入下一轮。
	Save
	CreateCredential
	VerifyCredential
	InspectResources
	CreateDatabase
	RevokePublicConnect
	CreateRole
	TransferOwnership
	InspectExtensions
	InstallExtensions
	VerifyLogin
	DropDatabase
	DropRole
	DestroyCredential
	// Finalize 表示外部资源均已清理，可移除 finalizer 并删除 Database。
	Finalize
)

type Database struct {
	snapshot     Snapshot
	dependencies Dependencies
	missing      []string
	pending      *registration
	rewrite      *credential.ApplicationCredential
}

func Reconstitute(snapshot Snapshot, dependencies Dependencies) *Database {
	return &Database{snapshot: snapshot, dependencies: dependencies}
}

func (d *Database) Snapshot() Snapshot { return d.snapshot }
func (d *Database) UID() string        { return d.snapshot.Binding.Identity.UID }

// Name 与 LoginRole 是 PostgreSQL 中的对象名，不是 Kubernetes 资源名。
func (d *Database) Name() string      { return d.snapshot.Binding.Name }
func (d *Database) LoginRole() string { return d.snapshot.Binding.LoginRole }
func (d *Database) RoleOID() uint32   { return d.snapshot.Resources.RoleOID }

func (d *Database) CredentialLocation() credential.Location { return *d.snapshot.Credential.Location }
func (d *Database) CredentialVersion() int64                { return d.snapshot.Credential.Version }

func (d *Database) RequiresProvisioning() bool { return d.snapshot.Binding.Source == "Provision" }

// PlanCredential 决定凭据准备的下一步。configured 是部署配置给出的位置，只在首次固定时采用。
func (d *Database) PlanCredential(configured credential.Location, configErr error) Action {
	current := d.snapshot.Credential
	if !current.resume() {
		return Wait
	}
	if issue := d.check(); issue != nil {
		d.reportCredential(issue.credential, issue.message)
		return Wait
	}
	if configErr != nil {
		d.reportCredential(CredentialUnavailable, "凭据存储位置配置无效，未执行外部写入")
		return Wait
	}
	if current.Location == nil {
		d.snapshot.Credential.Location = &configured
		d.snapshot.Credential.Phase, d.snapshot.Credential.Message = CredentialPinned, "凭据位置已固定，等待创建"
		return Save
	}
	if *current.Location != configured {
		d.reportCredential(CredentialUnavailable, "部署配置与固定凭据位置不一致；请恢复原 mount/path 配置，未迁移或改密")
		return Wait
	}
	if current.Version > 0 {
		return VerifyCredential
	}
	return CreateCredential
}

// GenerateCredential 只在固定位置确认为空时生成凭据。existing 是读取固定位置的结果；
// 返回 false 时聚合已记录原因。创建结果丢失后，下一轮会看到未确认的已有值而转为冲突。
func (d *Database) GenerateCredential(existing error) (credential.ApplicationCredential, bool) {
	if existing == nil || errors.Is(existing, credential.ErrApplicationCredentialInvalid) {
		d.reportCredential(CredentialConflict, "固定位置已有未确认的凭据；请人工核实，未认领或覆盖")
		return credential.ApplicationCredential{}, false
	}
	if !errors.Is(existing, ErrNotFound) {
		d.reportCredential(CredentialUnavailable, "创建前无法确认凭据位置是否为空，等待依赖恢复")
		return credential.ApplicationCredential{}, false
	}
	value, err := credential.GenerateApplicationCredential(d.LoginRole(), d.Name(), d.dependencies.Instance.Endpoint)
	if err != nil {
		d.reportCredential(CredentialInvalidTarget, "应用凭据目标无效，未执行外部写入")
		return credential.ApplicationCredential{}, false
	}
	return value, true
}

// CredentialCreated 只接受首次创建并回读得到的版本 1，不能把后续写入认作首次供应。
// 结果不确定时不在此判定冲突：下一轮读取固定位置，已有未确认值才转为冲突，仍为空则重试。
func (d *Database) CredentialCreated(version int64, err error) {
	if errors.Is(err, ErrConflict) || (err == nil && version != 1) {
		d.reportCredential(CredentialConflict, "凭据创建冲突；请核对固定位置的版本历史，未认领、覆盖或重新生成密码")
		return
	}
	if err != nil {
		d.reportCredential(CredentialUnavailable, "凭据创建未确认成功，下一轮按固定位置的实际状态继续")
		return
	}
	d.snapshot.Credential.Version = version
	d.snapshot.Credential.Phase, d.snapshot.Credential.Message = CredentialPrepared, "凭据已创建并回读确认；尚未创建 PostgreSQL 资源或交付给 Tenant"
}

// VerifyCredential 检查已确认版本仍可读取且指向当前目标；返回 true 时凭据可用于供应。
func (d *Database) VerifyCredential(value credential.ApplicationCredential, err error) bool {
	switch {
	case errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound):
		d.reportCredential(CredentialConflict, "已确认凭据消失、版本变化或内容无效；请人工核实，未生成替代密码")
		return false
	case err != nil:
		d.reportCredential(CredentialUnavailable, "已确认凭据暂时无法读取；保留确认版本，等待依赖恢复")
		return false
	case !value.MatchesIdentity(d.LoginRole(), d.Name()):
		d.reportCredential(CredentialConflict, "已确认凭据的 database/loginRole 与本资源不一致；请人工核实，未修改凭据")
		return false
	case !value.MatchesTarget(d.LoginRole(), d.Name(), d.dependencies.Instance.Endpoint):
		d.planRewrite(value)
		return false
	}
	d.snapshot.Credential.Phase, d.snapshot.Credential.Message = CredentialPrepared, "已确认凭据可读取；尚未验证 PostgreSQL 资源或完成 Tenant 交付"
	return true
}

// planRewrite 处理只有连接信息不一致的已确认凭据。连接信息是 Instance 的可变属性，不是资源身份；
// 凭据（含管理员显式交给本资源的导入凭据）由本资源管理，以同一密码改写为 Instance 当前连接信息。
func (d *Database) planRewrite(value credential.ApplicationCredential) {
	rewritten, err := value.WithEndpoint(d.dependencies.Instance.Endpoint)
	if err != nil {
		d.reportCredential(CredentialInvalidTarget, "Instance 连接信息无效，未改写凭据")
		return
	}
	d.rewrite = &rewritten
	d.reportCredential(CredentialPending, "Instance 连接信息已变化，以同一密码改写凭据中的连接信息")
}

// CredentialRewrite 是本轮判定需要写入的新版本凭据（连接信息已更新）。
func (d *Database) CredentialRewrite() (credential.ApplicationCredential, bool) {
	if d.rewrite == nil {
		return credential.ApplicationCredential{}, false
	}
	return *d.rewrite, true
}

// CredentialRewritten 记录以确认版本为 CAS 前提的改写结果。改写只能产生确认版本的下一版本；
// 结果不确定时下一轮读取确认版本会发现版本已变化而转为冲突，与创建相同不自动认领。
func (d *Database) CredentialRewritten(version int64, err error) {
	previous := d.snapshot.Credential.Version
	switch {
	case errors.Is(err, ErrConflict) || (err == nil && version != previous+1):
		d.reportCredential(CredentialConflict, "改写连接信息时确认版本之后已有其他写入；请人工核实，未覆盖")
	case err != nil:
		d.reportCredential(CredentialUnavailable, "改写连接信息未确认成功，下一轮按确认版本的实际状态继续")
	default:
		d.snapshot.Credential.Version = version
		d.snapshot.Credential.Phase = CredentialPrepared
		d.snapshot.Credential.Message = fmt.Sprintf("已按 Instance 当前连接信息改写凭据（版本 %d）；消费方需改用新版本", version)
	}
	d.rewrite = nil
}

// PlanResources 在本轮凭据判定之后调用；只有凭据已确认可用时才允许观察 PostgreSQL。
func (d *Database) PlanResources() Action {
	if d.snapshot.Resources.Phase == ResourceConflict {
		return Wait
	}
	if issue := d.check(); issue != nil {
		d.reportResources(issue.resource, issue.message)
		return Wait
	}
	if prepared := d.snapshot.Credential; prepared.Version == 0 || prepared.Phase != CredentialPrepared {
		d.reportResources(ResourceUnavailable, "等待已确认且当前可用的应用凭据")
		return Wait
	}
	return InspectResources
}

// ObserveResources 根据本轮 PostgreSQL 事实决定下一项外部操作。每一步都由事实驱动，
// 可重复执行，不需要事务或持久检查点。应用角色最后创建，此前数据库对外不可连接。
func (d *Database) ObserveResources(facts ResourceFacts, err error) Action {
	if err != nil {
		d.reportResources(ResourceUnavailable, "无法验证当前 PostgreSQL 资源和管理权限")
		return Wait
	}
	if err := d.CheckResources(facts); err != nil {
		d.reportResources(ResourceConflict, err.Error()+"；请人工核对，未认领或覆盖")
		return Wait
	}
	switch {
	case facts.DatabaseOID == 0:
		return CreateDatabase
	case facts.PublicConnect:
		return RevokePublicConnect
	case facts.RoleOID == 0:
		return CreateRole
	case facts.OwnerOID != facts.RoleOID || !facts.ManagerConnect:
		return TransferOwnership
	}
	return InspectExtensions
}

// ObserveExtensions 按服务器实际可用列表判断申请；可用但未安装的扩展由 owner 安装。
// 扩展只追加，不卸载申请之外已安装的扩展。
func (d *Database) ObserveExtensions(facts ExtensionFacts, err error) Action {
	if err != nil {
		d.reportResources(ResourceUnavailable, "无法读取目标数据库的扩展状态")
		return Wait
	}
	requested := d.dependencies.Extensions
	if check := instance.ObserveExtensionSupport(facts.Available).Check(requested); check.Decision != instance.ExtensionsAccepted {
		d.reportResources(ResourceUnavailable, "实例不提供申请的扩展："+strings.Join(check.Unsupported, ", "))
		return Wait
	}
	installed := instance.NewExtensionSet(facts.Installed).Names()
	d.missing = nil
	for _, name := range requested.Names() {
		if _, found := slices.BinarySearch(installed, name); !found {
			d.missing = append(d.missing, name)
		}
	}
	if len(d.missing) != 0 {
		if d.Verifies() {
			d.reportResources(ResourceUnavailable, "缺少申请的扩展："+strings.Join(d.missing, ", ")+"；只读验证的资源不安装扩展")
			return Wait
		}
		return InstallExtensions
	}
	return VerifyLogin
}

// MissingExtensions 是本轮 ObserveExtensions 判定需要安装的扩展。
func (d *Database) MissingExtensions() []string { return slices.Clone(d.missing) }

func (d *Database) ExtensionsInstalled(err error) {
	d.Converged("扩展安装", err)
}

// LoginVerified 记录应用凭据实际登录结果；只有登录成功才算 Database 就绪。
func (d *Database) LoginVerified(err error) {
	if err != nil {
		d.reportResources(ResourceUnavailable, "已确认的应用凭据无法登录目标数据库")
		return
	}
	if d.Verifies() {
		d.register()
		d.snapshot.Binding.Phase = Available
		d.reportResources(ResourceAvailable, "资源已只读验证，应用凭据可登录；可被 Tenant 显式申请")
		return
	}
	d.reportResources(ResourceAvailable, "数据库、角色、权限与扩展已收敛，应用凭据可登录")
}

// Ready 表示资源已收敛且应用凭据本轮实际登录成功。
func (d *Database) Ready() bool { return d.snapshot.Resources.Phase == ResourceAvailable }

// CheckResources 拒绝未知同名对象，以及已确认对象消失、被重建或 owner 不是管理账号/应用角色。
// controller 不检查人工授予的特权或成员关系：除不可恢复情况外不人工改库，这类改动无法区分有意与异常。
func (d *Database) CheckResources(facts ResourceFacts) error {
	state := d.snapshot.Resources
	if state.DatabaseOID != facts.DatabaseOID {
		return fmt.Errorf("数据库身份不匹配：记录 OID=%d，观察 OID=%d", state.DatabaseOID, facts.DatabaseOID)
	}
	if state.RoleOID != facts.RoleOID {
		return fmt.Errorf("角色身份不匹配：记录 OID=%d，观察 OID=%d", state.RoleOID, facts.RoleOID)
	}
	if facts.DatabaseOID != 0 && facts.OwnerOID != facts.ManagerOID && (state.RoleOID == 0 || facts.OwnerOID != state.RoleOID) {
		return errors.New("数据库 owner 既不是管理账号也不是已确认角色")
	}
	return nil
}

func (d *Database) DatabaseCreated(oid uint32, err error) {
	if err == nil && oid == 0 {
		err = ErrUncertain
	}
	if err != nil {
		d.resourceFailure(err, "数据库创建")
		return
	}
	d.snapshot.Resources.DatabaseOID = oid
	d.snapshot.Resources.Phase, d.snapshot.Resources.Message = ResourcePending, "数据库已创建，等待撤销 PUBLIC CONNECT"
}

func (d *Database) RoleCreated(oid uint32, err error) {
	if err == nil && oid == 0 {
		err = ErrUncertain
	}
	if err != nil {
		d.resourceFailure(err, "登录角色创建")
		return
	}
	d.snapshot.Resources.RoleOID = oid
	d.snapshot.Resources.Phase, d.snapshot.Resources.Message = ResourcePending, "登录角色已创建，等待移交数据库 owner"
}

// Converged 记录可重复执行的收敛步骤；失败只等待重试，不需要人工处理。
func (d *Database) Converged(step string, err error) {
	if err != nil {
		d.reportResources(ResourceUnavailable, step+"暂未完成，保留已确认对象并等待重试")
		return
	}
	d.snapshot.Resources.Phase, d.snapshot.Resources.Message = ResourcePending, step+"已完成"
}

// resourceFailure 只对明确的同名冲突要求人工处理；结果不确定时由下一轮观察决定：
// 对象已存在而未记录即为冲突，仍不存在则重试。
func (d *Database) resourceFailure(err error, step string) {
	if errors.Is(err, ErrConflict) {
		d.reportResources(ResourceConflict, step+"遇到同名对象；请核对目标与已确认 OID，未自动认领、改密或清理")
		return
	}
	d.reportResources(ResourceUnavailable, step+"未确认成功，保留已确认步骤，下一轮按实际状态继续")
}

func (d *Database) reportCredential(phase CredentialPhase, message string) {
	d.snapshot.Credential.Phase = phase
	d.snapshot.Credential.Message = fmt.Sprintf("Database %s：%s", d.snapshot.Binding.Identity.Name, message)
}

func (d *Database) reportResources(phase ResourcePhase, message string) {
	b := d.snapshot.Binding
	d.snapshot.Resources.Phase = phase
	d.snapshot.Resources.Message = fmt.Sprintf("Database %s / Instance %s / database %s / role %s：%s",
		b.Identity.Name, b.Instance, b.Name, b.LoginRole, message)
}

type issue struct {
	credential CredentialPhase
	resource   ResourcePhase
	message    string
}

func stopped(message string) *issue { return &issue{CredentialStopped, ResourceStopped, message} }
func unavailable(message string) *issue {
	return &issue{CredentialUnavailable, ResourceUnavailable, message}
}
func conflict(message string) *issue { return &issue{CredentialConflict, ResourceConflict, message} }

// check 判断本轮是否具备供应资格：双向绑定一致、双方受保护、Instance 当前就绪且身份未变。
func (d *Database) check() *issue {
	database, tenant, owner := d.snapshot.Binding, d.dependencies.Tenant, d.dependencies.Instance
	if database.Deleting || database.Phase == binding.Deleting || database.Phase == "Released" {
		return stopped("Database 正在删除或已释放；保留凭据与 finalizer，不执行供应或清理")
	}
	if database.Tenant == nil || tenant == nil || tenant.Database == nil {
		return unavailable("等待 Database 与 Tenant 双向绑定完成")
	}
	if *database.Tenant != tenant.Identity || *tenant.Database != database.Identity {
		return conflict("双向绑定的名称或 UID 不匹配，未继续供应")
	}
	if tenant.Deleting || tenant.Phase != binding.Bound || !d.snapshot.Protected || !d.dependencies.TenantProtected {
		return stopped("Tenant 未完成绑定、正在删除或缺少 finalizer 保护，未继续供应")
	}
	request, err := tenant.Request.Resolve(tenant.Identity)
	if err != nil || (request.Provision != nil && !database.MatchesProvision(request, tenant.Identity)) || request.Name != database.Identity.Name {
		return conflict("Tenant 申请与 Database 目标不一致，未继续供应")
	}
	if owner == nil || database.InstanceUID == "" {
		return unavailable("等待 Instance 与已记录的实例身份")
	}
	if problem := owner.Check(&database); problem != nil {
		if problem.Reason == binding.Conflict {
			return conflict(problem.Message)
		}
		return unavailable(problem.Message)
	}
	return nil
}
