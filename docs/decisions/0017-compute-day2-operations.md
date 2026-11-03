# ADR-0017：计算后端的日常运维：存储失效、备份接入、放置、维护与迁入

- 状态：Proposed（含待维护者确认的问题，见文末）
- 日期：2026-11-03
- 依据：[对照 vSphere 功能集的设计缺口](../compute/vsphere-gap-analysis.md)；建立在
  [ADR-0015](0015-libvirt-lxc-compute-backend.md)、[ADR-0016](0016-instance-node-services.md) 之上。

## 问题

ADR-0015/0016 解决了实例的创建、冷启动、身份和控制台，但对照 vSphere 后，以下有现状证据的需求
还没有设计：存储暂时不可用时的处理、备份、宿主机维护与计划内迁移、异构 CPU、放置与反亲和、
故障切换容量、与现有 PVE 告警对等的监控、磁盘扩容、从 PVE 迁入现有客户机，以及镜像
和介质分发。证据见缺口分析。

本 ADR 只补这些缺口，不改变 ADR-0015 的分层：配置在中央，故障判定和启动在节点，防双开靠 DRBD。

## 决策

### 1. 存储暂时不可用：冻结而不是报错（VMCP）

2026-09-25 的事故中，新卷初始同步期间多个 DRBD 资源 quorum 反复丢失又恢复，`on-no-quorum io-error`
让客户机的 ext4 中止 journal 并进入只读，quorum 恢复后实例仍然坏着，只能人工修复。ADR-0015 沿用了
同样的参数，换掉 PVE 后同样的抖动会造成同样的结果。

- **受保护实例改用 drbd-reactor 的 `on-quorum-loss = "freeze"`**，对应 vSphere VMCP 对 APD 的处理：
  - 失去 quorum 时冻结实例的 cgroup，客户机只看到一次停顿，不会看到 EIO；
  - quorum 恢复且没有其他节点接管时解冻，实例继续运行；
  - 其他分区已经取得 quorum 并接管时，旧 Primary 被 `force-secondary`，服务停止，与现在的冷切换一致。
- 相应的 DRBD 参数改为 `on-no-quorum suspend-io`、`on-no-data-accessible suspend-io`、
  `on-suspended-primary-outdated force-secondary`，net 选项 `rr-conflict retry-connect`。这是
  drbd-reactor 文档列出的前提，此外需要 cgroup v2，Ubuntu 24.04 满足。
- ADR-0015 中 `on-no-quorum io-error` 的参数随之修订。PoC 01 只验证了 `io-error`，`freeze` 尚未测试。
- **限制初始同步与重同步的带宽**：事故的触发因素是两块新卷同时初始同步。在 VM 卷使用的 LINSTOR
  resource group 上设置 DRBD 的重同步速率上限（`c-max-rate` 等），并调查新建空卷能否跳过初始同步。

### 2. 备份：接入备份系统，Ayatori 只提供集成点

PVE 上的 9 个客户机和笔记本上的 dc1 目前都没有任何备份。

- **快照不是备份**：快照和数据在同一套存储上，存储坏了快照一起坏，也没有保留策略、异地副本和
  目录。vSphere 同样不自己做备份，而是提供 VADP（一致点、磁盘读取、changed block tracking），由
  Veeam 等第三方产品完成备份。本后端照此分工：
  - **备份系统**负责调度、保留策略、去重、加密、异地副本、目录和恢复；Ayatori 不保存备份数据；
  - **Ayatori 提供集成点**，对应 VADP。
- **分层**：
  - 有应用级备份的服务（PostgreSQL 的 pgBackRest、etcd 快照、OpenBao 快照）继续以应用级备份为准；
  - **VM** 在宿主机侧做镜像级备份，经下文的集成点；
  - **LXC** 是完整的 OS，按物理机的方式在容器内运行备份系统的 agent 做文件级备份。宿主机侧拿不到
    容器 rootfs 的一致时间点：thick LVM 不能快照，冻结容器读完整个卷的停顿又太长。
