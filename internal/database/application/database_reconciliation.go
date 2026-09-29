package application

import "context"

// DatabaseReconciliation 顺序编排两个已有用例，不复制它们的领域判断。
// 启用资源创建时由同一个 controller 驱动，避免两个 worker 争写同一 Database status。
type DatabaseReconciliation struct {
	Credentials  *CredentialPreparation
	Provisioning *DatabaseProvisioning
}

func (s DatabaseReconciliation) Reconcile(ctx context.Context, name string) error {
	if err := s.Credentials.Reconcile(ctx, name); err != nil {
		return err
	}
	return s.Provisioning.Reconcile(ctx, name)
}
