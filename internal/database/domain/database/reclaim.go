package database

import (
	"errors"
	"fmt"
)

// Released 是 Tenant 释放后的资源阶段：保留旧绑定身份，不再供应，也不自动重新分配。
const Released = "Released"

type ReclaimPolicy string

const (
	ReclaimRetain ReclaimPolicy = "Retain"
	ReclaimDelete ReclaimPolicy = "Delete"
)

// ResourceReclaiming 表示 Delete 策略的清理正在推进。
const ResourceReclaiming ResourcePhase = "Reclaiming"

// Lifecycle 是本轮在供应之前要处理的生命周期动作。
type Lifecycle uint8

const (
	// Continue 表示按正常流程供应或验证。
	Continue Lifecycle = iota
	// Hold 表示删除请求在等待释放，保留 finalizer。
	Hold
	// Forget 表示可以移除 finalizer，不触碰外部资源。
	Forget
	// Reclaim 表示按 Delete 策略清理外部资源。
	Reclaim
)

func (d *Database) released() bool { return d.snapshot.Binding.Phase == Released }

// PlanLifecycle 对照 PV 的 Retain/Delete：资源在释放前受保护；释放后 Retain 只放行删除，
// Delete 才清理外部资源。导入资源不隐含删除授权。
func (d *Database) PlanLifecycle() Lifecycle {
	b := d.snapshot.Binding
	bound := b.Tenant != nil && !d.released()
	switch {
	case bound && b.Deleting:
		d.reportResources(ResourceStopped, "Database 仍绑定 Tenant，先删除 Tenant 释放资源；未删除外部对象")
		return Hold
	case bound:
		return Continue
	// 从未绑定的资源没有供应过外部对象（供应以双向绑定为前提），删除时直接放行。
	case !d.released() || d.snapshot.ReclaimPolicy != ReclaimDelete:
		if b.Deleting {
			return Forget
		}
		return Continue
	case !d.RequiresProvisioning():
		d.reportResources(ResourceStopped, "导入资源不支持 Delete 回收；请改为 Retain 并人工处理外部对象")
		if b.Deleting {
			return Hold
		}
		return Continue
	default:
		return Reclaim
	}
}

// ObserveReclaim 按已确认 OID 决定下一项清理；对象已不存在则跳过，同名但身份不符则停止。
func (d *Database) ObserveReclaim(facts ResourceFacts, err error) Action {
	if err != nil {
		d.reportResources(ResourceUnavailable, "无法验证待清理的 PostgreSQL 资源")
		return Wait
	}
	state := d.snapshot.Resources
	if facts.DatabaseOID != 0 && facts.DatabaseOID != state.DatabaseOID {
		d.reportResources(ResourceConflict, fmt.Sprintf("同名数据库不是已确认对象（记录 OID=%d，观察 OID=%d）；未删除", state.DatabaseOID, facts.DatabaseOID))
		return Wait
	}
	if facts.RoleOID != 0 && facts.RoleOID != state.RoleOID {
		d.reportResources(ResourceConflict, fmt.Sprintf("同名角色不是已确认对象（记录 OID=%d，观察 OID=%d）；未删除", state.RoleOID, facts.RoleOID))
		return Wait
	}
	switch {
	case facts.DatabaseOID != 0:
		return DropDatabase
	case facts.RoleOID != 0:
		return DropRole
	case d.snapshot.Credential.Location != nil:
		return DestroyCredential
	default:
		return Finalize
	}
}

// Reclaimed 记录一步清理的结果；同名依赖等需人工处理时转为冲突，其余失败等待重试。
func (d *Database) Reclaimed(step string, err error) {
	switch {
	case errors.Is(err, ErrConflict):
		d.reportResources(ResourceConflict, step+"被依赖对象阻止；请人工核对，未扩大删除范围")
	case err != nil:
		d.reportResources(ResourceUnavailable, step+"未完成，保留 finalizer 并等待重试")
	default:
		d.reportResources(ResourceReclaiming, step+"已完成")
	}
}
