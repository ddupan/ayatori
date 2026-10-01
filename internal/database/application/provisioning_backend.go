package application

import (
	"context"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/database"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

// ResourceDatabase 是供应实际需要的后端能力，不暴露 SQL 或任意回调给 Reconciler。
type ResourceDatabase interface {
	InspectResources(context.Context, *database.Database) (database.ResourceFacts, error)
	CreateLoginRole(context.Context, *database.Database, credential.ApplicationCredential) (uint32, error)
	CreateOwnedDatabase(context.Context, *database.Database) (uint32, error)
	ConfigureAccess(context.Context, *database.Database) error
}

func (s *InstanceService) InspectResources(ctx context.Context, target instance.ObservationTarget, db *database.Database) (database.ResourceFacts, error) {
	var facts database.ResourceFacts
	err := s.withManagementConnection(ctx, target, false, func(connection Database) (err error) {
		facts, err = connection.InspectResources(ctx, db)
		return err
	})
	return facts, err
}

func (s *InstanceService) CreateLoginRole(ctx context.Context, target instance.ObservationTarget, db *database.Database, value credential.ApplicationCredential) (uint32, error) {
	var oid uint32
	err := s.withManagementConnection(ctx, target, true, func(connection Database) (err error) {
		oid, err = connection.CreateLoginRole(ctx, db, value)
		return err
	})
	return oid, err
}

func (s *InstanceService) CreateOwnedDatabase(ctx context.Context, target instance.ObservationTarget, db *database.Database) (uint32, error) {
	var oid uint32
	err := s.withManagementConnection(ctx, target, true, func(connection Database) (err error) {
		oid, err = connection.CreateOwnedDatabase(ctx, db)
		return err
	})
	return oid, err
}

func (s *InstanceService) ConfigureAccess(ctx context.Context, target instance.ObservationTarget, db *database.Database) error {
	return s.withManagementConnection(ctx, target, false, func(connection Database) error {
		return connection.ConfigureAccess(ctx, db)
	})
}

// 所有供应操作与观察共用同一个连接登记和 Secret 刷新边界。
// 创建后凭据回读失败不能冒充明确未执行；Reconciler 必须保留不确定诊断。
func (s *InstanceService) withManagementConnection(ctx context.Context, target instance.ObservationTarget, creating bool, operation func(Database) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.connection(ctx, target)
	if err != nil {
		return database.ErrUnavailable
	}
	metadata, err := current.database.InspectManagement(ctx)
	checks := metadata.Management
	if err != nil || checks.Connection != instance.CheckPassed || checks.Roles != instance.CheckPassed || checks.Databases != instance.CheckPassed || checks.Grants != instance.CheckPassed {
		return database.ErrUnavailable
	}
	if err := s.checkCredentials(ctx, target, current.credentials); err != nil {
		return database.ErrUnavailable
	}
	if err := operation(current.database); err != nil {
		return err
	}
	if err := s.checkCredentials(ctx, target, current.credentials); err != nil {
		if creating {
			return database.ErrUncertain
		}
		return database.ErrUnavailable
	}
	return nil
}
