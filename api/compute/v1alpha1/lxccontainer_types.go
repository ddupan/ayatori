package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ContainerNetwork 是容器的一块网卡，由 Ayatori 渲染为 lxc.net.N（ADR-0015）。
type ContainerNetwork struct {
	// Network 是 LibvirtNetwork 的名字，必须是有 bridge 的网络：容器的 veth 接在这个 bridge 上。
	// +kubebuilder:validation:MinLength=1
	Network string `json:"network"`
	// MAC 地址。不写时由 Ayatori 从对象 UID 稳定生成。
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`
	// +optional
	MAC string `json:"mac,omitempty"`
}

// LXCContainerSpec 按归属拆开：引用 Ayatori 对象的部分是类型化字段，其余是原生 lxc 配置（ADR-0015）。
type LXCContainerSpec struct {
	// Config 是原生 lxc 配置文本，不含 Ayatori 拥有的 lxc.uts.name、lxc.rootfs.path 与 lxc.net.*：
	// 出现时 Agent 拒绝下发并在 status 中说明。其余内容原样透传，例如
	// lxc.include = /usr/share/lxc/config/common.conf。
	// +kubebuilder:validation:MinLength=1
	Config string `json:"config"`
	// Rootfs 是本节点上 LibvirtStorageVolume 的名字，内容是 ext4 文件系统，通常由 Rootfs 类型的
	// Image 创建（ADR-0019 §9）。
	// +kubebuilder:validation:MinLength=1
	Rootfs string `json:"rootfs"`
	// Networks 按顺序渲染为 lxc.net.N。
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Networks []ContainerNetwork `json:"networks,omitempty"`
	// NodeName 是运行容器的节点。本版本只支持显式指定。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName"`
	// RunState 为 Running 时，容器停止后由 Agent 拉起。容器拿不到停止原因，容器内 poweroff 与崩溃
	// 无法区分，都会被拉起（PoC 08）。
	// +kubebuilder:default=Running
	// +optional
	RunState RunState `json:"runState,omitempty"`
}

type LXCContainerStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// NodeName 是实际定义了该容器的节点。
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// State 是 liblxc 报告的状态，例如 running、stopped。
	// +optional
	State string `json:"state,omitempty"`
	// Message 是最近一次下发或启动失败的原因；成功时为空。
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name is the container hostname: at most 63 characters"
// +kubebuilder:selectablefield:JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
type LXCContainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              LXCContainerSpec `json:"spec"`
	// +optional
	Status LXCContainerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type LXCContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LXCContainer `json:"items"`
}
