# PoC 11：与 kubelet 共用宿主机

- 日期：2026-10-31
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 6 项（kubelet 部分；网络
  机制部分见 [PoC 03](poc-03-state-volume-and-bridge-netfilter.md)）
- 环境：poc-1 上单节点 k0s v1.36.4+k0s.1（官方二进制，核对 sha256 后由维护者安装），默认配置：CNI 为
  kube-router，kube-proxy 为 iptables 模式。同节点上运行 vm1、ct1（uefi 在 poc-2）。
- 结论：**可以共存，但 kubelet 必须配置 `systemReserved` 并强制执行，否则一个 Pod 就能让整台宿主机
  失去响应**，上面的实例随之卡住，而且不会触发冷切换。

## 网络

vm1 与 ct1 在 poc-1 上测三种流量：同一 bridge（`br-lan`）内的二层流量，跨 bridge（virbr0 → lxcbr0）
经宿主机 FORWARD 的路由流量，经 NAT 出网。

| 状态 | `br_netfilter` | `bridge-nf-call-iptables` | FORWARD 策略 | 三种流量 |
|---|---|---|---|---|
| 未装 k0s | 未加载 | — | ACCEPT | 全通 |
| k0s 运行中 | 已加载 | 1 | ACCEPT | 全通 |
| 再加 default namespace 的 default-deny NetworkPolicy | 已加载 | 1 | ACCEPT | 全通；Pod 出网被拒 |

- k0s 默认的 kube-router 不改 FORWARD 默认策略，NetworkPolicy 只作用于 Pod 地址。PoC 03 的风险
  （FORWARD 为 DROP 时实例 bridge 流量全断）取决于 CNI，换成 Calico 等或宿主机上装 Docker 时要重测。

## cgroup 与 OOM 优先级

- QEMU 在 `machine.slice/machine-qemu-*.scope`，LXC 在 `lxc.payload.<名字>`，Pod 在 `kubepods`。三者
  互不嵌套，kubelet 不管理前两者。
- k0s 默认使用 cgroupfs 驱动（`/sys/fs/cgroup/kubepods`，不是 `kubepods.slice`）。ADR-0015 要求统一
  使用 systemd 驱动，实施时需要在 k0s 配置中改掉。
- `oom_score_adj`：QEMU 与容器 init 为 0，BestEffort Pod 为 1000。

## 内存压力

BestEffort Pod 不断分配内存。

**不设预留**（allocatable 只扣 100Mi 驱逐阈值，kubepods 的 `memory.max` 约等于整机内存）：

- 第一次用内存型 emptyDir（tmpfs）写 3.5 GiB，第二次用匿名内存。两次都在可用内存降到几百 MiB 后
  **整台宿主机失去响应**，持续数分钟，kubelet 的驱逐和内核 OOM 都没能及时恢复。tmpfs 页不会因杀掉
  进程而释放，只能等驱逐删除 emptyDir。
- 卡住期间：
  - 宿主机内核仍回应 ping，DRBD 连接正常，对端看到的仍是 Primary，**不会发生冷切换**；
  - 实例随宿主机一起卡住；
  - 告警正常触发：两个 exporter 的 `ExporterDown` 与 `InstanceDown`（PoC 06 的规则）。
- 从 PVE 硬重置 poc-1 后，poc-2 约 10 秒内接管全部三个实例。

**`--system-reserved=memory=2Gi`**（kubelet 默认 `enforceNodeAllocatable: [pods]`）：

- allocatable 降为约 2.2 GiB，kubepods 的 `memory.max` 被设为约 2.3 GiB。
- 同样的 Pod：kubepods 在约 2.3 GiB 处封顶，kubelet 约 2 分钟后将其驱逐（`Evicted`）。整个过程中节点
  一直有响应，vm1 与 ct1 持续运行，客户机内正常。

结论：静态切分不是可选项。`systemReserved`（或 `kubeReserved`）必须覆盖 VM/LXC 的全部内存与宿主机
自身开销，并保持 `enforceNodeAllocatable` 包含 `pods`，这样 Pod 的内存压力被限制在 kubepods 内。

## 其他发现

### 节点本地的启动失败不应让实例停在原地

poc-1 重启后，PoC 中用 `ip link` 临时建的 `br-lan` 不存在了，network `lan` 无法启动。vm1 与 ct1 被
evict 到 poc-1 后：

- 都报启动失败；
- 按"启动失败不 evict"的规则（PoC 02），资源停在 poc-1 上，持有 Primary 却没有运行；
- 只有 `InstanceDown` 告警。

这类失败只发生在这一台节点上，换个节点就能启动。建议改为：启动失败时执行
`drbd-reactorctl evict --keep-masked`，本节点对该实例保持屏蔽，由下一个节点尝试；所有候选节点都失败时
全部屏蔽，实例停机并告警。这样不会来回弹跳（PoC 02 担心的情况），节点本地问题也能自动绕开。屏蔽由
Agent 在节点状态恢复后解除。尚未验证。

### 宿主机"活着但不响应"时没有自动切换

这是 PoC 01 列为未覆盖的情况，这次因内存耗尽实际出现。kernel 与 DRBD 正常工作，quorum 不受影响，
drbd-reactor 也不做服务健康检查，所以实例会一直卡在这台宿主机上，直到人工处理。可以考虑：

- 宿主机级 watchdog：systemd `RuntimeWatchdogSec` 只能覆盖 PID 1 卡死，这次 PID 1 未必卡住；
- Agent 自检，失败时主动 demote 或重启宿主机（自我 fencing），对应 vSphere 的 host isolation response；
- 先依赖告警与人工处理。

静态切分已经消除了这次的触发原因；是否需要自我 fencing，留待出现其他触发原因时再评估。

## 未覆盖

- CPU 切分（`reservedSystemCPUs` 与 libvirt `<vcpupin>`/`<emulatorpin>`）；
- k0s 改用 systemd cgroup 驱动；
- 其他 CNI（Calico 等）与宿主机上的 Docker。
