# ADR-0020：LINSTOR/DRBD 存储集成：卷、自动冷启动与在线迁移

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-11-03
- 依据：维护者讨论；从 [ADR-0015](0015-libvirt-lxc-compute-backend.md) 拆出存储一节的 LINSTOR
  部分、"自动冷启动"与 DRBD 可观测性，从 [ADR-0019](0019-compute-day2-operations.md) 拆出存储失效、
  卷接管与 DRBD 上的迁移；对照了 LINBIT 的各个 hypervisor 集成与 LINSTOR 1.35 的源码。

## 问题

LINSTOR/DRBD 是计算后端中唯一的非原生部分：libvirt 没有 LINSTOR pool 驱动，卷的创建和放置要调用
LINSTOR API，自动冷启动依赖 DRBD quorum 与 drbd-reactor，在线迁移需要临时双 Primary。这些内容原先
散在 ADR-0015 的存储、自动冷启动、可观测性三节和 ADR-0019 的三节中，并让计算核心与 DRBD 绑在了
一起。

本 ADR 把它写成一个独立的存储集成：计算核心只认 ADR-0015 定义的**存储能力**，LINSTOR/DRBD 怎样
提供这些能力由本 ADR 负责。将来接入 Ceph 时同样写成一个集成。

## 评估

- **为 libvirt 写一个 LINSTOR 存储后端**：libvirt 的存储后端是源码树内的 C 代码，没有稳定的外挂接口，
  只能提交上游或自己维护打补丁的 libvirt。即便写成，它也只覆盖单台主机上的卷创建与路径解析；
  冷启动、迁移前的 diskless 接入与双 Primary、副本放置都在集群层面，libvirt 的存储后端碰不到。
  CloudStack、OpenNebula、oVirt 等平台也都在自己的管理层集成 LINSTOR，而不是在 libvirt 中。不采用。
- **永久开启 `allow-two-primaries`**（linstor-proxmox、Incus 的做法）：单 Primary 是本集成防止双开的
  依据，永久允许两个 Primary 后，两个节点的 promoter 可能同时拉起实例。不采用。
- **不用 drbd-reactor，auto-promote 开启，由打开设备的 `open()` 防双开**（CloudStack 的做法，LINBIT
  文档认为单靠 DRBD quorum 即可安全）：在线迁移会完全落在主流路径上，但"谁来重启"需要决策者。
  集中决策会让冷启动依赖控制面，违背 ADR-0015；由各节点 Agent 竞争启动，等于重写 drbd-reactor 的
  promoter。不采用。
- **迁移时自己切换 DRBD 参数**（CloudStack agent、OpenNebula 钩子的做法）：LINSTOR 1.35 起提供
  `make-available --auto-manage-dual-primary` 与 `unmake-available`，由 LINSTOR 只在源与目标之间打开、
  事后恢复，linstor-csi 与 LINBIT 的 Cinder 分支已改用它。改用 LINSTOR 的 API。

没有找到任何集成同时使用 drbd-reactor 的单 Primary HA 与 VM 在线迁移：XCP-ng 只用 drbd-reactor
保护 LINSTOR controller 自己的数据库。本 ADR 中"显式 promote 与 promoter 交接"是没有先例的部分，
列入待验证。

## 决策

### 定位

- 集成在 Ayatori 这一层，相当于 CloudStack 的 KVM agent 加管理端：
  - **compute controller** 经 LINSTOR REST API（golinstor）管理 resource definition、副本放置、
    diskless 接入、扩容、换池和迁移窗口；
  - **Compute Agent** 在节点上生成 drbd-reactor promoter 配置、挂载单元和监管进程，回写 DRBD 状态。
- 代码位于 `internal/compute/adapter/linstor`，与 libvirt adapter 并列。
- 代价：`virsh vol-list` 看不到 LINSTOR 卷，ADR-0015 中"`virsh` 看到的结果与声明一致"不适用于
  LINSTOR，用 `linstor resource list` 查看。

### 提供的存储能力

