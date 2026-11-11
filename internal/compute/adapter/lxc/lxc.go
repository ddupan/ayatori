// Package lxc 经 go-lxc 操作节点上的 LXC 容器（ADR-0015）。
package lxc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	golxc "github.com/lxc/go-lxc"
	"golang.org/x/sys/unix"
)

const (
	// ext4IocResizeFS 是 EXT4_IOC_RESIZE_FS，即 _IOW('f', 16, __u64)；x/sys 没有定义。
	ext4IocResizeFS = 0x40086610

	configFile      = "config"
	startTimeout    = 30 * time.Second
	shutdownTimeout = 30 * time.Second
)

// Client 操作 liblxc 默认路径下的容器。
type Client struct{ path string }

func New() *Client { return &Client{path: golxc.DefaultConfigPath()} }

// Container 是容器的现状；State 是小写的 liblxc 状态。
type Container struct {
	Found bool
	State string
}

// Running 与 libvirt 的 Domain.Running 一致：冻结也算在运行。
func (c Container) Running() bool { return c.State == "running" || c.State == "frozen" }

// Stopped 表示容器确实已停止；启动或停止途中都不是。
func (c Container) Stopped() bool { return c.State == "stopped" }

func (c *Client) Container(name string) (Container, error) {
	ct, err := golxc.NewContainer(name, c.path)
	if err != nil {
		return Container{}, err
	}
	defer func() { _ = ct.Release() }()
	if !ct.Defined() {
		return Container{}, nil
	}
	return Container{Found: true, State: strings.ToLower(ct.State().String())}, nil
}

// Config 返回节点上的配置文件内容；不存在时为空。文件内容就是漂移判断的依据。
func (c *Client) Config(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.path, name, configFile)) //nolint:gosec // 名字来自 Kubernetes 对象名，不含路径分隔符
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}

// Define 原子地写入配置。运行中的容器继续用旧配置，下次启动生效。
func (c *Client) Define(name, config string) error {
	dir := filepath.Join(c.path, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp := filepath.Join(dir, configFile+".tmp")
	if err := os.WriteFile(tmp, []byte(config), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, configFile))
}

// Start 启动容器并等待进入 running。失败的细节在容器目录的 lxc.log 中。
func (c *Client) Start(name string) error {
	ct, err := golxc.NewContainer(name, c.path)
	if err != nil {
		return err
	}
	defer func() { _ = ct.Release() }()
	log := filepath.Join(c.path, name, "lxc.log")
	if err := ct.SetLogFile(log); err != nil {
		return err
	}
	if err := ct.SetLogLevel(golxc.WARN); err != nil {
		return err
	}
	if err := ct.Start(); err != nil {
		return fmt.Errorf("%w (see %s)", err, log)
	}
	if !ct.Wait(golxc.RUNNING, startTimeout) {
		return fmt.Errorf("container did not reach RUNNING within %s (see %s)", startTimeout, log)
	}
	return nil
}

// GrowRootfs 在线扩大运行中容器的 ext4 rootfs（ADR-0019 §7）。loop 设备先刷新容量。文件系统只挂载在容器的 mount
// namespace 里，宿主机上的 resize2fs 看不到它已挂载，可能按离线方式改写；所以经 /proc/<pid>/root 打开容器的根目录，
// 直接发 resize2fs 在线时用的 ioctl。
func (c *Client) GrowRootfs(name, rootfs string, blocks uint64) error {
	ct, err := golxc.NewContainer(name, c.path)
	if err != nil {
		return err
	}
	defer func() { _ = ct.Release() }()
	pid := ct.InitPid()
	if pid <= 0 {
		return fmt.Errorf("container %s has no init process", name)
	}
	if err := refreshLoop(rootfs); err != nil {
		return err
	}
	root, err := os.Open(fmt.Sprintf("/proc/%d/root", pid)) //nolint:gosec // 路径由 init 的 pid 拼出
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, root.Fd(), ext4IocResizeFS, uintptr(unsafe.Pointer(&blocks))); errno != 0 { //nolint:gosec // ioctl 的参数必须是指针
		return fmt.Errorf("EXT4_IOC_RESIZE_FS: %w", errno)
	}
	return nil
}

// refreshLoop 让挂着 backing 文件的 loop 设备重新读取文件大小；块设备 rootfs 没有 loop，什么也不做。
func refreshLoop(backing string) error {
	files, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file) //nolint:gosec // 路径来自上面固定的 glob
		if err != nil || strings.TrimSpace(string(data)) != backing {
			continue
		}
		dev := filepath.Join("/dev", filepath.Base(filepath.Dir(filepath.Dir(file))))
		f, err := os.Open(dev) //nolint:gosec // 同上
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return unix.IoctlSetInt(int(f.Fd()), unix.LOOP_SET_CAPACITY, 0) //nolint:gosec // 文件描述符不会溢出 int
	}
	return nil
}

// Remove 停止容器并删除它的配置目录；容器不存在时什么也不做。不调用 liblxc 的 Destroy：它会连 rootfs
// 一起删，而 rootfs 是独立的卷，是否删除由卷的 reclaimPolicy 决定。
func (c *Client) Remove(name string) error {
	ct, err := golxc.NewContainer(name, c.path)
	if err != nil {
		return err
	}
	defer func() { _ = ct.Release() }()
	if ct.Defined() && ct.State() != golxc.STOPPED {
		if err := ct.Shutdown(shutdownTimeout); err != nil {
			if err := ct.Stop(); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(filepath.Join(c.path, name))
}
