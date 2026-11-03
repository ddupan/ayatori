package agent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.ddupan.top/ayatori/internal/infra/node"
)

// Role 是计算节点的角色（ADR-0022 的节点角色模型）：Node label node-role.kubernetes.io/compute，
// 控制面按它选择计算节点。
const Role = "compute"

type nodeFacts struct {
	ready        bool
	cpus, memory int64
}

// nodeFacts 由主循环在每轮收敛后调用并发布，heartbeat 只读发布的结果，不碰 libvirt 连接。
func (a *Agent) nodeFacts() nodeFacts {
	if !a.connected || a.hv == nil {
		return nodeFacts{}
	}
	cpus, memory, err := a.hv.NodeInfo()
	if err != nil {
		return nodeFacts{}
	}
	return nodeFacts{ready: true, cpus: cpus, memory: memory}
}

// heartbeat 续约 <node>-compute Lease，并写入 Node 中属于 compute 的部分：角色 label、
// compute.ayatori.ddupan.top/Ready，以及容量。capacity 是整机，allocatable 扣除宿主机预留（ADR-0015 资源模型）。
func (a *Agent) heartbeat(ctx context.Context, kube client.Client) {
	r := &node.Registrar{Client: kube, Node: a.config.NodeName, Role: Role}
	r.Run(ctx, func() node.Status {
		f := *a.facts.Load()
		if !f.ready {
			return node.Status{Reason: "LibvirtUnavailable"}
		}
		memory := resource.NewQuantity(f.memory, resource.BinarySI)
		free := memory.DeepCopy()
		free.Sub(a.config.HostReservedMemory)
		cpus := *resource.NewQuantity(f.cpus, resource.DecimalSI)
		return node.Status{
			Ready:       true,
			Reason:      "AgentReady",
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: cpus, corev1.ResourceMemory: *memory},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: cpus, corev1.ResourceMemory: free},
		}
	}, func(msg string, err error) { a.logger.Debug(msg, "error", err) })
}
