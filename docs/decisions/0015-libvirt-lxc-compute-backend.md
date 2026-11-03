# ADR-0015：自建 libvirt/LXC 计算后端，冷启动由节点自治

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-10-30
- 依据：维护者讨论；修订 [ADR-0007](0007-compute-node-and-vm-boundary.md) 中北向 VM API、Agent
  与 HA 的部分，与 [ADR-0013](0013-api-only-control-plane-on-lxc.md)、
  [ADR-0014](0014-kaas-cross-backend-node-supply.md) 保持一致。
- 修订：2026-11-03 LINSTOR/DRBD 的部分（卷、自动冷启动的机制、DRBD 可观测性）拆到
  [ADR-0018](0018-linstor-drbd-storage-integration.md)，本 ADR 只保留计算核心与存储能力的契约。

## 问题

- **Proxmox HA 不可靠，也不符合需求。** 我们需要的不是真正的 HA，而是宿主机故障后在其他节点
  **自动冷启动**。PVE HA 把 corosync 全集群 quorum 和 watchdog 自我 fencing 绑在一起，网络抖动
  可能导致整台宿主机重启。
- **VM 和 LXC 的工作负载不只是固定声明的资源**，也有动态创建和销毁的实例。
- **VM 的配置项非常多**，包括 Windows 和 retro VM 需要的 i440fx、IDE、软驱、SB16、
  cirrus/std VGA、纯 BIOS 引导等设备模型。
- **LXC 系统容器资源占用低，又是完整的 OS**，适合承载 etcd、DNS 等目前跑在 LXC 中的基础服务。
- **存储有三类：** 现有的 LINSTOR 共享存储，未来可能接入的远程 Ceph，以及本地存储。
- **宿主机的控制权要留在我们手里。** 将来可能把 Kubernetes 扩展到这些宿主机上调度 Pod。

## 评估

- **Incus**：VM 基本只有 q35、virtio 和 UEFI 一条路径，`raw.qemu` 只能零散地补几处，承载不了
  Windows 和 retro VM。它的定位是 microcloud，不是完整的 hypervisor，而且 incusd 会接管宿主机
  上的 bridge 和存储。
- **CloudStack**：引入它的成本和 OpenStack 相当。
- **OpenNebula**：对需求来说过重，会引入多个控制组件，并接管宿主机。
- **KubeVirt**：VM spec 是强类型的，设备模型有限；VM 的生命周期和 HA 依附在 Kubernetes 控制面
  上。
- **libvirt 的 LXC 驱动**：上游处于边缘维护状态，用起来不顺手。
- **自己抽象一个强类型的通用 VM API**：会越做越像一个残缺的 domain XML。
- **控制面直接通过远程 libvirt 管理节点**：控制面需要持有所有宿主机的 root 凭据。而且冷启动
  无论如何都需要节点上的 Agent，因此不采用。
- **Ayatori 自定引用约定（如 `pool='ayatori'`），由 Agent 下发前替换**：用户必须学习 Ayatori 私有
  约定，节点上 `virsh` 看到的也和声明不一致。改为由中央下发 libvirt 原生 pool/network 对象。
- **强类型字段 + 设备 profile 生成 domain XML**：能降低手写 XML 的门槛，但 pool/network 统一后
  门槛已大幅下降，暂不做，等手写 XML 被证明是反复的负担时再评估。
- **单独的 REST API 服务**：动态资源对 CRD 不构成阻碍，Pod 本身就是高频创建和销毁的对象。自建
  REST 服务要重新实现 watch、RBAC、审计、乐观并发和版本化，Agent 也就没法再用 informer watch。

## 决策

### 定位与分层

- 计算层是 Ayatori 的一个**独立 backend**，地位和 PVE 相同。它有独立的模块和 API group
  `compute.ayatori.ddupan.top`，但复用 Ayatori 现有的 kube-apiserver（ADR-0013），不另起
  apiserver。将来如果 churn 或权限隔离被证明需要拆分，因为 API group 是独立的，迁移成本也低。
- ADR-0014 的 `Machine` 是它的消费方之一，其 libvirt adapter 负责生成 `LibvirtDomain`。
- 临时实例的调用方直接使用本 API，不经过 Ayatori 的其他资源。

组件：

