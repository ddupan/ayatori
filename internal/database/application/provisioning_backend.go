package application

import (
	"context"
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/provisioning"
)

var (
	ErrResourceConflict    = errors.New("PostgreSQL resource conflict; manual resolution required")
	ErrResourceUncertain   = errors.New("PostgreSQL creation outcome uncertain; manual resolution required")
	ErrResourceUnavailable = errors.New("PostgreSQL operation unavailable before creation")
)

// ResourceDatabase 是供应实际需要的后端能力，不暴露 SQL 或任意回调给用例。
type ResourceDatabase interface {
	InspectResources(context.Context, string, string) (provisioning.Observation, error)
	CreateLoginRole(context.Context, credential.ApplicationCredential) (uint32, error)
	CreateOwnedDatabase(context.Context, string, string, uint32) (uint32, error)
	ConfigureAccess(context.Context, string, string, provisioning.State) error
}

type ProvisioningBackend interface {
	InspectResources(context.Context, instance.ObservationTarget, string, string) (provisioning.Observation, error)
	CreateLoginRole(context.Context, instance.ObservationTarget, credential.ApplicationCredential) (uint32, error)
	CreateOwnedDatabase(context.Context, instance.ObservationTarget, string, string, uint32) (uint32, error)
	ConfigureAccess(context.Context, instance.ObservationTarget, string, string, provisioning.State) error
}

func (s *InstanceService) InspectResources(ctx context.Context, target instance.ObservationTarget, name, role string) (provisioning.Observation, error) {
	var result provisioning.Observation
	err := s.withManagementConnection(ctx, target, false, func(database Database) (err error) {
		result, err = database.InspectResources(ctx, name, role)
		return err
	})
	if err != nil {
		return provisioning.Observation{}, err
	}
	return result, nil
}

func (s *InstanceService) CreateLoginRole(ctx context.Context, target instance.ObservationTarget, value credential.ApplicationCredential) (uint32, error) {
	var oid uint32
	err := s.withManagementConnection(ctx, target, true, func(database Database) (err error) {
		oid, err = database.CreateLoginRole(ctx, value)
		return err
	})
	if err != nil {
		return 0, err
	}
	return oid, nil
}

func (s *InstanceService) CreateOwnedDatabase(ctx context.Context, target instance.ObservationTarget, name, role string, roleOID uint32) (uint32, error) {
	var oid uint32
	err := s.withManagementConnection(ctx, target, true, func(database Database) (err error) {
		oid, err = database.CreateOwnedDatabase(ctx, name, role, roleOID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return oid, nil
}

func (s *InstanceService) ConfigureAccess(ctx context.Context, target instance.ObservationTarget, name, role string, state provisioning.State) error {
	return s.withManagementConnection(ctx, target, false, func(database Database) error {
		return database.ConfigureAccess(ctx, name, role, state)
	})
}

// 所有供应操作与观察共用同一个连接登记和 Secret 刷新边界。
// 创建后凭据回读失败不能冒充明确未执行；用例必须保留不确定诊断。
func (s *InstanceService) withManagementConnection(ctx context.Context, target instance.ObservationTarget, creating bool, operation func(Database) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.connection(ctx, target)
	if err != nil {
		return ErrResourceUnavailable
	}
	metadata, err := current.database.InspectManagement(ctx)
	checks := metadata.Management
	if err != nil || checks.Connection != instance.CheckPassed || checks.Roles != instance.CheckPassed || checks.Databases != instance.CheckPassed || checks.Grants != instance.CheckPassed {
		return ErrResourceUnavailable
	}
	if err := s.checkCredentials(ctx, target, current.credentials); err != nil {
		return ErrResourceUnavailable
	}
	if err := operation(current.database); err != nil {
		return err
	}
	if err := s.checkCredentials(ctx, target, current.credentials); err != nil {
		if creating {
			return ErrResourceUncertain
		}
		return ErrResourceUnavailable
	}
	return nil
}