- **VM 的集成点**：
  - 一致点：有 qemu-guest-agent 的 VM 先 fsfreeze，其他 VM 是崩溃一致的；
  - 读取：libvirt 的 pull 模式备份（`virDomainBackupBegin`），QEMU 把时间点镜像经 NBD 导出给备份
    系统读取，不依赖底层存储能否快照；
  - 增量：libvirt checkpoint（QEMU dirty bitmap），对应 CBT。bitmap 的持久化依赖 qcow2，DRBD 上的
    raw 块设备只能用非持久 bitmap，QEMU 重启后需要一次全量，待验证；
  - 恢复：备份系统写入一个新卷，用 §9 的接管方式让 domain 引用它。
- **备份系统的选型待调研**，候选需要支持 libvirt/QEMU 的增量备份接口和 Linux/Windows 的客户机 agent。
  集成点的 API 形态（由谁触发、如何授权、在哪个节点读取）取决于选定的产品，选型后再定。
- 告警按实例的最近一次成功备份时间，替代现在的 `pve_not_backed_up_total`，数据来自备份系统。

### 3. 宿主机维护与计划内迁移

节点经常整台重装（2026-07-25 三台都经 PXE 重装过），升级需要逐台重启，roadmap 的 Laptop Rebuild
验收场景也要求节点能排空和退出。

- **沿用 Node API 的语义**（ADR-0007）：
  - `kubectl cordon`（`spec.unschedulable`）：controller 不再把新实例放到该节点；
  - **维护**：给 Node 打 taint `compute.ayatori.ddupan.top/maintenance:NoExecute`，Agent 据此排空本节点：
    - DRBD 上的受保护实例：`drbd-reactorctl evict --keep-masked`，由其他候选节点接管；
    - 本地存储实例无法迁走：正常关机，**不写**电源意图标记，宿主机重启后由 Agent 按 `runState`
      拉起；
  - 排空完成后，Agent 在 Node 上设置 condition `SafeToRemove=True`（roadmap 已有的名字），运维再
    重启或重装；
  - 去掉 taint 后，Agent 执行 `--unmask`。
- **计划内冷迁移**：修改 `spec.nodeName`（§5）后 evict 一次。
- **冷迁移有停机**：等于客户机关机加启动。这对 VyOS 意味着网关中断，是否需要例外见文末待决问题 2。
- 宿主机的升级本身仍由 Chef/apt 负责，Ayatori 只提供排空和 `SafeToRemove`。不做 Lifecycle Manager
  式的滚动编排，逐台执行写进 runbook。

### 4. 异构 CPU（EVC）

pve1 是 Intel，pve2/pve3 是 AMD。VM 100 用 `host` CPU 在线迁移失败，改成 `x86-64-v2-AES` 后才正常。冷
切换虽然不要求 CPU 相同，但客户机会在下次启动时看到另一颗 CPU：Linux 不受影响，Windows 可能要求
重新激活，retro 系统对 CPUID 更敏感。

- Agent 把宿主机的 CPU 厂商和型号写进 Node label，例如 `compute.ayatori.ddupan.top/cpu-vendor`，
  取自 libvirt capabilities。
- domain 写了具名 CPU 型号时，Agent 在下发前用 `virConnectCompareHypervisorCPU` 检查本节点能否提供；
  不能提供就写入该节点的 status，controller 将其排除出候选节点。
- domain 使用 `host-passthrough`/`host-model`，而候选节点的 CPU 型号不一致时，controller 设置一个
  warning condition，但不拒绝，保持 ADR-0015 不限制原生配置的原则。示例和迁入工具默认使用具名型号
  （`x86-64-v2-AES` 已在 PVE 上验证过）。

### 5. 放置与反亲和

LB 每个环境两台分在不同节点，etcd 每个节点一个，sandbox1/2 分开，目前都靠 inventory 手工保证。
应用自带冗余不等于不用共享存储：成员放在 DRBD 上，宿主机故障后仍可冷启动恢复冗余，只是不能和同组
成员落在同一台宿主机上。

