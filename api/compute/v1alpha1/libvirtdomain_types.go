package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RunState 只表示创建后与宿主机重启后是否应当拉起实例，不表示任何时候都必须在运行：
// 客户机内关机或运维人员关机都是有意的停止（ADR-0015）。
// +kubebuilder:validation:Enum=Running;Stopped
type RunState string

const (
	RunStateRunning RunState = "Running"
	RunStateStopped RunState = "Stopped"
)

// DomainDisk 把 Ayatori 的卷或镜像绑定到 domain 中 alias 相同的磁盘（ADR-0026）。Ayatori 按它填写该磁盘的
// source 与 driver.type（卷的格式）。
// +kubebuilder:validation:XValidation:rule="has(self.volume) != has(self.image)",message="exactly one of volume and image must be set"
type DomainDisk struct {
	// Alias 是 domain 中磁盘的 alias.name，libvirt 要求以 ua- 开头。
	// +kubebuilder:validation:Pattern=`^ua-[A-Za-z0-9_-]+$`
	// +kubebuilder:validation:MaxLength=64
	Alias string `json:"alias"`
	// Volume 是本节点上 LibvirtStorageVolume 的名字。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Volume string `json:"volume,omitempty"`
	// Image 是 ISO 类型的 Image 的名字，只能绑定到只读光驱。副本更新后，下次启动读到新版本（ADR-0019 §9）。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Image string `json:"image,omitempty"`
}

// DomainInterface 把 LibvirtNetwork 绑定到 domain 中 alias 相同的网卡（ADR-0026）。Ayatori 按它填写该网卡的
// source 与 target（端口名）；mac 不写时由对象 UID 稳定生成。
type DomainInterface struct {
	// Alias 是 domain 中网卡的 alias.name，libvirt 要求以 ua- 开头。
	// +kubebuilder:validation:Pattern=`^ua-[A-Za-z0-9_-]+$`
	// +kubebuilder:validation:MaxLength=64
	Alias string `json:"alias"`
	// Network 是 LibvirtNetwork 的名字。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Network string `json:"network"`
}

// LibvirtDomainSpec：domain 是 libvirtxml.Domain 的机械映射，就是交给 libvirt 的 domain；disks 与 interfaces
// 按 alias 把 Ayatori 对象绑定到其中的设备（ADR-0026）。Ayatori 拥有的字段写了即拒绝。
// +kubebuilder:validation:XValidation:rule="!has(self.domain.name) && !has(self.domain.uuid) && !has(self.domain.metadata)",message="domain.name, domain.uuid and domain.metadata are set by Ayatori"
// +kubebuilder:validation:XValidation:rule="!has(self.domain.devices) || !has(self.domain.devices.disks) || self.domain.devices.disks.all(d, !has(d.source) && !has(d.backingStore) && !has(d.mirror) && (!has(d.driver) || !has(d.driver.type)))",message="disk source, backingStore, mirror and driver.type are set by Ayatori: bind a volume or image in spec.disks"
// +kubebuilder:validation:XValidation:rule="!has(self.domain.devices) || !has(self.domain.devices.interfaces) || self.domain.devices.interfaces.all(i, !has(i.source) && !has(i.target) && has(i.alias) && has(self.interfaces) && self.interfaces.exists(b, b.alias == i.alias.name))",message="every interface must be bound to a network in spec.interfaces by its alias; source and target are set by Ayatori"
// +kubebuilder:validation:XValidation:rule="!has(self.domain.os) || (!has(self.domain.os.nvRam) && (!has(self.domain.os.loader) || !has(self.domain.os.loader.path)))",message="firmware state (os.nvRam, os.loader.path) is placed by Ayatori"
// +kubebuilder:validation:XValidation:rule="!has(self.domain.devices) || !has(self.domain.devices.tpms) || self.domain.devices.tpms.all(t, !has(t.backend) || !has(t.backend.emulator) || !has(t.backend.emulator.source))",message="vTPM state (backend.emulator.source) is placed by Ayatori"
// +kubebuilder:validation:XValidation:rule="!has(self.domain.devices) || !has(self.domain.devices.graphics) || self.domain.devices.graphics.all(g, (!has(g.vnc) || !(has(g.vnc.socket) || has(g.vnc.port) || has(g.vnc.autoPort) || has(g.vnc.webSocket) || has(g.vnc.listen) || has(g.vnc.listeners))) && (!has(g.spice) || !(has(g.spice.port) || has(g.spice.tlsPort) || has(g.spice.autoPort) || has(g.spice.listen) || has(g.spice.listeners))))",message="VNC and SPICE listen on a local unix socket set by Ayatori"
// +kubebuilder:validation:XValidation:rule="!has(self.disks) || self.disks.all(b, has(self.domain.devices) && has(self.domain.devices.disks) && self.domain.devices.disks.exists(d, has(d.alias) && d.alias.name == b.alias))",message="every spec.disks alias must name a disk in domain.devices.disks"
// +kubebuilder:validation:XValidation:rule="!has(self.interfaces) || self.interfaces.all(b, has(self.domain.devices) && has(self.domain.devices.interfaces) && self.domain.devices.interfaces.exists(i, has(i.alias) && i.alias.name == b.alias))",message="every spec.interfaces alias must name an interface in domain.devices.interfaces"
type LibvirtDomainSpec struct {
	// Domain 是 libvirtxml.Domain 按 ADR-0026 的规则映射成的对象，schema 由 internal/compute/domainspec 生成。
	// 默认 UEFI、ACPI，并带一个 alias 为 ua-ayatori-console 的串口作为控制台。
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Domain runtime.RawExtension `json:"domain"`
	// Disks 按 alias 绑定卷或镜像。
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=alias
	// +optional
	Disks []DomainDisk `json:"disks,omitempty"`
	// Interfaces 按 alias 绑定网络；顺序决定端口名，增删中间的项会改变后面网卡的端口名。
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=alias
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
