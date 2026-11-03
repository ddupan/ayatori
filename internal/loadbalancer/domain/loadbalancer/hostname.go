package loadbalancer

import (
	"net/netip"
	"slices"
)

// RecordAction 是聚合要求 Reconciler 对 Hostname 记录执行的下一步。
type RecordAction uint8

const (
	RecordWait RecordAction = iota
	PublishRecord
	RemoveRecord
)

// Hostname 是 Ayatori 独占 zone 中的名称 <name>.<namespace>.<zone>；名称合法性由 CRD 保证。
func (lb *LoadBalancer) Hostname(zone string) string {
	return lb.snapshot.Owner.Name + "." + lb.snapshot.Owner.Namespace + "." + zone
}

// PlanRecord 根据 zone 中观察到的 A 记录决定下一步。VIP 确认后即发布，不等待数据面；
// 删除时须在主机清理后、释放 VIP 前删除记录，避免名称指向可能分给他人的地址。
func (lb *LoadBalancer) PlanRecord(record []netip.Addr, observeErr error, deleting bool) RecordAction {
	lb.recordObserved = true
	switch {
	case observeErr != nil:
		lb.recordMessage = "无法观察 DNS 记录：" + observeErr.Error()
		return RecordWait
	case deleting && len(record) > 0:
		return RemoveRecord
	case deleting:
		lb.recordReleased = true
		return RecordWait
	case !lb.snapshot.Address.IsValid():
		lb.recordMessage = "等待 VIP"
		return RecordWait
	case slices.Equal(record, []netip.Addr{lb.snapshot.Address}):
		lb.recordPublished = true
		return RecordWait
	default:
		lb.recordMessage = "发布 DNS 记录"
		return PublishRecord
	}
}

// RecordPublished 记录本轮写入的结果；成功时视为已发布，下一轮回读复核。
func (lb *LoadBalancer) RecordPublished(err error) {
	if err != nil {
		lb.recordMessage = "DNS 记录写入失败：" + err.Error()
		return
	}
	lb.recordPublished, lb.recordMessage = true, ""
}

// Published 表示本轮确认 Hostname 记录指向 VIP；observed 为 false 表示未启用 DNS。
func (lb *LoadBalancer) Published() (observed, published bool, message string) {
	return lb.recordObserved, lb.recordPublished, lb.recordMessage
}

// RecordReleased 表示删除时已确认记录不存在；未启用 DNS 时恒为 true。
func (lb *LoadBalancer) RecordReleased() bool { return !lb.recordObserved || lb.recordReleased }
