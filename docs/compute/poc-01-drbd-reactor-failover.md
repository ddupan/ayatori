# PoC 01：drbd-reactor promoter 冷启动 libvirt VM 与 LXC

- 日期：2026-10-30
- 对应：[ADR-0018](../decisions/0018-linstor-drbd-storage-integration.md) 待验证第 1 项（原 ADR-0015 第 1 项）
- 结论：**通过**。四个场景中都没有出现两边同时写；网络分区和断电后能自动冷启动，旧节点恢复后
  不会抢回实例。

## 环境

在 PVE 上起了三台嵌套 VM（VMID 900–902，tag `poc`），每个 PVE 节点一台。宿主机侧不使用
`pve-rg`，不接入 PVE 的 LINSTOR。

| 节点 | PVE | 角色 | 规格 |
|---|---|---|---|
| drbd-poc-1 | pve1 | diskful，libvirt + LXC + drbd-reactor | 2 vCPU / 2.5 GiB，复制 10.60.0.71（labnet），管理 192.168.10.136 |
| drbd-poc-2 | pve2 | diskful，同上 | 同上，10.60.0.72，管理 192.168.10.137 |
| drbd-poc-3 | pve3 | diskless tiebreaker，不装 drbd-reactor | 1 vCPU / 1 GiB，10.60.0.73 |

- 系统 Ubuntu 24.04；`ppa:linbit/linbit-drbd9-stack` 提供 DRBD 9.3.4 和 drbd-reactor 1.12.0。
- 每个 diskful 节点用一块 4G 盘建 LVM，划出 `vm1`（1.5G）和 `ct1`（2G）两个 LV。直接用
  `drbdadm` 配置，不用 LINSTOR。
- DRBD 选项：`auto-promote no; quorum majority; on-no-quorum io-error;
  on-no-data-accessible io-error; rr-conflict retry-connect`。
- 管理网卡单独挂在 LAN 上，测试时只断开承载 DRBD 复制的 labnet 网卡，因此分区期间仍能观察旧
  节点的状态。

两种工作负载：

- **vm1**：CirrOS，写入 `/dev/drbd100`，domain XML 是 `type='block'` 的原生磁盘。两个 diskful
  节点都 `virsh define` 了同一份定义，autostart 关闭。用 `poc-vm@.service` 包装，
  `ExecStart=virsh start`，`ExecStop=virsh destroy`。
- **ct1**：Alpine 3.24 privileged 容器，rootfs 是 `/dev/drbd101` 上的 ext4。两个节点都有同一份
  lxc config。容器内每秒向 `/var/log/beat` 追加一行时间戳并 `sync`，用来检查数据连续性。

promoter 配置：

```toml
[[promoter]]
[promoter.resources.vm1]
start = ["poc-vm@vm1.service"]
on-drbd-demote-failure = "reboot-immediate"

[[promoter]]
[promoter.resources.ct1]
start = ["var-lib-lxc-ct1-rootfs.mount", "lxc@ct1.service"]
on-drbd-demote-failure = "reboot-immediate"
```

Ubuntu 的 `resource-agents-base` 不包含 `ocf:heartbeat:Filesystem`，所以改用手写的 systemd
`.mount` 单元，drbd-reactor 可以直接使用。

## 结果

| 场景 | 过程 | 结果 |
|---|---|---|
| A. 分区：断开活跃节点的复制网卡 | poc-1 运行两者，`qm set --net0 …,link_down=1` | poc-1 失去 quorum 后停止服务并 demote；poc-2 接管。从断网到运行：vm1 约 7 秒，ct1 约 11 秒。容器写入中断 18 秒 |
| A'. 恢复网卡 | 恢复 poc-1 的链路 | 12 秒内完成 resync；poc-1 作为 Secondary 回归，不启动任何东西，没有 split-brain |
| B. 断电：硬关闭活跃节点 | poc-2 运行两者，`qm stop 901` | poc-1 接管：ct1 约 10 秒，vm1 约 14 秒。容器写入中断 20 秒 |
| B'. 断电节点恢复 | `qm start 901` | poc-2 开机后，promoter 自动通过 `drbd@<res>.service` up 资源并作为 Secondary 回归，不抢回实例 |
| C. tiebreaker 已离线，两个 diskful 节点再分区 | 停 poc-3，再断 poc-1 的复制网卡 | 两边都失去 quorum，全部停止，没有一边继续运行。服务中断，但没有 split-brain |
| C'. 恢复链路 | 恢复 poc-1 的链路（poc-3 仍离线） | 两节点重新构成多数，10 秒内自动恢复。vm1 落在 poc-2，ct1 落在 poc-1 |

