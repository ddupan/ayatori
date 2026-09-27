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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

var ErrApplicationCredentialInvalid = errors.New("application credential is invalid")

var applicationIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ApplicationCredential 是内存中的应用连接凭据，不得放入 CR 或普通日志。
// 它与 Instance 管理凭据分开，固定输出交付合同中的七键，不生成带密码的 URI。
type ApplicationCredential struct {
	username string
	password string
	database string
	endpoint instance.Endpoint
}

func NewApplicationCredential(username, password, database string, endpoint instance.Endpoint) (ApplicationCredential, error) {
	if !applicationIdentifier.MatchString(username) || !applicationIdentifier.MatchString(database) || password == "" {
		return ApplicationCredential{}, ErrApplicationCredentialInvalid
	}
	if endpoint.Validate() != nil {
		return ApplicationCredential{}, ErrApplicationCredentialInvalid
	}
	return ApplicationCredential{
		username: username,
		password: password,
		database: database,
		endpoint: endpoint,
	}, nil
}

// GenerateApplicationCredential 仅供已获准首次创建凭据的供应步骤调用。
// 不能在读取失败、写入结果不确定或重启后无条件重新调用。
func GenerateApplicationCredential(username, database string, endpoint instance.Endpoint) (ApplicationCredential, error) {
	password := make([]byte, 32)
	rand.Read(password)
	return NewApplicationCredential(username, base64.RawURLEncoding.EncodeToString(password), database, endpoint)
}

func (c ApplicationCredential) String() string   { return "[redacted application credential]" }
func (c ApplicationCredential) GoString() string { return c.String() }
func (c ApplicationCredential) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted application credential]"`), nil
}

// SecretData 只在凭据后端或数据库连接边界使用；返回值包含明文密码，禁止记录日志。
// 每次返回独立 map，调用方不能修改已经构造的凭据。
func (c ApplicationCredential) SecretData() map[string]any {
	endpoint := c.endpoint.Values()
	return map[string]any{
		"username": c.username,
		"password": c.password,
		"database": c.database,
		"host":     endpoint.Host,
		"hostaddr": endpoint.HostAddr,
		"port":     strconv.Itoa(endpoint.Port),
		"sslmode":  string(endpoint.TLSMode),
	}
}

func (c ApplicationCredential) Validate() error {
	_, err := NewApplicationCredential(c.username, c.password, c.database, c.endpoint)
	return err
}

// MatchesTarget 只比较连接目标，不向用例暴露密码；管理库名不是应用连接目标的一部分。
func (c ApplicationCredential) MatchesTarget(username, database string, endpoint instance.Endpoint) bool {
	actual, wanted := c.endpoint.Values(), endpoint.Values()
	return c.username == username && c.database == database && actual.Host == wanted.Host &&
		actual.HostAddr == wanted.HostAddr && actual.Port == wanted.Port && actual.TLSMode == wanted.TLSMode
}

// ParseApplicationCredential 拒绝缺键、非字符串或非法连接参数，不回显后端内容。
func ParseApplicationCredential(data map[string]any) (ApplicationCredential, error) {
	values := make(map[string]string, 7)
	for _, key := range []string{"username", "password", "database", "host", "hostaddr", "port", "sslmode"} {
		value, ok := data[key].(string)
		if !ok || value == "" {
			return ApplicationCredential{}, ErrApplicationCredentialInvalid
		}
		values[key] = value
	}
	port, err := strconv.Atoi(values["port"])
	if err != nil {
		return ApplicationCredential{}, ErrApplicationCredentialInvalid
	}
	endpoint, err := instance.NewEndpoint(instance.EndpointValues{
		Host:               values["host"],
		HostAddr:           values["hostaddr"],
		Port:               port,
		ManagementDatabase: values["database"],
		TLSMode:            instance.TLSMode(values["sslmode"]),
	})
	if err != nil {
		return ApplicationCredential{}, ErrApplicationCredentialInvalid
	}
	return NewApplicationCredential(values["username"], values["password"], values["database"], endpoint)
}
