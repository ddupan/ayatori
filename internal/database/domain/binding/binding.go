// Package binding 定义资源与申请的纯绑定规则，不访问 Kubernetes 或数据库。
package binding

import (
	"fmt"
	"regexp"
)

const (
	Binding               = "Binding"
	Bound                 = "Bound"
	Deleting              = "Deleting"
	Conflict              = "Conflict"
	DependencyUnavailable = "DependencyUnavailable"
)

type Identity struct {
	Name string
	UID  string
}

type TenantIdentity struct {
	Namespace string
	Name      string
	UID       string
}

// Request 保留用户输入；Resolve 产生默认值已确定的目标，不修改原请求。
type Request struct {
	Provision        *ProvisionRequest
	ExistingDatabase string
}

type ProvisionRequest struct {
	Instance  string
	Database  string
	LoginRole string
}

type Target struct {
	Name      string
	Provision *ProvisionRequest
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func (r Request) Resolve(tenant TenantIdentity) (Target, error) {
	if (r.Provision == nil) == (r.ExistingDatabase == "") {
		return Target{}, fmt.Errorf("必须且只能选择动态申请或已有 Database")
	}
	if r.Provision == nil {
		return Target{Name: r.ExistingDatabase}, nil
	}
	provision := *r.Provision
	if provision.Database == "" {
		provision.Database = tenant.Name
	}
	if provision.LoginRole == "" {
		provision.LoginRole = tenant.Name
	}
	if !identifier.MatchString(provision.Database) || !identifier.MatchString(provision.LoginRole) {
		return Target{}, fmt.Errorf("动态 database/loginRole 必须符合 PostgreSQL identifier 规则；省略时使用 Tenant 名称")
	}
	return Target{Name: DynamicDatabaseName(tenant.UID), Provision: &provision}, nil
}

func DynamicDatabaseName(tenantUID string) string { return "tenant-" + tenantUID }

type Tenant struct {
	Identity TenantIdentity
	Request  Request
	Phase    string
	Deleting bool
	Database *Identity
}

// Database 是绑定所需的资源事实，不包含存储版本、Conditions 或客户端对象。
type Database struct {
	Identity    Identity
	Instance    string
	InstanceUID string
	Name        string
	LoginRole   string
	Source      string
	Tenant      *TenantIdentity
	Phase       string
	Deleting    bool
	Ready       bool
}

type Instance struct {
	Identity Identity
	Deleting bool
	Ready    bool
}

type Issue struct {
	Reason  string
	Message string
}

func (d Database) MatchesProvision(target Target, tenant TenantIdentity) bool {
	return target.Provision != nil && d.Source == "Provision" && d.Instance == target.Provision.Instance &&
		d.Name == target.Provision.Database && d.LoginRole == target.Provision.LoginRole &&
		d.Tenant != nil && *d.Tenant == tenant
}

func (i Instance) Check(database *Database) *Issue {
	if i.Deleting || !i.Ready {
		return &Issue{DependencyUnavailable, "Instance 正在删除或尚无当前版本的 Ready 观察"}
	}
	if database != nil && database.InstanceUID != "" && database.InstanceUID != i.Identity.UID {
		return &Issue{Conflict, "Instance UID 已变化；请核实实例身份，未迁移或接管资源"}
	}
	return nil
}

func (d Database) CanBind(tenant Tenant) *Issue {
	if tenant.Database != nil && tenant.Database.UID != d.Identity.UID {
		return &Issue{Conflict, fmt.Sprintf("Database %s 的 UID 与已记录绑定不同；请核实同名重建，未接管新对象", d.Identity.Name)}
	}
	if d.Deleting || d.Phase == "Released" || d.Phase == Deleting {
		return &Issue{Conflict, "Database 正在删除或处于 Released；请由管理员核实并处理，未重新分配"}
	}
	if d.Tenant != nil {
		if *d.Tenant != tenant.Identity {
			return &Issue{Conflict, fmt.Sprintf("Database %s 已绑定 Tenant %s/%s（UID %s）；未抢占",
				d.Identity.Name, d.Tenant.Namespace, d.Tenant.Name, d.Tenant.UID)}
		}
		return nil
	}
	if d.Phase != "Available" || !d.Ready {
		return &Issue{DependencyUnavailable, "Database 尚未完成验证并进入 Available，等待资源观察"}
	}
	return nil
}
