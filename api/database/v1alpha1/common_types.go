package v1alpha1

import "k8s.io/apimachinery/pkg/types"

// ObjectName 定位集群级资源，不携带 namespace 或隐式跨 API group 引用。
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
type ObjectName string

// PostgreSQLIdentifier 是第一版受管 database 与 login role 使用的名称。
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,62}$`
type PostgreSQLIdentifier string

// InstanceReference 仅引用同 API group 的集群级 PostgreSQLInstance。
type InstanceReference struct {
	Name ObjectName `json:"name"`
}

// DatabaseReference 是 Tenant 对已有集群级 PostgreSQLDatabase 的选择。
type DatabaseReference struct {
	Name ObjectName `json:"name"`
}

// BoundDatabaseReference 记录已经参与绑定的对象身份，而非仅记录可复用的名称。
type BoundDatabaseReference struct {
	Name ObjectName `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}

// TenantReference 是 Database 的当前绑定记录，不是允许绑定名单。
type TenantReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string     `json:"namespace"`
	Name      ObjectName `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}

// ReclaimPolicy 控制资源释放后的处置，只有资源管理者可以修改。
// +kubebuilder:validation:Enum=Retain;Delete
type ReclaimPolicy string

const (
	ReclaimRetain ReclaimPolicy = "Retain"
	ReclaimDelete ReclaimPolicy = "Delete"
)

// CredentialReference 定位 OpenBao KV v2 凭据，不包含任何秘密值。
// 管理员在导入声明中指定，controller 在 status 中固定位置；两者均须检查部署允许的范围。
type CredentialReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Mount string `json:"mount"`
	// Path 是 mount 内的逻辑路径，不含 KV v2 的 data/ API 前缀。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path"`
}