| 组件 | 位置 | 负责 |
|---|---|---|
| compute controller | 中央，一个进程 | 检查配置能否解析；按 pool/network/volume 的分布计算候选节点；创建时检查容量；调用 LINSTOR API 创建卷并按候选节点放置；finalizer 和删除编排 |
| Compute Agent | 每台宿主机 | 写 Node/Lease（ADR-0007）；watch 与本节点相关的对象；在本机定义 pool、network、volume、domain；调用 liblxc；定义 libvirt secret；生成 drbd-reactor 配置；回写实际运行位置和状态 |
| drbd-reactor（LINBIT） | 每台宿主机 | 运行时决定由哪个节点运行实例 |

### 资源

北向**透传原生配置**，不另做抽象。本后端是**把 libvirt 对象下发到各节点的中央控制器**：
domain、pool、network、volume 都使用 libvirt 原生 XML，用户只需要懂 libvirt 本身的概念，
各节点上 `virsh pool-list`/`net-list` 看到的结果与声明一致。

| 资源 | 内容 | 下发到哪 |
|---|---|---|
| `LibvirtStoragePool` | 原生 pool XML + `nodeSelector` | 匹配的节点 |
| `LibvirtNetwork` | 原生 network XML + `nodeSelector` | 匹配的节点 |
| `LibvirtStorageVolume` | pool 引用 + 原生 volume XML 或 `size`；可选 `source.url` 导入镜像；`reclaimPolicy`（`Retain`/`Delete`） | 共享 pool 在任一就绪节点创建一次；本地 pool 在指定节点创建 |
| `LibvirtDomain` | `spec.domainXML` 原生 domain XML，`virsh dumpxml` 的输出可以直接使用 | 候选节点 |
| `LXCContainer` | `spec.config` 原生 lxc config 文本，`rootfs` 引用 volume，`networks` 引用 network | 候选节点 |

- domain 中直接写原生引用：`<source pool='ceph-ssd' volume='win98'/>`、
  `<source network='vlan20'/>`。network 引用由 libvirt 自己解析；**卷引用由 Agent 在 `DefineXML`
  之前用 `virsh vol-path` 解析成 `file`/`block` 路径**，因为 AppArmor（`virt-aa-helper`）不会把
  `type='volume'` 的源加入每个 VM 的 profile，QEMU 会被拒绝访问（PoC 02）。
- **候选节点由 controller 计算**：domain 引用的 pool 和 network 都已下发到的节点；若引用本地
  pool 上的卷，则只有该卷所在节点。
- **domain/LXC 共有的 Ayatori 自有字段：** `nodeName` 或候选节点、`ha`（`Restart`/`None`）、
  `runState`。
  - `runState: Running` 只表示"创建后和宿主机故障后应当拉起"，不表示"任何时候都必须在运行"。
  - `ha: Restart` 只对**崩溃**生效（libvirt 原因为 `crashed`/`failed`、QEMU 或容器 init 消失）。
    客户机主动关机（`shutdown`）、运维人员在控制台关机或 `destroy`，都是有意的停止，不重启，与
    vSphere HA 和 KubeVirt `RerunOnFailure` 的语义一致（PoC 01）。
  - 开关机、重置、换盘等运行时操作不改 spec，见 [ADR-0016](0016-instance-node-services.md)。
- **由 Ayatori 填写的字段，用户写了也会被覆盖：**
  - domain 的 `name`、`uuid`；
  - lxc 的 `lxc.uts.name`、`lxc.rootfs.path` 和网络 link。
- **被引用的 volume 不能删除**（与 PVC 保护相同，用 finalizer 实现）；删除 domain 默认不删除卷。
- XML 中直接写死 `/dev/...` 或 bridge 名同样允许，但 Ayatori 不管理这些资源，也无法据此计算
  候选节点，相应的实例不进入自动冷启动。

**本后端是内部控制器，不对外提供服务，因此不限制原生配置的内容。** `<qemu:commandline>`、
`<hostdev>`、`lxc.hook.*`、privileged 容器等都可以直接使用。controller 只检查配置能否解析，
以及 Ayatori 负责填写的字段，不维护黑名单或白名单。

这样做的代价是：能创建对象就等于拥有宿主机 root 权限。因此：

