package application

import (
	"context"
	"errors"
	"fmt"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
)

var (
	ErrCredentialLocation    = errors.New("credential location is outside the configured scope")
	ErrCredentialUnavailable = errors.New("credential backend unavailable")
	ErrCredentialNotFound    = errors.New("application credential not found")
	ErrCredentialConflict    = errors.New("credential creation requires manual conflict resolution")
	ErrCredentialUncertain   = errors.New("credential creation outcome is uncertain; manual resolution required")
)

// CredentialStore 只表达本用例需要的凭据操作，不提供覆盖或删除。
// version=0 的读取只用于观察是否已有值，成功不能作为认领依据。
type CredentialStore interface {
	ProvisionLocation(string) (credentialdomain.Location, error)
	ReadCredential(context.Context, credentialdomain.Location, int64) (credentialdomain.ApplicationCredential, error)
	CreateCredential(context.Context, credentialdomain.Location, credentialdomain.ApplicationCredential) (int64, error)
}

// CredentialRecord 是同一轮观察的事实，状态中永远不保存密码。
type CredentialRecord struct {
	credentialdomain.Target
	Revision           string
	TenantGeneration   int64
	InstanceGeneration int64
	Status             credentialdomain.State
}

// CredentialResources 的写入必须检查 Database UID/resourceVersion，保留其他状态。
// CheckCurrent 在外部操作前后回读本轮三个资源，拒绝陈旧快照；它不是跨系统事务。
type CredentialResources interface {
	Load(context.Context, string) (*CredentialRecord, error)
	Save(context.Context, *CredentialRecord, credentialdomain.State) (*CredentialRecord, error)
	CheckCurrent(context.Context, *CredentialRecord) error
}

type CredentialPreparation struct {
	Resources CredentialResources
	Store     CredentialStore
}

func (s CredentialPreparation) Reconcile(ctx context.Context, name string) error {
	record, err := s.Resources.Load(ctx, name)
	if err != nil || record == nil || !record.RequiresPreparation() {
		return err
	}
	if state, canContinue := record.Status.Resume(); !canContinue {
		if state == record.Status {
			return nil
		}
		return s.report(ctx, record, state.Phase, state.Message)
	}
	if issue := record.Check(); issue != nil {
		return s.report(ctx, record, issue.Phase, issue.Message)
	}
	location, err := s.Store.ProvisionLocation(record.Database.Identity.UID)
	if err != nil {
		return s.report(ctx, record, credentialdomain.Unavailable, "凭据存储位置配置无效，未执行外部写入")
	}
	if issue := record.Status.CheckLocation(location); issue != nil {
		return s.report(ctx, record, issue.Phase, issue.Message)
	}
	if record.Status.Location == nil {
		status := record.Status
		status.Location = &location
		status = status.WithPhase(credentialdomain.Pinned, "凭据位置已固定，等待创建")
		record, err = s.Resources.Save(ctx, record, status)
		if err != nil {
			return err
		}
	}
	if record.Status.Confirmed() {
		return s.observe(ctx, record)
	}
	return s.create(ctx, record)
}

func (s CredentialPreparation) create(ctx context.Context, record *CredentialRecord) error {
	_, err := s.Store.ReadCredential(ctx, *record.Status.Location, 0)
	if err == nil || errors.Is(err, credentialdomain.ErrApplicationCredentialInvalid) {
		return s.report(ctx, record, credentialdomain.Conflict, "固定位置已有未确认的凭据；请人工核实，未认领或覆盖")
	}
	if !errors.Is(err, ErrCredentialNotFound) {
		return s.report(ctx, record, credentialdomain.Unavailable, "创建前无法确认凭据位置是否为空，等待依赖恢复")
	}
	credential, err := credentialdomain.GenerateApplicationCredential(record.Database.LoginRole, record.Database.Name, record.Instance.Endpoint)
	if err != nil {
		return s.report(ctx, record, credentialdomain.InvalidTarget, "应用凭据目标无效，未执行外部写入")
	}
	status := record.Status
	status = status.WithPhase(credentialdomain.Creating, "凭据创建已开始；尚无成功确认时不得重入创建")
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
		return s.report(ctx, record, credentialdomain.Unavailable, "凭据创建在执行前被拒绝，等待认证或权限恢复")
	}
	if err != nil {
		return s.report(ctx, record, credentialdomain.Conflict,
			"凭据创建冲突或结果不确定；请核对固定位置的版本历史，未认领、覆盖或重新生成密码")
	}
	confirmed, issue := record.Status.Created(version)
	if issue != nil {
		return s.report(ctx, record, issue.Phase, issue.Message)
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	_, err = s.Resources.Save(ctx, record, confirmed)
	return err
}

func (s CredentialPreparation) observe(ctx context.Context, record *CredentialRecord) error {
	credential, err := s.Store.ReadCredential(ctx, *record.Status.Location, record.Status.Version)
	if errors.Is(err, ErrCredentialConflict) || errors.Is(err, ErrCredentialNotFound) {
		return s.report(ctx, record, credentialdomain.Conflict, "已确认凭据消失、版本变化或内容无效；请人工核实，未生成替代密码")
	}
	if err != nil {
		return s.report(ctx, record, credentialdomain.Unavailable, "已确认凭据暂时无法读取；保留确认版本，等待依赖恢复")
	}
	if issue := record.CheckCredential(credential); issue != nil {
		return s.report(ctx, record, issue.Phase, issue.Message)
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	status := record.Status
	status.Phase = credentialdomain.Prepared
	status.Message = "已确认凭据可读取；尚未验证 PostgreSQL 资源或完成 Tenant 交付"
	_, err = s.Resources.Save(ctx, record, status)
	return err
}

func (s CredentialPreparation) report(ctx context.Context, record *CredentialRecord, phase credentialdomain.Phase, message string) error {
	status := record.Status
	status.Phase = phase
	status.Message = fmt.Sprintf("Database %s：%s", record.Database.Identity.Name, message)
	_, err := s.Resources.Save(ctx, record, status)
	return err
}
