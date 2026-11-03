package agent

import (
	"errors"
	"strings"
	"testing"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
)

func TestPrepareDomain(t *testing.T) {
	d := &computev1alpha1.LibvirtDomain{
		Namespace: "infra", Name: "dns1", UID: "1b2c3d4e-0000-4000-8000-000000000001",
		Spec: computev1alpha1.LibvirtDomainSpec{DomainXML: `<domain type='kvm'>
  <name>whatever</name><uuid>00000000-0000-0000-0000-000000000000</uuid><memory>524288</memory>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <devices>
    <disk type='volume' device='disk'><source pool='local' volume='dns1-root' startupPolicy='mandatory'/><target dev='vda' bus='virtio'/></disk>
    <disk type='volume' device='cdrom'><source pool='iso' volume='seed.iso'/><target dev='sda' bus='sata'/><readonly/></disk>
    <disk type='file' device='disk'><source file='/srv/raw.img'/><target dev='vdb' bus='virtio'/></disk>
    <interface type='network'><source network='lan'/><target dev='vnet7'/><model type='virtio'/></interface>
    <interface type='network'><source network='mgmt'/><model type='virtio'/></interface>
  </devices>
  <qemu:commandline xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'><qemu:arg value='-no-hpet'/></qemu:commandline>
</domain>`},
	}
	vols := map[string]Volume{"local/dns1-root": {Path: "/dev/data/dns1-root", Block: true}, "iso/seed.iso": {Path: "/var/lib/libvirt/iso/seed.iso"}}
	out, err := prepareDomain(d, func(pool, volume string) (Volume, error) {
		v, ok := vols[pool+"/"+volume]
		if !ok {
			return Volume{}, errors.New("no such volume")
		}
		return v, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<name>infra_dns1</name>", "<uuid>1b2c3d4e-0000-4000-8000-000000000001</uuid>",
		`<disk type="block" device="disk">`, `<source dev="/dev/data/dns1-root" startupPolicy="mandatory">`,
		`<disk type="file" device="cdrom">`, `<source file="/var/lib/libvirt/iso/seed.iso">`,
		`<source file="/srv/raw.img">`,
		`<target dev="ay1b2c3d4en0">`, `<target dev="ay1b2c3d4en1">`,
		"-no-hpet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prepared XML lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "vnet7") || strings.Contains(out, "pool=") {
		t.Errorf("user port name or volume reference survived:\n%s", out)
	}

	if _, err := prepareDomain(&computev1alpha1.LibvirtDomain{Spec: computev1alpha1.LibvirtDomainSpec{DomainXML: d.Spec.DomainXML}},
		func(string, string) (Volume, error) { return Volume{}, errors.New("gone") }); err == nil || !strings.Contains(err.Error(), "local/dns1-root") {
		t.Fatalf("unresolvable volume: err = %v", err)
	}
}

func TestPoolHashIgnoresUsage(t *testing.T) {
	a := `<pool type='dir'><name>p</name><capacity unit='bytes'>100</capacity><allocation unit='bytes'>1</allocation><available unit='bytes'>99</available><target><path>/srv/p</path></target></pool>`
	b := strings.NewReplacer("'100'", "'100'", ">1<", ">50<", ">99<", ">50<").Replace(a)
	c := strings.Replace(a, "/srv/p", "/srv/q", 1)
	ha, _ := poolHash(a)
	hb, _ := poolHash(b)
	hc, _ := poolHash(c)
	if ha != hb || ha == hc {
		t.Fatalf("usage must not count, config must: %s %s %s", ha, hb, hc)
	}
}