数据检查：

- 容器心跳日志的时间戳全程单调递增，只有上表列出的中断。旧节点断网后没有任何写入被持久化：
  场景 A 中最后一条来自 poc-1 的记录正好是断网时刻。
- 旧节点失去 quorum 后，ext4 收到 EIO，journal 中止，文件系统变为只读后被卸载。新 Primary 回放
  journal 后，`dumpe2fs` 显示 `clean`。

## 发现与对设计的影响

1. **单 Primary + quorum 足以防止双写，不需要 STONITH。** 这符合 ADR-0015 的判断：fencing 的
   粒度是单个卷。
2. **"实例在运行"不等于"实例是权威的"。** 场景 A 中，ct1 在旧节点上还显示 RUNNING 约 1 秒，此时
   新节点已经启动，但旧节点没有 quorum，写入只会得到 EIO。因此 Agent 回写 status 的"实际运行
   位置"应当以 **DRBD Primary 且有 quorum** 为准，不能以 `virsh domstate` 或 `lxc-info` 为准。
3. **每个 resource 独立竞争。** 场景 C' 中两个实例落在了不同节点。需要把相关实例放在一起，或者
   需要倾向某个节点时，由 Agent 生成 `preferred-nodes`。
4. **tiebreaker 和 diskful 节点同时出故障就会全停。** 这是 quorum 的预期代价。重要实例可以用 3 个
   diskful 副本，或者把 tiebreaker 放在故障域独立的节点上。
5. **`io-error` 会让客户机看到 EIO。** ext4 能正确处理（journal 中止、只读、卸载），新节点能干净
   接管。`suspend-io` + freeze 可以避免客户机看到错误，但前提条件更多（cgroup freeze、
   `on-suspended-primary-outdated force-secondary` 等），本次没有测试，留作 VMCP 方向的后续项。
6. **接管时间**：从故障发生到实例开始运行，大约 7–14 秒。应用实际中断时间还要加上客户机启动，
   本次容器约 18–20 秒。
7. **promoter 会生成 `drbd@<res>.service` 依赖**，节点开机后自动 up 资源，不需要启用
   `drbd.service`。将来 Agent 只需生成 promoter 配置，不必自己管理资源的 up/down。
8. 实例自身崩溃的处理见下一节。

## 实例崩溃

promoter 文档明确写着它"完全不做服务监控"。崩溃分三类，最终验证结果如下：

| 崩溃类型 | 谁处理 | 实测 |
|---|---|---|
| 客户机内核 panic | libvirt：`<on_crash>restart</on_crash>` + `<panic model='isa'/>` | 发出 `Crashed Panicked` 事件后约 30 ms 原地复位，QEMU 进程不变；客户机 22 秒后恢复 |
| 客户机卡死但不 panic | QEMU：`<watchdog model='i6300esb' action='reset'/>` | 启用 watchdog 后不再喂狗，31 秒（超时 30 秒）后发出 `watchdog: reset` 事件并原地复位；41 秒后恢复 |
| QEMU 进程或容器 init 死亡 | 节点上的监管进程原地重启，次数耗尽后 `OnFailure=` 执行 evict | 前两次崩溃分别在 3 秒、6 秒内原地重启；第三次在 13 秒内换到另一节点 |

客户机 panic 和卡死在 QEMU 内部完成复位，监管进程和 DRBD 都不会察觉。domain 的状态会显示
`running (crashed)`，status 回写时不应把它当成故障。

### 过程中排除的方案