| 能力（ADR-0015） | 本集成 |
|---|---|
| 换节点 | 能：路径 `/dev/drbd/by-res/<res>/<vol>` 在所有节点上一致 |
| 自动冷启动 | 能：DRBD quorum + drbd-reactor promoter，见下 |
| VM 在线迁移 | 能：临时双 Primary，见下；LXC 不支持 |
| 不停机换存储池或副本节点 | 能：LINSTOR 逐副本重放 |

### 版本

- LINSTOR ≥ 1.35（`auto_manage_dual_primary`），DRBD 9，drbd-reactor。
- libvirt ≥ 10.10、swtpm ≥ 0.7：NVRAM 与 vTPM 状态放在块设备上（见"实例的卷布局"）。计算节点的底座
  为 Debian 13（libvirt 11.3、swtpm 0.7.1），见 ADR-0015。
- DRBD 9 内核模块、drbd-reactor 与 LINSTOR 的包来源待确认：LINBIT 为 Ubuntu 提供 PPA，为 Debian 只有
  PVE 专用的仓库，需要确认它们在 Debian 13 原生内核上可用。

### resource group 与 DRBD 参数

- 计算后端的卷使用自己的 resource group，与 PVE 和 KaaS 子集群的 PVC（ADR-0014）分开。
- DRBD 参数：`auto-promote no`、`quorum majority`、`on-no-quorum suspend-io`、
  `on-no-data-accessible suspend-io`、`on-suspended-primary-outdated force-secondary`，net 选项
  `rr-conflict retry-connect`、`protocol C`。drbd-reactor promoter 设 `on-quorum-loss = "freeze"`：
  - 2026-09-25 的事故中，新卷初始同步期间多个资源 quorum 反复丢失又恢复，`io-error` 让 sandbox1 和
    etcd CT150 的 rootfs 变为只读，控制面中断 33 分钟。`freeze` 在失去 quorum 时冻结实例的 cgroup，
    客户机只看到一次停顿；quorum 恢复且没有其他节点接管时解冻；其他分区已经接管时，旧 Primary 被
    `force-secondary`，服务停止。对应 vSphere VMCP 对 APD 的处理。Incus 的 LINSTOR 驱动默认也是
    `suspend-io`。
  - 需要 cgroup v2。PoC 01 只验证了 `io-error`，`freeze` 尚未测试。
- 至少 3 个副本，或者 2 个副本加 diskless tiebreaker。drbd-reactor 不做节点级 STONITH，完全依赖 DRBD
  quorum。
- **tiebreaker 必须在故障发生前就在线**：故障后才接入的 tiebreaker 没有见证旧 Primary 的最后状态，
  DRBD 不会因此恢复 quorum（PoC 04）。PoC 中有两个资源的 tiebreaker 掉线 11 小时无人察觉，因此要
  按连接状态告警（见"可观测性"）。
- **限制初始同步与重同步的带宽**（`c-max-rate` 等，设在 resource group 上）：事故的触发因素是两块新卷
  同时初始同步。迁移和换池同样占用这条 1G 链路，共用这一限速。调查新建空卷能否跳过初始同步。
- 本地 `logical`/`zfs` pool 不得指向 LINSTOR/DRBD 的底层 VG 或 zpool：libvirt 会把 DRBD 的底层卷也
  列成 pool 中的卷，经 libvirt 删除或直接挂载它们会绕过 DRBD（PoC 10）。

### 卷

- `LibvirtStorageVolume` 的 `spec.linstor` 后端（与 `spec.libvirt` 互斥）由 controller 调用 LINSTOR API 创建，在候选节点上
  放置副本；不放副本的候选节点按需以 diskless 方式接入。
- domain 的 `disks` 引用卷对象，Agent 渲染为 `<disk type='block'>` 和 `/dev/drbd/by-res/<res>/<vol>`；
  该路径在所有节点上一致，但不出现在 API 中，本地卷换成 LINSTOR 卷时 domain 不需要改。
- **扩容**：controller 扩大 volume definition，之后的处理与 ADR-0019 相同。
- **换存储池（SSD↔HDD）或换副本所在节点**：修改卷的 spec，controller 经 LINSTOR 逐个副本重放（加新
  副本、同步完成后再删旧副本，`toggle-disk`），全程保持 quorum。设备路径不变，domain 不需要改，
  VM 与 LXC 都不停机。
