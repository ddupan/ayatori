package agent

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
)

func TestPrepareDomain(t *testing.T) {
	const uid = "1b2c3d4e-0000-4000-8000-000000000001"
	native := `<domain type='kvm'><memory>524288</memory>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <devices><serial type='pty'/></devices>
  <qemu:commandline xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'><qemu:arg value='-no-hpet'/></qemu:commandline>
</domain>`
	d := &computev1alpha1.LibvirtDomain{Namespace: "infra", Name: "dns1", UID: uid, Spec: computev1alpha1.LibvirtDomainSpec{
		DomainXML: native,
		Disks: []computev1alpha1.DomainDisk{
			{Volume: "dns1-root", BootOrder: 1},
			{Volume: "seed", Bus: computev1alpha1.DiskBusSATA, ReadOnly: true},
			{Volume: "data", Bus: computev1alpha1.DiskBusSCSI},
			{Volume: "dos", Bus: computev1alpha1.DiskBusFDC},
			{Image: "debian-iso", Bus: computev1alpha1.DiskBusIDE},
		},
		Interfaces: []computev1alpha1.DomainInterface{
			{Network: "lan", MAC: "52:54:00:AA:BB:CC"},
			{Network: "mgmt", Model: "e1000e"},
		},
	}}
	vols := map[string]Volume{
		"dns1-root": {Path: "/dev/data/dns1-root", Block: true},
		"seed":      {Path: "/var/lib/libvirt/iso/seed.iso", Format: "iso"},
		"data":      {Path: "/images/data.qcow2", Format: "qcow2"},
		"dos":       {Path: "/images/dos.img", Format: "raw"},
	}
	resolve := func(disk computev1alpha1.DomainDisk) (Volume, error) {
		if disk.Image != "" {
			return Volume{Path: "/images/" + disk.Image}, nil
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
	if !strings.Contains(out, `<disk type="file" device="cdrom"><driver name="qemu" type="raw"></driver><source file="/images/debian-iso"></source><target dev="hda" bus="ide"></target><readonly></readonly>`) {
		t.Fatalf("cdrom not rendered:\n%s", out)
	}
	for _, want := range []string{
		"<name>infra_dns1</name>", "<uuid>" + uid + "</uuid>",
		`<disk type="block" device="disk"><driver name="qemu" type="raw"></driver><source dev="/dev/data/dns1-root"></source><target dev="vda" bus="virtio"></target><boot order="1"></boot>`,
		`<source file="/var/lib/libvirt/iso/seed.iso"></source><target dev="sda" bus="sata"></target><readonly></readonly>`,
		`<driver name="qemu" type="qcow2"></driver><source file="/images/data.qcow2"></source><target dev="sdb" bus="scsi">`,
		`<disk type="file" device="floppy">`, `<target dev="fda" bus="fdc">`,
		`<mac address="52:54:00:aa:bb:cc"></mac><source network="lan"></source><target dev="ay1b2c3d4en0"></target><model type="virtio">`,
		`<mac address="` + macAddress(uid, 1) + `"></mac><source network="mgmt"></source><target dev="ay1b2c3d4en1"></target><model type="e1000e">`,
		`<serial type="pty">`, "-no-hpet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prepared XML lacks %s:\n%s", want, out)
		}
	}
	if macAddress(uid, 0) == macAddress(uid, 1) {
		t.Error("generated MACs must differ between interfaces")
	}

	d.Spec.Firmware = &computev1alpha1.DomainFirmware{Type: computev1alpha1.FirmwareUEFI, SecureBoot: true, TPM: true}
	out, err = prepareDomain(d, resolve)
	if err != nil {
		t.Fatal(err)
	}
	out = regexp.MustCompile(`>\s+<`).ReplaceAllString(out, "><")
	for _, want := range []string{
		`<os firmware="efi">`, `<features><acpi></acpi></features>`,
		`<firmware><feature enabled="yes" name="secure-boot"></feature><feature enabled="yes" name="enrolled-keys"></feature></firmware>`,
		`<tpm model="tpm-crb"><backend type="emulator" version="2.0"></backend></tpm>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prepared XML lacks %s:\n%s", want, out)
		}
	}
	d.Spec.Firmware = &computev1alpha1.DomainFirmware{Type: computev1alpha1.FirmwareUEFI}
	if out, err = prepareDomain(d, resolve); err != nil || !strings.Contains(out, `<feature enabled="no" name="secure-boot">`) || strings.Contains(out, "enrolled-keys") || strings.Contains(out, "<tpm") {
		t.Errorf("UEFI without secure boot: err = %v\n%s", err, out)
	}
	d.Spec.Firmware = &computev1alpha1.DomainFirmware{Type: computev1alpha1.FirmwareBIOS}
	if out, err = prepareDomain(d, resolve); err != nil || strings.Contains(out, "firmware") {
		t.Errorf("BIOS must leave the firmware to libvirt: err = %v\n%s", err, out)
	}
	d.Spec.Firmware = nil

	d.Spec.Disks = append(d.Spec.Disks, computev1alpha1.DomainDisk{Volume: "gone"})
	if _, err := prepareDomain(d, resolve); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("unresolvable volume: err = %v", err)
	}

	for _, owned := range []string{"<name>x</name>", "<uuid>" + uid + "</uuid>"} {
		d.Spec.DomainXML = strings.Replace(native, "<memory>", owned+"<memory>", 1)
		if _, err := prepareDomain(d, resolve); err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("%s in domainXML: err = %v", owned, err)
		}
	}
	const osEnd = "</os>"
	for _, owned := range []struct{ old, new string }{
		{"<serial", "<tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm><serial"},
		{"<os>", "<os firmware='efi'>"},
		{osEnd, "<firmware><feature enabled='yes' name='secure-boot'/></firmware></os>"},
		{osEnd, "<loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader></os>"},
		{osEnd, "<nvram>/srv/vars.fd</nvram></os>"},
	} {
		d.Spec.DomainXML = strings.Replace(native, owned.old, owned.new, 1)
		if _, err := prepareDomain(d, resolve); err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("%s in domainXML: err = %v", owned.new, err)
		}
	}
	for _, owned := range []string{`<disk type='file'><source file='/srv/raw.img'/><target dev='vdb'/></disk>`, `<interface type='bridge'><source bridge='br0'/></interface>`} {
		d.Spec.DomainXML = strings.Replace(native, "<serial", owned+"<serial", 1)
		if _, err := prepareDomain(d, resolve); err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("%s in domainXML: err = %v", owned, err)
		}
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
