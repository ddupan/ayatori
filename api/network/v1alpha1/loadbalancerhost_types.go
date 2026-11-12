package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LoadBalancerHostSpec 由管理员持有，agent 只读（ADR-0022）。
type LoadBalancerHostSpec struct {
	// DataPlaneURL 已废弃、不再使用：manager 不再连接 LB 主机（ADR-0022）。只为仍在 GitOps 中声明它的
	// 环境保留，下一个 minor 删除。
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() == 'https'",message="dataPlaneURL must be an https URL"
	// +optional
	DataPlaneURL string `json:"dataPlaneURL,omitempty"`
}

type LoadBalancerHostStatus struct {
	// ObservedGeneration 是本 status 观察的 spec 版本。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// VIPs 是 agent 报告的每个 VIP 是否已发给路由器；agent 的 Lease 过期时由 controller 清空。
	// 只呈现观察，LoadBalancer 的 Attachment 以它为准。
	// +listType=map
	// +listMapKey=address
	// +kubebuilder:validation:MaxItems=256
	// +optional
	VIPs []AgentVIPStatus `json:"vips,omitempty"`
	// LoadBalancers 是 agent 在本机收敛各 LoadBalancer 的结果（ADR-0022）：每个 LoadBalancer 一项，
	// controller 据此汇总 LoadBalancer 的 status。对象删除完成后移除该项。
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=1024
	// +optional
	LoadBalancers []HostLoadBalancerStatus `json:"loadBalancers,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type AgentVIPStatus struct {
	// +kubebuilder:validation:MaxLength=15
	Address   string `json:"address"`
	Announced bool   `json:"announced"`
	// Reason 是未宣告的原因：not-local、frontend-not-open 或 not-advertised。
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Reason string `json:"reason,omitempty"`
}

// HostLoadBalancerStatus 是一个 LoadBalancer 在本机的代理配置事实。
type HostLoadBalancerStatus struct {
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// UID 区分同名重建的对象：本机配置按 UID 命名。
	// +kubebuilder:validation:MaxLength=36
	UID string `json:"uid"`
	// Digest 是本机配置文件与之完全一致的期望配置摘要；不一致时为空。controller 以同一算法计算
	// 期望的摘要并比较，Backend 变化不改变 LoadBalancer 的 generation，所以不用 generation。
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Digest string `json:"digest,omitempty"`
	// Present 表示本机存在属于该 LoadBalancer 的任何配置。
	Present bool `json:"present"`
	// Reload 是本机最近一次 reload 的结果：Succeeded、Pending 或 Failed。
	// +kubebuilder:validation:Enum=Succeeded;Pending;Failed
	Reload string `json:"reload"`
	// Message 是 reload 失败或无法观察本机代理时的说明。
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
	// Servers 是运行中的进程里各 server 的健康检查结论（ADR-0023）。
	// +listType=map
	// +listMapKey=port
	// +listMapKey=address
	// +listMapKey=endpointPort
	// +kubebuilder:validation:MaxItems=1024
	// +optional
	Servers []HostServerStatus `json:"servers,omitempty"`
}

type HostServerStatus struct {
	// Port 是 LoadBalancer 的端口。
	Port int32 `json:"port"`
	// +kubebuilder:validation:MaxLength=253
	Address      string `json:"address"`
	EndpointPort int32  `json:"endpointPort"`
	// +kubebuilder:validation:MaxLength=16
	State string `json:"state"`
	// +kubebuilder:validation:MaxLength=16
	// +optional
	CheckStatus string `json:"checkStatus,omitempty"`
	// +optional
	CheckCode int32 `json:"checkCode,omitempty"`
}

// LoadBalancerHost 是一台 LB 主机；每个 LoadBalancer 落地到全部 LoadBalancerHost。由该主机的 agent
// 以与 Node 同名注册并写入 status（ADR-0022）；删除由管理员发起，先清理该主机上全部 LoadBalancer
// 配置。退役时先停 agent，否则它会重新注册。
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=lbhost
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="name must be a DNS label because it appears in LoadBalancer status"
// +kubebuilder:printcolumn:name="Data Plane",type=string,JSONPath=`.spec.dataPlaneURL`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=='DataPlaneReachable')].status`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.conditions[?(@.type=='AgentReady')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type LoadBalancerHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +optional
	Spec LoadBalancerHostSpec `json:"spec,omitzero"`
	// +optional
	Status LoadBalancerHostStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LoadBalancerHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LoadBalancerHost `json:"items"`
}
