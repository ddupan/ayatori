package agent

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1ac "k8s.io/client-go/applyconfigurations/coordination/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ManagedByLabel 是 Agent 写入 Node 的 ownership label（ADR-0007）。
	ManagedByLabel = "compute.ayatori.ddupan.top/managed-by"
	leaseDuration  = 40 * time.Second
	renewInterval  = 10 * time.Second
	// nodeStatusInterval 与 kubelet 的 nodeStatusReportFrequency 相同：没有变化时只靠 Lease 表明存活。
	nodeStatusInterval = 5 * time.Minute
)

// heartbeat 续约 Lease，并在容量或 libvirt 连通性变化时（至少每 5 分钟）更新 Node 的 status。
// Node 的 label 由管理员维护，Agent 只拥有自己写入的字段。
func (a *Agent) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()
	var last nodeFacts
	var lastWrite time.Time
	for {
		if err := a.renewLease(ctx); err != nil {
			a.logger.Debug("renew lease", "error", err)
		}
		facts := *a.facts.Load()
		if facts != last || time.Since(lastWrite) > nodeStatusInterval {
			if err := a.applyNode(ctx, facts); err != nil {
				a.logger.Debug("apply node", "error", err)
			} else {
				last, lastWrite = facts, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) renewLease(ctx context.Context) error {
	lease := coordinationv1ac.Lease(a.config.NodeName, corev1.NamespaceNodeLease).
		WithSpec(coordinationv1ac.LeaseSpec().
			WithHolderIdentity(a.config.NodeName).
			WithLeaseDurationSeconds(int32(leaseDuration / time.Second)).
			WithRenewTime(metav1.NewMicroTime(time.Now())))
	return a.kube.Apply(ctx, lease, a.fieldOwner(), client.ForceOwnership)
}

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

// applyNode 写入 Node：capacity 是整机，allocatable 扣除宿主机预留（ADR-0015 资源模型）。
func (a *Agent) applyNode(ctx context.Context, f nodeFacts) error {
	node := corev1ac.Node(a.config.NodeName).WithLabels(map[string]string{ManagedByLabel: "compute-agent"})
	if err := a.kube.Apply(ctx, node, a.fieldOwner(), client.ForceOwnership); err != nil {
		return err
	}
	ready, reason := corev1.ConditionFalse, "LibvirtUnavailable"
	status := corev1ac.NodeStatus()
	if f.ready {
		ready, reason = corev1.ConditionTrue, "AgentReady"
		memory := resource.NewQuantity(f.memory, resource.BinarySI)
		free := memory.DeepCopy()
		free.Sub(a.config.HostReservedMemory)
		cpus := *resource.NewQuantity(f.cpus, resource.DecimalSI)
		status.WithCapacity(corev1.ResourceList{corev1.ResourceCPU: cpus, corev1.ResourceMemory: *memory}).
			WithAllocatable(corev1.ResourceList{corev1.ResourceCPU: cpus, corev1.ResourceMemory: free})
	}
	status.WithConditions(corev1ac.NodeCondition().
		WithType(corev1.NodeReady).
		WithStatus(ready).
		WithReason(reason).
		WithLastHeartbeatTime(metav1.Now()))
	return a.kube.Status().Apply(ctx, corev1ac.Node(a.config.NodeName).WithStatus(status), a.fieldOwner(), client.ForceOwnership)
}

func (a *Agent) fieldOwner() client.FieldOwner {
	return client.FieldOwner("ayatori-compute-agent/" + a.config.NodeName)
}
