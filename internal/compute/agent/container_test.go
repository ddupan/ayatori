package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
)

const lan = "lan"

func container(config string, networks ...computev1alpha1.ContainerNetwork) *computev1alpha1.LXCContainer {
	return &computev1alpha1.LXCContainer{Name: "web", UID: "0123456789abcdef",
		Spec: computev1alpha1.LXCContainerSpec{Config: config, Networks: networks}}
}

func TestPrepareContainer(t *testing.T) {
	bridge := func(n string) (string, error) { return "br-" + n, nil }
	file := Volume{Path: "/p/root", Format: formatRaw}
	got, err := prepareContainer(container("lxc.arch = linux64\n", computev1alpha1.ContainerNetwork{Network: lan},
		computev1alpha1.ContainerNetwork{Network: "wan", MAC: "02:00:00:00:00:02"}), file, "", bridge)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lxc.arch = linux64\nlxc.uts.name = web\n", "lxc.rootfs.path = loop:/p/root\n",
		"lxc.net.0.type = veth\nlxc.net.0.link = br-lan\nlxc.net.0.flags = up\nlxc.net.0.hwaddr = " + macAddress("0123456789abcdef", 0),
		"lxc.net.0.veth.pair = ay01234567n0\n", "lxc.net.1.link = br-wan\n", "lxc.net.1.hwaddr = 02:00:00:00:00:02\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config lacks %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "lxc.mount.entry") {
		t.Errorf("no userData, no seed mount:\n%s", got)
	}
	got, err = prepareContainer(container("lxc.arch = linux64"), file, "/state/seed/ns_web", bridge)
	if err != nil || !strings.Contains(got, "lxc.mount.entry = /state/seed/ns_web var/lib/cloud/seed/nocloud none bind,ro,create=dir 0 0\n") {
		t.Errorf("seed must be mounted read-only: %v\n%s", err, got)
	}

	got, err = prepareContainer(container("lxc.arch = linux64"), Volume{Path: "/dev/drbd1", Block: true}, "", bridge)
	if err != nil || !strings.Contains(got, "lxc.rootfs.path = /dev/drbd1\n") {
		t.Errorf("block rootfs must be used directly: %v\n%s", err, got)
	}
	if _, err = prepareContainer(container("lxc.arch = linux64"), Volume{Path: "/p/root", Format: "qcow2"}, "", bridge); err == nil {
		t.Error("a qcow2 file cannot be a rootfs")
	}
	broken := errors.New("no bridge")
	if _, err = prepareContainer(container("x = 1", computev1alpha1.ContainerNetwork{Network: lan}), file, "",
		func(string) (string, error) { return "", broken }); !errors.Is(err, broken) {
		t.Errorf("bridge error lost: %v", err)
	}
}

func TestWriteSeed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "seed", "ns_web")
	c := container("")
	c.Spec.UserData = "#cloud-config\npackages: [htop]\n"
	if err := writeSeed(dir, c); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"meta-data": "instance-id: 0123456789abcdef\nlocal-hostname: web\n", "user-data": c.Spec.UserData} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	c.Spec.UserData = "#cloud-config\n"
	if err := writeSeed(dir, c); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "user-data")); string(got) != c.Spec.UserData {
		t.Errorf("user-data not updated: %q", got)
	}
}

func TestPrepareContainerRejectsOwnedKeys(t *testing.T) {
	for _, line := range []string{"lxc.uts.name = x", "lxc.rootfs.path=dir:/x", "  lxc.net.0.type = veth", "lxc.net ="} {
		_, err := prepareContainer(container("lxc.arch = linux64\n"+line+"\n"), Volume{Format: formatRaw}, "", nil)
		if err == nil || !strings.Contains(err.Error(), "owned by Ayatori") {
			t.Errorf("%q: got %v", line, err)
		}
	}
	for _, line := range []string{"# lxc.net.0.type = veth", "lxc.rootfs.options = noatime", "lxc.include = /c"} {
		if _, err := prepareContainer(container(line), Volume{Format: formatRaw}, "", nil); err != nil {
			t.Errorf("%q must pass through: %v", line, err)
		}
	}
}

func TestCheckImageTypeRootfs(t *testing.T) {
	ustar := make([]byte, 512)
	copy(ustar[tarMagicOffset:], tarMagic)
	for _, ok := range [][]byte{ustar, {0x1f, 0x8b, 8}, {0xfd, '7', 'z', 'X', 'Z', 0, 1}, {0x28, 0xb5, 0x2f, 0xfd, 1}} {
		if err := checkImageType(bytes.NewReader(ok), computev1alpha1.ImageRootfs); err != nil {
			t.Errorf("%x: %v", ok[:2], err)
		}
	}
	for _, bad := range [][]byte{[]byte("not an archive"), make([]byte, 512), {}} {
		if err := checkImageType(bytes.NewReader(bad), computev1alpha1.ImageRootfs); err == nil {
			t.Errorf("%q accepted as a rootfs", bad)
		}
	}
}

// mkfsReadsTarballs 判断本机 mke2fs 能否直接读归档（1.47.1 起）。CI 的 Ubuntu 24.04 是 1.47.0，只能在
// Debian 13 的构建容器与 Dev 节点上验证。
func mkfsReadsTarballs(t *testing.T) bool {
	t.Helper()
	out, _ := exec.Command("mkfs.ext4", "-V").CombinedOutput()
	m := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`).FindSubmatch(out)
	if m == nil {
		return false
	}
	version := func(i int) int { n, _ := strconv.Atoi(string(m[i])); return n }
	return version(1) > 1 || version(2) > 47 || (version(2) == 47 && version(3) >= 1)
}

func TestBuildRootfs(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH")+":/usr/sbin:/sbin")
	if !mkfsReadsTarballs(t) {
		t.Skip("mke2fs cannot read tarballs here")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	w := tar.NewWriter(gz)
	body := "web\n"
	for _, h := range []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))},
		{Name: "su", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 1000, Gid: 1000},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(w.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive, path := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "root")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	const capacity = 64 << 20
	if err := buildRootfs(path, archive, capacity); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != capacity {
		t.Fatalf("size: %v %v", info, err)
	}
	debugfs := func(cmd string) string {
		out, err := exec.Command("debugfs", "-R", cmd, path).CombinedOutput()
		if err != nil {
			t.Fatalf("debugfs %s: %v: %s", cmd, err, out)
		}
		return string(out)
	}
	if got := debugfs("cat /etc/hostname"); !strings.Contains(got, body) {
		t.Errorf("content: %q", got)
	}
	if got := debugfs("stat /su"); !strings.Contains(got, "Mode:  04755") || !regexp.MustCompile(`User:\s+1000\s+Group:\s+1000`).MatchString(got) {
		t.Errorf("owner and mode lost: %s", got)
	}
	if err := buildRootfs(path, filepath.Join(dir, "missing"), capacity); err == nil {
		t.Error("a missing archive must fail")
	}
}
