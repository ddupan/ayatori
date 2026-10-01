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
	CreateDatabase(context.Context, *database.Database) (uint32, error)
	RevokePublicConnect(context.Context, *database.Database) error
	CreateLoginRole(context.Context, *database.Database, credential.ApplicationCredential) (uint32, error)
	TransferOwnership(context.Context, *database.Database) error
}

func (s *InstanceService) InspectResources(ctx context.Context, target instance.ObservationTarget, db *database.Database) (database.ResourceFacts, error) {
	var facts database.ResourceFacts
	err := s.withManagementConnection(ctx, target, func(connection Database) (err error) {
		facts, err = connection.InspectResources(ctx, db)
		return err
	})
	return facts, err
}

func (s *InstanceService) CreateDatabase(ctx context.Context, target instance.ObservationTarget, db *database.Database) (uint32, error) {
	var oid uint32
	err := s.withManagementConnection(ctx, target, func(connection Database) (err error) {
		oid, err = connection.CreateDatabase(ctx, db)
		return err
	})
	return oid, err
}

func (s *InstanceService) RevokePublicConnect(ctx context.Context, target instance.ObservationTarget, db *database.Database) error {
	return s.withManagementConnection(ctx, target, func(connection Database) error {
		return connection.RevokePublicConnect(ctx, db)
	})
}

func (s *InstanceService) CreateLoginRole(ctx context.Context, target instance.ObservationTarget, db *database.Database, value credential.ApplicationCredential) (uint32, error) {
	var oid uint32
	err := s.withManagementConnection(ctx, target, func(connection Database) (err error) {
		oid, err = connection.CreateLoginRole(ctx, db, value)
		return err
	})
	return oid, err
}

func (s *InstanceService) TransferOwnership(ctx context.Context, target instance.ObservationTarget, db *database.Database) error {
	return s.withManagementConnection(ctx, target, func(connection Database) error {
		return connection.TransferOwnership(ctx, db)
	})
}

// 所有供应操作与观察共用同一个连接登记和 Secret 刷新边界，执行前重新确认原生管理能力。
// 不持有登记锁：不同 Instance、不同 Database 的操作并发执行，同一对象由 controller 串行。
func (s *InstanceService) withManagementConnection(ctx context.Context, target instance.ObservationTarget, operation func(Database) error) error {
	current, err := s.connection(ctx, target)
	if err != nil {
		return database.ErrUnavailable
	}
	metadata, err := current.database.InspectManagement(ctx)
	checks := metadata.Management
	if err != nil || checks.Connection != instance.CheckPassed || checks.Roles != instance.CheckPassed || checks.Databases != instance.CheckPassed || checks.Grants != instance.CheckPassed {
		return database.ErrUnavailable
	}
	if err := s.checkCredentials(ctx, current); err != nil {
		return database.ErrUnavailable
	}
	return operation(current.database)
}