- 本 API group 的写权限通过 RBAC 只授予管理员和受信任的内部组件，例如 `Machine` 的 adapter。
- 所有变更通过 apiserver 审计日志留痕。

### 下发与漂移

- **libvirt：**
  - 下发时在 domain 的 `<metadata>` 中写入 Ayatori 命名空间元素，记录 generation 和输入 hash。
  - 只有 spec 变化时才调用 `DefineXML`，不 diff `dumpxml` 的输出。libvirt 自动补充的默认值
    （PCI 地址、MAC、controller）不算漂移。
  - 订阅 libvirt 的 `DEFINED` 事件；hash 不一致说明有人在带外修改过，重新下发。
  - inactive 配置与运行中配置不一致时，设置 `RestartRequired`。
- **LXC：** Agent 使用 `go-lxc` 绑定 liblxc，不 fork `lxc-*` 命令。配置文件以文件 hash 判断是否
  漂移。
- **libvirt autostart 必须关闭。** 实例启动只由 Agent（依据本地状态）或 drbd-reactor 决定。

### Agent 与控制面的通信

- **Agent 维护本地状态，watch 只负责更新它。** 参照 vSphere FDM 在每台主机本地缓存受保护
  VM 列表：
  - 链路为 informer → 本地状态 → 本地 reconcile → libvirt/liblxc/drbd-reactor。本地
    reconcile 只读本地状态，从不直接读 API。
  - 本地状态按对象保存 spec、generation 和 resourceVersion，每个对象一个文件，写临时文件后
    fsync 再 rename，保证原子。只接受 resourceVersion 更新的版本。
  - Agent 启动时先加载本地状态并完成一轮 reconcile，再去连接 API。控制面不可用时，节点重启后
    仍能按本地状态拉起实例。
  - **删除只在 API 可达时发生**：watch 收到删除事件，或者重新 list 后对象已不存在，才从本地
    状态移除并清理实例。API 不可达期间绝不清理。
  - 控制面恢复后以 API 为准：更新的 resourceVersion 覆盖本地状态，Agent 回写离线期间的 status。
  - 本地状态只决定"应当是什么"，不决定 DRBD 实例跑在哪个节点（见自动冷启动一节）。
- **Agent 主动 watch Ayatori API**，和 kubelet 的做法一样。节点不开放入站端口，控制面也不持有
  宿主机凭据。
- **controller 给每个候选节点打一个 label**，Agent 按 label selector 只 watch 与自己相关的对象。
- **Agent 身份**：用 SPIRE JWT-SVID 登录 apiserver，沿用 ADR-0014 的 structured authentication。
- **写权限**：用 ValidatingAdmissionPolicy 根据 `request.userInfo`，只允许 Agent 修改候选节点
  包含自己的对象的 status。
- **读权限无法按节点限制。** 每个 Agent 都能读到所有配置，因此配置（包括 cloud-init user-data）中
  不得包含密钥；实例通过 SPIFFE 身份自行向 Bao 取用（ADR-0016）。Ceph key 等
  一律通过 Bao 交付。

### 存储

Ayatori 不抽象磁盘格式。存储能力尽量通过 libvirt 原生 pool 表达：

| 存储 | libvirt 表达 | LXC rootfs | 能否换节点 |
|---|---|---|---|
| Ceph RBD | `rbd` 类型 pool，各节点定义同一个 pool，卷共享；Ceph key 通过 libvirt secret 交付 | Agent krbd map 后挂载 | 能（HA 待定） |
| 本地 | `dir`/`logical`/`zfs` 类型 pool | Agent 取卷路径直接使用或挂载 | 不能，固定在卷所在节点 |
| LINSTOR/DRBD | **例外**，见 ADR-0018 | 挂载 DRBD 设备 | 能 |

**LINSTOR 没有 libvirt pool 驱动**，是本设计中唯一的非原生部分，由 Ayatori 直接集成，见
[ADR-0018](0018-linstor-drbd-storage-integration.md)。

**存储能力**：计算核心不认识具体存储，只按卷所在存储声明的能力决定实例能做什么：

| 能力 | 本地 pool | LINSTOR/DRBD（ADR-0018） | Ceph RBD |
|---|---|---|---|
| 换节点 | 复制卷后才能换 | 能 | 能 |
| 自动冷启动 | 不能，只在原节点拉起 | 能 | 待设计 |
| VM 在线迁移 | 经 `--copy-storage-all` 复制磁盘 | 能 | 未设计 |
| 不停机换存储池 | VM 经 `blockcopy` | 能，LXC 也可以 | 未设计 |

