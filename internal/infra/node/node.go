// Package node 是各节点 agent（compute Agent、LB agent）共用的节点身份与注册逻辑（ADR-0021、
// ADR-0022）：一台主机一个身份、一个 Node，可以承担多个角色；每个角色一个 agent，各自持有证书、
// 以自己的 field manager 只写 Node 中属于自己角色的部分，并续约按角色命名的 Lease。
//
// 命名都能从角色名推出，deploy/flux/nodes 的准入策略据此约束 agent 只写自己的角色：
//
//	证书用户名  system:ayatori:node:<node>
//	证书组      system:ayatori:nodes、system:ayatori:role:<role>
//	Node label  node-role.kubernetes.io/<role>
//	condition   <role>.ayatori.ddupan.top/Ready
//	Lease       kube-node-lease/<node>-<role>
package node

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1ac "k8s.io/client-go/applyconfigurations/coordination/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
)

const (
	// UserPrefix 与 NodesGroup：不用 kubelet 的 system:node:/system:nodes，那是 Node authorizer 专用的。
	UserPrefix = "system:ayatori:node:"
	NodesGroup = "system:ayatori:nodes"
	// RoleGroupPrefix 加角色名是角色组：RBAC 只把角色相关的权限授予它。
	RoleGroupPrefix = "system:ayatori:role:"
	// RoleLabelPrefix 加角色名是 Node 上的角色 label；控制面按它正向选择节点（没有 taint 与调度器）。
	RoleLabelPrefix = "node-role.kubernetes.io/"

	LeaseDuration = 40 * time.Second
	RenewInterval = 10 * time.Second
	// StatusInterval 与 kubelet 的 nodeStatusReportFrequency 相同：没有变化时只靠 Lease 表明存活。
	StatusInterval = 5 * time.Minute
)

// Identity 返回节点 node 上 role 角色 agent 的证书身份。
func Identity(dir, node, role string) clientcert.Identity {
	return clientcert.Identity{
		Dir:          dir,
		CommonName:   UserPrefix + node,
		Organization: []string{NodesGroup, RoleGroupPrefix + role},
		UID:          -1,
	}
}

// ConditionType 是 role 角色在 Node 上的就绪 condition。
func ConditionType(role string) corev1.NodeConditionType {
	return corev1.NodeConditionType(role + ".ayatori.ddupan.top/Ready")
}

// Registrar 注册并维护 Node 中属于一个角色的部分。
type Registrar struct {
	Client client.Client
	Node   string
	Role   string

	ready      *bool
	transition metav1.Time
}

// fieldOwner 按角色区分：同一台主机上各 agent 共同持有 Node，各自只拥有自己 apply 的字段。
func (r *Registrar) fieldOwner() client.FieldOwner {
	return client.FieldOwner(r.Role + "-agent")
}

// LeaseName 是该角色的心跳 Lease。按角色分开：同机另一个 agent 不能替挂掉的 agent 续约。
func (r *Registrar) LeaseName() string { return r.Node + "-" + r.Role }

// RenewLease 续约本角色的 Lease。
func (r *Registrar) RenewLease(ctx context.Context) error {
	lease := coordinationv1ac.Lease(r.LeaseName(), corev1.NamespaceNodeLease).
		WithSpec(coordinationv1ac.LeaseSpec().
			WithHolderIdentity(r.Node).
			WithLeaseDurationSeconds(int32(LeaseDuration / time.Second)).
			WithRenewTime(metav1.NewMicroTime(time.Now())))
	return r.Client.Apply(ctx, lease, r.fieldOwner(), client.ForceOwnership)
}

// Status 是该角色写入 Node status 的部分。Capacity 与 Allocatable 只由拥有它们的角色填写（compute）。
type Status struct {
	Ready           bool
	Reason, Message string
	Capacity        corev1.ResourceList
	Allocatable     corev1.ResourceList
}

// Apply 写入角色 label 与本角色的 status。Node 不存在时由此创建；agent 不删除 Node。
func (r *Registrar) Apply(ctx context.Context, s Status) error {
	node := corev1ac.Node(r.Node).WithLabels(map[string]string{RoleLabelPrefix + r.Role: ""})
	if err := r.Client.Apply(ctx, node, r.fieldOwner(), client.ForceOwnership); err != nil {
		return err
	}
	now := metav1.Now()
	if r.ready == nil || *r.ready != s.Ready {
		r.ready, r.transition = &s.Ready, now
	}
	ready := corev1.ConditionFalse
	if s.Ready {
		ready = corev1.ConditionTrue
	}
	status := corev1ac.NodeStatus().WithConditions(corev1ac.NodeCondition().
		WithType(ConditionType(r.Role)).
		WithStatus(ready).
		WithReason(s.Reason).
		WithMessage(s.Message).
		WithLastHeartbeatTime(now).
		WithLastTransitionTime(r.transition))
	if s.Capacity != nil {
		status.WithCapacity(s.Capacity)
	}
	if s.Allocatable != nil {
		status.WithAllocatable(s.Allocatable)
	}
	return r.Client.Status().Apply(ctx, corev1ac.Node(r.Node).WithStatus(status), r.fieldOwner(), client.ForceOwnership)
}

// Run 每 RenewInterval 续约 Lease；status() 的结果有变化，或距上次写入超过 StatusInterval 时写 Node。
// status 由调用方从自己发布的事实中读取，不应访问后端。写入失败（控制面不可用、证书尚未签发）
// 留到下一轮。
func (r *Registrar) Run(ctx context.Context, status func() Status, logf func(msg string, err error)) {
	ticker := time.NewTicker(RenewInterval)
	defer ticker.Stop()
	var last *Status
	var lastWrite time.Time
	for {
		if err := r.RenewLease(ctx); err != nil {
			logf("renew lease", err)
		}
		s := status()
		if last == nil || !equal(*last, s) || time.Since(lastWrite) > StatusInterval {
			if err := r.Apply(ctx, s); err != nil {
				logf("apply node", err)
			} else {
				last, lastWrite = &s, time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func equal(a, b Status) bool {
	return a.Ready == b.Ready && a.Reason == b.Reason && a.Message == b.Message &&
		equality.Semantic.DeepEqual(a.Capacity, b.Capacity) && equality.Semantic.DeepEqual(a.Allocatable, b.Allocatable)
}
