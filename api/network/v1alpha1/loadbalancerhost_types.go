package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LoadBalancerHostSpec 指向一台 LB 主机上的 Data Plane API（ADR-0018）。凭据按环境统一，留在
// controller 的凭据 Secret，资源里只有地址。
type LoadBalancerHostSpec struct {
	// DataPlaneURL 是该主机 Data Plane API 的 HTTPS 地址，controller 以 mTLS 与 Basic 凭据访问。
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() == 'https'",message="dataPlaneURL must be an https URL"
	DataPlaneURL string `json:"dataPlaneURL"`
	// AgentURL 是该主机 LB agent 指标端口的地址，controller 读取其只读的 GET /status。
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() in ['http', 'https']",message="agentURL must be an http or https URL"
	AgentURL string `json:"agentURL"`
}

type LoadBalancerHostStatus struct {
	// ObservedGeneration 是本 status 观察的 spec 版本。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// VIPs 是 agent 最近一次报告的每个 VIP 是否已发给路由器；agent 不应答时清空。只呈现观察，
	// LoadBalancer 的 Attachment 以它为准。
	// +listType=map
	// +listMapKey=address
	// +kubebuilder:validation:MaxItems=256
	// +optional
	VIPs []AgentVIPStatus `json:"vips,omitempty"`
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

// LoadBalancerHost 是一台 LB 主机；每个 LoadBalancer 落地到全部 LoadBalancerHost。由部署方创建，
// 删除时先清理该主机上全部 LoadBalancer 配置。
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
	Spec              LoadBalancerHostSpec `json:"spec"`
	// +optional
	Status LoadBalancerHostStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LoadBalancerHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LoadBalancerHost `json:"items"`
}
