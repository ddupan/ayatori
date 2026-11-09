# ADR-0019：计算后端的日常运维：备份接入、放置、维护、迁移与迁入

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-11-03
- 依据：[对照 vSphere 功能集的设计缺口](../compute/vsphere-gap-analysis.md)；建立在
  [ADR-0015](0015-libvirt-lxc-compute-backend.md)、[ADR-0016](0016-instance-node-services.md) 之上；
  DRBD 相关的部分见 [ADR-0020](0020-linstor-drbd-storage-integration.md)；§10 修订
  [ADR-0007](0007-compute-node-and-vm-boundary.md) 中"不以透明 live migration 为目标"。
- 修订：2026-11-06 §9 镜像改为独立的 `Image` 资源。

## 问题

ADR-0015/0016 解决了实例的创建、冷启动、身份和控制台，但对照 vSphere 后，以下有现状证据的需求
还没有设计：备份、宿主机维护、计算与磁盘迁移、异构 CPU、放置与反亲和、故障切换容量、与现有 PVE
告警对等的监控、磁盘扩容、从 PVE 迁入现有客户机，以及镜像和介质分发。证据见缺口分析。

本 ADR 只写与具体存储无关的部分。各项在 LINSTOR/DRBD 上怎样实现，以及存储暂时不可用时的处理，见
ADR-0020。

## 决策

### 1. 备份：接入备份系统，Ayatori 只提供集成点

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
  - 增量：libvirt checkpoint（QEMU dirty bitmap），对应 CBT。bitmap 的持久化依赖 qcow2，raw 块设备
    只能用非持久 bitmap，QEMU 重启后需要一次全量，待验证；
  - 恢复：备份系统写入一个新卷，由 domain 引用它；LINSTOR 上的接管方式见 ADR-0020。
- **备份系统的选型以后再定，不阻塞本 ADR 和当前实现**。候选需要支持 libvirt/QEMU 的增量备份接口和
  Linux/Windows 的客户机 agent。集成点的 API 形态（由谁触发、如何授权、在哪个节点读取）取决于选定的
  产品，选型时另行决定并验证。
- 告警按实例的最近一次成功备份时间，替代现在的 `pve_not_backed_up_total`，数据来自备份系统。

### 2. 宿主机维护

节点经常整台重装（2026-07-25 三台都经 PXE 重装过），升级需要逐台重启，roadmap 的 Laptop Rebuild
验收场景也要求节点能排空和退出。

- **沿用 Node API 的语义**（ADR-0007）：
  - `kubectl cordon`（`spec.unschedulable`）：controller 不再把新实例放到该节点；
  - **维护**：给 Node 打 taint `compute.ayatori.ddupan.top/maintenance:NoExecute`，controller 按每个
    实例的 `spec.evacuate` 为本节点上运行的实例创建迁移对象（§10），对应 Incus 的 `cluster.evacuate`：

    | `evacuate` | 行为 |
    |---|---|
    | `Auto`（默认） | 能在线迁移的在线迁移，存储能换节点的冷迁移，其余关机 |
    | `LiveMigrate` | 只在线迁移，做不到时排空失败并告警 |
    | `Migrate` | 冷迁移 |
    | `Stop` | 关机，**不写**电源意图标记，宿主机重启后由 Agent 按 `runState` 拉起 |

  - 迁移对象记录原节点，维护结束后可以按它迁回；
  - 本节点不再运行任何实例后，Agent 在 Node 上设置 condition `SafeToRemove=True`（roadmap 已有的
    名字），运维再重启或重装；去掉 taint 后解除维护期间的屏蔽。
- 宿主机的升级本身仍由 Chef/apt 负责，Ayatori 只提供排空和 `SafeToRemove`。不做 Lifecycle Manager
  式的滚动编排，逐台执行写进 runbook。

### 3. 异构 CPU（EVC）

pve1 是 Intel，pve2/pve3 是 AMD。VM 100 用 `host` CPU 在线迁移失败，改成 `x86-64-v2-AES` 后才正常。冷
切换虽然不要求 CPU 相同，但客户机会在下次启动时看到另一颗 CPU：Linux 不受影响，Windows 可能要求
重新激活，retro 系统对 CPUID 更敏感。

