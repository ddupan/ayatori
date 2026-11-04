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
	// Ports 是各端口的可用成员数（ADR-0023）。
	// +listType=map
	// +listMapKey=port
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Ports []PortStatus `json:"ports,omitempty"`
	// Endpoints 是各端口的各成员在各主机上的健康检查结论，每个（端口、成员、主机）一项（ADR-0023）。
	// 平铺而非嵌套：kube-state-metrics 的 custom resource state 只展开一层列表，嵌套的明细导不出指标。
	// 观察不到的主机不列出。
	// +listType=map
	// +listMapKey=port
	// +listMapKey=address
	// +listMapKey=endpointPort
	// +listMapKey=host
	// +kubebuilder:validation:MaxItems=4096
	// +optional
	Endpoints []EndpointStatus `json:"endpoints,omitempty"`
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

type PortStatus struct {
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Name string `json:"name,omitempty"`
	Port int32  `json:"port"`
	// ReadyEndpoints 是至少在一台主机上 UP 的成员数。
	ReadyEndpoints int32 `json:"readyEndpoints"`
	// Endpoints 是引用的 Backend 中的成员数。
	Endpoints int32 `json:"endpoints"`
}

// EndpointStatus 是一个端口的一个成员在一台主机上的检查结论，对应 HAProxy exporter 的
// server_status、server_check_status 与 server_check_code。
type EndpointStatus struct {
	// Port 是 LoadBalancer 的端口。
	Port int32 `json:"port"`
	// +kubebuilder:validation:MaxLength=253
	Address             string `json:"address"`
	EndpointPort        int32  `json:"endpointPort"`
	EndpointObservation `json:",inline"`
}

// EndpointObservation 是一台主机上最近一次观察到的检查结论。
type EndpointObservation struct {
	// +kubebuilder:validation:MaxLength=63
	Host string `json:"host"`
	// State 是 UP、DOWN、MAINT、DRAIN、NOLB 或 NOCHECK；翻转途中的「UP 1/3」记为 UP。
	// +kubebuilder:validation:MaxLength=16
	State string `json:"state"`
	// CheckStatus 是最近一次检查的结论，如 L4OK、L4CON、L4TOUT、L7OK、L7STS。
	// +kubebuilder:validation:MaxLength=16
	// +optional
	CheckStatus string `json:"checkStatus,omitempty"`
	// CheckCode 是 HTTP 检查的返回码；L4 检查没有。
	// +optional
	CheckCode int32 `json:"checkCode,omitempty"`
	// LastTransitionTime 是 State 最近一次变化的时间。
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
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
// +kubebuilder:printcolumn:name="EndpointsReady",type=string,JSONPath=`.status.conditions[?(@.type=='EndpointsReady')].status`
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
