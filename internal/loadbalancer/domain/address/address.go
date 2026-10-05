// Package address 定义 VIP 分配的纯规则。占用的唯一性由 API server 的对象名唯一性保证，
// 这里不实现锁或位图，只决定候选地址与无主记录。
package address

import (
	"net/netip"
	"time"
)

// Grace 是 Address 创建后、LoadBalancer status 记录前允许存在的窗口；超过才可判为无主。
const Grace = time.Minute

// Owner 是 Address 记录的 LoadBalancer 身份。同名重建的 LoadBalancer UID 不同，不是同一所有者。
type Owner struct {
	Namespace string
	Name      string
	UID       string
}

// Pool 是可分配 VIP 的单一 IPv4 前缀。前缀短于 /31 时不分配网络与广播地址。
type Pool struct {
	Prefix netip.Prefix
}

func (p Pool) Contains(addr netip.Addr) bool {
	if !p.Prefix.Contains(addr) {
		return false
	}
	if p.Prefix.Bits() >= 31 {
		return true
	}
	first := p.Prefix.Masked().Addr()
	return addr != first && addr != last(p.Prefix)
}

// Next 按地址顺序返回第一个未被占用的候选。
// ponytail: 线性扫描，/24 级别的池足够；池扩大到上万地址再改用有序集合。
func (p Pool) Next(taken map[netip.Addr]Owner) (netip.Addr, bool) {
	for addr := p.Prefix.Masked().Addr(); p.Prefix.Contains(addr); addr = addr.Next() {
		if _, used := taken[addr]; !used && p.Contains(addr) {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

func last(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Masked().Addr().As4()
	host := 32 - prefix.Bits()
	for i := 3; i >= 0 && host > 0; i-- {
		bits := min(host, 8)
		bytes[i] |= byte(1<<bits - 1)
		host -= bits
	}
	return netip.AddrFrom4(bytes)
}

// Orphaned 判断 Address 是否已无所有者。parentUID 为空表示 LoadBalancer 不存在。
// 返回 false 时 wait 为距离可判定的剩余时间，零表示所有者仍有效、无需复查。
func Orphaned(owner Owner, parentUID string, age time.Duration) (orphan bool, wait time.Duration) {
	if parentUID == owner.UID {
		return false, 0
	}
	if age < Grace {
		return false, Grace - age
	}
	return true, 0
}
