package agent

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"libvirt.org/go/libvirtxml"
)

func TestCPULevelLabels(t *testing.T) {
	const v2, v3, v4 = cpuLevelPrefix + "v2", cpuLevelPrefix + "v3", cpuLevelPrefix + "v4"
	// 模拟只到某个特性为止的节点：要求的特性里出现 missing 就不能提供。
	node := func(missing string) func(string) (bool, error) {
		return func(xml string) (bool, error) { return missing == "" || !strings.Contains(xml, `"`+missing+`"`), nil }
	}
	for missing, want := range map[string][]string{
		"":        {v2, v3, v4},
		"avx512f": {v2, v3},
		"avx2":    {v2},
		"sse4.2":  {},
	} {
		got, err := cpuLevelLabels(node(missing))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("missing %q: got %v, want %v", missing, got, want)
		}
		for _, k := range want {
			if got[k] != "true" {
				t.Fatalf("missing %q: got %v, want %v", missing, got, want)
			}
		}
	}
	if _, err := cpuLevelLabels(func(string) (bool, error) { return false, errors.New("boom") }); err == nil {
		t.Fatal("error not returned")
	}
}

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
