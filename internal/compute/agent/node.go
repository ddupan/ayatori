package agent

import (
	"context"
	"maps"
	"runtime"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"libvirt.org/go/libvirtxml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.ddupan.top/ayatori/internal/infra/node"
)

// Role 是计算节点的角色（ADR-0022 的节点角色模型）：Node label node-role.kubernetes.io/compute，
// 控制面按它选择计算节点。
const Role = "compute"

// nfd 是 node-feature-discovery 的 label 前缀，CPU 的 label 沿用它的命名（ADR-0019 §3）。
const nfd = "feature.node.kubernetes.io/"

type nodeFacts struct {
	ready        bool
	cpus, memory int64
	// cpuLabels 是最近一次读到的 CPU label。libvirt 不可用时保留：CPU 不会在 Agent 运行期间改变，
	// 移除 label 只会让放置结果抖动。
	cpuLabels map[string]string
}

// nodeFacts 由主循环在每轮收敛后调用并发布，heartbeat 只读发布的结果，不碰 libvirt 连接。
func (a *Agent) nodeFacts() nodeFacts {
	f := nodeFacts{cpuLabels: a.facts.Load().cpuLabels}
	if !a.connected || a.hv == nil {
		return f
	}
	cpus, memory, err := a.hv.NodeInfo()
	if err != nil {
		return f
	}
	cpu, err := a.hv.HostCPU()
	if err != nil {
		return f
	}
	labels := cpuLabels(cpu)
	levels, err := cpuLevelLabels(a.hv.CPUProvides)
	if err != nil {
		a.logger.Error("compare CPU levels", "error", err)
		levels = maps.Clone(f.cpuLabels)
		maps.DeleteFunc(levels, func(k, _ string) bool { return !strings.HasPrefix(k, cpuLevelPrefix) })
	}
	maps.Copy(labels, levels)
	return nodeFacts{ready: true, cpus: cpus, memory: memory, cpuLabels: labels}
}

// cpuLevelPrefix 加档位名是节点能提供该档 CPU 的 label，值为 "true"（ADR-0019 §3）。
const cpuLevelPrefix = "compute.ayatori.ddupan.top/x86-64-"

// cpuLevels 是 x86-64 psABI 的微架构档位，后一档包含前一档，特性名是 libvirt 的写法。
var cpuLevels = []struct {
	name     string
	features []string
}{
	{"v2", []string{"cx16", "lahf_lm", "popcnt", "pni", "ssse3", "sse4.1", "sse4.2"}},
	{"v3", []string{"avx", "avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe", "xsave"}},
	{"v4", []string{"avx512f", "avx512bw", "avx512cd", "avx512dq", "avx512vl"}},
}

// cpuLevelLabels 从低到高询问 hypervisor，到第一个不能提供的档位为止。用 libvirt 实际的比较而不是 CPU
// 型号推断：型号相同时，微码或 BIOS 也可能关掉部分特性。基础型号是 kvm64：qemu64 带 AMD 专有特性，Intel
// 节点上不兼容。
func cpuLevelLabels(provides func(xml string) (bool, error)) (map[string]string, error) {
	l := map[string]string{}
	cpu := libvirtxml.DomainCPU{Mode: "custom", Match: "exact", Model: &libvirtxml.DomainCPUModel{Fallback: "forbid", Value: "kvm64"}}
	for _, level := range cpuLevels {
		for _, name := range level.features {
			cpu.Features = append(cpu.Features, libvirtxml.DomainCPUFeature{Policy: "require", Name: name})
		}
		xml, err := cpu.Marshal()
		if err != nil {
			return nil, err
		}
		ok, err := provides(xml)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		l[cpuLevelPrefix+level.name] = "true"
	}
	return l, nil
}

// cpuLabels 把 capabilities 中的 CPU 厂商与 CPUID 签名换成 node-feature-discovery 的 label，值与 NFD
// 一致：libvirt 的 vendor 已是 Intel/AMD，family 与 model 是十进制。
func cpuLabels(cpu libvirtxml.CapsHostCPU) map[string]string {
	l := map[string]string{}
	if cpu.Vendor != "" {
		l[nfd+"cpu-model.vendor_id"] = cpu.Vendor
	}
	if cpu.Signature != nil {
		l[nfd+"cpu-model.family"] = strconv.Itoa(cpu.Signature.Family)
		l[nfd+"cpu-model.id"] = strconv.Itoa(cpu.Signature.Model)
	}
	return l
}

// labels 是 Agent 决定的 Node label（ADR-0019 §4）：从节点事实发现的，再叠加配置中的 nodeLabels。
// 与 libvirt 是否可用无关，不就绪时也照常写入，以免 label 随之消失。
func (a *Agent) labels(f nodeFacts) map[string]string {
	l := map[string]string{
		corev1.LabelHostname:   a.config.NodeName,
		corev1.LabelArchStable: runtime.GOARCH,
		corev1.LabelOSStable:   runtime.GOOS,
	}
	maps.Copy(l, f.cpuLabels)
	maps.Copy(l, a.config.NodeLabels)
	return l
}

// heartbeat 续约 <node>-compute Lease，并写入 Node 中属于 compute 的部分：角色 label 与其他 label、
// compute.ayatori.ddupan.top/Ready，以及容量。capacity 是整机，allocatable 扣除宿主机预留（ADR-0015 资源模型）。
func (a *Agent) heartbeat(ctx context.Context, kube client.Client) {
	r := &node.Registrar{Client: kube, Node: a.config.NodeName, Role: Role}
	r.Run(ctx, func() node.Status {
		f := *a.facts.Load()
		if !f.ready {
			return node.Status{Reason: "LibvirtUnavailable", Labels: a.labels(f)}
		}
		memory := resource.NewQuantity(f.memory, resource.BinarySI)
		free := memory.DeepCopy()
		free.Sub(a.config.HostReservedMemory)
		cpus := *resource.NewQuantity(f.cpus, resource.DecimalSI)
		return node.Status{
			Ready:       true,
			Reason:      "AgentReady",
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: cpus, corev1.ResourceMemory: *memory},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: cpus, corev1.ResourceMemory: free},
			Labels:      a.labels(f),
		}
	}, func(msg string, err error) { a.logger.Debug(msg, "error", err) })
}
