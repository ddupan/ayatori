package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirtxml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/lxc"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
	"go.ddupan.top/ayatori/internal/compute/cloudinit"
	"go.ddupan.top/ayatori/internal/compute/naming"
)

// ContainerRuntime 是节点上的 LXC 操作。liblxc 没有 libvirt 那样的进程内 test 驱动，集成测试换成内存实现，
// 真实的容器生命周期在 Dev 环境验证。
type ContainerRuntime interface {
	Container(name string) (lxc.Container, error)
	Config(name string) (string, error)
	Define(name, config string) error
	Start(name string) error
	Remove(name string) error
	// GrowRootfs 在线把运行中容器的 ext4 rootfs 扩大到 blocks 个块。
	GrowRootfs(name, rootfs string, blocks uint64) error
}

// prepareContainer 把 rootfs 与 networks 渲染进原生配置（ADR-0015）。这些键归 Ayatori，config 中出现就拒绝，
// 不静默覆盖。bridge 把 LibvirtNetwork 的名字解析为节点上的 bridge。seed 不为空时把这个目录只读挂载为容器的
// NoCloud seed（ADR-0016）。
func prepareContainer(c *computev1alpha1.LXCContainer, rootfs Volume, seed string, bridge func(string) (string, error)) (string, error) {
	if owned := ownedConfigKeys(c.Spec.Config); len(owned) > 0 {
		return "", fmt.Errorf("config must not set %s: owned by Ayatori", strings.Join(owned, ", "))
	}
	path := rootfs.Path
	if !rootfs.Block {
		if rootfs.Format != formatRaw {
			return "", fmt.Errorf("rootfs volume must be raw, got %q", rootfs.Format)
		}
		path = "loop:" + path
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(c.Spec.Config, "\n"))
	fmt.Fprintf(&b, "\nlxc.uts.name = %s\nlxc.rootfs.path = %s\n", c.Name, path)
	if seed != "" {
		fmt.Fprintf(&b, "lxc.mount.entry = %s %s none bind,ro,create=dir 0 0\n", seed, seedMountPoint)
	}
	for i, n := range c.Spec.Networks {
		link, err := bridge(n.Network)
		if err != nil {
			return "", fmt.Errorf("network %s: %w", n.Network, err)
		}
		mac := n.MAC
		if mac == "" {
			mac = macAddress(string(c.UID), i)
		}
		fmt.Fprintf(&b, "lxc.net.%[1]d.type = veth\nlxc.net.%[1]d.link = %[2]s\nlxc.net.%[1]d.flags = up\n"+
			"lxc.net.%[1]d.hwaddr = %[3]s\nlxc.net.%[1]d.veth.pair = %[4]s\n", i, link, mac, naming.PortName(string(c.UID), i))
	}
	return b.String(), nil
}

// seedMountPoint 是 cloud-init 查找本地 NoCloud seed 的位置，相对容器的 rootfs。
const seedMountPoint = "var/lib/cloud/seed/nocloud"