- Agent 把宿主机的 CPU 厂商和型号写进 Node label，沿用 node-feature-discovery 的命名和取值：
  `feature.node.kubernetes.io/cpu-model.vendor_id`（`Intel`/`AMD`）、`cpu-model.family`、`cpu-model.id`
  （CPUID 签名，十进制），取自 libvirt capabilities 的 `vendor` 与 `signature`。libvirt 的 CPU 型号名
  （如 `Skylake-Client-IBRS`）随 libvirt 版本变化，不作为 label。
- domain 写了具名 CPU 型号时，Agent 在下发前用 `virConnectCompareHypervisorCPU` 检查本节点能否提供；
  不能提供就写入该节点的 status，controller 将其排除出候选节点。
- domain 使用 `host-passthrough`/`host-model`，而候选节点的 CPU 型号不一致时，controller 设置一个
  warning condition，但不拒绝，保持 ADR-0015 不限制原生配置的原则。示例和迁入工具默认使用具名型号
  （`x86-64-v2-AES` 已在 PVE 上验证过）。在线迁移要求具名型号或两端型号一致。

### 4. 放置：Node label 加 Kubernetes 的亲和表达

LB 每个环境两台分在不同节点，etcd 每个节点一个，sandbox1/2 分开，目前都靠 inventory 手工保证。
应用自带冗余不等于不用共享存储：成员放在共享存储上，宿主机故障后仍可冷启动恢复冗余，只是不能和
同组成员落在同一台宿主机上。

**放置规则复用 Kubernetes 的表达**，与 ADR-0007 复用 Node 一样只采用 API contract，调度由 Ayatori
自己实现：

- **Node label 全部由 Agent 决定**，不用 `kubectl label` 手工维护：
  - 从节点事实发现：well-known 的 `kubernetes.io/hostname`、`kubernetes.io/arch`、`kubernetes.io/os`，
    CPU 厂商与型号沿用 node-feature-discovery 的命名（§3）；
  - 发现不了的拓扑信息（例如 `topology.kubernetes.io/zone` 区分 PVE 节点与笔记本）写在 Agent 配置中，
    由 Chef Policyfile 按节点下发，相当于 kubelet 的 `--node-labels`；
  - Agent 以自己的 field manager 做 SSA，label 集合由它完整拥有，不再出现的 label 随之移除。
  - 节点能给自己打任意 label，也就能吸引实例。Kubernetes 用 NodeRestriction 限制 kubelet 可写的
    label；这里 Agent 本来就有宿主机 root，VAP（ADR-0015）只限定它写自己的 Node，不再限制 label。
- **实例上的字段**，类型直接使用 `k8s.io/api/core/v1`：
  - `spec.nodeSelector`：简写，与 `LibvirtNetwork`/`LibvirtStoragePool` 已有的 `nodeSelector` 一致；
  - `spec.affinity.nodeAffinity`（`NodeAffinity`）：`required` 限定候选节点，`preferred` 表达首选节点；
  - `spec.affinity.instanceAffinity` / `instanceAntiAffinity`（`PodAffinityTerm`、
    `WeightedPodAffinityTerm`）：用 `labelSelector` 选中其他实例，`topologyKey` 指定按哪个 Node label
    分组。字段改名是因为对象不是 Pod，语义与 Pod 的亲和相同；
  - `spec.nodeName`：与 Pod 相同，是硬绑定，不经调度，也不会冷启动到其他节点。本地存储实例相当于
    隐式绑定在卷所在节点。
- **都不写时**，controller 在满足容量的候选节点中随机选一个，不做打分；有 `preferred` 时按权重选。
- **"IgnoredDuringExecution" 照样成立**：label 变化不会让运行中的实例搬走。冷启动换节点相当于一次
  重新调度，因此 `required` 规则在冷启动时也要满足。
- **执行位置**：
  - 创建和迁移时，controller 按全部规则计算候选节点和副本位置；
  - 冷启动时不依赖控制面，由节点本地检查：
    - `nodeAffinity.required` 已体现在候选节点里；
    - `topologyKey: kubernetes.io/hostname` 的 `instanceAntiAffinity.required`：启动前检查本机是否已在
      运行被选中的实例。被选中的实例若在本机运行，本机一定是它的候选节点，它的对象和 label 就在
      Agent 的本地状态里，因此只需要本机的信息。命中时不启动，交给下一个候选节点，没有可用节点时
      实例保持停止并告警，vSphere HA 对 must 规则也是宁可不重启。DRBD 上的实现见 ADR-0020；
    - 其他 `topologyKey`（例如 zone）无法在单个节点上判断，controller 在创建时把成员的候选节点划分到
      不同的拓扑域，冷启动只会落在各自的域内；
    - `preferred` 的反亲和照常启动，违反时导出指标并告警，提示人工迁开。