- **放置由用户明确指定，否则随机**：
  - `spec.nodeName` 写了，就是实例的首选节点：创建时放在那里，并写入 promoter 的
    `preferred-nodes`，策略为 `start-only`，即不在首选节点恢复后自动切回，避免第二次中断。宿主机
    故障后仍可冷启动到其他候选节点；
  - 不写时，controller 在满足容量和反亲和的候选节点中随机选一个，不做打分；
  - 本地存储实例只能在卷所在节点，与 ADR-0015 相同。
- **反亲和**：`spec.antiAffinity.group` 给出组名（namespace 内），`policy` 为 `Required` 或
  `Preferred`，对应 vSphere 的 must/should 规则。
  - **创建时**：controller 选择节点和 LINSTOR 副本位置时避开同组成员。3 台节点、每个资源 2 个副本时，
    两个成员的候选节点必然有交集，因此只靠创建时的放置不够。
  - **冷启动时，由节点本地检查，不依赖控制面**：监管进程启动实例前检查本机是否已在运行同组成员。
    同组成员若在本机运行，本机一定是它的候选节点，它的对象就在 Agent 的本地状态里，因此这项检查
    只需要本机的信息。
    - `Required`：本机已有同组成员时不启动，按启动失败处理（`evict --keep-masked`），由下一个候选
      节点尝试；没有可用节点时实例保持停止并告警。vSphere HA 对 must 规则也是宁可不重启。
    - `Preferred`：照常启动，导出指标并告警，提示需要人工把实例迁开。
  - 同组成员恢复到不同节点后，屏蔽由 Agent 解除，与 ADR-0015 启动失败的解除方式相同。

### 6. 故障切换容量

宿主机为 16G/8G/8G。ADR-0015 在创建时按保障量检查容量，但不知道一台宿主机故障后，其余节点能否
承接它上面的受保护实例。承接不下时，`memory.low` 会被突破（PoC 14），Pod 层先被压到零。

- controller 对每个节点计算：该节点上作为 Primary 运行的受保护实例的保障量之和，与这些实例的其他
  候选节点的剩余 allocatable 之差。结果导出为指标，不足时告警，对应 vSphere 的百分比策略。
- **只告警，不拒绝创建**：节点很少，硬性拒绝会让大部分实例无法创建。

### 7. 与现有 PVE 告警对等的监控

ADR-0015 的告警只覆盖 HA。换掉 PVE 后，现有的 pve-exporter 告警需要对应项：

| 现有告警 | 替代 |
|---|---|
| `StorageAlmostFull` | LINSTOR controller 自带的 `/metrics`（storage pool 容量）；本地 libvirt pool 由 Agent 导出 |
| `OnbootGuestDown` | ADR-0015 已有的"应运行的实例没有任何节点在运行" |
| `GuestLockedLong` | Agent 导出每个对象最后一次 reconcile 成功的时间，长时间失败告警 |
| `NodeOffline` | node-exporter 抓取失败；Lease 过期由 controller 导出 |
| `pve_not_backed_up_total` | 备份系统给出的最近一次成功备份时间（§2） |

- **实例资源指标**（CPU、内存、balloon、磁盘和网络 I/O）由 Agent 从
  `virConnectGetAllDomainStats` 和 LXC 的 cgroup 导出，实例名同样使用 `name` 标签。不另装 libvirt
  exporter，因为它不覆盖 LXC，标签也不一致。
- 节点 journald 中 drbd-reactor、Agent 和监管进程的日志接入集中日志，补上现有 PVE 节点 journald
  未采集的缺口。

### 8. 磁盘扩容

- `LibvirtStorageVolume` 的 `size` 只允许增大，用 CEL `self >= oldSelf` 校验。
- 由 controller（LINSTOR 卷）或 Agent（本地 pool，`vol-resize`）扩大卷。实例运行中时，Agent 调用
  `virDomainBlockResize` 让客户机看到新容量；客户机内的分区和文件系统扩展由客户机自己完成，例如
  cloud-init 的 growpart。LXC 的 ext4 rootfs 由 Agent 在线执行 `resize2fs`。

### 9. 从 PVE 迁入现有客户机

现有客户机最终都要迁过来，它们的磁盘已经在 LINSTOR 上。