**LXC 复用 libvirt 对象作为节点上的登记表**：liblxc 不认识 libvirt 对象，由 Agent 解析——
`rootfs` 引用的 volume 从 libvirt 取得路径；`networks` 引用的 network 从其定义中查出 bridge，
填入 `lxc.net.N.link`。VM 与 LXC 因此共用同一份 pool/network 声明。

LXC 的 rootfs 第一版使用 `download` 模板拉取 images.linuxcontainers.org 的镜像。镜像与介质的
分发见 [ADR-0017](0017-compute-day2-operations.md)，卷克隆以后再做。

### 网络

- 宿主机上的 bridge 和 VLAN 由 Chef 管理，属于宿主机基础配置，放进各环境的 Policyfile。
- `LibvirtNetwork` 使用 `<forward mode='bridge'>` 指向 Chef 创建的 bridge。libvirt 只在 OVS 下
  才会打 VLAN tag，使用 Linux bridge 时不交给它；将来换成 OVS 后，`<vlan><tag>` 可以直接写在
  network XML 中。
- IPAM 暂不做：沿用现有 DHCP 或在 cloud-init 中写静态 IP。domain 中固定 MAC，冷启动换节点后
  IP 不变；实际 IP 通过 qemu-guest-agent 或 DHCP 租约回写 status。

### 自动冷启动：节点自治，不依赖控制面

参照 vSphere HA：故障判定和重启由宿主机上的 agent 完成，vCenter 只负责下发配置；真正防止
双开的是存储锁（VMFS 文件锁），不需要全集群 quorum。

- **防止双开由存储提供**，因此自动冷启动是存储能力：LINSTOR/DRBD 用单 Primary + quorum 与
  drbd-reactor promoter 实现，见 [ADR-0018](0018-linstor-drbd-storage-integration.md)；Ceph 的 RBD
  exclusive-lock 是协作式的，fencing 需要加入黑名单（blocklist），出现真实需求时再设计。
- **谁来启动实例**：
  - 具备自动冷启动能力的存储上、`ha: Restart` 的实例，换节点只由该存储集成的机制决定，Agent 不得
    在其他节点启动它；
  - 其他实例由 Agent 根据本地状态中的 `runState` 启动，包括本地存储实例在宿主机重启后的原地拉起。
- **电源意图**：有意关机的实例不应被重新拉起。本地实例记在 Agent 的本地状态中；可换节点的实例必须
  记在随实例迁移的位置，DRBD 上是实例的状态卷（ADR-0018）。
- **控制面只观察**实例实际跑在哪个节点，并写入 status。实际位置由存储集成判定（DRBD 以 Primary 且有
  quorum 为准）。实例换了节点不算漂移，控制面不得把它"纠正"回去。
- **容量预留**在创建时由控制面按实例保障量检查（对应 vSphere 的 admission control），见"资源模型"。

### 可观测性：告警不依赖控制面

HA 降级通常是静默的：PoC 04 中两个资源的 tiebreaker 掉线 11 小时，期间一切照常运行，直到下一次
故障才暴露；PoC 05 中 evict 后残留的 QEMU 也没有任何提示。因此 HA 的健康状态首先是告警问题，
前端展示（ADR-0016）只是补充。

- **采集和告警路径与 HA 一样不依赖控制面**：中央 VictoriaMetrics 直接抓取节点上的 exporter，沿用
  现有的规则与 Telegram 通知。Ayatori apiserver、controller 和前端都不在这条路径上。
- Agent 导出各实例的状态；"应运行的实例没有任何节点在运行"按 `runState` 判断并告警。存储相关的
  告警由各存储集成定义，DRBD 见 ADR-0018。
- Agent 指标的实例名用 `name` 标签，不用 `instance`：后者是 Prometheus 的抓取目标标签，重名会被
  改写为 `exported_instance`，按实例聚合的规则会全部算错（PoC 06）。
- 控制面可用时，Agent 同时把这些状态写进 status 和 Event，供前端和 `kubectl` 查看；告警不依赖
  这一路。

