package database

import (
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
)

// 外部操作结果分类。adapter 把后端错误翻译成这些类别，聚合据此决定状态迁移。
var (
	ErrNotFound    = errors.New("external object not found")
	ErrUnavailable = errors.New("external operation was not executed; waiting for dependency")
	ErrConflict    = errors.New("external object conflicts; manual resolution required")
	ErrUncertain   = errors.New("external operation outcome is uncertain; manual resolution required")
)

// CredentialPhase 表达凭据准备进度，不依赖 Kubernetes Condition 的类型或 Reason。
type CredentialPhase uint8

const (
	CredentialPending CredentialPhase = iota
	CredentialPinned
	CredentialCreating
	CredentialPrepared
	CredentialConflict
	CredentialUnavailable
	CredentialStopped
	CredentialInvalidTarget
)

type CredentialState struct {
	Location *credential.Location
	// Version 只在首次创建并回读成功后记录；零值表示尚未确认。
	Version int64
	Phase   CredentialPhase
	Message string
}

// resume 决定新一轮是否可以继续。未确认的创建不能靠读取成功认领。
func (s CredentialState) resume() (CredentialState, bool) {
	if s.Version != 0 {
		return s, true
	}
	switch s.Phase {
	case CredentialConflict:
		return s, false
	case CredentialCreating:
		s.Phase, s.Message = CredentialConflict, "凭据创建未留下成功确认；请核对固定位置与后端历史并人工处理，未重新生成密码"
		return s, false
	default:
		return s, true
	}
}

type ResourcePhase string

const (
	ResourcePending          ResourcePhase = "Pending"
	ResourceCreatingRole     ResourcePhase = "CreatingRole"
	ResourceCreatingDatabase ResourcePhase = "CreatingDatabase"
	ResourceAvailable        ResourcePhase = "Available"
	ResourceConflict         ResourcePhase = "Conflict"
	ResourceUnavailable      ResourcePhase = "Unavailable"
	ResourceStopped          ResourcePhase = "Stopped"
)

// ResourceState 中的 OID 是本次成功创建并回读的对象身份，不是从名称推导出的管理授权。
// 仅保存在 CR；不建立 PostgreSQL registry，也不承诺备份还原后的自动认领。
type ResourceState struct {
	RoleOID     uint32
	DatabaseOID uint32
	Phase       ResourcePhase
	Message     string
}

func (s ResourceState) resume() (ResourceState, bool) {
	if s.Phase == ResourceConflict {
		return s, false
	}
	if (s.Phase == ResourceCreatingRole && s.RoleOID == 0) || (s.Phase == ResourceCreatingDatabase && s.DatabaseOID == 0) {
		s.Phase, s.Message = ResourceConflict, "外部创建未留下成功确认；请核对目标角色和数据库，不自动认领或重复创建"
		return s, false
	}
	return s, true
}

// ResourceFacts 是 PostgreSQL catalog 的原始事实；OID 为零表示对象不存在。
type ResourceFacts struct {
	RoleOID          uint32
	DatabaseOID      uint32
	OwnerOID         uint32
	AllowConnections bool
	PublicConnect    bool
}
