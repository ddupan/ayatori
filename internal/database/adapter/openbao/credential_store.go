package openbao

import (
	"context"

	credentialdomain "git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
)

var _ application.CredentialStore = (*Credentials)(nil)

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
