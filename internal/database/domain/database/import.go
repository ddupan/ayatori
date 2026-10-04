package database

import (
	"errors"

	"git.ddupan.top/panxiao81/ayatori/internal/database/domain/credential"
)

// Available 是导入资源完成登记后的阶段，表示可以被 Tenant 显式申请。
const Available = "Available"

// 导入是管理员对已有对象的显式登记，也是 Released 资源重新开放的唯一途径：删除 Retain 的旧记录后
// 重新导入，新记录拥有新 UID，旧绑定身份不会延续。导入只读验证，不改密码、owner 或权限，不装扩展。
type registration struct {
	roleOID, databaseOID uint32
	version              int64
}

// PlanImport 判断本轮能否验证导入目标；冲突需要人工处理，不自动重入。
func (d *Database) PlanImport() Action {
	switch {
	case d.released():
		d.reportResources(ResourceStopped, "Database 已释放；如需重新开放，请删除本记录后重新导入")
		return Wait
	case d.snapshot.Resources.Phase == ResourceConflict:
		return Wait
	case d.snapshot.ImportLocation == nil:
		d.reportResources(ResourceUnavailable, "导入资源缺少 spec.credentialRef")
		return Wait
	}
	owner := d.dependencies.Instance
	if owner == nil || owner.Deleting || !owner.Ready {
		d.reportResources(ResourceUnavailable, "等待 Instance 当前 Ready")
		return Wait
	}
	if uid := d.snapshot.Binding.InstanceUID; uid != "" && uid != owner.Identity.UID {
		d.reportResources(ResourceConflict, "Instance UID 已变化；请核实实例身份，未迁移或接管资源")
		return Wait
	}
	return InspectResources
}

// ImportLocation 是管理员在 spec 中声明的已有凭据位置。
func (d *Database) ImportLocation() credential.Location { return *d.snapshot.ImportLocation }

// ObserveImport 只读核对已有凭据与 catalog 事实。首次登记以本轮完整观察到的 OID 和凭据版本为确认
// 身份——导入本身就是对该名称的显式授权；登记后与动态供应一样拒绝重建、owner 变化和版本漂移。
func (d *Database) ObserveImport(value credential.ApplicationCredential, version int64, credentialErr error,
	facts ResourceFacts, factsErr error) Action {
	switch {
	case errors.Is(credentialErr, ErrConflict):
		d.reportResources(ResourceConflict, "已确认的导入凭据消失、版本变化或内容无效；请人工核实，未生成替代密码")
		return Wait
	case errors.Is(credentialErr, ErrNotFound):
		d.reportResources(ResourceUnavailable, "spec.credentialRef 位置没有凭据")
		return Wait
	case credentialErr != nil:
		d.reportResources(ResourceUnavailable, "无法读取导入凭据，或位置不在部署配置的范围内")
		return Wait
	case d.dependencies.Instance == nil || !value.MatchesTarget(d.LoginRole(), d.Name(), d.dependencies.Instance.Endpoint):
		d.reportResources(ResourceConflict, "导入凭据与 Instance/database/loginRole 不一致；未修改凭据")
		return Wait
	case factsErr != nil:
		d.reportResources(ResourceUnavailable, "无法验证导入目标的 PostgreSQL 资源")
		return Wait
	case facts.DatabaseOID == 0 || facts.RoleOID == 0:
		d.reportResources(ResourceUnavailable, "导入目标数据库或登录角色不存在")
		return Wait
	}
	state := d.snapshot.Resources
	if state.DatabaseOID != 0 && (facts.DatabaseOID != state.DatabaseOID || facts.RoleOID != state.RoleOID) {
		d.reportResources(ResourceConflict, "已登记的数据库或角色被重建；请人工核对，未认领或覆盖")
		return Wait
	}
	if facts.OwnerOID != facts.RoleOID {
		d.reportResources(ResourceConflict, "导入数据库的 owner 不是 loginRole；导入不更换 owner")
		return Wait
	}
	if !facts.ManagerConnect {
		d.reportResources(ResourceUnavailable, "管理账号对导入数据库没有 CONNECT；导入不修改权限，请管理员授予")
		return Wait
	}
	if state.DatabaseOID == 0 {
		d.pending = &registration{roleOID: facts.RoleOID, databaseOID: facts.DatabaseOID, version: version}
	}
	return InspectExtensions
}

// register 在应用凭据实际登录成功后写入首次登记的确认身份。
func (d *Database) register() {
	if d.pending == nil {
		return
	}
	location := *d.snapshot.ImportLocation
	d.snapshot.Resources.RoleOID, d.snapshot.Resources.DatabaseOID = d.pending.roleOID, d.pending.databaseOID
	d.snapshot.Credential.Location, d.snapshot.Credential.Version = &location, d.pending.version
	d.snapshot.Credential.Phase, d.snapshot.Credential.Message = CredentialPrepared, "导入凭据已登记并可读取"
	d.snapshot.Binding.InstanceUID = d.dependencies.Instance.Identity.UID
	d.pending = nil
}
