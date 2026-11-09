package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PostgreSQLEndpoint 区分证书主机名与实际连接 IP；未声明 hostaddr 时连接按 host 解析。
type PostgreSQLEndpoint struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="hostaddr must be a single IPv4 or IPv6 address"
	// +optional
	HostAddr string `json:"hostaddr,omitempty"`
	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default=postgres
	// +optional
	Database PostgreSQLIdentifier `json:"database,omitempty"`
	// +kubebuilder:default=verify-full
	// +kubebuilder:validation:Enum=disable;require;verify-ca;verify-full
	// +optional
	SSLMode string `json:"sslMode,omitempty"`
}

// AdminCredentialReference 只能读取 controller namespace 的 Secret。
type AdminCredentialReference struct {
	Name ObjectName `json:"name"`
	// +kubebuilder:default=username
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
	// +kubebuilder:default=password
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

type PostgreSQLInstanceSpec struct {
	Endpoint           PostgreSQLEndpoint       `json:"endpoint"`
	AdminCredentialRef AdminCredentialReference `json:"adminCredentialRef"`
}

type PostgreSQLInstanceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Validating;Ready;Deleting
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	PostgreSQLVersion string `json:"postgresqlVersion,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
type PostgreSQLInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              PostgreSQLInstanceSpec `json:"spec"`
	// +optional
	Status PostgreSQLInstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type PostgreSQLInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgreSQLInstance `json:"items"`
}
