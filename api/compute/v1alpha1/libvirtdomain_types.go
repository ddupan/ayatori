package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// RunState 只表示创建后与宿主机重启后是否应当拉起实例，不表示任何时候都必须在运行：
// 客户机内关机或运维人员关机都是有意的停止（ADR-0015）。
// +kubebuilder:validation:Enum=Running;Stopped
type RunState string

const (
	RunStateRunning RunState = "Running"
	RunStateStopped RunState = "Stopped"
)

type LibvirtDomainSpec struct {
	// DomainXML 是原生 domain XML，`virsh dumpxml` 的输出可以直接使用。name 与 uuid 由 Ayatori 填写；
	// <source pool='…' volume='…'/> 由 Agent 解析为具体路径后再下发（AppArmor 不认 pool 卷引用）。
	// +kubebuilder:validation:MinLength=1
	DomainXML string `json:"domainXML"`
	// NodeName 是运行实例的节点。本版本只支持显式指定，候选节点与冷切换在后续版本加入。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName"`
	// +kubebuilder:default=Running
	// +optional
	RunState RunState `json:"runState,omitempty"`
}

type LibvirtDomainStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// NodeName 是实际定义了该实例的节点。
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// State 与 Reason 是 libvirt 报告的状态与原因，例如 running/booted、shutoff/shutdown。
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message 是最近一次下发或启动失败的原因；成功时为空。
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:selectablefield:JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
type LibvirtDomain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              LibvirtDomainSpec `json:"spec"`
	// +optional
	Status LibvirtDomainStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LibvirtDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LibvirtDomain `json:"items"`
}