### 资源模型：实例有保障，Pod 是弹性层

**Node 的内存语义**照搬 kubelet（ADR-0007 的 Node 由 Agent 写入）：

- `status.capacity`：整机真实内存。
- `status.allocatable`：整机内存减去宿主机自身预留，即实例可用的上限。
- `MemoryPressure` 等 condition：按真实可用内存判定，压力大时控制面不再往该节点放新实例。
- 实际用量走指标（见"可观测性"），不写进 Node。

**实例的三个量**，对应 vSphere 的 reservation、limit、shares：

| | 含义 | VM | LXC |
|---|---|---|---|
| 配置量 | 实例看到的内存 | domain XML 的 `<memory>`（`<currentMemory>` 为 balloon 目标） | `lxc.cgroup2.memory.max`，**必须设置**，由 Agent 按 spec 生成 |
| 保障量 | 承诺给到、不会被回收的部分；默认等于配置量 | Agent 在实例启动后写入其 scope 的 `memory.low`（libvirt 的 `min_guarantee` 不支持 QEMU），并逐级设置祖先 slice 的 `MemoryLow`，不小于其下各级之和（PoC 14） | 同左（payload cgroup 直接在根下） |
| 权重 | 争抢时的比例 | `cpu.weight` 等 | 同左 |

- **准入只按保障量记账**：本节点实例保障量之和不超过 allocatable。配置量之和可以超过物理内存，即超售；可按 pool
  设置超售比例上限（类似 OpenStack 的 `ram_allocation_ratio`）。冷切换所需的容量同样按保障量计算。
- **VM 默认开启 free page reporting**（`<memballoon model='virtio' freePageReporting='on'/>`），客户机释放的内存还给
  宿主机（PoC 12）。KSM 按需开启。
- 实例超出保障量的部分要能被回收，需要宿主机 swap/zswap；否则超售 VM 在内存紧张时只能被 OOM 杀掉（等同崩溃）。
  PoC 14 中开启 swap + zswap 后，实例超出保障量的部分被换出，没有任何 OOM，客户机照常工作。Pod 不使用 swap
  （`k8s.slice` 设 `MemorySwapMax=0`，kubelet 加 `--fail-swap-on=false`）。
- `memory.low` 是软保护：各级保护之和超过扣除内核开销后的物理内存时会被突破（PoC 14）。准入时保障量之和不得超过
  allocatable，宿主机预留要覆盖宿主机进程的实际占用并给内核留出余量。不用 `memory.min` 做实例保障，它会把 OOM
  推给别人。

**Pod 是弹性层**，用剩下的内存，压力大时最先被牺牲：

- kubelet 以 `--cgroup-root=/k8s.slice` 运行在 Agent 管理的 `k8s.slice` 下，`systemReserved` 只覆盖宿主机自身。
  kubelet 的 allocatable 因此偏大，允许 Pod 使用实例暂时没用到的内存。
- **Agent 动态设置 `k8s.slice` 的 `MemoryMax`** = 整机内存 − 宿主机预留 − 本节点正在运行的实例保障量之和。实例
  （包括冷切换过来的实例）启动前先压低上限，停止后放宽。重启后上限会丢失，Agent 必须在 kubelet 启动前设好。
- 超用时在 `k8s.slice` 内被 OOM，BestEffort 最先；节点和实例不受影响（PoC 12）。没有硬上限时，一个 Pod 就能让整台
  宿主机失去响应、实例卡住且不会冷切换（PoC 11）。**systemd-oomd 在无 swap 时救不了节点**（PoC 12），只能作补充。
- 代价：实例冷切换到某节点时，那里的 Pod 可能被挤掉。需要硬保障的 Pod 应放在不运行实例的节点上。
- 不采用"每个实例在 workload 集群建占位 Pod、由 k8s 调度器统一记账"：实例冷启动将依赖 workload 集群的
  apiserver，违背"HA 不依赖控制面"。
- CPU 以 `cpu.weight` 按比例共享，本来就没有闲置问题；只有做了 CPU 绑定的实例需要静态划分
  （`reservedSystemCPUs` 配合 `<vcpupin>`/`<emulatorpin>`）。

### 与 kubelet 共用宿主机

