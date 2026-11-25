package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type LibvirtStoragePoolSpec struct {
	// XML 是原生 pool XML，原样下发；其中的 name 与 uuid 由 Ayatori 填写为对象名与对象 UID，
	// domain 因此可以用 <source pool='对象名' volume='…'/> 引用。
	// +kubebuilder:validation:MinLength=1
	XML string `json:"xml"`
	// NodeSelector 按 Node 的 label 选择下发到哪些计算节点；为空时下发到所有计算节点。
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type LibvirtStoragePoolStatus struct {
	// +listType=map
	// +listMapKey=nodeName
	// +optional
	Nodes []NodeObjectStatus `json:"nodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type LibvirtStoragePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              LibvirtStoragePoolSpec `json:"spec"`
	// +optional
	Status LibvirtStoragePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LibvirtStoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LibvirtStoragePool `json:"items"`
}
