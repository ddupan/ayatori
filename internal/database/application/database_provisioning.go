package application

import (
	"context"
	"errors"
	"fmt"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/provisioning"
)

type ProvisioningRecord struct {
	credential.Target
	Revision           string
	TenantGeneration   int64
	InstanceGeneration int64
	InstanceTarget     instance.ObservationTarget
	Credentials        credential.State
	State              provisioning.State
}

type ProvisioningResources interface {
	Load(context.Context, string) (*ProvisioningRecord, error)
	Save(context.Context, *ProvisioningRecord, provisioning.State) (*ProvisioningRecord, error)
	CheckCurrent(context.Context, *ProvisioningRecord) error
}

type DatabaseProvisioning struct {
	Resources   ProvisioningResources
	Credentials CredentialStore
	Backend     ProvisioningBackend
}

func (s DatabaseProvisioning) Reconcile(ctx context.Context, name string) error {
	record, err := s.Resources.Load(ctx, name)
	if err != nil || record == nil || !record.RequiresPreparation() {
		return err
	}
	if state, resume := record.State.Resume(); !resume {
		if state == record.State {
			return nil
		}
		_, err := s.Resources.Save(ctx, record, state)
		return err
	}
	if issue := record.Check(); issue != nil {
		phase := provisioning.Unavailable
		if issue.Phase == credential.Conflict {
			phase = provisioning.Conflict
		}
		if issue.Phase == credential.Stopped {
			phase = provisioning.Stopped
		}
		return s.report(ctx, record, phase, issue.Message)
	}
	if !record.Credentials.Confirmed() || record.Credentials.Location == nil || record.Credentials.Phase != credential.Prepared {
		return s.report(ctx, record, provisioning.Unavailable, "等待已确认且当前可用的应用凭据")
	}
	value, err := s.Credentials.ReadCredential(ctx, *record.Credentials.Location, record.Credentials.Version)
	if err != nil {
		return s.report(ctx, record, provisioning.Unavailable, "无法读取已确认凭据；未执行 PostgreSQL 写入")
	}
	if issue := record.CheckCredential(value); issue != nil {
		return s.report(ctx, record, provisioning.Conflict, issue.Message)
	}
	observed, err := s.Backend.InspectResources(ctx, record.InstanceTarget, record.Database.Name, record.Database.LoginRole)
	if err != nil {
		return s.report(ctx, record, provisioning.Unavailable, "无法验证当前 PostgreSQL 资源和管理权限")
	}
	if err := record.State.Check(observed); err != nil {
		return s.report(ctx, record, provisioning.Conflict, err.Error()+"；请人工核对，未认领或覆盖")
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	if record.State.RoleOID == 0 {
		return s.createRole(ctx, record, value)
	}
	if record.State.DatabaseOID == 0 {
		return s.createDatabase(ctx, record)
	}
	if err := s.Backend.ConfigureAccess(ctx, record.InstanceTarget, record.Database.Name, record.Database.LoginRole, record.State); err != nil {
		return s.backendFailure(ctx, record, err, "数据库访问权限收敛")
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	return s.report(ctx, record, provisioning.Available, "角色和数据库已确认，PUBLIC CONNECT 已撤销；扩展与 Tenant 交付尚未完成")
}

func (s DatabaseProvisioning) createRole(ctx context.Context, record *ProvisioningRecord, value credential.ApplicationCredential) error {
	record, err := s.Resources.Save(ctx, record, record.State.WithPhase(provisioning.CreatingRole, "开始创建登录角色；尚未持久确认"))
	if err != nil {
		return err
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	oid, err := s.Backend.CreateLoginRole(ctx, record.InstanceTarget, value)
	if err != nil {
		return s.backendFailure(ctx, record, err, "登录角色创建")
	}
	if oid == 0 {
		return s.backendFailure(ctx, record, ErrResourceUncertain, "登录角色创建")
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	state := record.State.WithPhase(provisioning.Pending, "登录角色已创建并回读，等待创建数据库")
	state.RoleOID = oid
	_, err = s.Resources.Save(ctx, record, state)
	return err
}

func (s DatabaseProvisioning) createDatabase(ctx context.Context, record *ProvisioningRecord) error {
	record, err := s.Resources.Save(ctx, record, record.State.WithPhase(provisioning.CreatingDatabase, "开始创建数据库；尚未持久确认"))
	if err != nil {
		return err
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	oid, err := s.Backend.CreateOwnedDatabase(ctx, record.InstanceTarget, record.Database.Name, record.Database.LoginRole, record.State.RoleOID)
	if err != nil {
		return s.backendFailure(ctx, record, err, "数据库创建")
	}
	if oid == 0 {
		return s.backendFailure(ctx, record, ErrResourceUncertain, "数据库创建")
	}
	if err := s.Resources.CheckCurrent(ctx, record); err != nil {
		return err
	}
	state := record.State.WithPhase(provisioning.Pending, "数据库已创建并回读，等待收紧访问权限；连接入口仍关闭")
	state.DatabaseOID = oid
	_, err = s.Resources.Save(ctx, record, state)
	return err
}

func (s DatabaseProvisioning) backendFailure(ctx context.Context, record *ProvisioningRecord, err error, step string) error {
	if errors.Is(err, ErrResourceUnavailable) {
		return s.report(ctx, record, provisioning.Unavailable, step+"被拒绝或暂不可用，保留已确认步骤并等待依赖恢复")
	}
	return s.report(ctx, record, provisioning.Conflict, step+"冲突或结果不确定；请核对目标与已确认 OID，未自动认领、改密或清理")
}

func (s DatabaseProvisioning) report(ctx context.Context, record *ProvisioningRecord, phase provisioning.Phase, message string) error {
	message = fmt.Sprintf("Database %s / Instance %s / database %s / role %s：%s", record.Database.Identity.Name,
		record.Database.Instance, record.Database.Name, record.Database.LoginRole, message)
	_, err := s.Resources.Save(ctx, record, record.State.WithPhase(phase, message))
	return err
}
