package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// DatabaseProvisionRequest 仅用于动态申请，省略名称时由 controller 按 Tenant 名称解析。
type DatabaseProvisionRequest struct {
	InstanceRef InstanceReference `json:"instanceRef"`
	// +optional
	Database PostgreSQLIdentifier `json:"database,omitempty"`
	// +optional
	LoginRole PostgreSQLIdentifier `json:"loginRole,omitempty"`
}

// PostgreSQLTenantSpec 显式选择动态申请或已有 Database，不重复声明来源。
// +kubebuilder:validation:XValidation:rule="has(self.provision) != has(self.databaseRef)",message="exactly one of provision and databaseRef is required"
type PostgreSQLTenantSpec struct {
	// +optional
	Provision *DatabaseProvisionRequest `json:"provision,omitempty"`
	// +optional
	DatabaseRef *DatabaseReference `json:"databaseRef,omitempty"`
	// Extensions 保留后端扩展名称的原样拼写，不按 SQL identifier 限制。
	// +listType=set
	// +optional
	Extensions []string `json:"extensions,omitempty"`
	// SecretName 指定 Tenant namespace 内的投射目标，省略时使用合同约定的默认名称。
	// +optional
	SecretName ObjectName `json:"secretName,omitempty"`
}

type PostgreSQLTenantStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// DatabaseRef 只有在资源侧确认绑定后才写入。
	// +optional
	DatabaseRef *BoundDatabaseReference `json:"databaseRef,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// SecretName 是已观察到的同 namespace 投射目标，不包含凭据值。
	// +optional
	SecretName ObjectName `json:"secretName,omitempty"`
	// CredentialURL 只含 OpenBao API 位置，禁止嵌入认证信息。
	// +optional
	CredentialURL string `json:"credentialURL,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || !(oldSelf.status.phase in ['Binding', 'Bound', 'Deleting']) || ((has(self.spec.provision) == has(oldSelf.spec.provision)) && (!has(oldSelf.spec.provision) || self.spec.provision == oldSelf.spec.provision) && (has(self.spec.databaseRef) == has(oldSelf.spec.databaseRef)) && (!has(oldSelf.spec.databaseRef) || self.spec.databaseRef == oldSelf.spec.databaseRef))",message="binding target cannot change after binding starts"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || !(oldSelf.status.phase in ['Binding', 'Bound', 'Deleting']) || (has(self.status) && has(self.status.phase) && self.status.phase in ['Binding', 'Bound', 'Deleting'])",message="binding progress cannot return to an unbound state"
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.databaseRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type PostgreSQLTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              PostgreSQLTenantSpec `json:"spec"`
	// +optional
	Status PostgreSQLTenantStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type PostgreSQLTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgreSQLTenant `json:"items"`
}
