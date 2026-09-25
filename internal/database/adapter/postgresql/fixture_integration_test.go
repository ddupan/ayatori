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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	secretadapter "git.ddupan.top/panxiao81/ayatori/internal/database/adapter/kubernetes"
	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

const (
	fixtureAddress        = "127.0.0.1"
	managementUsernameKey = "login"
	managementPasswordKey = "credential"
	unrelatedNamespace    = "unrelated"
	fixtureHost           = "fixture.invalid"
	fixtureUser           = "postgres"
	fixtureExtension      = "plpgsql"
	dockerExec            = "exec"
	fixtureImage          = "postgres@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73"
	fixturePassword       = "AYATORI-TEST-ONLY-initial-password"
	rotatedPassword       = "AYATORI-TEST-ONLY-rotated-password"
	controllerNamespace   = "database-controller"
	secretName            = "management"
)

// fixture 不接受外部 DSN，只创建自己的临时容器并按确切 ID 清理。
func postgresFixture(t *testing.T, ctx context.Context) (string, int) {
	t.Helper()
	output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "-p", "127.0.0.1::5432",
		"-e", "POSTGRES_PASSWORD="+fixturePassword, fixtureImage).Output()
	if err != nil {
		t.Fatalf("cannot start isolated PostgreSQL fixture: %s", fixtureCommandError(err))
	}
	id := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatal("unexpected container identifier")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if exec.CommandContext(cleanup, "docker", "rm", "-f", id).Run() != nil {
			t.Error("fixture cleanup failed")
		}
	})
	output, err = exec.CommandContext(ctx, "docker", "inspect", "--format", `{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}`, id).Output()
	if err != nil {
		t.Fatalf("cannot inspect fixture port: %s", fixtureCommandError(err))
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatal("invalid fixture port")
	}
	// 初次 init 的临时服务器只监听 Unix socket，必须等最终 TCP listener。
	for exec.CommandContext(ctx, "docker", dockerExec, id, "pg_isready", "-h", fixtureAddress, "-U", fixtureUser).Run() != nil {
		select {
		case <-ctx.Done():
			t.Fatal("fixture startup timed out")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return id, port
}

// Output 将 stderr 保存在 ExitError 中；保留诊断，但不打印命令参数和测试密码。
func fixtureCommandError(err error) string {
	detail := err.Error()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		detail += ": " + strings.TrimSpace(string(exitErr.Stderr))
	}
	redactor := strings.NewReplacer(
		fixturePassword, "[REDACTED]",
		rotatedPassword, "[REDACTED]",
	)
	return redactor.Replace(detail)
}

func TestFixtureCommandErrorPreservesDiagnosticsAndRedactsPasswords(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing docker executable",
			err:  &exec.Error{Name: "docker", Err: exec.ErrNotFound},
			want: "executable file not found",
		},
		{
			name: "daemon failure from stderr",
			err:  &exec.ExitError{Stderr: []byte("Cannot connect to the Docker daemon")},
			want: "Cannot connect to the Docker daemon",
		},
		{
			name: "passwords in stderr",
			err:  &exec.ExitError{Stderr: []byte("failure: " + fixturePassword + " " + rotatedPassword)},
			want: "failure: [REDACTED] [REDACTED]",
		},
		{
			name: "password in error text",
			err:  errors.New("failure: " + fixturePassword),
			want: "failure: [REDACTED]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := fixtureCommandError(tt.err)
			if !strings.Contains(detail, tt.want) {
				t.Fatalf("diagnostic lost expected information: %q", tt.want)
			}
			if strings.Contains(detail, fixturePassword) || strings.Contains(detail, rotatedPassword) {
				t.Fatal("diagnostic exposed a fixture password")
			}
		})
	}
}