- **接管已有 resource**（从 PVE 迁入、从备份恢复）：卷可以引用已有的 LINSTOR resource，
  `reclaimPolicy` 默认 `Retain`。接管时：
  - 把 resource 移到计算后端的 resource group，参数随之改为上面的值；
  - **删除 resource definition 上的 `DrbdOptions/Net/allow-two-primaries`**：linstor-proxmox 在创建时
    把它永久设为 `yes`，不删除就失去了单 Primary 的保证；
  - PVE 侧先解除磁盘引用再删除定义，`qm destroy` 会删除磁盘。

### 实例的卷布局

一个受保护的实例对应一个 LINSTOR resource，该实例跟着走的所有东西都是它的 volume，随同一个 Primary
切换：

| volume | 内容 | 写入方 |
|---|---|---|
| 磁盘 | 客户机磁盘，多块盘各占一个 volume | 用户在 `disks` 中按 alias 绑定卷对象，Agent 填写 source |
| NVRAM | raw 块设备，`<nvram type='block'>` | `os.firmware` 为 `efi` 时由 Agent 填写；`os.nvRam` 归 Ayatori，写了即拒绝（ADR-0026） |
| vTPM 状态 | raw 块设备，`<backend type='emulator'><source type='file' path='/dev/drbd/by-res/...'/>` | 同上 |
| 状态卷 | ext4，几十 MiB，只放电源意图标记 | 监管进程 |

- NVRAM 与 vTPM 默认都在节点本地（`/var/lib/libvirt/qemu/nvram/`、`/var/lib/libvirt/swtpm/<uuid>/`），
  冷切换后启动项丢失、BitLocker 要求恢复密钥（PoC 02）。
- PoC 03 把它们放在 ext4 状态卷上的文件中，冷切换已验证可行；但 ext4 不能被两个节点同时挂载，
  在线迁移时目标端的 QEMU 无法在启动时打开它们。改成 raw volume 后，冷切换照常随 Primary 走，在线
  迁移时随双 Primary 两边都能打开。
- **电源意图**仍写在状态卷上：实例被有意关机时写入"off"标记，接管的节点读到标记就不启动，开机操作
  清除标记。QEMU 不访问状态卷，在线迁移切换完成后由源端卸载、目标端挂载即可。
- LXC 的 rootfs 本来就在 DRBD 上，电源意图同样写入状态卷。

### 自动冷启动

参照 vSphere HA：故障判定和重启由宿主机上的 agent 完成，vCenter 只负责下发配置；真正防止双开的是
VMFS 文件锁，不需要全集群 quorum。

- **DRBD 的单 Primary 相当于 VMFS 锁。** 使用 drbd-reactor 的 promoter 插件：候选节点竞争 promote，
  赢的节点启动 `start` 中列出的单元；失去 quorum 的节点按上面的 `freeze` 处理。
- **promoter 的 `start` 内容：**
  - VM：挂载状态卷，然后是 `ayatori-vm@<name>.service`，用 Agent 预先放好的定义启动。
  - LXC：挂载 rootfs 与状态卷的 systemd `.mount` 单元（不依赖 Pacemaker 的 `ocf:heartbeat:Filesystem`），然后是
    LXC 版监管进程，不是 `lxc@<name>.service`。
- **首选节点**：`nodeAffinity.preferred`（ADR-0019）按权重排序后写入 promoter 的 `preferred-nodes`，
  策略 `start-only`，原节点恢复后不自动切回。`required` 规则体现在副本与 diskless 的放置上，promoter 只会
  在这些节点上竞争。
- **fencing 的粒度是单个卷**，而不是整台宿主机。网络分区最多让个别实例停止，不会重启宿主机。
- **谁来启动实例**：DRBD 上 `ha: Restart` 的实例，换节点只由 promoter 决定，Agent 不得在其他节点启动
  它，否则会绕过单 Primary 互斥。
