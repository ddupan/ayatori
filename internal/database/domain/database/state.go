package database

import (
	"errors"

	"go.ddupan.top/ayatori/internal/database/domain/credential"
)

// 外部操作结果分类。adapter 把后端错误翻译成这些类别，聚合据此决定状态迁移。
var (
	ErrNotFound    = errors.New("external object not found")
	ErrUnavailable = errors.New("external operation was not executed; waiting for dependency")
	ErrConflict    = errors.New("external object conflicts; manual resolution required")
	ErrUncertain   = errors.New("external operation outcome is uncertain; manual resolution required")
)

// CredentialPhase 表达凭据准备进度，持久化在 status.credentialPhase，不依赖 Condition 的 Reason。
type CredentialPhase string

const (
	CredentialPending       CredentialPhase = "Pending"
	CredentialPinned        CredentialPhase = "Pinned"
	CredentialPrepared      CredentialPhase = "Prepared"
	CredentialConflict      CredentialPhase = "Conflict"
	CredentialUnavailable   CredentialPhase = "Unavailable"
	CredentialStopped       CredentialPhase = "Stopped"
	CredentialInvalidTarget CredentialPhase = "InvalidTarget"
)

type CredentialState struct {
	Location *credential.Location
	// Version 只在首次创建并回读成功后记录；零值表示尚未确认。
	Version int64
	Phase   CredentialPhase
	Message string
}

// resume 决定新一轮是否可以继续。未确认版本的冲突需要人工处理，不自动重入创建。
func (s CredentialState) resume() bool {
	return s.Version != 0 || s.Phase != CredentialConflict
}

type ResourcePhase string

const (
	ResourcePending     ResourcePhase = "Pending"
	ResourceAvailable   ResourcePhase = "Available"
	ResourceConflict    ResourcePhase = "Conflict"
	ResourceUnavailable ResourcePhase = "Unavailable"
	ResourceStopped     ResourcePhase = "Stopped"
)

// ResourceState 中的 OID 是本次成功创建并回读的对象身份，不是从名称推导出的管理授权。
// 仅保存在 CR；不建立 PostgreSQL registry，也不承诺备份还原后的自动认领。
type ResourceState struct {
	RoleOID     uint32
	DatabaseOID uint32
	Phase       ResourcePhase
	Message     string
}

// ExtensionFacts 是目标库内的原始事实：服务器可用的扩展与本库已安装的扩展。
type ExtensionFacts struct {
	Available []string
	Installed []string
}

// ResourceFacts 是 PostgreSQL catalog 的原始事实；OID 为零表示对象不存在。
type ResourceFacts struct {
	RoleOID     uint32
	DatabaseOID uint32
	OwnerOID    uint32
	// ManagerOID 是当前管理账号；供应期间它先作为 owner，最后才移交给应用角色。
	ManagerOID     uint32
	PublicConnect  bool
	ManagerConnect bool
}
