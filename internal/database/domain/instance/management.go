package instance

// ManagementFacts 是一次查询得到的管理账号原始属性，不含就绪结论。
// AvailableExtensions 是服务器提供的可用列表，不是已安装列表或安装授权。
type ManagementFacts struct {
	Version             string
	AvailableExtensions []string
	Superuser           bool
	CreateRole          bool
	CreateDatabase      bool
	ReadOnly            bool
}

// Checks 判断原生管理能力：要求非 superuser 且具备 CREATEROLE/CREATEDB。
// 属性只取当前执行角色本身，不能从可继承的成员关系推导。
func (f ManagementFacts) Checks() ManagementChecks {
	native := func(allowed bool) CheckResult {
		if allowed && !f.Superuser {
			return CheckPassed
		}
		return CheckInsufficientPrivileges
	}
	checks := ManagementChecks{
		Connection: CheckPassed,
		Metadata:   CheckPassed,
		Roles:      native(f.CreateRole),
		Databases:  native(f.CreateDatabase),
		// 供应时以管理账号建库、建角色并移交 owner；不授权操作任意导入角色或他人数据库。
		Grants: native(f.CreateRole && f.CreateDatabase),
		// 数据库 owner 可安装 trusted 扩展；具体扩展仍需逐请求执行和回读。
		Extensions: native(f.CreateRole && f.CreateDatabase),
	}
	if f.ReadOnly {
		checks.Databases = CheckUnavailable
	}
	return checks
}
