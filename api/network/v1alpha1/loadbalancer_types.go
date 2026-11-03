package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LoadBalancerSpec 声明一个 L4 TCP 入口：VIP 与各端口引用的 Backend。
// +kubebuilder:validation:XValidation:rule="has(self.address) == has(oldSelf.address) && (!has(self.address) || self.address == oldSelf.address)",message="address is immutable"
type LoadBalancerSpec struct {
	// Address 请求池内指定的 VIP；省略时自动分配。
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:XValidation:rule="isIP(self) && ip.isCanonical(self) && ip(self).family() == 4",message="address must be a canonical IPv4 address"
	// +optional
	Address string `json:"address,omitempty"`
	// +listType=map
	// +listMapKey=port
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Ports []LoadBalancerPort `json:"ports"`
}

// LoadBalancerPort 是 VIP 上的一个 TCP 端口及其引用的 Backend，类似 Octavia listener。
type LoadBalancerPort struct {
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// BackendRef 引用同 namespace 的 Backend；不存在时该端口拒绝连接。
	BackendRef BackendRef `json:"backendRef"`
	// IdleTimeout 是客户端与后端两侧连接的空闲上限。
	// +kubebuilder:default="1h"
	// +optional
	IdleTimeout metav1.Duration `json:"idleTimeout,omitzero"`
}

// BackendRef 只按名称引用；跨 namespace 引用需要授权机制，首版不支持。
type BackendRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

type LoadBalancerStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Address 是已确认占用的 VIP；一经记录不再改变。
	// +kubebuilder:validation:MaxLength=15
	// +optional
	Address string `json:"address,omitempty"`
	// Hostname 是已确认指向 VIP 的 DNS 名称 <name>.<namespace>.<zone>；未启用 DNS 时为空。
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// Hosts 是本轮在各 LB 主机上的落地进度，只呈现观察，不作为下一轮的依据。
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Hosts []HostStatus `json:"hosts,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type HostStatus struct {
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Pending;Configured;Ready;Removed;Unavailable
	Phase string `json:"phase"`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
}

// LoadBalancer 是 namespace 内消费方声明的 L4 入口。
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=lb
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="name must be a DNS label because it becomes part of the hostname"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.address)) || (has(self.status) && has(self.status.address) && self.status.address == oldSelf.status.address)",message="recorded address cannot change or be removed"
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=`.status.conditions[?(@.type=='Programmed')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type LoadBalancer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              LoadBalancerSpec `json:"spec"`
	// +optional
	Status LoadBalancerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LoadBalancerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LoadBalancer `json:"items"`
}