- **两个 Node 对象分属不同的 apiserver。** Agent 写到 Ayatori，kubelet 写到 workload 集群，
  两者互不相关；内存的分层见上一节。
- **cgroup：** libvirt 在 `machine.slice`，LXC 在自己的 payload cgroup，kubelet 在 Agent 管理的 `k8s.slice` 下。
  k0s 默认使用 cgroupfs 驱动，实施时评估是否统一为 systemd 驱动。
- **网络：** k8s 要求打开 `net.bridge.bridge-nf-call-iptables=1`，这是全局开关，打开后**所有**
  Linux bridge 上的 VM/LXC 流量都会经过宿主机 iptables FORWARD。PoC 03 实测：FORWARD 策略被设为
  DROP（Docker、部分严格 CNI）时实例 bridge 流量 100% 丢包。放行规则必须位于 FORWARD 链内部，另起
  一个更高优先级的 nftables chain 无效。因此运行 kubelet 的节点上，实例网络倾向使用 **OVS**
  （不是 Linux bridge，`br_netfilter` 不作用于它，还能在 network XML 中直接写 VLAN tag）；在
  不运行 kubelet 的节点上，Linux bridge 仍然可用。不得用 `modprobe -r br_netfilter` 回退：它会连带
  卸载 `bridge` 模块，宿主机上所有 Linux bridge 随之消失（PoC 04）。

### 自举与 break-glass

etcd、DNS，甚至 Ayatori 控制面本身，都可能迁到本后端管理的 LXC 中。这种"控制面运行在它管理的
资源上"的做法成立，前提是：

- 运行中的实例和自动冷启动都不依赖控制面，靠的是 Agent 的本地状态、libvirt/LXC 的持久配置和
  drbd-reactor。
- 控制面完全丢失时，在节点上用 `virsh` 或 `lxc-*` 手工拉起控制面所在的实例；Agent 恢复后按
  名字认领已有实例，不能重复创建。这条路径必须写成 runbook 并实际演练过。
- Agent、LINSTOR 和 apiserver 的连接地址不得依赖被迁入本后端的 DNS。

## 待验证（Accepted 之前必须完成）

1. drbd-reactor 冷切换：已移至 [ADR-0018](0018-linstor-drbd-storage-integration.md) 第 1 项。
2. 与 PVE 现有 LINSTOR resource group 共存：已移至 [ADR-0018](0018-linstor-drbd-storage-integration.md) 第 2 项。
3. 各节点下发同名 `rbd`、`logical` pool 和 bridge 型 network 后，domain 使用原生引用，冷启动换
   节点仍能正常解析；LXC 能通过 Agent 解析同一份 pool/network。
   **大部分完成（2026-10-31）**：`logical` pool 与 bridge 型 network 通过，见
   [PoC 10](../compute/poc-10-native-pool-network-refs.md)；`rbd` 待有 Ceph 时验证。
4. Windows 和至少一个 retro 系统（例如 Win9x/DOS，i440fx + IDE + SB16）能否通过 domain XML 透传
   运行。
   **部分完成（2026-10-30）**：设备模型通过，见 [PoC 02](../compute/poc-02-retro-and-windows.md)；
   NVRAM/vTPM 跟随冷切换已在 PoC 03 通过；完整安装和 virtio 驱动未测。
5. `go-lxc` 的 cgo 构建方式和 Agent 的发布形态；能否取得容器 init 的 wait status（PoC 04）。
   **已完成（2026-10-31）**：见 [PoC 08](../compute/poc-08-go-lxc.md)。go-lxc 动态链接 liblxc，Agent
   按节点发行版构建。liblxc 的 API、monitor 和 hook 都拿不到停止原因；因此 LXC 改为只看电源意图
   标记，不解析日志（见 ADR-0018"自动冷启动"）。
6. 宿主机同时运行 kubelet 和 CNI 时，`bridge-nf-call-iptables` 对 VM/LXC 流量的实际影响。
   **已完成（2026-10-31）**：网络机制见 PoC 03；kubelet 共存见
   [PoC 11](../compute/poc-11-kubelet-coexistence.md)；内存分层与动态上限见
   [PoC 12](../compute/poc-12-memory-tiers-and-overcommit.md)。CPU 绑定与其他 CNI 未测。
