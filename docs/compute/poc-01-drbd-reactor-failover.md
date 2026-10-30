# PoC 01：drbd-reactor promoter 冷启动 libvirt VM 与 LXC

- 日期：2026-10-30
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 1 项
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
8. `poc-vm@.service` 用的是 `Type=oneshot` + `RemainAfterExit`，客户机自己崩溃时 promoter 感知
   不到。正式实现的单元需要跟踪 QEMU 进程，或者订阅 libvirt 事件。

## 未覆盖

ADR-0015 的其余待验证项，以及：

- 带磁盘的 tiebreaker 和 3 副本；
- 通过 LINSTOR 管理的资源；
- `suspend-io` + freeze；
- 内核挂死（宿主机没有掉电但不响应）；
- 客户机内部崩溃。
