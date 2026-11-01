package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LoadBalancerHostSpec 指向一台 LB 主机上的 Data Plane API（ADR-0015）。凭据按环境统一，留在
// controller 的凭据 Secret，资源里只有地址。
type LoadBalancerHostSpec struct {
	// DataPlaneURL 是该主机 Data Plane API 的 HTTPS 地址，controller 以 mTLS 与 Basic 凭据访问。
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() == 'https'",message="dataPlaneURL must be an https URL"
	DataPlaneURL string `json:"dataPlaneURL"`
}

type LoadBalancerHostStatus struct {
	// ObservedGeneration 是本 status 观察的 spec 版本。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// LoadBalancerHost 是一台 LB 主机；每个 LoadBalancer 落地到全部 LoadBalancerHost。由部署方创建，
// 删除时先清理该主机上全部 LoadBalancer 配置。
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=lbhost
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="name must be a DNS label because it appears in LoadBalancer status"
// +kubebuilder:printcolumn:name="Data Plane",type=string,JSONPath=`.spec.dataPlaneURL`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=='DataPlaneReachable')].status`
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
