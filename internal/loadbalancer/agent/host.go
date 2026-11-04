package agent

import (
	"context"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1alpha1 "git.ddupan.top/panxiao81/ayatori/api/network/v1alpha1"
	"git.ddupan.top/panxiao81/ayatori/internal/infra/node"
)

const (
	fieldOwner     = client.FieldOwner(Role + "-agent")
	agentReadyType = "AgentReady"
	// staleRounds 是本机宣告循环多少轮没有成功收敛即报告未就绪：Lease 由另一个 goroutine 续约，
	// 宣告循环卡住时 controller 从 Lease 看不出来。
	staleRounds = 5
)

// hostReport 是写入 LoadBalancerHost status 的 agent 报告；与上次写入相同且未到 node.StatusInterval
// 时不写。
type hostReport struct {
	ready           bool
	reason, message string
	vips            []networkv1alpha1.AgentVIPStatus
}

// reportHost 以 SSA 注册与 Node 同名的 LoadBalancerHost（不带 spec，ADR-0022），并写入 AgentReady 与
// vips。agent 只拥有这些字段：spec 与 finalizer 归管理员与 controller，准入策略也禁止 agent 改动。
func (a *Agent) reportHost(ctx context.Context, kube client.Client, logf func(string, error)) {
	ticker := time.NewTicker(node.RenewInterval)
	defer ticker.Stop()
	var last *hostReport
	var lastWrite time.Time
	var transition metav1.Time
	for {
		report := a.hostReport()
		if last == nil || last.ready != report.ready {
			transition = metav1.Now()
		}
		if last == nil || !reflect.DeepEqual(*last, report) || time.Since(lastWrite) > node.StatusInterval {
			if err := a.applyHost(ctx, kube, report, transition); err != nil {
				logf("apply LoadBalancerHost", err)
			} else {
				last, lastWrite = &report, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) hostReport() hostReport {
	status := a.Status()
	ready, message := status.Ready(time.Now(), staleRounds*a.config.Interval.Duration)
	report := hostReport{ready: ready, reason: "Ready", message: message}
	if !ready {
		report.reason = "NotReady"
	}
	for _, vip := range status.VIPs {
		report.vips = append(report.vips, networkv1alpha1.AgentVIPStatus{
			Address: vip.Address.String(), Announced: vip.Announced, Reason: string(vip.Reason),
		})
	}
	return report
}

func (a *Agent) applyHost(ctx context.Context, kube client.Client, report hostReport, transition metav1.Time) error {
	if err := kube.Apply(ctx, client.ApplyConfigurationFromUnstructured(a.hostObject()), fieldOwner, client.ForceOwnership); err != nil {
		return err
	}
	status := metav1.ConditionFalse
	if report.ready {
		status = metav1.ConditionTrue
	}
	vips := make([]any, 0, len(report.vips))
	for _, vip := range report.vips {
		item := map[string]any{"address": vip.Address, "announced": vip.Announced}
		if vip.Reason != "" {
			item["reason"] = vip.Reason
		}
		vips = append(vips, item)
	}
	// Apply 会把服务端返回的对象（含 managedFields）写回参数，status 用新的对象。
	object := a.hostObject()
	object.Object["status"] = map[string]any{
		"vips": vips,
		"conditions": []any{map[string]any{
			"type": agentReadyType, "status": string(status), "reason": report.reason, "message": report.message,
			"lastTransitionTime": transition.UTC().Format(time.RFC3339),
		}},
	}
	// controller 在 Lease 过期时覆盖 AgentReady 与 vips；agent 恢复后以 force 收回。
	return kube.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(object), fieldOwner, client.ForceOwnership)
}

// hostObject 是只含身份的 LoadBalancerHost：agent 不拥有 spec、label、finalizer。
func (a *Agent) hostObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(networkv1alpha1.GroupVersion.WithKind("LoadBalancerHost"))
	object.SetName(a.config.NodeName)
	return object
}
