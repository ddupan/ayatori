package kubernetes

import (
	databasev1alpha1 "git.ddupan.top/panxiao81/ayatori/api/database/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func instanceRecord(object *databasev1alpha1.PostgreSQLInstance) (*application.InstanceRecord, error) {
	identity, err := instance.NewIdentity(string(object.UID), object.Name)
	if err != nil {
		return nil, err
	}
	revision, err := instance.NewRevision(object.Generation)
	if err != nil {
		return nil, err
	}
	spec := object.Spec
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host: spec.Endpoint.Host, HostAddr: spec.Endpoint.HostAddr,
		Port: int(spec.Endpoint.Port), ManagementDatabase: string(spec.Endpoint.Database),
		TLSMode: instance.TLSMode(spec.Endpoint.SSLMode),
	})
	if err != nil {
		return nil, err
	}
	credential, err := instance.NewCredentialReference(instance.CredentialReferenceValues{
		Name:        string(spec.AdminCredentialRef.Name),
		UsernameKey: spec.AdminCredentialRef.UsernameKey, PasswordKey: spec.AdminCredentialRef.PasswordKey,
	})
	if err != nil {
		return nil, err
	}
	definition, err := instance.NewDefinition(endpoint, credential)
	if err != nil {
		return nil, err
	}
	target, err := instance.NewObservationTarget(identity, revision, definition)
	if err != nil {
		return nil, err
	}
	return &application.InstanceRecord{
		Target: target, Revision: object.ResourceVersion, Deleting: !object.DeletionTimestamp.IsZero(),
	}, nil
}