- 3 台节点、每个资源 2 个副本时，两个按 hostname 反亲和的成员，候选节点必然有交集，所以 hostname
  反亲和必须靠冷启动时的本地检查，不能只靠创建时的放置。
- 不采用 tolerations：维护 taint（§2）对所有实例生效，留在节点上的方式由 `evacuate` 表达。

### 5. 故障切换容量

宿主机为 16G/8G/8G。ADR-0015 在创建时按保障量检查容量，但不知道一台宿主机故障后，其余节点能否
承接它上面的受保护实例。承接不下时，`memory.low` 会被突破（PoC 14），Pod 层先被压到零。

- controller 对每个节点计算：该节点上运行的、可自动冷启动的实例的保障量之和，与这些实例的其他
  候选节点的剩余 allocatable 之差。结果导出为指标，不足时告警，对应 vSphere 的百分比策略。
- **只告警，不拒绝创建**：节点很少，硬性拒绝会让大部分实例无法创建。

### 6. 与现有 PVE 告警对等的监控

ADR-0015 和 ADR-0020 的告警只覆盖 HA。换掉 PVE 后，现有的 pve-exporter 告警需要对应项：

| 现有告警 | 替代 |
|---|---|
| `StorageAlmostFull` | LINSTOR controller 自带的 `/metrics`（storage pool 容量）；本地 libvirt pool 由 Agent 导出（`ayatori_compute_agent_pool_bytes{type="capacity"\|"available"}`） |
| `OnbootGuestDown` | ADR-0015 已有的"应运行的实例没有任何节点在运行"：`max by (namespace, name) (ayatori_compute_agent_domain_should_run) == 1 unless on (namespace, name) max by (namespace, name) (ayatori_compute_agent_domain_running) == 1`。客户机内关机或 destroy 的实例 `should_run` 为 0，不告警 |
| `GuestLockedLong` | Agent 导出每个对象最后一次 reconcile 成功的时间（`ayatori_compute_agent_object_last_success_timestamp_seconds`，从未成功为 0），长时间失败告警；libvirt 不可用见 `ayatori_compute_agent_libvirt_up` |
| `NodeOffline` | node-exporter 抓取失败；Lease 过期由 controller 导出 |
| `pve_not_backed_up_total` | 备份系统给出的最近一次成功备份时间（§1） |

- **实例资源指标**（CPU、内存、balloon、磁盘和网络 I/O）由 Agent 从
  `virConnectGetAllDomainStats` 和 LXC 的 cgroup 导出，实例名同样使用 `name` 标签。不另装 libvirt
  exporter，因为它不覆盖 LXC，标签也不一致。VM 部分为 `ayatori_compute_agent_domain_cpu_seconds_total`、
  `_memory_bytes{type=current|maximum|rss}`、`_disk_{read,written}_bytes_total{device}`、
  `_network_{receive,transmit}_bytes_total{device}`；只导出运行中的实例。
- 节点 journald 中 drbd-reactor、Agent 和监管进程的日志接入集中日志，补上现有 PVE 节点 journald
  未采集的缺口。

### 7. 磁盘扩容

- `LibvirtStorageVolume` 的 `size` 只允许增大，用 CEL `self >= oldSelf` 校验。
- 由 controller（LINSTOR 卷）或 Agent（本地 pool，`vol-resize`）扩大卷。实例运行中时，Agent 调用
  `virDomainBlockResize` 让客户机看到新容量；客户机内的分区和文件系统扩展由客户机自己完成，例如
  cloud-init 的 growpart。LXC 的 ext4 rootfs 由 Agent 在线执行 `resize2fs`。

### 8. 从 PVE 迁入现有客户机

现有客户机最终都要迁过来，它们的磁盘已经在 LINSTOR 上，原地接管而不复制，见 ADR-0020。