// writeSeed 把容器的 NoCloud seed 写入 dir，内容未变时不重写。容器启动时 cloud-init 读取一次；之后的修改在
// 下次启动时可见，与 VM 经元数据服务读取的效果一致。
func writeSeed(dir string, c *computev1alpha1.LXCContainer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // 只读挂载给容器内的 cloud-init 读取
		return err
	}
	for name, content := range map[string]string{
		"meta-data": cloudinit.MetaData(string(c.UID), c.Name),
		"user-data": c.Spec.UserData,
	} {
		path := filepath.Join(dir, name)
		if have, err := os.ReadFile(path); err == nil && string(have) == content { //nolint:gosec // Agent 自己的状态目录
			continue
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil { //nolint:gosec // 同上
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// ownedConfigKeys 返回配置中归 Ayatori 的键。
func ownedConfigKeys(config string) []string {
	var owned []string
	for line := range strings.Lines(config) {
		key, _, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if ok && !strings.HasPrefix(key, "#") && (key == "lxc.uts.name" || key == "lxc.rootfs.path" || key == "lxc.net" || strings.HasPrefix(key, "lxc.net.")) {
			owned = append(owned, key)
		}
	}
	return owned
}

// networkBridge 从 LibvirtNetwork 在本节点的定义中查出 bridge（PoC 10）。
func (a *Agent) networkBridge(name string) (string, error) {
	n, err := a.hv.Network(name)
	if err != nil {
		return "", err
	}
	if !n.Found {
		return "", errors.New("network not defined on this node yet")
	}
	var x libvirtxml.Network
	if err := x.Unmarshal(n.XML); err != nil {
		return "", err
	}
	if x.Bridge == nil || x.Bridge.Name == "" {
		return "", errors.New("network has no bridge")
	}
	return x.Bridge.Name, nil
}

// applyContainer 下发配置，并在容器停止时按 runState 拉起。容器拿不到停止原因，不记录电源意图：
// 有意关机要等运行时操作接口（ADR-0016），到时由 Agent 先写标记再停止（PoC 08）。
// 配置以文件内容判断漂移（ADR-0015）。
func (a *Agent) applyContainer(c *computev1alpha1.LXCContainer) (computev1alpha1.LXCContainerStatus, error) {
	status := computev1alpha1.LXCContainerStatus{ObservedGeneration: c.Generation, NodeName: a.config.NodeName}
	fail := func(err error) (computev1alpha1.LXCContainerStatus, error) {
		status.Message = err.Error()
		return status, err
	}
	shouldRun := c.Spec.RunState != computev1alpha1.RunStateStopped
	a.metrics.shouldRun.WithLabelValues(c.Namespace, c.Name).Set(boolGauge(shouldRun))
	running := a.metrics.running.WithLabelValues(c.Namespace, c.Name)
	rootfs, err := a.resolveVolume(c.Spec.Rootfs)
	if err != nil {
		return fail(fmt.Errorf("rootfs %s: %w", c.Spec.Rootfs, err))
	}
	name := domainName(c.Namespace, c.Name)
	seed := ""
	if c.Spec.UserData != "" {
		seed = a.seedDir(name)
		if err := writeSeed(seed, c); err != nil {
			return fail(fmt.Errorf("cloud-init seed: %w", err))
		}
	} else if err := os.RemoveAll(a.seedDir(name)); err != nil {
		return fail(err)
	}
	want, err := prepareContainer(c, rootfs, seed, a.networkBridge)
	if err != nil {
		return fail(err)
	}
	have, err := a.Containers.Config(name)
	if err != nil {
		return fail(err)
	}
	if have != want {
		if err := a.Containers.Define(name, want); err != nil {
			return fail(err)
		}
		a.logger.Info("defined", "kind", kindContainer, "key", name)
	}
	cur, err := a.Containers.Container(name)
	if err != nil {
		return fail(err)
	}
	if err := a.growRootfs(name, rootfs, cur); err != nil {
		status.State = cur.State
		return fail(fmt.Errorf("grow rootfs: %w", err))
	}
	if cur.Stopped() && shouldRun {
		if err := a.Containers.Start(name); err != nil {
			status.State = cur.State
			running.Set(0)
			return fail(err)
		}
		a.logger.Info("started", "container", name)
		if cur, err = a.Containers.Container(name); err != nil {
			return fail(err)
		}
	}
	running.Set(boolGauge(cur.Running()))
	status.State = cur.State
	return status, nil
}

// removeContainer 停止并删除容器的配置与 seed；rootfs 卷不受影响，由卷自己的 reclaimPolicy 决定。
func (a *Agent) removeContainer(r state.Record) error {
	ns, name, _ := strings.Cut(r.Key, "/")
	if err := a.Containers.Remove(domainName(ns, name)); err != nil {
		return err
	}
	return os.RemoveAll(a.seedDir(domainName(ns, name)))
}

// seedDir 是容器的 NoCloud seed 所在目录。本地状态只读 *.json，子目录不会被当作记录。
func (a *Agent) seedDir(name string) string {
	return filepath.Join(a.config.StateDir, "seed", name)
}
