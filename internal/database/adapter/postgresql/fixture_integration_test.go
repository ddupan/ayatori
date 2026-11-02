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
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kubeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	databasecontroller "git.ddupan.top/panxiao81/ayatori/internal/database/controller"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
	"git.ddupan.top/panxiao81/ayatori/internal/testenv"
)

const (
	fixtureAddress        = "127.0.0.1"
	managementUsernameKey = "login"
	managementPasswordKey = "credential"
	fixtureHost           = "fixture.invalid"
	fixtureUser           = "postgres"
	fixtureExtension      = "plpgsql"
	dockerExec            = "exec"
	fixturePassword       = "AYATORI-TEST-ONLY-initial-password"
	rotatedPassword       = "AYATORI-TEST-ONLY-rotated-password"
	secretName            = "management"
)

// resetSQL 把共用实例复位到 initdb 之后的状态：重建 postgres 库，删除其余库与非内置角色，
// 恢复超级用户密码、角色设置与 ALTER SYSTEM。经 stdin 交给容器内 psql，密码不进入命令参数。
const resetSQL = `SELECT format('DROP DATABASE %I WITH (FORCE)', datname) FROM pg_database
  WHERE datname NOT IN ('template0', 'template1') \gexec
CREATE DATABASE postgres;
SELECT format('DROP ROLE %I', rolname) FROM pg_roles WHERE rolname <> 'postgres' AND rolname !~ '^pg_' \gexec
ALTER ROLE postgres RESET ALL;
ALTER ROLE postgres PASSWORD '` + fixturePassword + `';
ALTER SYSTEM RESET ALL;
SELECT pg_reload_conf();
`

// postgresFixture 返回 hack/integration-test.sh 在 go test 之外启动的 PostgreSQL，并复位到初始状态。
// 原来每个场景各起一个容器，initdb 占了场景的大部分用时；测试串行运行，场景独占该实例。
// fixture 不接受外部 DSN，只认脚本给出的容器 ID。
func postgresFixture(t *testing.T, ctx context.Context) (string, int) {
	t.Helper()
	id, port := testenv.Container(t, "postgres", 5432)
	reset := exec.CommandContext(ctx, "docker", dockerExec, "-i", id,
		"psql", "-q", "-v", "ON_ERROR_STOP=1", "-U", fixtureUser, "-d", "template1")
	reset.Stdin = strings.NewReader(resetSQL)
	if output, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("cannot reset PostgreSQL fixture: %s", fixtureCommandError(fmt.Errorf("%w: %s", err, output)))
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

// credentialFixture 为每个场景创建独立 namespace、PostgreSQL 和应用服务，共用包级 API server。
type credentialFixture struct {
	namespace     string
	unrelated     string
	config        *rest.Config
	ctx           context.Context
	client        *kubernetes.Clientset
	secrets       databasecontroller.ManagementSecrets
	deniedSecrets databasecontroller.ManagementSecrets
	service       *postgresql.Instances
	target        instance.ObservationTarget
	containerID   string
	port          int
}

func newCredentialFixture(t *testing.T) *credentialFixture {
	t.Helper()
	// 覆盖整个场景（含 provisioning 的全部子测试），负载高时两分钟不够；卡死由 -test.timeout 兜底。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	config := testenv.APIServer(t)
	scenario := testenv.Scenario()
	namespace := fmt.Sprintf("database-controller-%d", scenario)
	unrelated := fmt.Sprintf("unrelated-%d", scenario)

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal("cannot create test client")
	}
	for _, namespace := range []string{namespace, unrelated} {
		_, err := client.CoreV1().Namespaces().Create(
			ctx,
			&corev1.Namespace{Name: namespace},
			metav1.CreateOptions{},
		)
		if err != nil {
			t.Fatal("cannot create fixture namespace")
		}
	}

	apiReader, err := kubeclient.New(config, kubeclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 不带任何 RBAC 的用户；以 impersonation 代替证书用户，共用的 API server 无需签发。
	denied := rest.CopyConfig(config)
	denied.Impersonate.UserName = fmt.Sprintf("without-secret-access-%d", scenario)
	deniedAPIReader, err := kubeclient.New(denied, kubeclient.Options{})
	if err != nil {
		t.Fatal(err)
	}

	containerID, port := postgresFixture(t, ctx)
	service := &postgresql.Instances{}
	t.Cleanup(service.Close)

	return &credentialFixture{
		namespace:     namespace,
		unrelated:     unrelated,
		config:        config,
		ctx:           ctx,
		client:        client,
		secrets:       databasecontroller.ManagementSecrets{Reader: apiReader, Namespace: namespace},
		deniedSecrets: databasecontroller.ManagementSecrets{Reader: deniedAPIReader, Namespace: namespace},
		service:       service,
		target:        target(t, port, instance.TLSDisable),
		containerID:   containerID,
		port:          port,
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
	secrets := f.client.CoreV1().Secrets(f.namespace)
	secret, err := secrets.Get(f.ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal("cannot read fixture Secret")
	}
	change(secret)
	if _, err := secrets.Update(f.ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal("cannot update fixture Secret")
	}
}

// observe 按 Reconciler 的顺序读取当前管理 Secret，再观察管理能力。
func (f *credentialFixture) observe(ctx context.Context, target instance.ObservationTarget) (instance.ManagementFacts, error) {
	credentials, err := f.secrets.Read(ctx, target.Definition().AdminCredential())
	if err != nil {
		return instance.ManagementFacts{}, err
	}
	return f.service.InspectManagement(ctx, target, credentials)
}

func (f *credentialFixture) observeVersion(t *testing.T) {
	t.Helper()
	facts, err := f.observe(f.ctx, f.target)
	if err != nil {
		t.Fatal("version observation failed", err)
	}
	if facts.Version == "" {
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
