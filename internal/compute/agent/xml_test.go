package agent

import (
	"errors"
	"regexp"
	"strings"
	"testing"

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
	resolve := func(name string) (Volume, error) {
		v, ok := vols[name]
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
