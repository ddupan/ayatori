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

// DiskBus 是磁盘接在客户机的哪种总线上。fdc 渲染为软驱。
// +kubebuilder:validation:Enum=virtio;scsi;sata;ide;fdc
type DiskBus string

const (
	DiskBusVirtio DiskBus = "virtio"
	DiskBusSCSI   DiskBus = "scsi"
	DiskBusSATA   DiskBus = "sata"
	DiskBusIDE    DiskBus = "ide"
	DiskBusFDC    DiskBus = "fdc"
)

// DomainDisk 是实例的一块磁盘，由 Ayatori 渲染为 <disk>（ADR-0015）。
type DomainDisk struct {
	// Volume 是本节点上 LibvirtStorageVolume 的名字。
	// +kubebuilder:validation:MinLength=1
	Volume string `json:"volume"`
	// +kubebuilder:default=virtio
	// +optional
	Bus DiskBus `json:"bus,omitempty"`
	// BootOrder 是启动顺序，从 1 开始；不写表示不从这块盘启动。与 domainXML 中 <os><boot> 不能同时使用。
	// +kubebuilder:validation:Minimum=1
	// +optional
	BootOrder int32 `json:"bootOrder,omitempty"`
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// DomainInterface 是实例的一块网卡，由 Ayatori 渲染为 <interface>（ADR-0015）。
type DomainInterface struct {
	// Network 是 LibvirtNetwork 的名字。
	// +kubebuilder:validation:MinLength=1
	Network string `json:"network"`
	// MAC 地址。不写时由 Ayatori 从对象 UID 稳定生成；冷启动换节点后不变。
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`
	// +optional
	MAC string `json:"mac,omitempty"`
	// Model 是网卡型号，例如 virtio、e1000e、rtl8139。
	// +kubebuilder:default=virtio
	// +kubebuilder:validation:MinLength=1
	// +optional
	Model string `json:"model,omitempty"`
}

// LibvirtDomainSpec 按归属拆开：引用 Ayatori 对象的设备是类型化字段，其余是原生 domain XML（ADR-0015）。
type LibvirtDomainSpec struct {
	// DomainXML 是原生 domain XML，不含 Ayatori 拥有的元素：<name>、<uuid>、<disk>、<interface>。
	// 这些元素出现时 Agent 拒绝下发并在 status 中说明。其余内容原样透传。
	// +kubebuilder:validation:MinLength=1
	DomainXML string `json:"domainXML"`
	// Disks 按顺序渲染为 <disk>。
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Disks []DomainDisk `json:"disks,omitempty"`
	// Interfaces 按顺序渲染为 <interface>。
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Interfaces []DomainInterface `json:"interfaces,omitempty"`
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
