package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"libvirt.org/go/libvirtxml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
)

// Volume 是卷引用在本节点上解析出的路径。
type Volume struct {
	Path  string
	Block bool
}

// domainName 是实例在 libvirt 中的名字。Kubernetes 的名字里不会出现 "_"，因此不会与别的对象冲突。
func domainName(namespace, name string) string { return namespace + "_" + name }

// portName 是实例第 i 块网卡在宿主机上的端口名。固定下来，按端口写的 nftables 规则（元数据服务）
// 在冷启动后仍然有效；Linux 接口名最长 15 字符（ADR-0016）。
// ponytail: 取 UID 前 8 个十六进制字符，同一节点上的实例碰撞概率可忽略；规模变大时改用节点内分配。
func portName(uid string, i int) string {
	return fmt.Sprintf("ay%sn%d", strings.ReplaceAll(uid, "-", "")[:8], i)
}

// prepareDomain 填写 Ayatori 负责的字段，并把 pool 卷引用解析成具体路径：AppArmor 不会把
// type='volume' 的源加入实例的 profile（PoC 02）。
func prepareDomain(d *computev1alpha1.LibvirtDomain, resolve func(pool, volume string) (Volume, error)) (string, error) {
	var dom libvirtxml.Domain
	if err := dom.Unmarshal(d.Spec.DomainXML); err != nil {
		return "", fmt.Errorf("parse domainXML: %w", err)
	}
	dom.Name = domainName(d.Namespace, d.Name)
	dom.UUID = string(d.UID)
	if dom.Devices != nil {
		for i := range dom.Devices.Disks {
			disk := &dom.Devices.Disks[i]
			if disk.Source == nil || disk.Source.Volume == nil {
				continue
			}
			ref := disk.Source.Volume
			vol, err := resolve(ref.Pool, ref.Volume)
			if err != nil {
				return "", fmt.Errorf("resolve volume %s/%s: %w", ref.Pool, ref.Volume, err)
			}
			src := *disk.Source
			src.Volume = nil
			if vol.Block {
				src.Block = &libvirtxml.DomainDiskSourceBlock{Dev: vol.Path}
			} else {
				src.File = &libvirtxml.DomainDiskSourceFile{File: vol.Path}
			}
			disk.Source = &src
		}
		for i := range dom.Devices.Interfaces {
			dom.Devices.Interfaces[i].Target = &libvirtxml.DomainInterfaceTarget{Dev: portName(string(d.UID), i)}
		}
	}
	return dom.Marshal()
}

// prepareNetwork 与 preparePool 把 name 与 uuid 填为对象名与 UID：各节点上同名，domain 按名字引用。
func prepareNetwork(n *computev1alpha1.LibvirtNetwork) (string, error) {
	var x libvirtxml.Network
	if err := x.Unmarshal(n.Spec.XML); err != nil {
		return "", fmt.Errorf("parse network xml: %w", err)
	}
	x.Name, x.UUID = n.Name, string(n.UID)
	return x.Marshal()
}

func preparePool(p *computev1alpha1.LibvirtStoragePool) (string, error) {
	var x libvirtxml.StoragePool
	if err := x.Unmarshal(p.Spec.XML); err != nil {
		return "", fmt.Errorf("parse pool xml: %w", err)
	}
	x.Name, x.UUID = p.Name, string(p.UID)
	return x.Marshal()
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func plainHash(xml string) (string, error) { return hash(xml), nil }

// poolHash 去掉容量统计与目标目录权限后再算 hash：前者随使用变化；后者是 libvirt 在 pool 启动时
// 从目录实际读回填入的（持久配置里也会出现），不是配置。只算一次 define 的话，每次 libvirtd 重启
// 或宿主机重启后自动启动 pool，都会被误判为配置漂移。
func poolHash(xml string) (string, error) {
	var x libvirtxml.StoragePool
	if err := x.Unmarshal(xml); err != nil {
		return "", err
	}
	x.Capacity, x.Allocation, x.Available = nil, nil, nil
	if x.Target != nil {
		x.Target.Permissions = nil
	}
	out, err := x.Marshal()
	return hash(out), err
}
