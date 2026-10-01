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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"git.ddupan.top/panxiao81/ayatori/internal/database/adapter/postgresql"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

func fixtureCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: fixtureHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{fixtureHost},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestPostgreSQLTLSHostIdentity(t *testing.T) {
	const psql = "psql"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id, port := postgresFixture(t, ctx)
	certPath, keyPath := fixtureCertificate(t)
	commands := [][]string{
		{"cp", certPath, id + ":/tmp/server.crt"},
		{"cp", keyPath, id + ":/tmp/server.key"},
		{dockerExec, "-u", "0", id, "chown", "postgres:postgres", "/tmp/server.crt", "/tmp/server.key"},
		{dockerExec, id, psql, "-U", fixtureUser, "-c", "ALTER SYSTEM SET ssl_cert_file='/tmp/server.crt'"},
		{dockerExec, id, psql, "-U", fixtureUser, "-c", "ALTER SYSTEM SET ssl_key_file='/tmp/server.key'"},
		{dockerExec, id, psql, "-U", fixtureUser, "-c", "ALTER SYSTEM SET ssl=on"},
		{dockerExec, id, psql, "-U", fixtureUser, "-c", "SELECT pg_reload_conf()"},
	}
	for _, args := range commands {
		if exec.CommandContext(ctx, "docker", args...).Run() != nil {
			t.Fatal("TLS fixture setup failed")
		}
	}
	credentials, err := instance.NewManagementCredentials(fixtureUser, fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	trusted := &postgresql.Instances{RootCert: certPath}
	defer trusted.Close()
	secured := target(t, port, instance.TLSVerifyFull)
	if facts, err := trusted.InspectManagement(ctx, secured, credentials); err != nil || facts.Version == "" {
		t.Fatal("trusted DNS SAN connection failed", err)
	}
	if _, err := trusted.InspectManagement(ctx, withHost(t, secured, "127.0.0.1"), credentials); err != nil {
		t.Fatal("trusted IP SAN connection failed", err)
	}
	if _, err := trusted.InspectManagement(ctx, withHost(t, secured, "wrong.invalid"), credentials); err == nil {
		t.Fatal("wrong TLS hostname accepted")
	}
	otherCA, _ := fixtureCertificate(t)
	untrusted := &postgresql.Instances{RootCert: otherCA}
	defer untrusted.Close()
	if _, err := untrusted.InspectManagement(ctx, secured, credentials); !errors.Is(err, instance.ErrAuthentication) {
		t.Fatal("wrong CA accepted")
	}
}

// withHost 只替换 TLS 校验使用的主机名，地址和其余配置不变。
func withHost(t *testing.T, original instance.ObservationTarget, host string) instance.ObservationTarget {
	t.Helper()
	values := original.Definition().Endpoint().Values()
	values.Host = host
	endpoint, err := instance.NewEndpoint(values)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := instance.NewDefinition(endpoint, original.Definition().AdminCredential())
	if err != nil {
		t.Fatal(err)
	}
	changed, err := instance.NewObservationTarget(original.Identity(), original.Revision(), definition)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}