1. **oneshot 单元**（`Type=oneshot` + `RemainAfterExit`）：`kill -9` QEMU 后，单元和 target 仍是
   `active`，DRBD 保持 Primary，实例**无声地停着**。`lxc@ct1`（`lxc-start -F`）虽然能变成
   `failed`，但 target 和挂载不受影响，同样没有恢复。
2. **`dependencies-as`/`target-as = "BindsTo"`**：同一种崩溃，有时原地重启，有时直接换节点，
   取决于 systemd job 的排队顺序。有一次 target 停止后连带停止了 `drbd-promote@`，stop job 取消
   了自动重启；另一次 `drbd-promote@` 没有停止，重启耗尽后 DRBD 卡在 Primary，无法切换。
   **结果不确定，不能采用**。保留默认的 `Requires`。
3. **在同一个单元上同时设置 `Restart=` 和 `OnFailure=`**（systemd 255）：第一次失败就触发
   `OnFailure=`，不会进行原地重启。VM 和 LXC 都复现了这一点（容器单次崩溃 3 秒后就被 evict）。
   因此重启计数不能交给 systemd。

### 采用的方案

- promoter 启动的单元是一个**监管进程**（`Type=simple`，不设置 `Restart=`）：
  - 启动实例后一直跟踪它；实例死亡后在本机重启，并按滑动窗口计数（PoC 中为 120 秒内 3 次）；
  - 次数耗尽后以非零值退出，单元进入 `failed`，`OnFailure=` 在节点本地执行
    `drbd-reactorctl evict <实例>`，并对单元执行 `reset-failed`，方便以后再次落回这个节点。
- 整条链路只用到节点本地的组件，不依赖 Agent 和控制面。
- 正式实现中监管进程订阅 libvirt lifecycle 事件，不轮询；LXC 由监管进程循环执行
  `lxc-start -F`（PoC 只验证了 VM 版，LXC 只验证了反例 3）。
- `evict` 按配置文件名查找，Agent 为**每个实例生成一个 promoter 配置文件**。

### 有意关机不重启

- 最初的监管进程对实例的**任何**停止都会重启，包括客户机内主动关机，这与 vSphere HA 和 KubeVirt
  `RerunOnFailure` 的语义不符。
- 改为只在 libvirt 原因为 `crashed`/`failed` 时重启；`shutdown`（客户机关机）和 `destroyed`
  （运维操作）以 0 退出。`ExecStop=` 加 `-` 前缀，避免对已停止的 domain 执行 `destroy` 时把单元
  标记为失败。
- 结果：客户机内 `poweroff` 后 vm1 保持 `shut off (shutdown)`，单元为 `inactive`，没有触发
  evict；`kill -9` QEMU 仍在 3 秒内原地重启。
- 未解决：有意关机后该节点仍持有 Primary。如果这个节点随后故障，接管节点会把实例重新开机。电源
  意图需要写在实例自己的 DRBD 资源上（ADR-0015 的实例状态卷），尚未验证。

### 其他发现

- **domain XML 必须启用 ACPI**（`<features><acpi/><apic/></features>`）。PoC 最初没写，客户机
  没有 ACPI，ISA pvpanic 设备无法被发现，panic 后宿主机收不到通知，客户机直到 `kernel.panic`
  计时（120 秒）后才自己重启。这属于 XML 的错误，不是 libvirt 的缺口。
- 客户机内需要在启动时加载 `pvpanic-mmio` 和 `i6300esb`，并有程序喂狗。
- 不写 `<uuid>` 时，各节点 define 出的 UUID 不同，再次 define 会报"domain 已存在"。UUID 必须
  由 Ayatori 统一填写（ADR-0015 已有此规则）。
- PoC 的客户机从 CirrOS 换成了 Alpine NoCloud 镜像：CirrOS 在没有元数据服务的环境里启动约需
  10 分钟，期间 SSH 不可用。

## 未覆盖

ADR-0015 的其余待验证项，以及：

- 带磁盘的 tiebreaker 和 3 副本；
- 通过 LINSTOR 管理的资源；
- `suspend-io` + freeze；
- 宿主机内核挂死（没有掉电但不响应）；
- LXC 版监管进程。
