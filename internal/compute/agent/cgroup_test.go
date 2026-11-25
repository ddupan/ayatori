package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContainerStats(t *testing.T) {
	sys := t.TempDir()
	const cg = "fs/cgroup/lxc.payload.default_ct/"
	const stats = "class/net/ay1n0/statistics/"
	writeTree(t, sys, map[string]string{
		cg + "cpu.stat":       "usage_usec 1500\nuser_usec 1\n",
		cg + "memory.current": "4096\n",
		cg + "memory.max":     "8192\n",
		cg + "memory.stat":    "anon 1024\nfile 3072\n",
		cg + "io.stat":        "7:0 rbytes=10 wbytes=20 rios=1 wios=2 dbytes=0 dios=0\n8:0 rbytes=30 wbytes=40\n",
		stats + "rx_bytes":    "100\n",
		stats + "tx_bytes":    "200\n",
	})
	if err := os.MkdirAll(filepath.Join(sys, "dev", "block"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/virtual/block/loop0", filepath.Join(sys, "dev", "block", "7:0")); err != nil {
		t.Fatal(err)
	}
	got, ok := containerStats(sys, "default_ct", []string{"ay1n0", "gone"})
	if !ok {
		t.Fatal("running container not found")
	}
	want := libvirt.DomainStats{Name: "default_ct", CPUTime: 1500000,
		Memory: map[string]uint64{"current": 4096, "maximum": 8192, "rss": 1024},
		Disks:  []libvirt.IOStats{{Device: "loop0", In: 10, Out: 20}, {Device: "8:0", In: 30, Out: 40}},
		Nics:   []libvirt.IOStats{{Device: "ay1n0", In: 200, Out: 100}}}
	if d := cmp.Diff(want, got); d != "" {
		t.Error(d)
	}
}

func TestContainerStatsWithoutLimitOrCgroup(t *testing.T) {
	sys := t.TempDir()
	const cg = "fs/cgroup/lxc.payload.c/"
	writeTree(t, sys, map[string]string{cg + "cpu.stat": "usage_usec 1\n", cg + "memory.max": "max\n"})
	got, ok := containerStats(sys, "c", nil)
	if !ok {
		t.Fatal("not found")
	}
	if _, set := got.Memory["maximum"]; set {
		t.Error("memory.max of max is no limit, not a value")
	}
	if _, ok := containerStats(sys, "stopped", nil); ok {
		t.Error("a stopped container has no cgroup")
	}
}
