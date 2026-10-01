package instance

import "errors"

var (
	// ErrCredentialsInvalid 表示管理 Secret 缺少用户名或密码字段。
	ErrCredentialsInvalid = errors.New("management credentials invalid")
	// ErrAuthentication 表示 PostgreSQL 拒绝认证或 TLS 校验失败。
	ErrAuthentication = errors.New("management authentication failed")
)

// ManagementCredentials 只存在于 Reconciler 与连接 adapter 的内存中，不进入持久化状态。
type ManagementCredentials struct {
	username string
	password string
}

func NewManagementCredentials(username, password string) (ManagementCredentials, error) {
	if username == "" || password == "" {
		return ManagementCredentials{}, ErrCredentialsInvalid
	}
	return ManagementCredentials{username: username, password: password}, nil
}

func (c ManagementCredentials) Username() string { return c.username }
func (c ManagementCredentials) Password() string { return c.password }
func (c ManagementCredentials) String() string   { return "[redacted management credentials]" }
func (c ManagementCredentials) GoString() string { return c.String() }

// MarshalJSON 显式隐藏内容，避免未来字段调整意外改变日志或序列化行为。
func (c ManagementCredentials) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted management credentials]"`), nil
}
