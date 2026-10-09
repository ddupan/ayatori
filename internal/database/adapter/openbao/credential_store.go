package openbao

import (
	"context"
	"errors"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
)

func (c *Credentials) ProvisionLocation(uid string) (credentialdomain.Location, error) {
	path, err := c.ProvisionPath(uid)
	if err != nil {
		return credentialdomain.Location{}, err
	}
	return credentialdomain.Location{Mount: c.mount, Path: path}, nil
}

func (c *Credentials) ReadCredential(ctx context.Context, location credentialdomain.Location, version int64) (credentialdomain.ApplicationCredential, error) {
	if location.Mount != c.mount {
		return credentialdomain.ApplicationCredential{}, ErrInvalidLocation
	}
	if version == 0 {
		return c.Read(ctx, location.Path)
	}
	return c.ReadConfirmed(ctx, location.Path, version)
}

func (c *Credentials) CreateCredential(ctx context.Context, location credentialdomain.Location, credential credentialdomain.ApplicationCredential) (int64, error) {
	if location.Mount != c.mount {
		return 0, ErrInvalidLocation
	}
	if err := c.Create(ctx, location.Path, credential); err != nil {
		return 0, err
	}
	// Create 的合同是 CAS=0 且回读版本 1 和七键完全一致。
	return 1, nil
}

// UpdateCredential 以确认版本为前提写入下一版本（改写连接信息），返回新版本号。
func (c *Credentials) UpdateCredential(ctx context.Context, location credentialdomain.Location, confirmed int64, credential credentialdomain.ApplicationCredential) (int64, error) {
	if location.Mount != c.mount {
		return 0, ErrInvalidLocation
	}
	return c.Update(ctx, location.Path, confirmed, credential)
}

// DestroyCredential 永久删除固定位置的全部版本与 metadata，并回读确认已不存在。
// 只在 Delete 回收时调用；位置必须是本适配器配置的 mount 与前缀。
func (c *Credentials) DestroyCredential(ctx context.Context, location credentialdomain.Location) error {
	if location.Mount != c.mount || !c.accepts(location.Path) {
		return ErrInvalidLocation
	}
	if err := c.kv.DeleteMetadata(ctx, location.Path); err != nil {
		return ErrUnavailable
	}
	if _, err := c.read(ctx, location.Path); !errors.Is(err, ErrNotFound) {
		return ErrUncertain
	}
	return nil
}

// ReadLatestCredential 读取导入时管理员声明位置的最新版本及其版本号；登记后改用确认版本读取。
func (c *Credentials) ReadLatestCredential(ctx context.Context, location credentialdomain.Location) (credentialdomain.ApplicationCredential, int64, error) {
	if location.Mount != c.mount {
		return credentialdomain.ApplicationCredential{}, 0, ErrInvalidLocation
	}
	secret, err := c.read(ctx, location.Path)
	if err != nil {
		return credentialdomain.ApplicationCredential{}, 0, err
	}
	if secret.VersionMetadata == nil || secret.VersionMetadata.Version < 1 {
		return credentialdomain.ApplicationCredential{}, 0, ErrUnavailable
	}
	value, err := credentialdomain.ParseApplicationCredential(secret.Data)
	if err != nil {
		return credentialdomain.ApplicationCredential{}, 0, ErrConflict
	}
	return value, int64(secret.VersionMetadata.Version), nil
}
