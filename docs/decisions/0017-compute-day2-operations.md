# ADR-0017：计算后端的日常运维：存储失效、备份、维护与迁入

- 状态：Proposed（含待维护者确认的问题，见文末）
- 日期：2026-11-03
- 依据：[对照 vSphere 功能集的设计缺口](../compute/vsphere-gap-analysis.md)；建立在
  [ADR-0015](0015-libvirt-lxc-compute-backend.md)、[ADR-0016](0016-instance-node-services.md) 之上。

## 问题

ADR-0015/0016 解决了实例的创建、冷启动、身份和控制台，但对照 vSphere 后，以下有现状证据的需求
还没有设计：存储暂时不可用时的处理、备份与恢复、宿主机维护与计划内迁移、异构 CPU、应用自带冗余
的实例摆放、故障切换容量、与现有 PVE 告警对等的监控、磁盘扩容、从 PVE 迁入现有客户机，以及镜像
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

### 2. 备份与恢复

PVE 上的 9 个客户机和笔记本上的 dc1 目前都没有任何备份。

- **分层**：
  - 有应用级备份的服务（PostgreSQL 的 pgBackRest、etcd 快照、OpenBao 快照）继续以应用级备份为准，
    它们是一致的，恢复路径也已存在；
  - 镜像级备份覆盖没有应用级备份的 pet 实例：dc1、Windows、retro、VyOS 等。
- **机制优先用 LINSTOR 自带的快照与 backup shipping**（`linstor backup create`、按计划执行的
  `linstor schedule`），上传到 SeaweedFS 的 S3 bucket。bucket 由 CI Terraform 管理（ADR-0011）。
  它同时覆盖 VM 和 LXC，增量备份基于快照，不需要自己实现。
  - **前提是 thin 存储**。现有 LINSTOR pool 是 thick LVM（`driver: lvm`），不能快照，需要改为
    LVM thin。thin pool 写满会让所有卷出错，必须有使用率告警（见 §7）。是否切换待维护者确认。
  - 不采用 libvirt `backup-begin`：它只覆盖 VM；增量备份依赖持久化的 dirty bitmap，而 bitmap 需要
    qcow2，DRBD 上的 raw 块设备只能做全量。
- 有 qemu-guest-agent 的 VM 在快照前执行 fsfreeze；其他实例是崩溃一致的。
- **API**：`LibvirtStorageVolume` 增加 `backup.schedule`，引用 LINSTOR 中的 schedule 名，由 controller
  对该卷启用。不新增 Backup CRD，恢复也不经过 Ayatori：`linstor backup restore` 到新 resource 后，
  用 §9 的 adopt 方式让卷引用它。出现频繁恢复的需求时再做恢复 API。
- **恢复必须演练**，并写成 runbook。告警按实例的最近一次成功备份时间，替代现在的
  `pve_not_backed_up_total`。

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
- **指定首选节点**：`spec.preferredNodes` 写入 promoter 的 `preferred-nodes`，策略为 `start-only`，
  也就是只在启动时偏好，不会在首选节点恢复后自动切回，避免第二次中断。改变首选节点后 evict 一次，
  就是计划内冷迁移。
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

### 5. 应用自带冗余的实例：用本地存储，不做冷启动

LB 每个环境两台分在不同节点，etcd 每个节点一个，sandbox1/2 分开，目前都靠 inventory 手工保证。

- 3 台节点、每个资源 2 个副本时，两个 DRBD 实例的候选节点必然有交集：一台宿主机故障后，冷启动可能
  把同一对的两个成员放到同一台宿主机上，这正是反亲和要避免的。vSphere 的 must 反亲和规则同样会让
  HA 放弃重启，而不是违反规则。
- 因此**应用自带冗余的实例使用本地 pool 和固定的 `nodeName`，`ha: None`**。冗余由应用承担，宿主机
  故障时少一个成员；摆放写在 git 里，一目了然。这与 PVE 上现有的 `pve-rg-local` 用法一致。
- 先靠约定，不实现反亲和规则。需要自动检查时，再加一个分组 label，由 controller 检查同组成员不在
  同一节点。

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
| `StorageAlmostFull` | LINSTOR controller 自带的 `/metrics`（storage pool 容量）；本地 libvirt pool 由 Agent 导出；thin pool 的 data/metadata 使用率（§2 切换后必需） |
| `OnbootGuestDown` | ADR-0015 已有的"应运行的实例没有任何节点在运行" |
| `GuestLockedLong` | Agent 导出每个对象最后一次 reconcile 成功的时间，长时间失败告警 |
| `NodeOffline` | node-exporter 抓取失败；Lease 过期由 controller 导出 |
| `pve_not_backed_up_total` | §2 的最近一次成功备份时间 |

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

1. **LINSTOR 存储从 thick LVM 改为 LVM thin**：这是 §2 用 LINSTOR 快照和 backup shipping 的前提。
   代价是现有 resource 需要迁移，并新增 thin pool 写满的风险。不切换的话，镜像级备份只能做全量。
2. **VyOS 冷迁移的网关中断**：VM 100 原本在线迁移过。可选做法：
   - 接受冷迁移的中断（客户机启动时间，约数十秒）；
   - 改为两台 VyOS 跑 VRRP，按 §5 用本地存储、各自固定节点（推荐，与 LB 的做法一致）；
   - 为它单独支持 libvirt 在线迁移：需要迁移期间临时允许 DRBD 双 Primary，与 promoter 的单 Primary
     模型冲突，复杂度最高，ADR-0007 也明确不以此为目标。

## 待验证（Accepted 之前必须完成）

1. `on-quorum-loss = "freeze"`：模拟 quorum 短暂丢失后恢复，VM 和 LXC 都能解冻并继续运行；其他分区
   接管时旧 Primary 停止服务，不会双开；客户机在冻结期间的时钟跳变可以接受。
2. LVM thin 上的 LINSTOR 快照、backup shipping 到 SeaweedFS，以及恢复到新 resource 后启动实例。
3. 维护 taint 的排空：DRBD 实例 evict 到其他节点，本地实例关机后在宿主机重启时被拉起；
   `SafeToRemove` 的时机正确。
4. `virConnectCompareHypervisorCPU` 在 Intel 与 AMD 节点上对具名型号给出的结果。
5. 接管一个由 PVE 创建的 LINSTOR resource 并启动，PVE 侧不再引用它。

## 结果

- 存储抖动不再把客户机变成只读，填补了 2026-09-25 事故暴露的缺口。
- 新增的 API 只有 `LibvirtStorageVolume` 的 `backup.schedule` 与 `size` 扩容、`preferredNodes`、
  维护 taint 和 `SafeToRemove`。备份、快照和调度都交给 LINSTOR，不自己实现。
- 应用自带冗余的实例明确不走冷启动，避免冷启动破坏它们的冗余。
- 迁入走一次性工具加 runbook，不为 PVE 迁移写长期运行的 controller。
