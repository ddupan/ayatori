package agent

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// prepareDomain 把类型化的 firmware、disks、interfaces 渲染进原生 domainXML，并填写 name 与 uuid（ADR-0015）。
// 这些元素归 Ayatori，domainXML 中出现就拒绝，不静默覆盖。卷解析成具体路径：AppArmor 不会把
// type='volume' 的源加入实例的 profile（PoC 02）。
func prepareDomain(d *computev1alpha1.LibvirtDomain, resolve func(computev1alpha1.DomainDisk) (Volume, error)) (string, error) {
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
	if dom.Devices != nil && len(dom.Devices.TPMs) > 0 {
		owned = append(owned, "<tpm>")
	}
	if dom.OS != nil && (dom.OS.Firmware != "" || dom.OS.FirmwareInfo != nil) {
		owned = append(owned, "<os> firmware")
	}
	if dom.OS != nil && dom.OS.Loader != nil {
		owned = append(owned, "<loader>")
	}
	if dom.OS != nil && dom.OS.NVRam != nil {
		owned = append(owned, "<nvram>")
	}
	if len(owned) > 0 {
		return "", fmt.Errorf("domainXML must not contain %s: Ayatori renders them from spec", strings.Join(owned, ", "))
	}
	dom.Name = domainName(d.Namespace, d.Name)
	dom.UUID = string(d.UID)
	if dom.Devices == nil {
		dom.Devices = &libvirtxml.DomainDeviceList{}
	}
	renderFirmware(&dom, d.Spec.Firmware)
	used := map[string]int{}
	for _, disk := range d.Spec.Disks {
		vol, err := resolve(disk)
		if err != nil {
			return "", fmt.Errorf("resolve disk %s: %w", disk.Volume+disk.Image, err)
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
		if disk.Image != "" {
			x.Device = "cdrom"
		}
		if disk.ReadOnly || disk.Image != "" {
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

// renderFirmware 让 libvirt 按固件描述文件自动选择 OVMF，SMM 也由它按描述文件打开。不写 <nvram> 和
// vTPM 的 <source>：本地存储用节点默认位置（ADR-0015）；DRBD 上的块设备随 LINSTOR 集成加入（ADR-0020）。
// BIOS 不写 firmware 属性，libvirt 默认就是 SeaBIOS，节点也不必有 BIOS 的描述文件。
func renderFirmware(dom *libvirtxml.Domain, f *computev1alpha1.DomainFirmware) {
	if f == nil {
		return
	}
	if f.Type == computev1alpha1.FirmwareUEFI {
		if dom.OS == nil {
			dom.OS = &libvirtxml.DomainOS{}
		}
		dom.OS.Firmware = "efi"
		if dom.Features == nil {
			dom.Features = &libvirtxml.DomainFeatureList{}
		}
		dom.Features.ACPI = &libvirtxml.DomainFeature{} // UEFI 必须有 ACPI，否则 libvirt 拒绝 define
		features := []libvirtxml.DomainOSFirmwareFeature{{Enabled: "no", Name: "secure-boot"}}
		if f.SecureBoot { // enrolled-keys：预置微软密钥，Windows 和带 shim 的 Linux 才能启动（PoC 02）
			features = []libvirtxml.DomainOSFirmwareFeature{{Enabled: "yes", Name: "secure-boot"}, {Enabled: "yes", Name: "enrolled-keys"}}
		}
		dom.OS.FirmwareInfo = &libvirtxml.DomainOSFirmwareInfo{Features: features}
	}
	if f.TPM {
		dom.Devices.TPMs = []libvirtxml.DomainTPM{{
			Model:   "tpm-crb",
			Backend: &libvirtxml.DomainTPMBackend{Emulator: &libvirtxml.DomainTPMBackendEmulator{Version: "2.0"}},
		}}
	}
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

// volumeCapacity 按 libvirt 的单位规则换算 volume XML 中的 capacity：KB 是 1000，K 与 KiB 是 1024。
func volumeCapacity(xml string) (uint64, error) {
	var x libvirtxml.StorageVolume
	if err := x.Unmarshal(xml); err != nil {
		return 0, fmt.Errorf("parse volume xml: %w", err)
	}
	if x.Capacity == nil {
		return 0, errors.New("volume xml has no capacity")
	}
	unit := strings.ToLower(x.Capacity.Unit)
	if unit == "" || unit == "b" || unit == "byte" || unit == "bytes" {
		return x.Capacity.Value, nil
	}
	power := strings.IndexByte("kmgtpe", unit[0]) + 1
	base := uint64(1024)
	if strings.HasSuffix(unit, "b") && !strings.HasSuffix(unit, "ib") {
		base = 1000
	}
	if power == 0 || len(unit) > 3 {
		return 0, fmt.Errorf("unknown capacity unit %q", x.Capacity.Unit)
	}
	value := x.Capacity.Value
	for range power {
		value *= base
	}
	return value, nil
}
