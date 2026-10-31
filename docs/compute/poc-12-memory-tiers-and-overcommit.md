# PoC 12：内存分层与超售

- 日期：2026-10-31
- 对应：ADR-0015"与 kubelet 共用宿主机"一节的内存模型（取代 [PoC 11](poc-11-kubelet-coexistence.md) 的静态切分）
- 环境：poc-1（4.3 GiB 内存，无 swap），单节点 k0s v1.36.4+k0s.1，vm1（Alpine，内存 1 GiB）与 ct1。
- 结论：
  - **动态上限可行**：Pod 作为弹性层放进 `k8s.slice`，由 Agent 按实例保障量动态设置 `MemoryMax`。Pod 超用时只在
    这一层内被 OOM，节点和实例不受影响。
  - **free page reporting 有效**：客户机释放的内存能还给宿主机。
  - **systemd-oomd 在无 swap 时救不了节点**：不能依赖它代替硬上限。

## free page reporting

客户机内向 tmpfs 写 400 MiB 后删除，测宿主机侧 QEMU 的 RSS：

| `<memballoon>` | 空闲 | 写入后 | 删除 15 秒后 |
|---|---|---|---|
| `model='virtio'`（默认） | 268M | 640M | 640M |
| `model='virtio' freePageReporting='on'` | 252M | 632M | **252M** |

客户机内核为 Alpine `6.12-virt`，QEMU 命令行中 `virtio-balloon-pci` 带 `free-page-reporting: true`。

## 动态上限

### 布置

- `k8s.slice`（systemd unit）：开启 CPU/内存/任务/IO 统计，`AllowedCPUs` 使 cpuset 控制器可用；kubelet 以
  `--cgroup-root=/k8s.slice` 运行，`--system-reserved=memory=512Mi` 只覆盖宿主机自身，allocatable 约 3.7 GiB。
- 实例的保障量由"Agent"写入 cgroup：
  - vm1：配置量 1 GiB，`memory.low` = 768M（超售）；`machine.slice` 的 `MemoryLow=1G`，因为 `memory.low` 需要祖先也有
    足够的保护值才生效；
  - ct1：`memory.max` = 512M，`memory.low` = 256M。
- `k8s.slice` 的 `MemoryMax` = 4413M − 宿主机 512M − 保障量之和 1024M ≈ 2877M，用 `systemctl set-property`
  设置（耗时 59 ms）。

### 结果

| 步骤 | 结果 |
|---|---|
| Pod `steady` 占用 1.2 GiB 后保持 | 正常运行 |
| Pod `hog` 无限增长 | `k8s.slice` 到达约 2.86 GiB 时，cgroup 内 OOM 以 `oom_group_kill` 杀掉 hog（`oom_score_adj` 1000）；steady 存活；**节点全程有响应**，vm1、ct1 正常 |
| 把上限压到 1100M（模拟一台 VM 冷切换过来） | 约 3 秒后 steady 被 OOM 杀掉；k0s 的系统 Pod 存活；实例正常 |
| 去掉上限（`MemoryMax=infinity`），只靠 systemd-oomd（`ManagedOOMMemoryPressure=kill`，阈值 40%，持续 10 秒） | `k8s.slice` 的 PSI 一直为 0，可用内存降到 99M 后**节点失去响应**，oomd 没有动作，最后从 PVE 硬重置；poc-2 接管全部实例 |

## 发现

- 硬上限是必需的，systemd-oomd 只能作补充。没有 swap 时，内存从"够用"到"整机抖动"几乎没有渐进的压力上升，PSI
  驱动的 oomd 来不及介入；oomd 启动时自己也提示 `No swap; memory pressure usage will be degraded`。
- `systemctl set-property --runtime` 设置的上限在重启后丢失。Agent 必须在 kubelet 启动前设好上限（或在 slice 单元
  中写一个保守的默认值），之后随本节点实例的启停调整。
- `memory.low` 在本次测试中没有被实际考验：硬上限让 Pod 在自己的 cgroup 内就被处理掉了，内核从未需要回收实例的
  内存。它的作用是在实例自身超售、需要回收超出保障量的部分时，保护保障量以内的内存；这需要 swap/zswap 才有意义。
- libvirt 的 `<memtune>` 在 QEMU 下没有 `min_guarantee`，`memory.low` 只能由 Agent 在实例启动后写入其 scope。

## 未覆盖

- 实例超出保障量部分的回收：宿主机 swap/zswap、KSM；
- k8s 调度器按偏大的 allocatable 放置 Pod 后的长期行为（Pod 在上限内被 OOM 的频率）；
- CPU 的权重与绑定。
