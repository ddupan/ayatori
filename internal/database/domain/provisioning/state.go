// Package provisioning 保存 PostgreSQL 资源供应的确认与恢复规则，不依赖后端或 API。
package provisioning

import "fmt"

type Phase string

const (
	Pending          Phase = "Pending"
	CreatingRole     Phase = "CreatingRole"
	CreatingDatabase Phase = "CreatingDatabase"
	Available        Phase = "Available"
	Conflict         Phase = "Conflict"
	Unavailable      Phase = "Unavailable"
	Stopped          Phase = "Stopped"
)

// OID 是已成功创建并回读的对象身份，不是从名称推导出的管理授权。
// 仅保存在 CR；不建立 PostgreSQL registry，也不承诺备份还原后的自动认领。
type State struct {
	RoleOID     uint32
	DatabaseOID uint32
	Phase       Phase
	Message     string
}

type Observation struct {
	RoleOID          uint32
	DatabaseOID      uint32
	RoleSafe         bool
	OwnerOID         uint32
	PublicConnect    bool
	AllowConnections bool
}

func (s State) WithPhase(phase Phase, message string) State {
	s.Phase, s.Message = phase, message
	return s
}

func (s State) Resume() (State, bool) {
	if s.Phase == Conflict {
		return s, false
	}
	if (s.Phase == CreatingRole && s.RoleOID == 0) || (s.Phase == CreatingDatabase && s.DatabaseOID == 0) {
		return s.WithPhase(Conflict, "外部创建未留下成功确认；请核对目标角色和数据库，不自动认领或重复创建"), false
	}
	return s, true
}

// Check 既阻止未知同名对象，也拒绝已确认对象消失、被重建或权限漂移。
func (s State) Check(o Observation) error {
	if s.RoleOID != o.RoleOID {
		return fmt.Errorf("角色身份不匹配：记录 OID=%d，观察 OID=%d", s.RoleOID, o.RoleOID)
	}
	if s.DatabaseOID != o.DatabaseOID {
		return fmt.Errorf("数据库身份不匹配：记录 OID=%d，观察 OID=%d", s.DatabaseOID, o.DatabaseOID)
	}
	if o.RoleOID != 0 && !o.RoleSafe {
		return fmt.Errorf("已确认角色的登录属性、特权或成员关系发生变化")
	}
	if o.DatabaseOID != 0 && o.OwnerOID != s.RoleOID {
		return fmt.Errorf("数据库 owner 与已确认角色不匹配")
	}
	return nil
}
