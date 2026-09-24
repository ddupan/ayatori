//go:build integration

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

package postgresql_test

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func TestMetadataObservesAvailableExtensionsWithoutInstalling(t *testing.T) {
	f := newCredentialFixture(t)
	f.createSecret(t, controllerNamespace)
	if installed := f.queryPostgres(t, "SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'hstore'"); installed != "0" {
		t.Fatal("fixture unexpectedly has hstore installed")
	}
	// 提供同名遮蔽对象，验证 adapter 不依赖管理账号可修改的 search_path。
	f.queryPostgres(t, "CREATE VIEW public.pg_available_extensions AS SELECT 'fake_extension'::name AS name")
	f.queryPostgres(t, "ALTER ROLE postgres SET search_path = public, pg_catalog")
	observed, err := f.service.ObserveMetadata(f.ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Target().Matches(f.target) || observed.Version() == "" {
		t.Fatal("metadata was not bound to the current target")
	}
	requested := instance.NewExtensionSet([]string{"hstore", fixtureExtension})
	if observed.Extensions().Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatal("available but uninstalled extension was omitted")
	}
	unsupported := instance.NewExtensionSet([]string{"fake_extension", "HSTORE"})
	check := observed.Extensions().Check(unsupported)
	if check.Decision != instance.ExtensionsUnsupported || len(check.Unsupported) != 2 {
		t.Fatal("metadata accepted shadowed or case-normalized extension names")
	}
	if installed := f.queryPostgres(t, "SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'hstore'"); installed != "0" {
		t.Fatal("metadata observation installed an extension")
	}
	if schemas := f.queryPostgres(t, "SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname = 'postgresql_tenant_operator'"); schemas != "0" {
		t.Fatal("metadata observation initialized the registry")
	}

	aggregate, err := instance.Reconstitute(f.target, instance.Snapshot{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := aggregate.ObserveExtensions(observed.Target(), observed.Extensions()); err != nil {
		t.Fatal(err)
	}
	if aggregate.CheckExtensions(requested).Decision != instance.ExtensionsAccepted {
		t.Fatal("domain rejected observed extension availability")
	}
	if err := aggregate.RequireProvisioningReady(); err == nil {
		t.Fatal("extension availability incorrectly authorized provisioning")
	}
}

func TestMetadataPermissionFailureAndRecovery(t *testing.T) {
	f := newCredentialFixture(t)
	f.createSecret(t, controllerNamespace)
	// 低权限账号也能读取可用列表；这不能证明具备 role/database/extension 管理权限。
	f.queryPostgres(t, "CREATE ROLE metadata_reader LOGIN PASSWORD '"+fixturePassword+"'")
	f.updateSecret(t, func(secret *corev1.Secret) {
		secret.Data["login"] = []byte("metadata_reader")
	})
	if flags := f.queryPostgres(t, "SELECT rolsuper, rolcreaterole, rolcreatedb FROM pg_catalog.pg_roles WHERE rolname = 'metadata_reader'"); flags != "f|f|f" {
		t.Fatal("metadata reader unexpectedly has management privileges")
	}
	requested := instance.NewExtensionSet([]string{fixtureExtension})
	observed, err := f.service.ObserveMetadata(f.ctx, f.target)
	if err != nil || observed.Extensions().Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatalf("read-only account could not observe metadata: %v", err)
	}

	// 仅操作本测试独占容器的 catalog ACL；失败不能转换成“不支持任何扩展”。
	f.queryPostgres(t, "REVOKE SELECT ON pg_catalog.pg_available_extensions FROM PUBLIC")
	failed, err := f.service.ObserveMetadata(f.ctx, f.target)
	if !errors.Is(err, application.ErrObservation) {
		t.Fatalf("metadata permission failure was not reported safely: %v", err)
	}
	if failed.Version() != "" || failed.Target().Validate() == nil {
		t.Fatal("permission failure returned partial metadata")
	}
	if failed.Extensions().Check(requested).Decision != instance.ExtensionSupportUnobserved {
		t.Fatal("permission failure returned an observed empty set")
	}
	if f.backendIDs(t) != "" {
		t.Fatal("failed metadata connection was retained")
	}
	f.queryPostgres(t, "GRANT SELECT ON pg_catalog.pg_available_extensions TO PUBLIC")
	recovered, err := f.service.ObserveMetadata(f.ctx, f.target)
	if err != nil || recovered.Extensions().Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatalf("metadata observation did not recover: %v", err)
	}
}