7. 用 ValidatingAdmissionPolicy 按节点身份限制 Agent 写 status 是否可行。
   **已完成（2026-10-31）**：可行，见 [PoC 09](../compute/poc-09-agent-status-vap.md)。按节点分开的
   status 字段待 API 设计时确定。
8. 控制面完全丢失后的手工恢复演练。
9. `OnFailure=` 触发 evict：已移至 [ADR-0018](0018-linstor-drbd-storage-integration.md) 第 3 项。
10. 控制面不可用时重启宿主机：Agent 按本地状态原地拉起本地存储实例，DRBD 实例由 promoter
   拉起；控制面恢复后，离线期间被删除的对象能被正确清理，不会误删其他对象。
11. 实例状态卷：已移至 [ADR-0018](0018-linstor-drbd-storage-integration.md) 第 4 项。
12. 第一批告警：已移至 [ADR-0018](0018-linstor-drbd-storage-integration.md) 第 5 项。
13. 启动失败时 `evict --keep-masked`：已移至 [ADR-0018](0018-linstor-drbd-storage-integration.md) 第 6 项。
14. 宿主机"活着但不响应"（内核与 DRBD 正常、用户态卡死）时的处理。
    **已决定（2026-10-31）**：不做专门处理，按宿主机故障对待。DRBD 连接随之中断时与断电相同，自动冷切换
    （PoC 13 中真实发生）；DRBD 仍连着时依赖告警（`ExporterDown`、`InstanceDown`）与人工经 PVE/IPMI
    重置，不做 Agent 自我 fencing。资源模型中的硬上限已消除 PoC 11 中的触发原因。
15. 实例超售：宿主机 swap/zswap 与 KSM 下，`memory.low` 能否保护保障量、超出部分能否被回收而不导致 QEMU 被 OOM。
    **已完成（2026-10-31）**：swap/zswap 部分通过，见 [PoC 14](../compute/poc-14-instance-overcommit-swap.md)；
    KSM 与可压缩负载下 zswap 的收益未测。

## 结果

- 对 ADR-0007 的修订：
  - 北向不再是强类型的通用 `VirtualMachine`，改为透传原生配置的 `LibvirtDomain` 和
    `LXCContainer`。
  - HA 运行时下沉到节点。"旧节点已被 fenced"由 DRBD quorum 和单 Primary 证明，而不是由控制面
    判定。
  - Node/Lease 的用法保持不变。
- 北向和 libvirt、LXC 绑定：换掉实现就要换 API。这是有意的取舍，换来的是完整的配置表达力。
- 原生配置不做限制，本 API 的写权限等同于宿主机 root。安全边界落在 RBAC 和审计上，不在配置
  校验上。
- 新增的控制组件只有一个 controller 和一个节点 Agent；drbd-reactor 是 LINBIT 维护的现成组件。
- PVE 继续作为 brownfield 后端，直到本后端可靠覆盖现有 VM 为止。

## 参考设计

本 ADR 的分层基本对应 vSphere。vSphere 的价值在于边界干净：配置在中央，故障判定和重启在节点，
防止双开靠存储锁，三者互不依赖。后续扩展时优先参照它的做法。

| vSphere | 本后端 |
|---|---|
| vCenter：配置、放置、admission control | compute controller |
| hostd + FDM，本地缓存受保护列表 | Compute Agent 与本地状态 |
| VMFS 文件锁 | DRBD 单 Primary + quorum |
| primary 主机重启故障 VM | drbd-reactor promoter |
| 端口组 / dvSwitch | `LibvirtNetwork` + Chef 管理的 bridge |
| datastore | `LibvirtStoragePool` |

逐项对照 vSphere 功能集的结果见[缺口分析](../compute/vsphere-gap-analysis.md)。其中有现状证据的
反亲和、维护模式、VMCP、故障切换容量、备份、迁入等，设计见
[ADR-0017](0017-compute-day2-operations.md)，LINSTOR/DRBD 上的实现见
[ADR-0018](0018-linstor-drbd-storage-integration.md)。仍无需求的：

- **重启优先级与顺序**：基础服务（etcd、DNS）先于依赖它们的实例启动。全站断电恢复成为问题时，
  参照 vSphere 的 orchestrated restart。

明确不参照的部分：Fault Tolerance。VM 的在线迁移与磁盘迁移见 ADR-0017。
