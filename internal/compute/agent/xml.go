package agent

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"libvirt.org/go/libvirtxml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
)

// Volume 是 domain 引用的卷在本节点上的事实。
type Volume struct {
	Path   string
	Block  bool
	Format string
}

// domainName 是实例在 libvirt 中的名字。Kubernetes 的名字里不会出现 "_"，因此不会与别的对象冲突。
func domainName(namespace, name string) string { return namespace + "_" + name }

// portName 是实例第 i 块网卡在宿主机上的端口名。固定下来，按端口写的 nftables 规则（元数据服务）
// 在冷启动后仍然有效；Linux 接口名最长 15 字符（ADR-0016）。
// ponytail: 取 UID 前 8 个十六进制字符，同一节点上的实例碰撞概率可忽略；规模变大时改用节点内分配。
func portName(uid string, i int) string {
	return fmt.Sprintf("ay%sn%d", strings.ReplaceAll(uid, "-", "")[:8], i)
}

// macAddress 是实例第 i 块网卡缺省的 MAC：由 UID 稳定生成，冷启动换节点后不变（DHCP 预留依赖它）。
// ponytail: 只有 24 位随机部分，homelab 规模下碰撞可忽略；规模变大时改用集中分配。
func macAddress(uid string, i int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", uid, i))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// diskTargets 是各总线上客户机看到的设备名前缀；scsi 与 sata 共用 sd。
var diskTargets = map[computev1alpha1.DiskBus]string{
	computev1alpha1.DiskBusVirtio: "vd", computev1alpha1.DiskBusSCSI: "sd", computev1alpha1.DiskBusSATA: "sd",
	computev1alpha1.DiskBusIDE: "hd", computev1alpha1.DiskBusFDC: "fd",
}

// prepareDomain 把类型化的 disks、interfaces 渲染进原生 domainXML，并填写 name 与 uuid（ADR-0015）。
// 这些元素归 Ayatori，domainXML 中出现就拒绝，不静默覆盖。卷解析成具体路径：AppArmor 不会把
// type='volume' 的源加入实例的 profile（PoC 02）。
func prepareDomain(d *computev1alpha1.LibvirtDomain, resolve func(volume string) (Volume, error)) (string, error) {
	var dom libvirtxml.Domain
	if err := dom.Unmarshal(d.Spec.DomainXML); err != nil {
		return "", fmt.Errorf("parse domainXML: %w", err)
	}
	var owned []string
	if dom.Name != "" {
		owned = append(owned, "<name>")
	}
	if dom.UUID != "" {
		owned = append(owned, "<uuid>")
	}
	if dom.Devices != nil && len(dom.Devices.Disks) > 0 {
		owned = append(owned, "<disk>")
	}
	if dom.Devices != nil && len(dom.Devices.Interfaces) > 0 {
		owned = append(owned, "<interface>")
	}
	if len(owned) > 0 {
		return "", fmt.Errorf("domainXML must not contain %s: Ayatori renders them from spec", strings.Join(owned, ", "))
	}
	dom.Name = domainName(d.Namespace, d.Name)
	dom.UUID = string(d.UID)
	if dom.Devices == nil {
		dom.Devices = &libvirtxml.DomainDeviceList{}
	}
	used := map[string]int{}
	for _, disk := range d.Spec.Disks {
		vol, err := resolve(disk.Volume)
		if err != nil {
			return "", fmt.Errorf("resolve volume %s: %w", disk.Volume, err)
		}
		bus := cmp.Or(disk.Bus, computev1alpha1.DiskBusVirtio)
		prefix := diskTargets[bus]
		x := libvirtxml.DomainDisk{
			Device: "disk",
			Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: "raw"},
			Target: &libvirtxml.DomainDiskTarget{Dev: prefix + string(rune('a'+used[prefix])), Bus: string(bus)},
		}
		used[prefix]++
		if bus == computev1alpha1.DiskBusFDC {
			x.Device = "floppy"
		}
		if vol.Format != "" && vol.Format != "iso" {
			x.Driver.Type = vol.Format
		}
		if vol.Block {
			x.Source = &libvirtxml.DomainDiskSource{Block: &libvirtxml.DomainDiskSourceBlock{Dev: vol.Path}}
		} else {
			x.Source = &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: vol.Path}}
		}
		if disk.ReadOnly {
			x.ReadOnly = &libvirtxml.DomainDiskReadOnly{}
		}
		if disk.BootOrder > 0 {
			x.Boot = &libvirtxml.DomainDeviceBoot{Order: uint(disk.BootOrder)}
		}
		dom.Devices.Disks = append(dom.Devices.Disks, x)
	}
	for i, nic := range d.Spec.Interfaces {
		mac := strings.ToLower(cmp.Or(nic.MAC, macAddress(string(d.UID), i)))
		dom.Devices.Interfaces = append(dom.Devices.Interfaces, libvirtxml.DomainInterface{
			Source: &libvirtxml.DomainInterfaceSource{Network: &libvirtxml.DomainInterfaceSourceNetwork{Network: nic.Network}},
			MAC:    &libvirtxml.DomainInterfaceMAC{Address: mac},
			Model:  &libvirtxml.DomainInterfaceModel{Type: cmp.Or(nic.Model, "virtio")},
			Target: &libvirtxml.DomainInterfaceTarget{Dev: portName(string(d.UID), i)},
		})
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

// prepareVolume 把 name 填为对象名：domain 按 <source pool='…' volume='对象名'/> 引用。
func prepareVolume(v *computev1alpha1.LibvirtStorageVolume) (string, error) {
	var x libvirtxml.StorageVolume
	if err := x.Unmarshal(v.Spec.Libvirt.XML); err != nil {
		return "", fmt.Errorf("parse volume xml: %w", err)
	}
	x.Name = v.Name
	return x.Marshal()
}

