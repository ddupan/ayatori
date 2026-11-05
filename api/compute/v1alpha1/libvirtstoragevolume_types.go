package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ReclaimPolicy 决定删除对象时如何处理 libvirt 中的卷。
// +kubebuilder:validation:Enum=Retain;Delete
type ReclaimPolicy string

const (
	// ReclaimRetain 保留卷及其数据，只是不再由 Ayatori 管理；同名对象再次创建时接管它。
	ReclaimRetain ReclaimPolicy = "Retain"
	// ReclaimDelete 删除卷。本节点上仍有实例引用它时，等引用消失后再删除。
	ReclaimDelete ReclaimPolicy = "Delete"
)

// LibvirtStorageVolumeSpec 在一个本地 pool 中创建卷（ADR-0015）。创建后不可修改：卷的内容是数据，
// Ayatori 不会为了让配置一致而重建它。扩容（ADR-0019 §7）与从 URL 导入（§9）在后续版本加入。
// +kubebuilder:validation:XValidation:rule="self.pool == oldSelf.pool && self.nodeName == oldSelf.nodeName && self.xml == oldSelf.xml",message="pool, nodeName and xml are immutable"
type LibvirtStorageVolumeSpec struct {
	// Pool 是 LibvirtStoragePool 的名字。
	// +kubebuilder:validation:MinLength=1
	Pool string `json:"pool"`
	// NodeName 是卷所在的节点。本版本只支持本地 pool，必须指定节点。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName"`
	// XML 是原生 volume XML，例如 <volume><capacity unit='G'>20</capacity><target><format type='qcow2'/></target></volume>。
	// name 由 Ayatori 填写为对象名，domain 因此可以用 <source pool='…' volume='对象名'/> 引用。
	// 卷已存在时直接接管，不按 XML 修改或覆盖。
	// +kubebuilder:validation:MinLength=1
	XML string `json:"xml"`
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

type LibvirtStorageVolumeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Path 是卷在节点上的路径；为空表示尚未创建。
	// +optional
	Path string `json:"path,omitempty"`
	// Capacity 是卷的容量（字节）。
	// +optional
	Capacity int64 `json:"capacity,omitempty"`
	// Message 是最近一次创建失败的原因；成功时为空。
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:selectablefield:JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.pool`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.path`
type LibvirtStorageVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              LibvirtStorageVolumeSpec `json:"spec"`
	// +optional
	Status LibvirtStorageVolumeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LibvirtStorageVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LibvirtStorageVolume `json:"items"`
}
