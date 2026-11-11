package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"

	"go.ddupan.top/ayatori/internal/compute/adapter/lxc"
)

const (
	ext4SuperblockOffset = 1024
	ext4SuperblockSize   = 1024
	ext4Magic            = 0xEF53
	ext4Feature64Bit     = 0x80
)

// ext4Geometry 从 superblock 读文件系统的块数与块大小。path 不是 ext4（或不存在）时 ok 为 false：Agent 只扩大
// 自己认识的文件系统。
func ext4Geometry(path string) (blocks, blockSize uint64, ok bool, err error) {
	f, err := os.Open(path) //nolint:gosec // 路径来自 libvirt 报告的卷路径
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, false, nil
	} else if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = f.Close() }()
	sb := make([]byte, ext4SuperblockSize)
	if _, err := f.ReadAt(sb, ext4SuperblockOffset); errors.Is(err, io.EOF) {
		return 0, 0, false, nil
	} else if err != nil {
		return 0, 0, false, err
	}
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != ext4Magic {
		return 0, 0, false, nil
	}
	blocks = uint64(le.Uint32(sb[0x04:]))
	if le.Uint32(sb[0x60:])&ext4Feature64Bit != 0 {
		blocks |= uint64(le.Uint32(sb[0x150:])) << 32
	}
	return blocks, 1024 << le.Uint32(sb[0x18:]), true, nil
}

// growRootfs 把 ext4 rootfs 扩大到卷的容量（ADR-0019 §7）。卷本身已由 volumeBackend 扩大；这里只让文件系统跟上，
// 中途失败时下一轮按 superblock 与卷容量的差值继续。启动或停止途中什么也不做，等状态稳定。
func (a *Agent) growRootfs(name string, rootfs Volume, cur lxc.Container) error {
	blocks, blockSize, ok, err := ext4Geometry(rootfs.Path)
	if err != nil || !ok {
		return err
	}
	want := rootfs.Capacity / blockSize
	if blocks >= want {
		return nil
	}
	switch {
	case cur.Running():
		a.logger.Info("growing rootfs online", "container", name, "blocks", want)
		return a.Containers.GrowRootfs(name, rootfs.Path, want)
	case cur.Stopped() || !cur.Found:
		a.logger.Info("growing rootfs offline", "container", name, "blocks", want)
		return growExt4Offline(rootfs.Path)
	}
	return nil
}

// growExt4Offline 扩大没有挂载的 ext4。resize2fs 要求先通过 e2fsck；e2fsck 退出码 1 表示修复了问题，可以继续。
func growExt4Offline(path string) error {
	var exit *exec.ExitError
	if out, err := exec.Command("e2fsck", "-f", "-p", path).CombinedOutput(); err != nil && //nolint:gosec // 路径来自 libvirt，不经 shell
		(!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return fmt.Errorf("e2fsck: %w: %s", err, bytes.TrimSpace(out))
	}
	if out, err := exec.Command("resize2fs", path).CombinedOutput(); err != nil { //nolint:gosec // 同上
		return fmt.Errorf("resize2fs: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
