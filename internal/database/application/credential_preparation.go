package application

import (
	"context"
	"errors"
	"fmt"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

var (
	ErrCredentialLocation    = errors.New("credential location is outside the configured scope")
	ErrCredentialUnavailable = errors.New("credential backend unavailable")
	ErrCredentialNotFound    = errors.New("application credential not found")
	ErrCredentialConflict    = errors.New("credential creation requires manual conflict resolution")
	ErrCredentialUncertain   = errors.New("credential creation outcome is uncertain; manual resolution required")
)

const (
	CredentialsReady          = "CredentialsReady"
	CredentialCreationStarted = "CreationStarted"
	CredentialPrepared        = "CredentialPrepared"
)

type CredentialLocation struct {
	Mount string
	Path  string
}

// CredentialStore 只表达本用例需要的凭据操作，不提供覆盖或删除。
// version=0 的读取只用于观察是否已有值，成功不能作为认领依据。
type CredentialStore interface {
	ProvisionLocation(string) (CredentialLocation, error)
	ReadCredential(context.Context, CredentialLocation, int64) (ApplicationCredential, error)
	CreateCredential(context.Context, CredentialLocation, ApplicationCredential) (int64, error)
}

type CredentialInstance struct {
	binding.Instance
	Generation int64
	Endpoint   instance.Endpoint
}

// CredentialRecord 是同一轮观察的事实，状态中永远不保存密码。
type CredentialRecord struct {
	Database          BindingDatabase
	Tenant            *BindingTenant
	Instance          *CredentialInstance
	DatabaseProtected bool
	TenantProtected   bool
	Status            CredentialStatus
}

type CredentialStatus struct {
	Location *CredentialLocation
	Version  int64
	Ready    bool
	Reason   string
	Message  string
}

// CredentialResources 的写入必须检查 Database UID/resourceVersion，保留其他状态。
// CheckCurrent 在外部操作前后回读本轮三个资源，拒绝陈旧快照；它不是跨系统事务。
type CredentialResources interface {
	Load(context.Context, string) (*CredentialRecord, error)
	Save(context.Context, *CredentialRecord, CredentialStatus) (*CredentialRecord, error)
	CheckCurrent(context.Context, *CredentialRecord) error
}

type CredentialPreparation struct {
	Resources CredentialResources
	Store     CredentialStore
}

func (s CredentialPreparation) Reconcile(ctx context.Context, name string) error {
	record, err := s.Resources.Load(ctx, name)
	if err != nil || record == nil || record.Database.Source != "Provision" {
		return err
	}
	// 未完成创建的重入不猜测后端结果。即使进程在实际发请求前退出，也需要人工核实。
	if record.Status.Version == 0 && record.Status.Reason == binding.Conflict {
		return nil // 保留首次冲突的具体原因，不因后端恢复而重入创建。
	}
	if record.Status.Version == 0 && record.Status.Reason == CredentialCreationStarted {
		return s.report(ctx, record, binding.Conflict,
			"凭据创建未留下成功确认；请核对固定位置与后端历史并人工处理，未重新生成密码")
	}
	if issue := record.check(); issue != nil {
		return s.report(ctx, record, issue.Reason, issue.Message)
	}
	location, err := s.Store.ProvisionLocation(record.Database.Identity.UID)
	if err != nil {
		return s.report(ctx, record, binding.DependencyUnavailable, "凭据存储位置配置无效，未执行外部写入")
	}
	if record.Status.Location == nil {
		status := record.Status
		status.Location = &location
		status.Ready, status.Reason, status.Message = false, "LocationPinned", "凭据位置已固定，等待创建"
		record, err = s.Resources.Save(ctx, record, status)
		if err != nil {
			return err
		}
	} else if *record.Status.Location != location {
		return s.report(ctx, record, binding.DependencyUnavailable,
			"部署配置与固定凭据位置不一致；请恢复原 mount/path 配置，未迁移或改密")
	}
	if record.Status.Version > 0 {
		return s.observe(ctx, record)
	}
	return s.create(ctx, record)
}

func (s CredentialPreparation) create(ctx context.Context, record *CredentialRecord) error {
	_, err := s.Store.ReadCredential(ctx, *record.Status.Location, 0)
	if err == nil || errors.Is(err, ErrApplicationCredentialInvalid) {
		return s.report(ctx, record, binding.Conflict, "固定位置已有未确认的凭据；请人工核实，未认领或覆盖")
	}
	if !errors.Is(err, ErrCredentialNotFound) {
		return s.report(ctx, record, binding.DependencyUnavailable, "创建前无法确认凭据位置是否为空，等待依赖恢复")
	}
	credential, err := GenerateApplicationCredential(record.Database.LoginRole, record.Database.Name, record.Instance.Endpoint)
	if err != nil {
		return s.report(ctx, record, "InvalidTarget", "应用凭据目标无效，未执行外部写入")
	}
	status := record.Status
	status.Ready, status.Reason = false, CredentialCreationStarted
	status.Message = "凭据创建已开始；尚无成功确认时不得重入创建"
	record, err = s.Resources.Save(ctx, record, status)
	if err != nil {
		return err
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	version, err := s.Store.CreateCredential(ctx, *record.Status.Location, credential)
	if errors.Is(err, ErrCredentialUnavailable) {
		// 适配器只在明确未执行写入（认证拒绝或请求前取消）时返回此错误。
		return s.report(ctx, record, binding.DependencyUnavailable, "凭据创建在执行前被拒绝，等待认证或权限恢复")
	}
	if err != nil || version != 1 {
		return s.report(ctx, record, binding.Conflict,
			"凭据创建冲突或结果不确定；请核对固定位置的版本历史，未认领、覆盖或重新生成密码")
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	status = record.Status
	status.Version, status.Ready, status.Reason = version, true, CredentialPrepared
	status.Message = "凭据已创建并回读确认；尚未创建 PostgreSQL 资源或交付给 Tenant"
	_, err = s.Resources.Save(ctx, record, status)
	return err
}

func (s CredentialPreparation) observe(ctx context.Context, record *CredentialRecord) error {
	credential, err := s.Store.ReadCredential(ctx, *record.Status.Location, record.Status.Version)
	if errors.Is(err, ErrCredentialConflict) || errors.Is(err, ErrCredentialNotFound) {
		return s.report(ctx, record, binding.Conflict, "已确认凭据消失、版本变化或内容无效；请人工核实，未生成替代密码")
	}
	if err != nil {
		return s.report(ctx, record, binding.DependencyUnavailable, "已确认凭据暂时无法读取；保留确认版本，等待依赖恢复")
	}
	if !credential.MatchesTarget(record.Database.LoginRole, record.Database.Name, record.Instance.Endpoint) {
		return s.report(ctx, record, binding.Conflict, "已确认凭据与当前 Instance/database/loginRole 不一致；请人工核实，未修改凭据")
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	status := record.Status
	status.Ready, status.Reason = true, CredentialPrepared
	status.Message = "已确认凭据可读取；尚未验证 PostgreSQL 资源或完成 Tenant 交付"
	_, err = s.Resources.Save(ctx, record, status)
	return err
}

func (s CredentialPreparation) report(ctx context.Context, record *CredentialRecord, reason, message string) error {
	status := record.Status
	status.Ready, status.Reason = false, reason
	status.Message = fmt.Sprintf("Database %s：%s", record.Database.Identity.Name, message)
	_, err := s.Resources.Save(ctx, record, status)
	return err
}
