package application

import (
	"context"
	"errors"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func TestInstanceFailurePresentation(t *testing.T) {
	cases := []struct {
		err    error
		reason string
	}{
		{ErrAuthentication, "AuthenticationFailed"},
		{ErrCredentialsInvalid, "InvalidCredentials"},
		{ErrCredentialsChanged, "CredentialsChanged"},
		{ErrCredentialsUnavailable, instanceDependencyUnavailable},
		{context.DeadlineExceeded, instanceDependencyUnavailable},
		{errors.New("private backend detail"), instanceDependencyUnavailable},
	}
	for _, test := range cases {
		reason, message := observationFailure(test.err)
		if reason != test.reason || message == "" || message == test.err.Error() {
			t.Fatal("观察失败没有安全且可诊断的状态")
		}
	}
	for _, failure := range []instance.Failure{
		instance.NoFailure, instance.ObservationIncomplete, instance.DependencyUnavailable,
		instance.AuthenticationFailed, instance.InsufficientPrivileges,
	} {
		reason, message := managementResult(failure)
		if reason == "" || message == "" || (reason == "ManagementReady") != (failure == instance.NoFailure) {
			t.Fatal("领域能力判定与状态不一致")
		}
	}
}

func TestMetadataCannotEstablishManagementReadiness(t *testing.T) {
	source := &sourceStub{}
	source.credentials, _ = NewCredentials("test", serviceTestPassword)
	connector := &connectorStub{}
	service, err := NewInstanceService(source, connector)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	target := serviceTarget(t, "uid", "postgres.test", "management", 1)
	if _, err := service.ObserveMetadata(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	connector.databases[0].metadata.Management = instance.ManagementChecks{
		Connection: instance.CheckPassed, Metadata: instance.CheckPassed,
		Roles: instance.CheckPassed, Databases: instance.CheckPassed,
		Grants: instance.CheckPassed, Extensions: instance.CheckPassed,
	}
	observation, err := service.ObserveMetadata(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := observation.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := instance.Reconstitute(target, instance.Snapshot{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := aggregate.BeginValidation(); err != nil {
		t.Fatal(err)
	}
	if err := aggregate.AssessManagement(capabilities); err != nil {
		t.Fatal(err)
	}
	if aggregate.Snapshot().Failure != instance.ObservationIncomplete {
		t.Fatal("metadata 入口不应携带完整管理检查")
	}
}
