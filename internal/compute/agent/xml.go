package agent

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"libvirt.org/go/libvirtxml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/cloudinit"
	"go.ddupan.top/ayatori/internal/compute/domainspec"
	"go.ddupan.top/ayatori/internal/compute/naming"
)

// formatRaw 是 libvirt 的 raw 卷格式。
const formatRaw = "raw"

// Volume 是卷在本节点上的事实。
type Volume struct {
	Found    bool
	Path     string
	Block    bool
	Format   string
	Capacity uint64
}

// domainName 是实例在 libvirt 中的名字。Kubernetes 的名字里不会出现 "_"，因此不会与别的对象冲突。
func domainName(namespace, name string) string { return namespace + "_" + name }

// macAddress 是实例第 i 块网卡缺省的 MAC：由 UID 稳定生成，冷启动换节点后不变（DHCP 预留依赖它）。
// ponytail: 只有 24 位随机部分，homelab 规模下碰撞可忽略；规模变大时改用集中分配。
func macAddress(uid string, i int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", uid, i))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// prepareDomain 把 spec.domain 映射为 domain XML，只填写 Ayatori 拥有的字段（ADR-0026）：name、uuid；绑定的
// 磁盘的 source 与 driver.type；绑定的网卡的 source、target 与缺省的 mac；VNC/SPICE 的本机 socket 监听。
// 其余内容原样交给 libvirt。卷解析成具体路径：AppArmor 不会把 type='volume' 的源加入实例的 profile（PoC 02）。
// 用户不能写这些字段由 CRD 的 CEL 规则保证，这里不再检查。
func prepareDomain(d *computev1alpha1.LibvirtDomain, resolve func(computev1alpha1.DomainDisk) (Volume, error)) (string, error) {
	dom, err := domainspec.Decode(d.Spec.Domain.Raw)
	if err != nil {
		return "", fmt.Errorf("spec.domain: %w", err)
	}
	dom.Name = domainName(d.Namespace, d.Name)
	dom.UUID = string(d.UID)
	if dom.Devices == nil {
		dom.Devices = &libvirtxml.DomainDeviceList{}
	}
	for _, b := range d.Spec.Disks {
		i := slices.IndexFunc(dom.Devices.Disks, func(x libvirtxml.DomainDisk) bool { return x.Alias != nil && x.Alias.Name == b.Alias })
		if i < 0 {
			return "", fmt.Errorf("no disk with alias %s", b.Alias)
		}
		disk := &dom.Devices.Disks[i]
		if b.Image != "" && (disk.Device != "cdrom" || disk.ReadOnly == nil) {
			return "", fmt.Errorf("disk %s: image %s can only back a read-only cdrom", b.Alias, b.Image)
		}
		vol, err := resolve(b)
		if err != nil {
			return "", fmt.Errorf("resolve disk %s: %w", b.Alias, err)
		}
		if disk.Driver == nil {
			disk.Driver = &libvirtxml.DomainDiskDriver{}
		}
		disk.Driver.Name = cmp.Or(disk.Driver.Name, "qemu")
		disk.Driver.Type = formatRaw
		if vol.Format != "" && vol.Format != "iso" {
			disk.Driver.Type = vol.Format
		}
		if vol.Block {
			disk.Source = &libvirtxml.DomainDiskSource{Block: &libvirtxml.DomainDiskSourceBlock{Dev: vol.Path}}
		} else {
			disk.Source = &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: vol.Path}}
		}
	}
	for n, b := range d.Spec.Interfaces {
		i := slices.IndexFunc(dom.Devices.Interfaces, func(x libvirtxml.DomainInterface) bool { return x.Alias != nil && x.Alias.Name == b.Alias })
		if i < 0 {
			return "", fmt.Errorf("no interface with alias %s", b.Alias)
		}
		nic := &dom.Devices.Interfaces[i]
		nic.Source = &libvirtxml.DomainInterfaceSource{Network: &libvirtxml.DomainInterfaceSourceNetwork{Network: b.Network}}
		nic.Target = &libvirtxml.DomainInterfaceTarget{Dev: naming.PortName(string(d.UID), n)}
		if nic.MAC == nil {
			nic.MAC = &libvirtxml.DomainInterfaceMAC{Address: macAddress(string(d.UID), n)}
		}
	}
	if d.Spec.UserData != "" {
		pointCloudInit(dom)
	}
	// 控制台不对外暴露端口，只监听本机 unix socket，由 Agent 中继（ADR-0016）。
	socket := []libvirtxml.DomainGraphicListener{{Socket: &libvirtxml.DomainGraphicListenerSocket{}}}
	for _, g := range dom.Devices.Graphics {
		if g.VNC != nil {
			g.VNC.Listeners = socket
		}
		if g.Spice != nil {
			g.Spice.Listeners = socket
		}
	}
	xml, err := domainspec.Marshal(dom)
	if err != nil {
		return "", fmt.Errorf("spec.domain: %w", err)
	}
	return xml, nil
}

// pointCloudInit 在 SMBIOS 的 system serial 中写入 NoCloud seed 的地址，cloud-init 从元数据服务读取 meta-data 与
// user-data（ADR-0016）。用户的其他 SMBIOS 内容保留；os.smBios 与 system serial 在有 userData 时归 Ayatori（CEL 校验）。
func pointCloudInit(dom *libvirtxml.Domain) {
	if dom.OS == nil {
		dom.OS = &libvirtxml.DomainOS{}
	}
	dom.OS.SMBios = &libvirtxml.DomainSMBios{Mode: "sysinfo"}
	i := slices.IndexFunc(dom.SysInfo, func(s libvirtxml.DomainSysInfo) bool { return s.SMBIOS != nil })
	if i < 0 {
		dom.SysInfo = append(dom.SysInfo, libvirtxml.DomainSysInfo{SMBIOS: &libvirtxml.DomainSysInfoSMBIOS{}})
		i = len(dom.SysInfo) - 1
	}
	smbios := dom.SysInfo[i].SMBIOS
	if smbios.System == nil {
		smbios.System = &libvirtxml.DomainSysInfoSystem{}
	}
	smbios.System.Entry = append(smbios.System.Entry, libvirtxml.DomainSysInfoEntry{Name: "serial", Value: cloudinit.SMBIOSSerial})
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
	if v.Spec.Libvirt.XML != "" {
		if err := x.Unmarshal(v.Spec.Libvirt.XML); err != nil {
			return "", fmt.Errorf("parse volume xml: %w", err)
		}
	}
	x.Name = v.Name
	if v.Spec.Size != nil {
		x.Capacity = &libvirtxml.StorageVolumeSize{Unit: "bytes", Value: uint64(v.Spec.Size.Value())}
	}
	return x.Marshal()
}

// volumeFormat 返回 volume XML 声明的格式；没写或解析不了时为空，解析错误由 prepareVolume 报告。
func volumeFormat(xml string) string {
	var x libvirtxml.StorageVolume
	if xml == "" || x.Unmarshal(xml) != nil || x.Target == nil || x.Target.Format == nil {
		return ""
	}
	return x.Target.Format.Type
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
