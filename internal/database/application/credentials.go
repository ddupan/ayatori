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

// Package application 定义 Database 用例与适配器之间的边界。
package application

import (
	"context"
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

var (
	ErrCredentialsUnavailable = errors.New("management credentials unavailable")
	ErrCredentialsInvalid     = errors.New("management credentials invalid")
)

// Credentials 只存在于应用与连接适配器内存，不进入领域对象或持久化状态。
type Credentials struct {
	username string
	password string
}

func NewCredentials(username, password string) (Credentials, error) {
	if username == "" || password == "" {
		return Credentials{}, ErrCredentialsInvalid
	}
	return Credentials{username: username, password: password}, nil
}

func (c Credentials) Username() string { return c.username }
func (c Credentials) Password() string { return c.password }
func (c Credentials) String() string   { return "[redacted management credentials]" }
func (c Credentials) GoString() string { return c.String() }

// MarshalJSON 显式隐藏内容，避免未来字段调整意外改变日志或序列化行为。
func (c Credentials) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted management credentials]"`), nil
}

// CredentialReader 返回本次读取的有效值；metadata 不参与凭据相等比较。
type CredentialReader interface {
	Read(context.Context, instance.CredentialReference) (Credentials, error)
}