- **磁盘原地接管，不复制**：`LibvirtStorageVolume` 的 `backend: linstor` 支持引用已有 resource，
  `reclaimPolicy` 默认 `Retain`。接管时把 resource 移到计算后端的 resource group，因为它需要
  `auto-promote no` 等与 PVE 不同的参数；能否与 PVE 的 resource group 共存是 ADR-0015 的待验证项 2。
- **配置转换是一次性工具，不是 controller**：读取 `/etc/pve/qemu-server/<vmid>.conf` 和
  `/etc/pve/lxc/<vmid>.conf`，生成 `LibvirtDomain`/`LXCContainer` 的 YAML 提交到 git，由人检查。
  保留 MAC，DHCP 预留依赖它。retro VM 的设备模型逐台核对。
- **逐台切换**：PVE 上关机，先从 PVE 配置中解除磁盘引用再删除 PVE 上的定义（`qm destroy` 会删除
  磁盘），然后接管并启动。写成 runbook，先用一台不重要的客户机演练。

### 10. 镜像与安装介质

- ISO、云镜像和 LXC rootfs 归档都作为 `LibvirtStorageVolume` 下发到各节点名为 `iso`/`images` 的本地
  `dir` pool（`nodeSelector` 选择全部节点）。每个节点各存一份，冷启动和换盘都不依赖网络。
- `source.url` 指向内网 SeaweedFS 中的镜像 bucket，不直连外网，因为家宽上联不稳定。要求同时写
  `source.sha256`，Agent 校验后才标记就绪。
- 写入块设备用 `qemu-img convert`，不经 `qm importdisk` 式的二次处理，因为后者曾写坏过分区表。
- LXC 不再使用 `download` 模板，改为从同一个 bucket 中预先同步的 rootfs 归档创建，取代 ADR-0015
  中"第一版使用 `download` 模板"的做法。

## 待维护者确认

1. **VyOS 冷迁移的网关中断**：VM 100 原本在线迁移过。可选做法：
   - 接受冷迁移的中断（客户机启动时间，约数十秒）；
   - 改为两台 VyOS 跑 VRRP，按 §5 设 `Required` 反亲和（推荐，与 LB 的做法一致）；
   - 为它单独支持 libvirt 在线迁移：需要迁移期间临时允许 DRBD 双 Primary，与 promoter 的单 Primary
     模型冲突，复杂度最高，ADR-0007 也明确不以此为目标。
2. **备份系统选型**：见 §2。

## 待验证（Accepted 之前必须完成）

1. `on-quorum-loss = "freeze"`：模拟 quorum 短暂丢失后恢复，VM 和 LXC 都能解冻并继续运行；其他分区
   接管时旧 Primary 停止服务，不会双开；客户机在冻结期间的时钟跳变可以接受。
2. libvirt pull 模式备份在 DRBD raw 块设备上的全量与增量，以及 QEMU 重启后 bitmap 丢失的处理；
   恢复到新卷后启动实例。
3. `Required` 反亲和：同组成员所在节点故障后，冷启动不会落到另一成员的节点；没有可用节点时保持
   停止并告警。
4. 维护 taint 的排空：DRBD 实例 evict 到其他节点，本地实例关机后在宿主机重启时被拉起；
   `SafeToRemove` 的时机正确。
5. `virConnectCompareHypervisorCPU` 在 Intel 与 AMD 节点上对具名型号给出的结果。
6. 接管一个由 PVE 创建的 LINSTOR resource 并启动，PVE 侧不再引用它。

## 结果

- 存储抖动不再把客户机变成只读，填补了 2026-09-25 事故暴露的缺口。
- 新增的 API：`nodeName` 改为首选节点、`antiAffinity`、维护 taint 和 `SafeToRemove`、
  `LibvirtStorageVolume` 的 `size` 扩容。
- 备份交给专门的备份系统，Ayatori 只提供一致点、读取和增量的集成点，不保存备份数据。
- 反亲和由节点在冷启动时本地检查，冷启动不会破坏应用自带的冗余。
- 迁入走一次性工具加 runbook，不为 PVE 迁移写长期运行的 controller。
