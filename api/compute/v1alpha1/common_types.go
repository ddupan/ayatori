package v1alpha1

// NodeObjectStatus 是一个节点对下发对象的观察。每个节点的 Agent 以自己的 field manager
// 经 server-side apply 只写自己那一项，节点之间不会互相覆盖。
type NodeObjectStatus struct {
	NodeName string `json:"nodeName"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Active 表示对象已在该节点定义并处于活动状态。
	Active bool `json:"active"`
	// Message 是最近一次下发失败的原因；成功时为空。
	// +optional
	Message string `json:"message,omitempty"`
}
