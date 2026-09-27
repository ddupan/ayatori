package credential

import (
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/binding"
	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"
)

type Location struct {
	Mount string
	Path  string
}

// Phase 表达凭据准备进度，不依赖 Kubernetes Condition 的类型或 Reason。
type Phase uint8

const (
	Pending Phase = iota
	Pinned
	Creating
	Prepared
	Conflict
	Unavailable
	Stopped
	InvalidTarget
)

type State struct {
	Location *Location
	Version  int64
	Phase    Phase
	Message  string
}

func (s State) WithPhase(phase Phase, message string) State {
	s.Phase, s.Message = phase, message
	return s
}

func (s State) Confirmed() bool { return s.Version > 0 }

// Created 只接受首次创建并回读得到的版本，不能把后续写入认作首次供应。
func (s State) Created(version int64) (State, *Issue) {
	if version != 1 {
		return s, &Issue{Conflict, "凭据创建冲突或结果不确定；请核对固定位置的版本历史，未认领、覆盖或重新生成密码"}
	}
	s.Version = version
	return s.WithPhase(Prepared, "凭据已创建并回读确认；尚未创建 PostgreSQL 资源或交付给 Tenant"), nil
}

// Resume 决定新一轮是否可以继续。未确认的创建不能靠读取成功认领。
func (s State) Resume() (State, bool) {
	if s.Version != 0 {
		return s, true
	}
	switch s.Phase {
	case Conflict:
		return s, false
	case Creating:
		return s.WithPhase(Conflict, "凭据创建未留下成功确认；请核对固定位置与后端历史并人工处理，未重新生成密码"), false
	default:
		return s, true
	}
}

func (s State) CheckLocation(configured Location) *Issue {
	if s.Location != nil && *s.Location != configured {
		return &Issue{Unavailable, "部署配置与固定凭据位置不一致；请恢复原 mount/path 配置，未迁移或改密"}
	}
	return nil
}

type Instance struct {
	binding.Instance
	Endpoint instance.Endpoint
}

// Target 只包含供应资格所需事实，不含 resourceVersion、Conditions 或 repository 对象。
type Target struct {
	Database          binding.Database
	Tenant            *binding.Tenant
	Instance          *Instance
	DatabaseProtected bool
	TenantProtected   bool
}

type Issue struct {
	Phase   Phase
	Message string
}

func (t Target) RequiresPreparation() bool { return t.Database.Source == "Provision" }

func (t Target) CheckCredential(value ApplicationCredential) *Issue {
	if t.Instance == nil || !value.MatchesTarget(t.Database.LoginRole, t.Database.Name, t.Instance.Endpoint) {
		return &Issue{Conflict, "已确认凭据与当前 Instance/database/loginRole 不一致；请人工核实，未修改凭据"}
	}
	return nil
}

func (t Target) Check() *Issue {
	database := t.Database
	if database.Deleting || database.Phase == binding.Deleting || database.Phase == "Released" {
		return &Issue{Stopped, "Database 正在删除或已释放；保留凭据与 finalizer，不执行供应或清理"}
	}
	if database.Tenant == nil || t.Tenant == nil || t.Tenant.Database == nil {
		return &Issue{Unavailable, "等待 Database 与 Tenant 双向绑定完成"}
	}
	if *database.Tenant != t.Tenant.Identity || *t.Tenant.Database != database.Identity {
		return &Issue{Conflict, "双向绑定的名称或 UID 不匹配，未创建凭据"}
	}
	if t.Tenant.Deleting || t.Tenant.Phase != binding.Bound || !t.DatabaseProtected || !t.TenantProtected {
		return &Issue{Stopped, "Tenant 未完成绑定、正在删除或缺少 finalizer 保护，未创建凭据"}
	}
	request, err := t.Tenant.Request.Resolve(t.Tenant.Identity)
	if err != nil || (request.Provision != nil && !database.MatchesProvision(request, t.Tenant.Identity)) || request.Name != database.Identity.Name {
		return &Issue{Conflict, "Tenant 申请与 Database 目标不一致，未创建凭据"}
	}
	if t.Instance == nil || database.InstanceUID == "" {
		return &Issue{Unavailable, "等待 Instance 与已记录的实例身份"}
	}
	if issue := t.Instance.Check(&database); issue != nil {
		phase := Unavailable
		if issue.Reason == binding.Conflict {
			phase = Conflict
		}
		return &Issue{phase, issue.Message}
	}
	return nil
}