- **配置转换是一次性工具，不是 controller**：读取 `/etc/pve/qemu-server/<vmid>.conf` 和
  `/etc/pve/lxc/<vmid>.conf`，生成 `LibvirtDomain`/`LXCContainer` 的 YAML 提交到 git，由人检查。
  磁盘与网卡生成为类型化的 `disks`/`interfaces`（ADR-0015），保留 MAC，DHCP 预留依赖它。retro VM 的设备模型逐台核对。
- **逐台切换**：PVE 上关机，接管磁盘，启动。写成 runbook，先用一台不重要的客户机演练。

### 9. 镜像与安装介质

2026-11-06 修订：镜像不再作为 `LibvirtStorageVolume` 下发。卷是某一处的一份数据，镜像是每个节点都要
一份的只读内容；用卷表达时，同一个 ISO 要给每个节点各建一个对象，URL 与校验和重复写多遍。对应
vSphere Content Library、CloudStack template、Harvester VirtualMachineImage。

```yaml
apiVersion: compute.ayatori.ddupan.top/v1alpha1
kind: Image                    # 集群级
metadata:
  name: debian-13              # 名字是一个频道，新机器总是用 spec 当前的版本
spec:
  type: Disk                   # Disk | ISO | Rootfs，创建后不可修改
  pool: images                 # 各节点上的 LibvirtStoragePool（dir），创建后不可修改
  nodeSelector: {}             # 缺省为全部计算节点
  source:                      # 二选一，可以修改，修改即更新镜像
    http:
      url: https://…/debian-13-genericcloud-20261201.qcow2
      sha256: …
    # oci:
    #   reference: registry…/debian-13-systemd@sha256:…
status:
  nodes:
  - nodeName: compute-dev1
    sha256: …                  # 本节点上现有副本的版本
    ready: true                # 与 spec 的版本一致时才为 true
    message: ""
```

- **type 表示内容，决定谁能用它：**

  | type | 内容 | 使用方 |
  |---|---|---|
  | `Disk` | qcow2 / raw | 卷的 `source.image`，由 libvirt 的 `virStorageVolCreateXMLFrom` 复制到新卷，再扩容到卷的 capacity |
  | `ISO` | ISO 9660 | domain 的 `disks[].image`，作为只读光驱挂载 |
  | `Rootfs` | tar 归档 | 卷的 `source.image`，解包为 LXC rootfs |

  VM 与 LXC 不能共用镜像，同一个发行版的两种形态是两个 Image。用错类型时，引用方被拒绝。格式不写在
  spec 中，Agent 下载后用 `qemu-img info` 或归档头核对是否与 `type` 一致，不一致时本节点报错、不就绪。
- **每个节点一份副本**：pool 中与 Image 同名的卷。Agent 下载到临时文件，校验 sha256 与格式后
  `rename` 覆盖原副本，每个节点各存一份，冷启动和换盘都不依赖网络。
- **source：**
  - `http` 指向内网 SeaweedFS 中的镜像 bucket，不直连外网，因为家宽上联不稳定；必须同时写 `sha256`。
  - `oci` 只用于 `Rootfs`，引用必须带 digest（与必须写 sha256 同理），只经内网 registry 或 Artifact
    Keeper 的 pull-through 缓存拉取。Agent 用现成的库（go-containerregistry 或 skopeo+umoci）拉取并
    展平各层，结果与 `http` 来的 rootfs 归档相同。Ayatori 的 LXC 是系统容器，只支持自带 init 的镜像；
    镜像配置中的 entrypoint、env 不转换为 `lxc.execute.cmd`，不做"运行任意应用镜像"。第一版只实现
    `http`，`oci` 等出现具体需求再实现。
- **允许更新，已有的机器不受影响：**
  - 修改 `source` 即更新。Agent 用 `rename` 原子替换副本，已经打开它的进程继续读旧的 inode。
  - `ISO`：所有 VM 共享同一个只读副本，原地更新。运行中的 VM 继续使用打开时的版本，下次启动或换盘时
    读到新版本。
  - `Disk`/`Rootfs`：卷是创建时的完整复制，与镜像不再有关系。Agent 在同一个 reconcile 循环中依次执行
    替换副本和从副本创建卷，不会读到替换到一半的副本；卷的 status 记录实际复制的 `image` 与 `sha256`。
  - 更新在各节点上有先后。节点上的副本与 spec 一致前，本节点上需要该镜像的新卷等待，原因写入卷的
    status，不会悄悄用旧版本。新版本下载或校验失败时，本节点报错，已有机器不受影响。
  - 需要固定版本时，另建一个不再更新的 Image（例如 `debian-13-20261101`），不另设更新策略字段。