- **实例自身崩溃**（PoC 01 已验证）：promoter 不监控服务，各层分工如下：
  - 客户机 panic 和卡死：domain XML 中写 `<features><acpi/></features>`、`<panic model='isa'/>`、
    `<on_crash>restart</on_crash>` 和 `<watchdog model='i6300esb' action='reset'/>`，由 libvirt/QEMU
    原地复位。此时 domain 状态为 `running (crashed)`，不算故障。
  - QEMU 进程或容器 init 死亡：promoter 启动的单元是一个**监管进程**，跟踪实例的生命周期（VM 订阅
    libvirt lifecycle 事件，LXC 循环执行 `lxc-start -F`），在本机重启并按滑动窗口计数。不得使用
    oneshot 单元，否则实例死亡后节点仍持有 Primary，既不重启也不切换。
  - 监管单元使用 `KillMode=mixed`。默认的 `control-group` 下，systemd 停止单元时会反复向 cgroup
    发送 SIGTERM，监管进程清理时新 fork 的 `virsh destroy` 也可能被杀，QEMU 残留，DRBD 只能强制
    demote 让其 IO 失败（PoC 05）。
  - 监管进程启动实例前必须确认本节点是该实例 DRBD 资源的 Primary，否则不启动。`virsh start`
    失败不计入重启次数，不得像崩溃那样 evict，否则会在节点之间来回弹跳（PoC 02）。但只停在原地也不对：
    节点本地的问题（如 bridge 缺失）会让实例持有 Primary 却一直不运行（PoC 11，PoC 13 中真实发生过）。
    因此启动失败时执行 `drbd-reactorctl evict --keep-masked`：本节点对该实例保持屏蔽，由下一个节点尝试；
    全部候选节点都失败时实例停机并告警，不会来回弹跳。Agent 确认本节点问题已解决后执行 `--unmask`，屏蔽
    状态回写 status 并导出为指标（PoC 13）。
  - **反亲和**（ADR-0019）：监管进程启动实例前，用本地状态中的对象和 label 计算
    `topologyKey: kubernetes.io/hostname` 的 `instanceAntiAffinity.required`，本机已在运行被选中的实例
    时按启动失败处理（`evict --keep-masked`）；`preferred` 照常启动并导出指标。
  - 重启次数耗尽：监管进程以非零值退出，单元的 `OnFailure=` 在节点本地执行
    `drbd-reactorctl evict <实例>`，交给其他节点。
  - 重启计数不能交给 systemd：systemd 255 下同一单元同时设置 `Restart=` 和 `OnFailure=` 时，第一次
    失败就会触发 `OnFailure=`。promoter 的依赖保持默认 `Requires`，`BindsTo` 会让结果取决于 job
    排队顺序。
  - Agent 为每个实例生成独立的 promoter 配置文件，`evict` 按文件名定位。单元名中的 `\x2d` 等
    转义在 TOML 双引号字符串中非法，必须用单引号字符串（PoC 03）。
  - **LXC 没有自己的关机语义，意图只看电源意图标记**（与 Incus 相同）。监管进程不区分容器怎么停的：
    有标记就保持关闭，没有标记一律按崩溃处理，在本机重启，次数耗尽后 evict。容器内执行 `poweroff`
    同样会被拉起，要关机必须经 Agent（先写标记再 `lxc-stop`）。
    - liblxc 的 API、monitor 通道、hook 和 `lxc-start -F` 的退出码都拿不到 init 的退出原因，Go 进程也
      不能前台启动容器（PoC 04、08）。
    - 启动失败按"容器没有进入 RUNNING"判断（`lxc-wait -s RUNNING`），不计入重启次数，不 evict；
      `lxc-start -F` 在容器已运行时也返回 0，退出码不能用。
    - `lxc-start` 不响应 SIGTERM，停止时由监管进程调用 `lxc-stop`。
  - 实例的重启由监管进程在内部完成。对 promoter 管理的单元执行 `systemctl restart` 会连带
    demote 资源，实例被其他节点接管。
- **实际运行位置以 DRBD Primary 且有 quorum 为准**，不以 `virsh domstate`/`lxc-info` 为准。
  PoC 01 观察到，失去 quorum 的旧节点上的实例还会短暂显示运行中。Agent 据此回写 status。

