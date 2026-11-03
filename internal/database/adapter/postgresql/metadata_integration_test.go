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

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func TestMetadataObservesAvailableExtensionsWithoutInstalling(t *testing.T) {
	f := newCredentialFixture(t)
	f.createSecret(t, f.namespace)
	if installed := f.queryPostgres(t, "SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'hstore'"); installed != "0" {
		t.Fatal("fixture unexpectedly has hstore installed")
	}
	// 提供同名遮蔽对象，验证 adapter 不依赖管理账号可修改的 search_path。
	f.queryPostgres(t, "CREATE VIEW public.pg_available_extensions AS SELECT 'fake_extension'::name AS name")
	f.queryPostgres(t, "ALTER ROLE postgres SET search_path = public, pg_catalog")
	schemasBefore := f.queryPostgres(t, "SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_catalog.pg_namespace")
	facts, err := f.observe(f.ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Version == "" {
		t.Fatal("observation returned no version")
	}
	support := instance.ObserveExtensionSupport(facts.AvailableExtensions)
	requested := instance.NewExtensionSet([]string{"hstore", fixtureExtension})
	if support.Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatal("available but uninstalled extension was omitted")
	}
	unsupported := instance.NewExtensionSet([]string{"fake_extension", "HSTORE"})
	check := support.Check(unsupported)
	if check.Decision != instance.ExtensionsUnsupported || len(check.Unsupported) != 2 {
		t.Fatal("metadata accepted shadowed or case-normalized extension names")
	}
	if installed := f.queryPostgres(t, "SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'hstore'"); installed != "0" {
		t.Fatal("metadata observation installed an extension")
	}
	if schemas := f.queryPostgres(t, "SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_catalog.pg_namespace"); schemas != schemasBefore {
		t.Fatal("metadata observation changed database schemas")
	}

	aggregate, err := instance.Reconstitute(f.target, instance.Snapshot{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := aggregate.ObserveExtensions(f.target, support); err != nil {
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
	f.createSecret(t, f.namespace)
	// 低权限账号也能读取可用列表；这不能证明具备 role/database/extension 管理权限。
	f.queryPostgres(t, "CREATE ROLE metadata_reader LOGIN PASSWORD '"+fixturePassword+"'")
	f.updateSecret(t, func(secret *corev1.Secret) {
		secret.Data["login"] = []byte("metadata_reader")
	})
	if flags := f.queryPostgres(t, "SELECT rolsuper, rolcreaterole, rolcreatedb FROM pg_catalog.pg_roles WHERE rolname = 'metadata_reader'"); flags != "f|f|f" {
		t.Fatal("metadata reader unexpectedly has management privileges")
	}
	requested := instance.NewExtensionSet([]string{fixtureExtension})
	facts, err := f.observe(f.ctx, f.target)
	if err != nil || instance.ObserveExtensionSupport(facts.AvailableExtensions).Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatalf("read-only account could not observe metadata: %v", err)
	}
	if facts.Checks().Grants == instance.CheckPassed {
		t.Fatal("read-only account was treated as a manager")
	}

	// 仅操作本测试独占容器的 catalog ACL；失败不能转换成“不支持任何扩展”。
	f.queryPostgres(t, "REVOKE SELECT ON pg_catalog.pg_available_extensions FROM PUBLIC")
	failed, err := f.observe(f.ctx, f.target)
	if err == nil || errors.Is(err, instance.ErrAuthentication) {
		t.Fatalf("metadata permission failure was not reported safely: %v", err)
	}
	if failed.Version != "" || failed.AvailableExtensions != nil {
		t.Fatal("permission failure returned partial metadata")
	}
	if f.backendIDs(t) != "" {
		t.Fatal("failed metadata connection was retained")
	}
	f.queryPostgres(t, "GRANT SELECT ON pg_catalog.pg_available_extensions TO PUBLIC")
	recovered, err := f.observe(f.ctx, f.target)
	if err != nil || instance.ObserveExtensionSupport(recovered.AvailableExtensions).Check(requested).Decision != instance.ExtensionsAccepted {
		t.Fatalf("metadata observation did not recover: %v", err)
	}
}
