package agent

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"libvirt.org/go/libvirtxml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/domainspec"
)

// spec 把 domain XML 转换为 spec.domain，测试用。
func spec(t *testing.T, xml string) runtime.RawExtension {
	t.Helper()
	var d libvirtxml.Domain
	if err := d.Unmarshal(xml); err != nil {
		t.Fatal(err)
	}
	js, err := domainspec.Encode(&d)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.RawExtension{Raw: js}
}

func TestPrepareDomain(t *testing.T) {
	const uid, root = "1b2c3d4e-0000-4000-8000-000000000001", "dns1-root"
	native := `<domain type='kvm'><memory>524288</memory>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <devices>
    <disk device='disk'><target dev='vda' bus='virtio'/><boot order='1'/><alias name='ua-root'/></disk>
    <disk device='disk'><driver discard='unmap'/><target dev='sdb' bus='scsi'/><alias name='ua-data'/></disk>
    <disk device='cdrom'><target dev='hda' bus='ide'/><readonly/><alias name='ua-iso'/></disk>
    <disk device='cdrom'><target dev='hdb' bus='ide'/><readonly/></disk>
    <interface type='network'><mac address='52:54:00:AA:BB:CC'/><model type='virtio'/><alias name='ua-lan'/></interface>
    <interface type='network'><model type='e1000e'/><alias name='ua-mgmt'/></interface>
    <serial type='pty'><target port='0'/><alias name='ua-ayatori-console'/></serial>
    <graphics type='vnc'/>
  </devices>
  <qemu:commandline xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'><qemu:arg value='-no-hpet'/></qemu:commandline>
</domain>`
	d := &computev1alpha1.LibvirtDomain{Namespace: "infra", Name: "dns1", UID: uid, Spec: computev1alpha1.LibvirtDomainSpec{
		Domain: spec(t, native),
		Disks: []computev1alpha1.DomainDisk{
			{Alias: "ua-root", Volume: root},
			{Alias: "ua-data", Volume: "data"},
			{Alias: "ua-iso", Image: "debian-iso"},
		},
		Interfaces: []computev1alpha1.DomainInterface{{Alias: "ua-lan", Network: lan}, {Alias: "ua-mgmt", Network: "mgmt"}},
	}}
	vols := map[string]Volume{
		root:   {Path: "/dev/data/dns1-root", Block: true},
		"data": {Path: "/images/data.qcow2", Format: "qcow2"},
	}
	resolve := func(disk computev1alpha1.DomainDisk) (Volume, error) {
		if disk.Image != "" {
			return Volume{Path: "/images/" + disk.Image, Format: "iso"}, nil
		}
		v, ok := vols[disk.Volume]
		if !ok {
			return Volume{}, errors.New("no such volume")
		}
		return v, nil
	}
	out, err := prepareDomain(d, resolve)
	if err != nil {
		t.Fatal(err)
	}
	out = regexp.MustCompile(`>\s+<`).ReplaceAllString(out, "><")
	for _, want := range []string{
		"<name>infra_dns1</name>", "<uuid>" + uid + "</uuid>",
		`<disk type="block" device="disk"><driver name="qemu" type="raw"></driver><source dev="/dev/data/dns1-root"></source><target dev="vda" bus="virtio"></target><boot order="1"></boot>`,
		`<driver name="qemu" type="qcow2" discard="unmap"></driver><source file="/images/data.qcow2"></source><target dev="sdb" bus="scsi">`,
		`<disk type="file" device="cdrom"><driver name="qemu" type="raw"></driver><source file="/images/debian-iso"></source><target dev="hda" bus="ide"></target><readonly></readonly>`,
		`<disk type="file" device="cdrom"><target dev="hdb" bus="ide"></target><readonly></readonly></disk>`, // 未绑定的空光驱
		`<mac address="52:54:00:AA:BB:CC"></mac><source network="lan"></source><target dev="ay1b2c3d4en0"></target><model type="virtio">`,
		`<mac address="` + macAddress(uid, 1) + `"></mac><source network="mgmt"></source><target dev="ay1b2c3d4en1"></target><model type="e1000e">`,
		`<serial type="pty"><target port="0"></target><alias name="ua-ayatori-console"></alias></serial>`,
		`<graphics type="vnc"><listen type="socket"></listen></graphics>`,
		"-no-hpet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prepared XML lacks %s:\n%s", want, out)
		}
	}
	if macAddress(uid, 0) == macAddress(uid, 1) {
		t.Error("generated MACs must differ between interfaces")
	}

	bad := d.DeepCopy()
	bad.Spec.Disks = append(bad.Spec.Disks, computev1alpha1.DomainDisk{Alias: "ua-gone", Volume: root})
	if _, err := prepareDomain(bad, resolve); err == nil || !strings.Contains(err.Error(), "ua-gone") {
		t.Errorf("binding to a missing disk: err = %v", err)
	}
	bad = d.DeepCopy()
	bad.Spec.Disks[1].Volume = "gone"
	if _, err := prepareDomain(bad, resolve); err == nil || !strings.Contains(err.Error(), "no such volume") {
		t.Errorf("unresolvable volume: err = %v", err)
	}
	// 镜像是共享的副本，只能作为只读光驱挂载。
	bad = d.DeepCopy()
	bad.Spec.Disks[1] = computev1alpha1.DomainDisk{Alias: "ua-data", Image: "debian-iso"}
	if _, err := prepareDomain(bad, resolve); err == nil || !strings.Contains(err.Error(), "read-only cdrom") {
		t.Errorf("image on a writable disk: err = %v", err)
	}
	if strings.Contains(out, "sysinfo") || strings.Contains(out, "smbios") {
		t.Errorf("no userData, no SMBIOS pointer:\n%s", out)
	}

	// userData：SMBIOS system serial 指向元数据服务上的 NoCloud seed，保留用户的其他 SMBIOS 内容。
	ci := d.DeepCopy()
	ci.Spec.UserData = "#cloud-config\n"
	ci.Spec.Domain = spec(t, strings.Replace(native, "<os>", `<sysinfo type='smbios'><bios><entry name='vendor'>Retro</entry></bios></sysinfo><os>`, 1))
	if out, err = prepareDomain(ci, resolve); err != nil {
		t.Fatal(err)
	}
	out = regexp.MustCompile(`>\s+<`).ReplaceAllString(out, "><")
	for _, want := range []string{
		`<sysinfo type="smbios"><bios><entry name="vendor">Retro</entry></bios><system><entry name="serial">ds=nocloud;s=http://169.254.169.254/nocloud/</entry></system></sysinfo>`,
		`<smbios mode="sysinfo"></smbios>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prepared XML lacks %s:\n%s", want, out)
		}
	}

	bad = d.DeepCopy()
	bad.Spec.Disks, bad.Spec.Interfaces = nil, nil
	bad.Spec.Domain.Raw = []byte(`{"type":"kvm","devices":{"graphics":[{"vnc":{},"spice":{}}]}}`)
	if _, err := prepareDomain(bad, resolve); err == nil || !strings.Contains(err.Error(), "spec.domain") {
		t.Errorf("lossy union: err = %v", err)
	}
}

func TestPoolHashIgnoresObservedValues(t *testing.T) {
	a := `<pool type='dir'><name>p</name><capacity unit='bytes'>100</capacity><allocation unit='bytes'>1</allocation><available unit='bytes'>99</available><target><path>/srv/p</path></target></pool>`
	b := strings.NewReplacer("'100'", "'100'", ">1<", ">50<", ">99<", ">50<").Replace(a)
	c := strings.Replace(a, "/srv/p", "/srv/q", 1)
	// pool 启动后 libvirt 按目录实际情况填入的权限（compute-dev1 上实测）。
	d := strings.Replace(a, "</path>", "</path><permissions><mode>0711</mode><owner>0</owner><group>0</group></permissions>", 1)
	ha, _ := poolHash(a)
	hb, _ := poolHash(b)
	hc, _ := poolHash(c)
	hd, _ := poolHash(d)
	if ha != hb || ha != hd || ha == hc {
		t.Fatalf("usage and permissions must not count, config must: %s %s %s %s", ha, hb, hc, hd)
	}
}

func TestPrepareVolumeSize(t *testing.T) {
	size := resource.MustParse("2Gi")
	v := &computev1alpha1.LibvirtStorageVolume{Spec: computev1alpha1.LibvirtStorageVolumeSpec{Size: &size,
		Libvirt: &computev1alpha1.LibvirtVolumeSource{XML: `<volume><capacity unit='G'>1</capacity><target><format type='qcow2'/></target></volume>`}}}
	v.Name = "root"
	out, err := prepareVolume(v)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := volumeCapacity(out); err != nil || got != 2<<30 || !strings.Contains(out, "qcow2") {
		t.Fatalf("capacity %d, err %v:\n%s", got, err, out)
	}
	v.Spec.Libvirt.XML = ""
	if out, err = prepareVolume(v); err != nil || !strings.Contains(out, "<name>root</name>") {
		t.Fatalf("size only: err %v:\n%s", err, out)
	}
}

func TestVolumeCapacity(t *testing.T) {
	for xml, want := range map[string]uint64{
		`<volume><capacity>512</capacity></volume>`:              512,
		`<volume><capacity unit='bytes'>512</capacity></volume>`: 512,
		`<volume><capacity unit='KB'>2</capacity></volume>`:      2000,
		`<volume><capacity unit='K'>2</capacity></volume>`:       2048,
		`<volume><capacity unit='KiB'>2</capacity></volume>`:     2048,
		`<volume><capacity unit='G'>20</capacity></volume>`:      20 << 30,
		`<volume><capacity unit='GiB'>20</capacity></volume>`:    20 << 30,
		`<volume><capacity unit='GB'>20</capacity></volume>`:     20_000_000_000,
		`<volume><capacity unit='T'>1</capacity></volume>`:       1 << 40,
	} {
		if got, err := volumeCapacity(xml); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", xml, got, err, want)
		}
	}
	for _, xml := range []string{`<volume></volume>`, `<volume><capacity unit='parsec'>1</capacity></volume>`} {
		if _, err := volumeCapacity(xml); err == nil {
			t.Errorf("%s accepted", xml)
		}
	}
}
