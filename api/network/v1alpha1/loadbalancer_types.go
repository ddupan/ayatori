package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LoadBalancerSpec 声明一个 L4 TCP 入口：VIP 与各端口的后端集合、健康检查。
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

// LoadBalancerPort 是 VIP 上的一个 TCP 端口及其后端集合，类似 Octavia listener + pool。
type LoadBalancerPort struct {
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Endpoints 由消费方整组替换；为空时该端口拒绝连接。
	// +listType=map
	// +listMapKey=address
	// +listMapKey=port
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// ProxyProtocol 向后端发送 PROXY 头以传递客户端地址；健康检查同样携带。
	// +kubebuilder:validation:Enum=None;V1;V2
	// +kubebuilder:default=None
	// +optional
	ProxyProtocol string `json:"proxyProtocol,omitempty"`
	// IdleTimeout 是客户端与后端两侧连接的空闲上限。
	// +kubebuilder:default="1h"
	// +optional
	IdleTimeout metav1.Duration `json:"idleTimeout,omitzero"`
	// HealthCheck 省略时以 TCP 探测各 endpoint 自身端口。默认值写全：apiserver 校验默认值时不展开嵌套默认。
	// +kubebuilder:default={type: "TCP", interval: "5s", timeout: "3s", healthyThreshold: 2, unhealthyThreshold: 3}
	// +optional
	HealthCheck HealthCheck `json:"healthCheck,omitzero"`
}

// Endpoint 是后端地址与端口；地址可以是 DNS 名称，由数据面周期解析。
type Endpoint struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="isIP(self) || self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$')",message="address must be an IP address or a DNS name"
	Address string `json:"address"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// HealthCheck 决定 endpoint 是否接收新连接。没有健康 endpoint 时拒绝连接，
// endpoint 被判失败时断开其已有连接；这两项是固定行为。
// +kubebuilder:validation:XValidation:rule="self.type == 'HTTP' ? has(self.path) : !has(self.path) && !has(self.expectedStatuses)",message="path is required for HTTP and not allowed for TCP checks"
// +kubebuilder:validation:XValidation:rule="duration(self.timeout) <= duration(self.interval)",message="timeout must not exceed interval"
type HealthCheck struct {
	// +kubebuilder:validation:Enum=TCP;HTTP
	// +kubebuilder:default=TCP
	// +optional
	Type string `json:"type,omitempty"`
	// Port 覆盖探测端口；省略时使用各 endpoint 自身端口。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^/`
	// +optional
	Path string `json:"path,omitempty"`
	// ExpectedStatuses 省略时期望 200。
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=100
	// +kubebuilder:validation:items:Maximum=599
	// +listType=set
	// +optional
	ExpectedStatuses []int32 `json:"expectedStatuses,omitempty"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="interval must be at least 1s"
	// +kubebuilder:default="5s"
	// +optional
	Interval metav1.Duration `json:"interval,omitzero"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="timeout must be at least 1s"
	// +kubebuilder:default="3s"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitzero"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=2
	// +optional
	HealthyThreshold int32 `json:"healthyThreshold,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	// +optional
	UnhealthyThreshold int32 `json:"unhealthyThreshold,omitempty"`
}

type LoadBalancerStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Address 是已确认占用的 VIP；一经记录不再改变。
	// +kubebuilder:validation:MaxLength=15
	// +optional
	Address string `json:"address,omitempty"`
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
	// +kubebuilder:validation:Enum=Pending;Configured;Announced;Withdrawn;Unavailable
	Phase string `json:"phase"`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
}

// LoadBalancer 是 namespace 内消费方声明的 L4 入口。
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=lb
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.status) && has(oldSelf.status.address)) || (has(self.status) && has(self.status.address) && self.status.address == oldSelf.status.address)",message="recorded address cannot change or be removed"
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
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
