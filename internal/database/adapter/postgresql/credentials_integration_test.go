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
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func TestManagementSecretScopeAndMissingDependencyRecovery(t *testing.T) {
	fixture := newCredentialFixture(t)
	reference := fixture.target.Definition().AdminCredential()

	fixture.createSecret(t, "unrelated")
	if _, err := fixture.reader.Read(fixture.ctx, reference); !errors.Is(err, application.ErrCredentialsUnavailable) {
		t.Fatal("a Secret in another namespace satisfied the reference")
	}
	if _, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target); !errors.Is(err, application.ErrCredentialsUnavailable) {
		t.Fatal("missing Secret did not fail closed")
	}

	fixture.createSecret(t, controllerNamespace)
	if _, err := fixture.deniedReader.Read(fixture.ctx, reference); !errors.Is(err, application.ErrCredentialsUnavailable) {
		t.Fatal("API server did not enforce Secret RBAC")
	}
	fixture.observeVersion(t)

	fixture.updateSecret(t, func(secret *corev1.Secret) {
		delete(secret.Data, "credential")
	})
	if _, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target); !errors.Is(err, application.ErrCredentialsInvalid) {
		t.Fatal("missing credential field reused a cached connection")
	}
	fixture.updateSecret(t, func(secret *corev1.Secret) {
		secret.Data["credential"] = []byte(fixturePassword)
	})
	fixture.observeVersion(t)

	err := fixture.client.CoreV1().Secrets(controllerNamespace).Delete(fixture.ctx, secretName, metav1.DeleteOptions{})
	if err != nil {
		t.Fatal("cannot delete fixture Secret")
	}
	if _, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target); !errors.Is(err, application.ErrCredentialsUnavailable) {
		t.Fatal("deleted Secret retained access")
	}
}

func TestEffectiveCredentialChangesReplaceConnection(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.createSecret(t, controllerNamespace)
	fixture.observeVersion(t)
	originalBackend := fixture.backendIDs(t)
	if originalBackend == "" {
		t.Fatal("management connection not visible in PostgreSQL")
	}

	fixture.updateSecret(t, func(secret *corev1.Secret) {
		secret.Labels = map[string]string{"changed": "true"}
		secret.Data["unrelated"] = []byte("ignored")
	})
	fixture.observeVersion(t)
	if fixture.backendIDs(t) != originalBackend {
		t.Fatal("metadata or unrelated fields rebuilt the connection")
	}

	// 先改变 Secret、暂不改变服务器密码：旧连接必须失效，新认证必须失败。
	fixture.updateSecret(t, func(secret *corev1.Secret) {
		secret.Data["credential"] = []byte(rotatedPassword)
	})
	version, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target)
	if !errors.Is(err, application.ErrAuthentication) || version != "" {
		t.Fatal("old connection bypassed changed credentials")
	}

	fixture.queryPostgres(t, "ALTER ROLE postgres PASSWORD '"+rotatedPassword+"'")
	fixture.observeVersion(t)
	if fixture.backendIDs(t) == originalBackend {
		t.Fatal("password rotation reused the old backend")
	}

	fixture.updateSecret(t, func(secret *corev1.Secret) {
		secret.Data["login"] = []byte("nonexistent")
	})
	if _, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target); !errors.Is(err, application.ErrAuthentication) {
		t.Fatal("username change did not require a new authentication")
	}
	fixture.updateSecret(t, func(secret *corev1.Secret) {
		secret.Data["login"] = []byte(fixtureUser)
	})
	fixture.observeVersion(t)
}

func TestObservationDiscardsResultWhenCredentialsChange(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.createSecret(t, controllerNamespace)

	reads := 0
	fixture.gate.beforeRead = func() {
		reads++
		if reads == 2 {
			fixture.updateSecret(t, func(secret *corev1.Secret) {
				secret.Data["credential"] = []byte(rotatedPassword)
			})
		}
	}

	version, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target)
	if !errors.Is(err, application.ErrCredentialsChanged) {
		t.Fatal("in-flight rotation was not detected")
	}
	if version != "" {
		t.Fatal("observation returned data obtained with stale credentials")
	}
	if fixture.backendIDs(t) != "" {
		t.Fatal("stale connection was retained after rotation")
	}
}

func TestConnectionReleaseAndServiceRestart(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.createSecret(t, controllerNamespace)
	fixture.observeVersion(t)

	fixture.service.Forget(fixture.target.Identity().Name())
	if fixture.backendIDs(t) != "" {
		t.Fatal("Forget retained a connection")
	}
	fixture.observeVersion(t)

	fixture.service.Close()
	fixture.service.Close()
	if _, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target); !errors.Is(err, application.ErrClosed) {
		t.Fatal("closed service accepted work")
	}

	restarted, err := application.NewInstanceService(fixture.reader, postgresql.Connector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if _, err := restarted.ObserveVersion(fixture.ctx, fixture.target); err != nil {
		t.Fatal("new service could not recover from stored Secret", err)
	}

	// 此 fixture 未启用 TLS；各加密模式均不得偷偷回退到明文连接。
	for _, mode := range []instance.TLSMode{instance.TLSRequire, instance.TLSVerifyCA, instance.TLSVerifyFull} {
		securedTarget := target(t, fixture.port, mode)
		if _, err := restarted.ObserveVersion(fixture.ctx, securedTarget); err == nil {
			t.Fatal("TLS policy silently downgraded to plaintext")
		}
	}
}

func TestConcurrentVersionObservations(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.createSecret(t, controllerNamespace)

	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			version, err := fixture.service.ObserveVersion(fixture.ctx, fixture.target)
			if err != nil || version == "" {
				t.Error("concurrent observation failed", err)
			}
		})
	}
	workers.Go(func() {
		fixture.service.Forget(fixture.target.Identity().Name())
	})
	workers.Wait()
	fixture.observeVersion(t)
}

func TestManagementConnectionRecoversAfterTimeout(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.createSecret(t, controllerNamespace)
	fixture.observeVersion(t)

	if err := exec.CommandContext(fixture.ctx, "docker", "pause", fixture.containerID).Run(); err != nil {
		t.Fatal("cannot pause isolated PostgreSQL fixture")
	}
	// 即使断言失败，也先恢复容器，再由 fixture 按原 ID 清理。
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupContext, "docker", "unpause", fixture.containerID).Run()
	})

	queryContext, cancel := context.WithTimeout(fixture.ctx, 500*time.Millisecond)
	version, err := fixture.service.ObserveVersion(queryContext, fixture.target)
	cancel()
	if err == nil || version != "" {
		t.Fatal("timed out PostgreSQL observation returned a successful result")
	}

	if err := exec.CommandContext(fixture.ctx, "docker", "unpause", fixture.containerID).Run(); err != nil {
		t.Fatal("cannot resume isolated PostgreSQL fixture")
	}
	fixture.observeVersion(t)
}
