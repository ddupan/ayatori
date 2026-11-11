package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
)

// sysRoot 是 sysfs 的挂载点。
const sysRoot = "/sys"

// readUint 读只含一个数字的文件；读不到或不是数字（例如 memory.max 的 "max"）时 ok 为 false。
func readUint(path string) (uint64, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // 路径由 Agent 按容器名拼出
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(string(bytes.TrimSpace(data)), 10, 64)
	return v, err == nil
}

// keyedValue 在 "key value" 形式的文件（cpu.stat、memory.stat）中取一个值。
func keyedValue(path, key string) (uint64, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // 路径由 Agent 按容器名拼出
	if err != nil {
		return 0, false
	}
	for line := range strings.Lines(string(data)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == key {
			n, err := strconv.ParseUint(v, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// containerStats 从 cgroup v2 与网卡统计读取容器的用量（ADR-0019 §6），语义对齐 VM 的指标：memory 的 current
// 是 cgroup 记账的内存（含页缓存），maximum 是 memory.max（无限制时缺项），rss 是匿名内存。容器不在运行时
// cgroup 不存在，ok 为 false。
func containerStats(sys, name string, ports []string) (libvirt.DomainStats, bool) {
	dir := filepath.Join(sys, "fs", "cgroup", "lxc.payload."+name)
	usec, ok := keyedValue(filepath.Join(dir, "cpu.stat"), "usage_usec")
	if !ok {
		return libvirt.DomainStats{}, false
	}
	s := libvirt.DomainStats{Name: name, CPUTime: usec * 1000, Memory: map[string]uint64{}}
	if v, ok := readUint(filepath.Join(dir, "memory.current")); ok {
		s.Memory["current"] = v
	}
	if v, ok := readUint(filepath.Join(dir, "memory.max")); ok {
		s.Memory["maximum"] = v
	}
	if v, ok := keyedValue(filepath.Join(dir, "memory.stat"), "anon"); ok {
		s.Memory["rss"] = v
	}
	s.Disks = containerDisks(sys, filepath.Join(dir, "io.stat"))
	for _, port := range ports {
		stats := filepath.Join(sys, "class", "net", port, "statistics")
		// 宿主机侧的 veth：它发送的是容器收到的。
		rx, rxOK := readUint(filepath.Join(stats, "tx_bytes"))
		tx, txOK := readUint(filepath.Join(stats, "rx_bytes"))
		if rxOK && txOK {
			s.Nics = append(s.Nics, libvirt.IOStats{Device: port, In: rx, Out: tx})
		}
	}
	return s, true
}

// containerDisks 解析 io.stat，例如 "7:0 rbytes=1 wbytes=2 rios=3 ..."；设备名取自 /sys/dev/block。
func containerDisks(sys, path string) []libvirt.IOStats {
	f, err := os.Open(path) //nolint:gosec // 路径由 Agent 按容器名拼出
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var disks []libvirt.IOStats
	for sc := bufio.NewScanner(f); sc.Scan(); {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		io := libvirt.IOStats{Device: fields[0]}
		if target, err := os.Readlink(filepath.Join(sys, "dev", "block", fields[0])); err == nil {
			io.Device = filepath.Base(target)
		}
		for _, field := range fields[1:] {
			k, v, _ := strings.Cut(field, "=")
			n, _ := strconv.ParseUint(v, 10, 64)
			switch k {
			case "rbytes":
				io.In = n
			case "wbytes":
				io.Out = n
			}
		}
		disks = append(disks, io)
	}
	return disks
}

// collectContainerStats 读取本 Agent 管理的运行中容器的用量。
func (a *Agent) collectContainerStats(seen map[objectID]bool) []instanceStats {
	records := a.store.List(kindContainer)
	out := make([]instanceStats, 0, len(records))
	for _, r := range records {
		var c computev1alpha1.LXCContainer
		if err := json.Unmarshal(r.Object, &c); err != nil || r.Deleted || !seen[objectID{kindContainer, c.Namespace, c.Name}] {
			continue
		}
		ports := make([]string, len(c.Spec.Networks))
		for i := range c.Spec.Networks {
			ports[i] = portName(string(c.UID), i)
		}
		if s, ok := containerStats(sysRoot, domainName(c.Namespace, c.Name), ports); ok {
			out = append(out, instanceStats{c.Namespace, c.Name, s})
		}
	}
	return out
}
