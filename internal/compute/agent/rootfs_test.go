package agent

import (
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.ddupan.top/ayatori/internal/compute/adapter/lxc"
)

type growRecorder struct {
	ContainerRuntime
	blocks uint64
}

func (g *growRecorder) GrowRootfs(_, _ string, blocks uint64) error { g.blocks = blocks; return nil }

func superblock(t *testing.T, blocks uint32, hi uint32, feat uint32, logSize uint32, magic uint16) string {
	t.Helper()
	sb := make([]byte, ext4SuperblockOffset+ext4SuperblockSize)
	raw := sb[ext4SuperblockOffset:]
	le := binary.LittleEndian
	le.PutUint32(raw[0x04:], blocks)
	le.PutUint32(raw[0x18:], logSize)
	le.PutUint16(raw[0x38:], magic)
	le.PutUint32(raw[0x60:], feat)
	le.PutUint32(raw[0x150:], hi)
	path := filepath.Join(t.TempDir(), "fs")
	if err := os.WriteFile(path, sb, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExt4Geometry(t *testing.T) {
	blocks, size, ok, err := ext4Geometry(superblock(t, 100, 7, 0, 2, ext4Magic))
	if err != nil || !ok || blocks != 100 || size != 4096 {
		t.Errorf("without the 64bit feature the high half is ignored: %d %d %t %v", blocks, size, ok, err)
	}
	blocks, _, _, _ = ext4Geometry(superblock(t, 1, 2, ext4Feature64Bit, 0, ext4Magic))
	if blocks != 2<<32|1 {
		t.Errorf("64bit block count: %d", blocks)
	}
	if _, _, ok, err = ext4Geometry(superblock(t, 1, 0, 0, 0, 0x1234)); ok || err != nil {
		t.Errorf("not ext4 must be ignored: %t %v", ok, err)
	}
	if _, _, ok, err = ext4Geometry(filepath.Join(t.TempDir(), "missing")); ok || err != nil {
		t.Errorf("missing file must be ignored: %t %v", ok, err)
	}
}

func TestGrowRootfsDecision(t *testing.T) {
	path := superblock(t, 100, 0, 0, 2, ext4Magic)
	rec := &growRecorder{}
	a := &Agent{Containers: rec, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	running := lxc.Container{Found: true, State: "running"}
	if err := a.growRootfs("c", Volume{Path: path, Capacity: 100 * 4096}, running); err != nil || rec.blocks != 0 {
		t.Errorf("same size must do nothing: %d %v", rec.blocks, err)
	}
	if err := a.growRootfs("c", Volume{Path: path, Capacity: 200*4096 + 1}, running); err != nil || rec.blocks != 200 {
		t.Errorf("online grow rounds down to blocks: %d %v", rec.blocks, err)
	}
	rec.blocks = 0
	if err := a.growRootfs("c", Volume{Path: path, Capacity: 200 * 4096}, lxc.Container{Found: true, State: "starting"}); err != nil || rec.blocks != 0 {
		t.Errorf("a transitional state must wait: %d %v", rec.blocks, err)
	}
	notExt4 := superblock(t, 1, 0, 0, 0, 0x1234)
	if err := a.growRootfs("c", Volume{Path: notExt4, Capacity: 1 << 30}, running); err != nil || rec.blocks != 0 {
		t.Errorf("only ext4 is grown: %d %v", rec.blocks, err)
	}
}

func TestGrowExt4Offline(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH")+":/usr/sbin:/sbin")
	if _, err := exec.LookPath("resize2fs"); err != nil {
		t.Skip("e2fsprogs not installed")
	}
	path := filepath.Join(t.TempDir(), "root")
	const small, large = 32 << 20, 64 << 20
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, small); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v: %s", err, out)
	}
	if err := os.Truncate(path, large); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Containers: &growRecorder{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.growRootfs("c", Volume{Path: path, Capacity: large}, lxc.Container{Found: true, State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	blocks, size, ok, err := ext4Geometry(path)
	if err != nil || !ok || blocks*size != large {
		t.Errorf("filesystem did not grow: %d*%d %t %v", blocks, size, ok, err)
	}
}