func target(t *testing.T, port int, mode instance.TLSMode) instance.ObservationTarget {
	t.Helper()
	id, err := instance.NewIdentity("fixture-uid", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := instance.NewRevision(1)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host:               fixtureHost,
		HostAddr:           fixtureAddress,
		Port:               port,
		ManagementDatabase: fixtureUser,
		TLSMode:            mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := instance.NewCredentialReference(instance.CredentialReferenceValues{
		Name:        secretName,
		UsernameKey: managementUsernameKey,
		PasswordKey: managementPasswordKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := instance.NewDefinition(endpoint, ref)
	if err != nil {
		t.Fatal(err)
	}
	value, err := instance.NewObservationTarget(id, revision, definition)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// 在真实读取前设置屏障，确定性验证观测期间 Secret 变化；实际数据仍来自 API server。
type gatedReader struct {
	application.CredentialReader
	beforeRead func()
}

func (r *gatedReader) Read(ctx context.Context, ref instance.CredentialReference) (application.Credentials, error) {
	if r.beforeRead != nil {
		r.beforeRead()
	}
	return r.CredentialReader.Read(ctx, ref)
}

// credentialFixture 为每个场景创建独立 API server、PostgreSQL 和应用服务。
type credentialFixture struct {
	config       *rest.Config
	ctx          context.Context
	client       *kubernetes.Clientset
	reader       *secretadapter.SecretCredentials
	deniedReader *secretadapter.SecretCredentials
	gate         *gatedReader
	service      *application.InstanceService
	target       instance.ObservationTarget
	containerID  string
	port         int
}

func newCredentialFixture(t *testing.T) *credentialFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		t.Fatal("envtest startup failed", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error("envtest cleanup failed", err)
		}
	})

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal("cannot create test client")
	}
	for _, namespace := range []string{controllerNamespace, unrelatedNamespace} {
		_, err := client.CoreV1().Namespaces().Create(
			ctx,
			&corev1.Namespace{Name: namespace},
			metav1.CreateOptions{},
		)
		if err != nil {
			t.Fatal("cannot create fixture namespace")
		}
	}

	reader, err := secretadapter.NewSecretCredentials(config, controllerNamespace)
	if err != nil {
		t.Fatal(err)
	}
	user, err := environment.AddUser(envtest.User{Name: "without-secret-access"}, config)
	if err != nil {
		t.Fatal(err)
	}
	deniedReader, err := secretadapter.NewSecretCredentials(user.Config(), controllerNamespace)
	if err != nil {
		t.Fatal(err)
	}

	containerID, port := postgresFixture(t, ctx)
	gate := &gatedReader{CredentialReader: reader}
	service, err := application.NewInstanceService(gate, postgresql.Connector{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	return &credentialFixture{
		config:       config,
		ctx:          ctx,
		client:       client,
		reader:       reader,
		deniedReader: deniedReader,
		gate:         gate,
		service:      service,
		target:       target(t, port, instance.TLSDisable),
		containerID:  containerID,
		port:         port,
	}
}

func (f *credentialFixture) createSecret(t *testing.T, namespace string) {
	t.Helper()
	secret := &corev1.Secret{
		Name: secretName,
		Data: map[string][]byte{
			managementUsernameKey: []byte(fixtureUser),
			managementPasswordKey: []byte(fixturePassword),
		},
	}
	if _, err := f.client.CoreV1().Secrets(namespace).Create(f.ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatal("cannot create fixture Secret")
	}
}

func (f *credentialFixture) updateSecret(t *testing.T, change func(*corev1.Secret)) {
	t.Helper()
	secrets := f.client.CoreV1().Secrets(controllerNamespace)
	secret, err := secrets.Get(f.ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal("cannot read fixture Secret")
	}
	change(secret)
	if _, err := secrets.Update(f.ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal("cannot update fixture Secret")
	}
}

func (f *credentialFixture) observeVersion(t *testing.T) {
	t.Helper()
	version, err := f.service.ObserveVersion(f.ctx, f.target)
	if err != nil {
		t.Fatal("version observation failed", err)
	}
	if version == "" {
		t.Fatal("successful observation returned an empty version")
	}
}

func (f *credentialFixture) queryPostgres(t *testing.T, sql string) string {
	t.Helper()
	output, err := exec.CommandContext(
		f.ctx, "docker", dockerExec, f.containerID,
		"psql", "-U", fixtureUser, "-tAc", sql,
	).Output()
	if err != nil {
		t.Fatal("fixture SQL failed")
	}
	return strings.TrimSpace(string(output))
}

func (f *credentialFixture) backendIDs(t *testing.T) string {
	t.Helper()
	return f.queryPostgres(t, `
		SELECT pid
		FROM pg_stat_activity
		WHERE application_name = 'ayatori-database-management'
		ORDER BY pid
	`)
}
