/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package application

import (
	"context"
	"errors"
	"testing"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const serviceTestPassword = "test-only"

// 延续源项目 Service 测试，用于穷举身份与装配失败；真实行为由 adapter 集成测试验证。
type sourceStub struct {
	credentials Credentials
	err         error
}

func (s *sourceStub) Read(context.Context, instance.CredentialReference) (Credentials, error) {
	return s.credentials, s.err
}

type databaseStub struct {
	closes   int
	err      error
	metadata DatabaseMetadata
}

func (d *databaseStub) InspectManagement(ctx context.Context) (DatabaseMetadata, error) {
	return d.InspectMetadata(ctx)
}

func (d *databaseStub) InspectMetadata(context.Context) (DatabaseMetadata, error) {
	return d.metadata, d.err
}
func (d *databaseStub) Close() {
	d.closes++
}

type connectorStub struct {
	databases []*databaseStub
	err       error
}

func (c *connectorStub) Connect(context.Context, instance.Endpoint, Credentials) (Database, error) {
	if c.err != nil {
		return nil, c.err
	}
	db := &databaseStub{
		metadata: DatabaseMetadata{
			Version:             "17",
			AvailableExtensions: []string{"plpgsql"},
		},
	}
	c.databases = append(c.databases, db)
	return db, nil
}

func serviceTarget(t *testing.T, uid, host, secret string, generation int64) instance.ObservationTarget {
	t.Helper()
	id, err := instance.NewIdentity(uid, "shared")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := instance.NewRevision(generation)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host:               host,
		HostAddr:           "127.0.0.1",
		Port:               5432,
		ManagementDatabase: "postgres",
		TLSMode:            instance.TLSDisable,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := instance.NewCredentialReference(instance.CredentialReferenceValues{
		Name:        secret,
		UsernameKey: "user",
		PasswordKey: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := instance.NewDefinition(endpoint, ref)
	if err != nil {
		t.Fatal(err)
	}
	target, err := instance.NewObservationTarget(id, revision, definition)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestInstanceConnectionIdentity(t *testing.T) {
	source := &sourceStub{credentials: Credentials{username: testUsername, password: serviceTestPassword}}
	connector := &connectorStub{}
	service, err := NewInstanceService(source, connector)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx := context.Background()
	cases := []struct {
		name            string
		target          instance.ObservationTarget
		wantConnections int
	}{
		{"initial connection", serviceTarget(t, "uid-1", "first", "admin", 1), 1},
		{"generation alone", serviceTarget(t, "uid-1", "first", "admin", 2), 1},
		{"endpoint changed", serviceTarget(t, "uid-1", "second", "admin", 3), 2},
		{"reference changed", serviceTarget(t, "uid-1", "second", "replacement", 4), 3},
		{"same name with new UID", serviceTarget(t, "uid-2", "second", "replacement", 1), 4},
	}
	for _, testCase := range cases {
		observation, err := service.ObserveMetadata(ctx, testCase.target)
		if err != nil {
			t.Fatal(err)
		}
		if !observation.Target().Matches(testCase.target) {
			t.Fatalf("%s: observation was bound to a previous target", testCase.name)
		}
		if len(connector.databases) != testCase.wantConnections {
			t.Fatalf("%s: got %d connections, want %d", testCase.name, len(connector.databases), testCase.wantConnections)
		}
	}
	for _, db := range connector.databases[:3] {
		if db.closes != 1 {
			t.Fatal("replaced connection not closed exactly once")
		}
	}
	service.Forget("shared")
	service.Forget("shared")
	if connector.databases[3].closes != 1 {
		t.Fatal("forget did not close exactly once")
	}
}

func TestMetadataObservationFreshnessAndFailure(t *testing.T) {
	source := &sourceStub{credentials: Credentials{username: testUsername, password: serviceTestPassword}}
	connector := &connectorStub{}
	service, err := NewInstanceService(source, connector)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx := context.Background()
	target := serviceTarget(t, "metadata-uid", "first", "admin", 1)
	observed, err := service.ObserveMetadata(ctx, target)
	if err != nil || observed.Version() != "17" || !observed.Target().Matches(target) {
		t.Fatalf("metadata observation: %v", err)
	}
	requested := instance.NewExtensionSet([]string{"plpgsql"})
	if observed.Extensions().Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatal("extension list was not observed")
	}

	// 连接可以复用，但每轮必须重新查询；旧观察还必须与 adapter 的可变 slice 脱离。
	database := connector.databases[0]
	database.metadata.AvailableExtensions[0] = "replacement"
	if observed.Extensions().Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatal("adapter mutation changed a completed observation")
	}
	refreshed, err := service.ObserveMetadata(ctx, target)
	if err != nil || refreshed.Extensions().Check(requested).Decision != instance.ExtensionsUnsupported {
		t.Fatalf("extension list was cached across observations: %v", err)
	}
	database.metadata.AvailableExtensions = nil
	empty, err := service.ObserveMetadata(ctx, target)
	if err != nil || empty.Extensions().Check(requested).Decision != instance.ExtensionsUnsupported {
		t.Fatalf("successful empty list was treated as unobserved: %v", err)
	}

	// 即使 adapter 附带部分数据，错误仍使整个观察失效。
	database.err = ErrObservation
	failed, err := service.ObserveMetadata(ctx, target)
	if !errors.Is(err, ErrObservation) || failed.Version() != "" || failed.Target().Validate() == nil {
		t.Fatal("failed query returned a bound observation")
	}
	if failed.Extensions().Check(requested).Decision != instance.ExtensionSupportUnobserved {
		t.Fatal("failed query was interpreted as an empty extension list")
	}
	if _, err := service.ObserveMetadata(ctx, target); err != nil {
		t.Fatalf("retry after query failure: %v", err)
	}
	connector.databases[1].metadata.Version = ""
	if _, err := service.ObserveMetadata(ctx, target); !errors.Is(err, ErrObservation) {
		t.Fatal("missing server version was accepted as complete metadata")
	}
}

func TestInstanceAssemblyFailureRecovery(t *testing.T) {
	ctx := context.Background()
	target := serviceTarget(t, "uid-1", "first", "admin", 1)
	source := &sourceStub{
		credentials: Credentials{username: testUsername, password: serviceTestPassword},
		err:         errors.New("unsafe source error"),
	}
	connector := &connectorStub{err: ErrConnection}
	service, err := NewInstanceService(source, connector)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if version, err := service.ObserveVersion(ctx, target); version != "" || !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatal("unsafe source error escaped")
	}
	source.err = nil
	if version, err := service.ObserveVersion(ctx, target); version != "" || !errors.Is(err, ErrConnection) {
		t.Fatal("connection failure returned evidence")
	}
	connector.err = nil
	if _, err := service.ObserveVersion(ctx, target); err != nil {
		t.Fatal(err)
	}
	connector.databases[0].err = ErrObservation
	if version, err := service.ObserveVersion(ctx, target); version != "" || !errors.Is(err, ErrObservation) {
		t.Fatal("failed query returned evidence")
	}
	if connector.databases[0].closes != 1 {
		t.Fatal("failed connection retained")
	}
	if _, err := service.ObserveVersion(ctx, target); err != nil {
		t.Fatal("retry failed", err)
	}
	service.Close()
	service.Close()
	if connector.databases[1].closes != 1 {
		t.Fatal("shutdown did not close once")
	}
	if _, err := service.ObserveVersion(ctx, target); !errors.Is(err, ErrClosed) {
		t.Fatal("closed service accepted work")
	}
}
