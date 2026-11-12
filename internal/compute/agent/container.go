package agent

import (
	"errors"
	"fmt"
	"strings"

	"libvirt.org/go/libvirtxml"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/lxc"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
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
// 不静默覆盖。bridge 把 LibvirtNetwork 的名字解析为节点上的 bridge。
func prepareContainer(c *computev1alpha1.LXCContainer, rootfs Volume, bridge func(string) (string, error)) (string, error) {
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
	want, err := prepareContainer(c, rootfs, a.networkBridge)
	if err != nil {
		return fail(err)
	}
	name := domainName(c.Namespace, c.Name)
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

// removeContainer 停止并删除容器的配置；rootfs 卷不受影响，由卷自己的 reclaimPolicy 决定。
func (a *Agent) removeContainer(r state.Record) error {
	ns, name, _ := strings.Cut(r.Key, "/")
	return a.Containers.Remove(domainName(ns, name))
}
