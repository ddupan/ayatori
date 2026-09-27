package openbao

import (
	"context"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
)

var _ application.CredentialStore = (*Credentials)(nil)

func (c *Credentials) ProvisionLocation(uid string) (application.CredentialLocation, error) {
	path, err := c.ProvisionPath(uid)
	if err != nil {
		return application.CredentialLocation{}, err
	}
	return application.CredentialLocation{Mount: c.mount, Path: path}, nil
}

func (c *Credentials) ReadCredential(ctx context.Context, location application.CredentialLocation, version int64) (application.ApplicationCredential, error) {
	if location.Mount != c.mount {
		return application.ApplicationCredential{}, ErrInvalidLocation
	}
	if version == 0 {
		return c.Read(ctx, location.Path)
	}
	return c.ReadConfirmed(ctx, location.Path, version)
}

func (c *Credentials) CreateCredential(ctx context.Context, location application.CredentialLocation, credential application.ApplicationCredential) (int64, error) {
	if location.Mount != c.mount {
		return 0, ErrInvalidLocation
	}
	if err := c.Create(ctx, location.Path, credential); err != nil {
		return 0, err
	}
	// Create 的合同是 CAS=0 且回读版本 1 和七键完全一致。
	return 1, nil
}