### VM 在线迁移

迁移对象与前置检查见 ADR-0019。DRBD 上的步骤：

1. controller 对目标节点调用 LINSTOR `make-available`，带 `auto_manage_dual_primary`：目标节点没有副本时
   以 diskless 接入；LINSTOR 以资源当前是 Primary 的节点为迁移源，只在源与目标之间设置
   `allow-two-primaries`（需要时切到 `protocol C`），并拒绝同一资源上冲突的迁移。
2. 目标节点 Agent 显式 promote（`auto-promote no`，QEMU 打开设备不会自动 promote）。此时源端仍是
   Primary，按 drbd-reactor 的文档，promoter 只在没有 Primary peer 时才 promote，因此不会插手，待验证。
3. 源节点 Agent 发起 libvirt 在线迁移（限速）。目标节点上已有 Agent 预先 define 的同名 domain，libvirt
   允许迁入并沿用这份持久定义。NVRAM 和 vTPM 都是 raw volume，两端都能打开；NVRAM 的内容随迁移流
   传输，目标端恢复运行时由 QEMU 写回。
4. **所有权只在切换成功后转移一次**，与 PVE HA 的 `migrate` 状态相同：在此之前源节点是 owner，迁移
   失败时实例留在源节点，目标端 demote 并撤销 make-available。
5. 切换成功后：
   - 源端监管进程收到 `STOPPED`/`MIGRATED` 事件，正常退出，不计为崩溃；promoter 停止服务、卸载
     状态卷、demote；
   - 目标端把资源交给 promoter：启动该实例的服务单元，挂载状态卷，监管进程发现 domain 已在运行，
     认领而不是再次启动；
   - controller 在源节点调用 `unmake-available`，恢复 `allow-two-primaries` 与 protocol，并删除临时的
     diskless 资源。
- 第 5 步中 promoter 能否接手一个已经是 Primary 的资源，是本集成最大的未知数。不行的话，需要在
   切换后让 promoter 重新竞争，而这一刻实例已经在目标端运行。
- vTPM 状态在块设备上时，libvirt 把它当作非共享存储，状态随迁移流写到目标端，而目标端写的是同一个
  设备。这个顺序是否安全未经验证，第一版可以先不支持带 vTPM 的 VM 在线迁移。

### 可观测性

ADR-0015 规定采集和告警不依赖控制面。DRBD 部分：

- **DRBD 状态用 drbd-reactor 自带的 Prometheus 插件**（`[[prometheus]]`，默认 `:9942`），不自己
  实现。它导出资源角色、quorum、各 peer 的连接状态、out-of-sync 字节数等。存储池容量用 LINSTOR
  controller 自带的 `/metrics`。
- **Agent 导出 DRBD 看不到的部分**：各实例的原地重启次数、重启耗尽与 evict、启动失败、屏蔽状态、
  电源意图，以及"实例在非 Primary 节点上运行"（PoC 05 的残留 QEMU）。

| 告警 | 依据 | 来源 |
|---|---|---|
| 冗余降级：副本或 tiebreaker 掉线 | `drbd_connection_state` 不是 `Connected` 持续 5 分钟 | PoC 04 |
| 失去 quorum | `drbd_device_quorum == 0` | PoC 01 |
| 强制 demote 后仍有打开者 | Agent 从 `drbdsetup events2` 导出；drbd-reactor 1.12 的 `drbd_resource_forceiofailures` 恒为 0（PoC 06） | PoC 05 |
| split-brain | 连接状态 `StandAlone` | — |
| 同步卡住 | `drbd_peerdevice_outofsync_bytes > 0` 持续较长时间 | — |
| 实例在非 Primary 节点运行 | Agent 指标 | PoC 05 |
| promoter 的 target 为 active 但本节点不是 Primary | Agent 指标；在 promoter 之外 demote 后 systemd 状态会过期，该节点实际无法再接管 | PoC 07 |
| 发生原地重启或 evict | Agent 计数器增加 | PoC 01 |
| 迁移窗口未关闭 | 资源上的 `allow-two-primaries` 存在超过迁移超时 | — |
| exporter 消失 | `drbdreactor_up` 缺失 | — |

