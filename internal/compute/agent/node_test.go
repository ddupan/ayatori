package agent

import (
	"maps"
	"testing"

	"libvirt.org/go/libvirtxml"
)

func TestCPULabels(t *testing.T) {
	// 一台 i7-8750H 的 virsh capabilities；/proc/cpuinfo 与 NFD 给出 GenuineIntel、family 6、model 158。
	var caps libvirtxml.Caps
	if err := caps.Unmarshal(`<capabilities><host><cpu><arch>x86_64</arch><model>Skylake-Client-noTSX-IBRS</model>
<vendor>Intel</vendor><signature family='6' model='158' stepping='10'/></cpu></host></capabilities>`); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
		"feature.node.kubernetes.io/cpu-model.family":    "6",
		"feature.node.kubernetes.io/cpu-model.id":        "158",
	}
	if got := cpuLabels(*caps.Host.CPU); !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
