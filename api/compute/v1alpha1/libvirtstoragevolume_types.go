package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReclaimPolicy 决定删除对象时如何处理 libvirt 中的卷。
// +kubebuilder:validation:Enum=Retain;Delete
type ReclaimPolicy string

const (
	// ReclaimRetain 保留卷及其数据，只是不再由 Ayatori 管理；同名对象再次创建时接管它。
	ReclaimRetain ReclaimPolicy = "Retain"
	// ReclaimDelete 删除卷。本节点上仍有实例引用它时，等引用消失后再删除。
	ReclaimDelete ReclaimPolicy = "Delete"
)

// LibvirtStorageVolumeSpec 声明一个卷（ADR-0015）。卷由一种存储后端提供，各后端是 spec 下互斥的
// 一块：本版本只有 libvirt（经 libvirt pool 管理）；ADR-0020 的 LINSTOR 以后作为并列的 linstor 加入。
// 后端与 nodeName 创建后不可修改：卷的内容是数据，Ayatori 不会为了让配置一致而重建它。
// 改变容量只能增大 size（ADR-0019 §7）。
// +kubebuilder:validation:XValidation:rule="has(self.libvirt)",message="a storage backend must be set: libvirt"
// +kubebuilder:validation:XValidation:rule="has(self.libvirt) == has(oldSelf.libvirt)",message="storage backend is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.source) == has(oldSelf.source)",message="source is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.libvirt) || has(self.libvirt.xml) || has(self.size)",message="a libvirt volume needs xml or size"
// +kubebuilder:validation:XValidation:rule="has(self.size) || !has(oldSelf.size)",message="size cannot be removed"
type LibvirtStorageVolumeSpec struct {
	// NodeName 表示卷只存在于该节点上（本地存储），引用它的实例只能在该节点运行。共享存储（rbd、
	// LINSTOR）以后不填。本版本只支持本地存储，必须指定。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nodeName is immutable"
	NodeName string `json:"nodeName"`
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	// Libvirt 表示卷在 libvirt pool 中，由节点上的 Agent 经 libvirt 创建与删除。
	// +optional
	Libvirt *LibvirtVolumeSource `json:"libvirt,omitempty"`
	// Size 是卷的容量，优先于 XML 中的 capacity，只能增大。卷小于它时 Agent 扩容；实例运行中时在线
	// 扩容，客户机内的分区和文件系统由客户机自己扩展。
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).isGreaterThan(quantity('0'))",message="size must be positive"
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).compareTo(quantity(string(oldSelf))) >= 0",message="size can only grow"
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
	// Source 是卷创建时复制的内容；不写时是空卷。只在创建时生效，之后卷与来源无关。
	// +optional
	Source *VolumeContentSource `json:"source,omitempty"`
}

// VolumeContentSource 是卷的初始内容。
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="source is immutable"
type VolumeContentSource struct {
	// Image 是 Disk 类型的 Image 的名字（ADR-0019 §9）。
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
}

// LibvirtVolumeSource 是 libvirt pool 中的卷。
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="libvirt volume is immutable"
type LibvirtVolumeSource struct {
	// Pool 是 LibvirtStoragePool 的名字。
	// +kubebuilder:validation:MinLength=1
	Pool string `json:"pool"`
	// XML 是原生 volume XML，例如 <volume><capacity unit='G'>20</capacity><target><format type='qcow2'/></target></volume>。
	// name 由 Ayatori 填写为对象名，domain 因此可以用 <source pool='…' volume='对象名'/> 引用。
	// 卷已存在时直接接管，不按 XML 修改或覆盖。不写时按 size 创建默认格式的卷。
	// +kubebuilder:validation:MinLength=1
	// +optional
	XML string `json:"xml,omitempty"`
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
	// Image 与 SHA256 是卷实际复制的镜像及其版本。
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	SHA256 string `json:"sha256,omitempty"`
	// Message 是最近一次创建失败的原因；成功时为空。
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:selectablefield:JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.libvirt.pool`
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
