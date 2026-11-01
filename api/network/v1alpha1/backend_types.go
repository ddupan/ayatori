package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackendSpec 是一组后端成员及与它们说话的方式；由维护成员的一方整体写入，与 VIP、端口分开授权
// （ADR-0015）。
type BackendSpec struct {
	// Endpoints 由消费方整组替换；为空时引用它的端口拒绝连接。
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
	// DrainTimeout 是 endpoint 被移出集合后保留已有连接的上限；超时后剩余连接由代理
	// 旧 worker 服务至主机级 hard-stop-after。
	// +kubebuilder:default="30s"
	// +optional
	DrainTimeout metav1.Duration `json:"drainTimeout,omitzero"`
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

// Backend 是 namespace 内被 LoadBalancer 端口引用的后端集合。
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Backend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              BackendSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type BackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Backend `json:"items"`
}