- **删除**：删除 Image 后各节点删除副本；本节点仍有 domain 挂载该 ISO 时延后删除，与卷的本地保护
  相同。从镜像创建的卷不受影响。
- 写入块设备用 `qemu-img convert`，不经 `qm importdisk` 式的二次处理，因为后者曾写坏过分区表。
- 第一版只实现 `Disk` 与 `ISO`：`Rootfs` 随 LXC 从镜像创建一起加入。副本所在的 pool 必须是 `dir` 类型，
  `rename` 由 Agent 直接操作文件，因此 Agent 的 systemd 单元不能用 `ProtectSystem=strict`（pool 目录由
  API 声明，无法预先列入 `ReadWritePaths`），改为 `ProtectSystem=full`。
- LXC 不再使用 `download` 模板，改为从 `Rootfs` 镜像创建，取代 ADR-0015 中"第一版使用 `download`
  模板"的做法。

### 10. 迁移

现状：VM 101 先建在 pve1 再用 `qm migrate` 离线挪走；VM 100 做过在线迁移（因 CPU 型号失败，见 §3）；
etcd 的 rootfs 用 `pct move-volume` 在存储之间搬过，要求容器停机并限速。

**原则：实例在哪个节点运行是运行时状态，磁盘放在哪里是配置。** 前者不改 spec，后者经 git 修改 spec，
vSphere 的 Storage vMotion 同样会改写 VM 的配置文件。

#### 迁移对象

计算迁移是长时间运行、可能失败的操作，用一个对象表达，参照 KubeVirt 的
`VirtualMachineInstanceMigration`，而不是控制台上一次性的动作：

- `InstanceMigration`（namespaced，与实例同一 namespace）：
  - `spec.instanceRef`：`LibvirtDomain` 或 `LXCContainer`；
  - `spec.targetNode`：可选，不写时 controller 在满足条件的候选节点中随机选；
  - `spec.mode`：`Auto`（默认，能在线就在线，否则冷迁移）、`Live`、`Cold`；
  - `spec.bandwidth`：可选，覆盖 Agent 配置中的默认限速；
  - `status`：`phase`（`Pending`、`Running`、`Succeeded`、`Failed`）、源节点、目标节点、失败原因。
- **所有权只在切换成功后转移一次**：与 PVE HA 的 `migrate` 状态相同，`Succeeded` 之前源节点是 owner，
  失败时实例留在源节点，不会触发冷启动。
- 计算控制台的"迁移"按钮、维护排空（§2）都只是创建这个对象；RBAC 按对 `instancemigrations` 的
  create 权限授权，审计照常记录。已完成对象的保留参照 ADR-0005。
- ADR-0015 已规定实例换节点不算漂移。要让实例长期以新节点为首选，另外修改 git 中的
  `nodeAffinity.preferred`（§4）。迁移不得违反 `required` 规则；`nodeName` 硬绑定的实例不能计算迁移。

#### 计算迁移

| | 冷迁移 | 在线迁移 |
|---|---|---|
| 共享存储上的 VM | 关机后在目标节点启动；DRBD 上由 promoter 接管，见 ADR-0020 | libvirt live migration；DRBD 上见 ADR-0020 |
| 共享存储上的 LXC | 同上 | 不支持 |
| 本地存储的 VM | 关机后由 Agent 之间复制卷，在目标节点建同名卷后启动；随后在 git 中改为新节点 | libvirt `--copy-storage-all`，经 NBD 边迁内存边复制磁盘；目标节点先建好同样大小的卷 |
| 本地存储的 LXC | 同本地 VM 的冷迁移 | 不支持 |

- **LXC 不做在线迁移**：需要 CRIU，Incus 文档说只支持"没有 systemd、没有网卡"的容器；PVE 也只有
  `pct migrate --restart`。
- **前置检查**（与冷启动共用）：目标节点是候选节点、CPU 兼容（§3）、容量按保障量足够并先压低目标
  节点 `k8s.slice` 的上限、`required` 亲和与反亲和（§4）。
