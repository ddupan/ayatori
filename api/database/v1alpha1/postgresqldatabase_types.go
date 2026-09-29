package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PostgreSQLDatabaseSpec 是一库、一个 login owner 及凭据的独立资源声明。
// +kubebuilder:validation:XValidation:rule="(self.source == 'Import') == has(self.credentialRef)",message="only imported databases require an existing credentialRef"
type PostgreSQLDatabaseSpec struct {
	InstanceRef InstanceReference    `json:"instanceRef"`
	Database    PostgreSQLIdentifier `json:"database"`
	LoginRole   PostgreSQLIdentifier `json:"loginRole"`
	// Source 明确区分创建与只读导入，不从后端同名对象推断。
	// +kubebuilder:validation:Enum=Provision;Import
	Source string `json:"source"`
	// +optional
	CredentialRef *CredentialReference `json:"credentialRef,omitempty"`
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	// TenantRef 由 controller 先写入；Released 时仍保留旧身份。
	// +optional
	TenantRef *TenantReference `json:"tenantRef,omitempty"`
}

type PostgreSQLDatabaseStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// InstanceUID 记录观察时的实例身份，不把同名新实例视为原目标。
	// +optional
	InstanceUID types.UID `json:"instanceUID,omitempty"`
	// CredentialRef 在首次外部写入前固定凭据位置；部署配置变化不迁移此位置。
	// +optional
	CredentialRef *CredentialReference `json:"credentialRef,omitempty"`
	// CredentialVersion 只在创建并回读成功后记录，不表示凭据当前仍可用。
	// 省略表示尚未确认；已有凭据不能仅凭读取成功补记确认。
	// +kubebuilder:validation:Minimum=1
	// +optional
	CredentialVersion int64 `json:"credentialVersion,omitempty"`
	// RoleOID 只在角色创建提交并回读成功后保存，后续不按同名认领。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	// +optional
	RoleOID int64 `json:"roleOID,omitempty"`
	// DatabaseOID 只在数据库创建并回读成功后保存；缺失不代表允许认领已有对象。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	// +optional
	DatabaseOID int64 `json:"databaseOID,omitempty"`
	// Phase 暂不冻结供应子阶段枚举；它不是操作授权或绑定的替代记录。
	// +optional
	Phase string `json:"phase,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.roleOID) || (has(self.status.credentialVersion) && has(self.status.instanceUID))",message="roleOID requires confirmed credentials and instanceUID"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.databaseOID) || has(self.status.roleOID)",message="databaseOID requires roleOID"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.roleOID)) || (has(self.status) && has(self.status.roleOID) && self.status.roleOID == oldSelf.status.roleOID)",message="confirmed roleOID cannot change or be removed"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.databaseOID)) || (has(self.status) && has(self.status.databaseOID) && self.status.databaseOID == oldSelf.status.databaseOID)",message="confirmed databaseOID cannot change or be removed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.credentialVersion) || has(self.status.credentialRef)",message="credentialVersion requires credentialRef"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.credentialRef)) || (has(self.status) && has(self.status.credentialRef) && self.status.credentialRef == oldSelf.status.credentialRef)",message="recorded credentialRef cannot change or be removed"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.credentialVersion)) || (has(self.status) && has(self.status.credentialVersion) && self.status.credentialVersion == oldSelf.status.credentialVersion)",message="confirmed credentialVersion cannot change or be removed"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.spec.tenantRef) || (has(oldSelf.status) && (has(oldSelf.status.instanceUID) || has(oldSelf.status.credentialRef)))) || (self.spec.instanceRef == oldSelf.spec.instanceRef && self.spec.database == oldSelf.spec.database && self.spec.loginRole == oldSelf.spec.loginRole && self.spec.source == oldSelf.spec.source && has(self.spec.credentialRef) == has(oldSelf.spec.credentialRef) && (!has(oldSelf.spec.credentialRef) || self.spec.credentialRef == oldSelf.spec.credentialRef))",message="managed database target cannot change after observation or binding starts"
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.database`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type PostgreSQLDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              PostgreSQLDatabaseSpec `json:"spec"`
	// +optional
	Status PostgreSQLDatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type PostgreSQLDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgreSQLDatabase `json:"items"`
}