- **不能只看 quorum。** PoC 05 后实测：tiebreaker 下线时 `drbd_device_quorum` 仍是 1，只有对应
  peer 的 `drbd_connection_state` 变成 `Connecting`。剩下两个节点仍构成多数，所以 quorum 不变，
  但再坏一个节点就会全部停机。冗余降级必须按连接状态告警。

## 待验证（Accepted 之前必须完成）

前 6 项原在 ADR-0015 中，编号对应关系见各项括号。

1. （原 ADR-0015 第 1 项）两台以上节点上，用 drbd-reactor promoter 分别跑一个 libvirt VM 和一个 LXC
   容器，拔网线和断电时不会两边同时运行。
   **已完成（2026-10-30）**：见 [PoC 01](../compute/poc-01-drbd-reactor-failover.md)。
2. （原第 2 项）DRBD 参数能否和 PVE 现有的 LINSTOR resource group 共存于同一 LINSTOR 集群；接管一个由
   PVE 创建的 resource 并启动，PVE 侧不再引用它，`allow-two-primaries` 已删除。
3. （原第 9 项）`OnFailure=` 触发 `drbd-reactorctl evict`，完成"原地重启 → 换节点"；`<on_crash>` 与
   watchdog。
   **已完成（2026-10-30）**：见 PoC 01；LXC 版见 [PoC 04](../compute/poc-04-metadata-media-lxc-supervisor.md)。
4. （原第 11 项）实例状态卷：NVRAM、vTPM 状态和电源意图随冷切换迁移；有意关机的实例在原节点故障后
   不会被其他节点开机。
   **已完成（2026-10-30）**：文件形式见 [PoC 03](../compute/poc-03-state-volume-and-bridge-netfilter.md)。
   改为 raw volume 后见第 10 项。
5. （原第 12 项）第一批告警接入中央 VictoriaMetrics，逐条触发，确认 Telegram 能收到。
   **部分完成（2026-10-31）**：见 [PoC 06](../compute/poc-06-observability.md)；接入中央 VictoriaMetrics
   与 Telegram 待实现时进行。
6. （原第 13 项）启动失败时 `evict --keep-masked`；屏蔽的解除。
   **已完成（2026-10-31）**：见 [PoC 13](../compute/poc-13-start-failure-keep-masked.md)。
7. `on-quorum-loss = "freeze"`：quorum 短暂丢失后恢复，VM 和 LXC 都能解冻并继续运行；其他分区接管时
   旧 Primary 停止服务，不会双开；客户机在冻结期间的时钟跳变可以接受。
8. VM 在线迁移：LINSTOR `auto_manage_dual_primary` 打开与关闭窗口、目标端显式 promote 时 promoter
   不插手、切换后 promoter 接手已是 Primary 的资源、监管进程认领；迁移中途失败时实例仍只在一个节点
   运行；限速下 1G 链路上 DRBD 不出现 quorum 抖动。
9. 逐副本更换存储池或副本节点期间，实例持续运行、quorum 不丢。
10. Debian 13 上：DRBD 9 内核模块、drbd-reactor 与 LINSTOR 的安装；NVRAM 与 vTPM 状态改为 raw volume
    后随冷切换迁移；vTPM 在块设备上的在线迁移是否安全；重新确认依赖版本的 PoC 结论，例如 systemd 255
    下 `Restart=` 与 `OnFailure=` 的冲突（Debian 13 为 systemd 257）。
11. hostname 的 `required` 反亲和：被选中的实例所在节点故障后，冷启动不会落到另一成员的节点；没有可用节点时保持
    停止并告警。

## 结果

- 计算核心只认存储能力，LINSTOR/DRBD 的全部细节集中在本 ADR 与 `internal/compute/adapter/linstor`。
- 不写 libvirt 存储后端，LINSTOR 卷不出现在 `virsh vol-list` 中。
- 迁移窗口交给 LINSTOR 1.35 的 API，与 LINBIT 自己的集成方向一致；没有先例的部分收窄为显式
  promote 与 promoter 交接。
- 依赖 Debian 13 底座提供的 libvirt ≥ 10.10。