- **libvirt 参数**：目标节点上 Agent 预先 define 了同名 domain，libvirt 允许迁入并沿用这份持久定义；
  auto-converge 默认开启（与 Incus 相同），post-copy 不开启，因为中途失败会丢失客户机。
- **迁移通道**：控制连接走 Agent 之间的 SPIFFE mTLS 中继（与控制台相同），节点之间不交换 root
  凭据；内存和磁盘数据由 QEMU 直连传输。所有节点只有一条 1G 链路，与 DRBD 复制共用，迁移必须限速，
  否则会重演 2026-09-25 的 quorum 抖动（ADR-0020）。

#### 磁盘迁移

| 场景 | 做法 | 实例是否停机 |
|---|---|---|
| DRBD 卷换存储池或副本节点 | 修改卷的 spec，LINSTOR 逐副本重放，见 ADR-0020 | 不停机 |
| 卷换 pool 或类型（本地↔本地、本地↔DRBD） | 新建 `LibvirtStorageVolume`，用 `source.volume` 声明从旧卷复制内容，再把 domain 的引用改为新卷 | VM：Agent 发现运行中的 domain 某块盘的引用改为"从当前盘复制"的新卷时，用 libvirt `blockcopy`（transient job，与 oVirt 相同）镜像到新卷，同步后 pivot，再重新 define；LXC：停机复制（`qemu-img convert` 或 rsync）后启动 |
| 跨节点的本地卷 | 见计算迁移中的本地存储行 | 视计算迁移方式 |

- 用 spec 声明磁盘迁移与 KubeVirt 的 `updateVolumesStrategy: Migration` 是同一思路。
- 旧卷不会被自动删除：没有实例引用后，按它的 `reclaimPolicy` 处理（ADR-0015 的卷保护 finalizer）。
- 只声明新卷而不写 `source.volume` 时，新卷是空盘，Agent 不会复制任何数据，避免误把"换一块新盘"
  当成迁移。

## 已确认

- **短时间停机可以接受**：VyOS 网关等实例在宿主机故障时冷启动、计划内维护时冷迁移，中断时间为
  客户机关机加启动，不为此要求在线迁移或 VRRP 双机。在线迁移（§10）是减少维护中断的手段，不是
  前提。
- **备份系统以后再选**，见 §1。

## 待验证（Accepted 之前必须完成）

1. hostname 的 `required` 反亲和在冷启动时生效（DRBD 上见 ADR-0020）；zone 的反亲和由候选节点划分
   保证。
2. 维护排空：按 `evacuate` 创建迁移对象，各类实例按预期迁走或关机；`SafeToRemove` 的时机正确。
3. `virConnectCompareHypervisorCPU` 在 Intel 与 AMD 节点上对具名型号给出的结果。
4. 本地存储 VM 的 `--copy-storage-all` 在线迁移，限速下 1G 链路上 DRBD 不出现 quorum 抖动。
5. 运行中 VM 的 `blockcopy` 换卷后重新 define，冷启动使用新卷。
6. ~~ISO 镜像原地更新~~：已在 compute-dev1 用真实 QEMU 验证。运行中的 VM 继续持有旧 inode，重启后读到
   新版本；AppArmor 允许 QEMU 读取 `rename` 换上的新文件（pool 不在 `/var/lib/libvirt/images` 下时
   `virt-aa-helper` 的探测会被拒绝并记入审计日志，不影响启动）。

DRBD 上的冷切换、在线迁移、换池、接管与反亲和的验证见 ADR-0020。

## 结果

- 新增的 API：`nodeSelector`、`affinity`（复用 Kubernetes 类型）、`evacuate`、维护 taint 和 `SafeToRemove`、
  `InstanceMigration`、`LibvirtStorageVolume` 的 `size` 扩容、`source.volume` 与 `source.image`、`Image`。
- VM 支持在线迁移，修订 ADR-0007；LXC 只做冷迁移。
- 备份交给专门的备份系统，Ayatori 只提供一致点、读取和增量的集成点，不保存备份数据。
- 反亲和在冷启动时由节点本地检查，冷启动不会破坏应用自带的冗余。
- 迁入走一次性工具加 runbook，不为 PVE 迁移写长期运行的 controller。
