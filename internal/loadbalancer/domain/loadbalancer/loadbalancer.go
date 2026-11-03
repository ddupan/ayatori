// Package loadbalancer 定义 LoadBalancer 聚合：VIP 认领与释放、逐主机收敛与 Programmed 判定。
// 聚合只做判断与状态迁移，不访问 Kubernetes 或数据面；外部操作由 Reconciler 执行。
package loadbalancer

import (
	"net/netip"
	"slices"
	"strings"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/domain/address"
)

// Snapshot 是 LoadBalancer 自身的持久状态，不含 resourceVersion 或 Conditions。
type Snapshot struct {
	Owner address.Owner
	// Requested 是 spec 请求的 VIP；零值表示自动分配。
	Requested netip.Addr
	// Address 是 status 中已确认的 VIP；一经记录不再改变。
	Address netip.Addr
	Ports   []Port
}

// Action 是聚合要求 Reconciler 执行的下一步。
type Action uint8

const (
	// Wait 表示没有外部写入；保存聚合记录的状态即可。
	Wait Action = iota
	// Save 表示只有状态变化（如采用已认领的地址）。
	Save
	// Claim 表示以候选地址为名创建 Address；AlreadyExists 时等待下一轮重新观察。
	Claim
	// Release 表示删除属于本 LoadBalancer 的 Address。
	Release
	// Finalize 表示外部占用均已释放，可移除 finalizer。
	Finalize
)

// Programmed condition 的 Reason。
const (
	ReasonAddressPending   = "AddressPending"
	ReasonPoolExhausted    = "PoolExhausted"
	ReasonAddressOutOfPool = "AddressOutOfPool"
	ReasonAddressConflict  = "AddressConflict"
	ReasonDataPlanePending = "DataPlanePending"
	ReasonProgrammed       = "Programmed"
	ReasonHostsDegraded    = "HostsDegraded"
	ReasonAllHostsReady    = "AllHostsReady"
)

// ResolvedRefs condition 的 Reason，命名沿用 Gateway API。
const (
	ReasonResolvedRefs    = "ResolvedRefs"
	ReasonBackendNotFound = "BackendNotFound"
)

type LoadBalancer struct {
	snapshot    Snapshot
	reason      string
	message     string
	attachments []Attachment
	// health 是本轮各可观察 Host 上 server 的健康检查结论（ADR-0023）。
	health map[string][]ServerHealth

	recordObserved  bool
	recordPublished bool
	recordReleased  bool
	recordMessage   string
}

func Reconstitute(snapshot Snapshot) *LoadBalancer {
	return &LoadBalancer{snapshot: snapshot, reason: ReasonAddressPending, message: "等待认领 VIP"}
}

func (lb *LoadBalancer) Snapshot() Snapshot { return lb.snapshot }

// Programmed 表示 VIP 已确认且至少一台 Host 满足宣告规则。须在本轮 PlanHost 之后调用。
func (lb *LoadBalancer) Programmed() (programmed bool, reason, message string) {
	if lb.reason != ReasonDataPlanePending {
		return false, lb.reason, lb.message
	}
	for _, attachment := range lb.attachments {
		if attachment.Phase == AttachmentReady {
			return true, ReasonProgrammed, "至少一台主机满足宣告条件"
		}
	}
	return false, ReasonDataPlanePending, "VIP 已分配，尚无主机满足宣告条件"
}

// Degraded 表示存在未就绪（配置未生效或不满足宣告规则）的 Host，返回这些 Host 的名称。
func (lb *LoadBalancer) Degraded() []string {
	var hosts []string
	for _, attachment := range lb.attachments {
		if attachment.Phase != AttachmentReady {
			hosts = append(hosts, attachment.Host)
		}
	}
	return hosts
}

// ResolvedRefs 表示各端口引用的 Backend 都存在；缺失时返回说明，对应端口拒绝连接。
func (lb *LoadBalancer) ResolvedRefs() (resolved bool, reason, message string) {
	var missing []string
	for _, port := range lb.snapshot.Ports {
		if port.MissingBackend && !slices.Contains(missing, port.Backend) {
			missing = append(missing, port.Backend)
		}
	}
	if len(missing) > 0 {
		return false, ReasonBackendNotFound, "引用的 Backend 不存在：" + strings.Join(missing, ", ")
	}
	return true, ReasonResolvedRefs, ""
}

// PlanAddress 根据当前占用决定 VIP 的下一步。taken 是已观察到的全部受管 Address。
// 已记录的地址只会被重新认领，不会更换；被他人占用时报告冲突而不是改用新地址。
func (lb *LoadBalancer) PlanAddress(pool address.Pool, taken map[netip.Addr]address.Owner) (netip.Addr, Action) {
	if lb.snapshot.Address.IsValid() {
		return lb.claim(lb.snapshot.Address, taken)
	}
	if lb.snapshot.Requested.IsValid() {
		if !pool.Contains(lb.snapshot.Requested) {
			lb.report(ReasonAddressOutOfPool, "请求的 VIP 不在地址池内")
			return netip.Addr{}, Wait
		}
		return lb.claim(lb.snapshot.Requested, taken)
	}
	// 上一轮创建成功但 status 未写入时，先采用自己已占用的地址。
	for addr, owner := range taken {
		if owner == lb.snapshot.Owner && pool.Contains(addr) {
			return lb.claim(addr, taken)
		}
	}
	addr, ok := pool.Next(taken)
	if !ok {
		lb.report(ReasonPoolExhausted, "地址池已无可分配的 VIP")
		return netip.Addr{}, Wait
	}
	return addr, Claim
}

func (lb *LoadBalancer) claim(addr netip.Addr, taken map[netip.Addr]address.Owner) (netip.Addr, Action) {
	owner, used := taken[addr]
	switch {
	case !used:
		return addr, Claim
	case owner != lb.snapshot.Owner:
		lb.report(ReasonAddressConflict, "VIP "+addr.String()+" 已被其他 LoadBalancer 占用")
		return netip.Addr{}, Wait
	case lb.snapshot.Address == addr:
		lb.report(ReasonDataPlanePending, "")
		return addr, Wait
	default:
		lb.AddressClaimed(addr)
		return addr, Save
	}
}

// AddressClaimed 记录已确认占用的 VIP。
func (lb *LoadBalancer) AddressClaimed(addr netip.Addr) {
	lb.snapshot.Address = addr
	lb.report(ReasonDataPlanePending, "")
}

// PlanRelease 返回删除时需要释放的 Address：包括 status 未记录、但由本对象创建的占用。
func (lb *LoadBalancer) PlanRelease(taken map[netip.Addr]address.Owner) ([]netip.Addr, Action) {
	var owned []netip.Addr
	for addr, owner := range taken {
		if owner == lb.snapshot.Owner {
			owned = append(owned, addr)
		}
	}
	if len(owned) == 0 {
		return nil, Finalize
	}
	return owned, Release
}

func (lb *LoadBalancer) report(reason, message string) { lb.reason, lb.message = reason, message }
