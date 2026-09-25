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
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.spec.tenantRef) || (has(oldSelf.status) && has(oldSelf.status.instanceUID))) || (self.spec.instanceRef == oldSelf.spec.instanceRef && self.spec.database == oldSelf.spec.database && self.spec.loginRole == oldSelf.spec.loginRole && self.spec.source == oldSelf.spec.source && has(self.spec.credentialRef) == has(oldSelf.spec.credentialRef) && (!has(oldSelf.spec.credentialRef) || self.spec.credentialRef == oldSelf.spec.credentialRef))",message="managed database target cannot change after observation or binding starts"
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
