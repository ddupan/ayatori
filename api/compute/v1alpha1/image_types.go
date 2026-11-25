package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ImageType 表示镜像的内容，决定谁能使用它（ADR-0019 §9）。
// +kubebuilder:validation:Enum=Disk;ISO;Rootfs
type ImageType string

const (
	// ImageDisk 是 qcow2 或 raw 磁盘镜像，由卷的 source.image 完整复制。
	ImageDisk ImageType = "Disk"
	// ImageISO 是 ISO 9660 介质，由 domain 的 disks[].image 作为只读光驱挂载。
	ImageISO ImageType = "ISO"
	// ImageRootfs 是 tar 归档（可压缩），由卷的 source.image 解包为 LXC 的 ext4 rootfs。
	ImageRootfs ImageType = "Rootfs"
)

// ImageHTTPSource 从 HTTP(S) 下载镜像，应指向内网镜像 bucket：家宽上联不稳定。
type ImageHTTPSource struct {
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`
	// SHA256 是镜像内容的校验和，同时标识版本。
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
}

// ImageSource 是镜像的来源。修改即更新镜像。
// +kubebuilder:validation:XValidation:rule="has(self.http)",message="an image source must be set: http"
type ImageSource struct {
	// +optional
	HTTP *ImageHTTPSource `json:"http,omitempty"`
}

// ImageSpec 声明每个选中节点上都要有一份的只读内容（ADR-0019 §9）。副本是 pool 中与对象同名的文件。
type ImageSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type ImageType `json:"type"`
	// Pool 是各节点上的 LibvirtStoragePool，必须是 dir 类型：副本靠 rename 原子替换。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pool is immutable"
	Pool string `json:"pool"`
	// NodeSelector 按 Node 的 label 选择节点；为空时是所有计算节点。
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Source       ImageSource       `json:"source"`
}

// ImageNodeStatus 是一个节点上的副本。
type ImageNodeStatus struct {
	NodeName string `json:"nodeName"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// SHA256 是本节点现有副本的版本；为空表示还没有副本。
	// +optional
	SHA256 string `json:"sha256,omitempty"`
	// Ready 表示现有副本与 spec 的版本一致。
	Ready bool `json:"ready"`
	// Message 是下载进度或最近一次失败的原因。
	// +optional
	Message string `json:"message,omitempty"`
}

type ImageStatus struct {
	// +listType=map
	// +listMapKey=nodeName
	// +optional
	Nodes []ImageNodeStatus `json:"nodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.pool`
type Image struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              ImageSpec `json:"spec"`
	// +optional
	Status ImageStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type ImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Image `json:"items"`
}
