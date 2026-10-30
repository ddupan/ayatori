# ADR-0015：自建 libvirt/LXC 计算后端，冷启动由节点自治

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-10-30
- 依据：维护者讨论；修订 [ADR-0007](0007-compute-node-and-vm-boundary.md) 中北向 VM API、Agent
  与 HA 的部分，与 [ADR-0013](0013-api-only-control-plane-on-lxc.md)、
  [ADR-0014](0014-kaas-cross-backend-node-supply.md) 保持一致。

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
  `<source network='vlan20'/>`。Agent 不做任何替换，由 libvirt 自己解析。
- **候选节点由 controller 计算**：domain 引用的 pool 和 network 都已下发到的节点；若引用本地
  pool 上的卷，则只有该卷所在节点。
- **domain/LXC 共有的 Ayatori 自有字段：** `nodeName` 或候选节点、`ha`（`Restart`/`None`）、
  `runState`。
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
- **读权限无法按节点限制。** 每个 Agent 都能读到所有配置，因此配置中不得包含密钥，Ceph key 等
  一律通过 Bao 交付。

### 存储

Ayatori 不抽象磁盘格式。存储能力尽量通过 libvirt 原生 pool 表达：

| 存储 | libvirt 表达 | LXC rootfs | 能否换节点 |
|---|---|---|---|
| Ceph RBD | `rbd` 类型 pool，各节点定义同一个 pool，卷共享；Ceph key 通过 libvirt secret 交付 | Agent krbd map 后挂载 | 能（HA 待定） |
| 本地 | `dir`/`logical`/`zfs` 类型 pool | Agent 取卷路径直接使用或挂载 | 不能，固定在卷所在节点 |
| LINSTOR/DRBD | **例外**，见下 | 挂载 DRBD 设备 | 能 |

**LINSTOR 没有 libvirt pool 驱动**，是本设计中唯一的非原生部分：

- `LibvirtStorageVolume` 增加 `backend: linstor` 分支，由 controller 调用 LINSTOR API 创建卷，
  并在候选节点上放置副本或 diskless 资源。
- domain 中写 `<disk type='block'>` 和 `/dev/drbd/by-res/<res>/0`。该路径在所有节点上一致，
  本身就是稳定引用。
- 一个受保护的实例对应一个 LINSTOR resource，多块盘用该 resource 的多个 volume。
- VM 卷和子集群的 PVC 使用不同的 LINSTOR resource group。

**LXC 复用 libvirt 对象作为节点上的登记表**：liblxc 不认识 libvirt 对象，由 Agent 解析——
`rootfs` 引用的 volume 从 libvirt 取得路径；`networks` 引用的 network 从其定义中查出 bridge，
填入 `lxc.net.N.link`。VM 与 LXC 因此共用同一份 pool/network 声明。

LXC 的 rootfs 第一版使用 `download` 模板拉取 images.linuxcontainers.org 的镜像。卷克隆和镜像
管理以后再做。

### 网络

- 宿主机上的 bridge 和 VLAN 由 Chef 管理，属于宿主机基础配置，放进各环境的 Policyfile。
- `LibvirtNetwork` 使用 `<forward mode='bridge'>` 指向 Chef 创建的 bridge。libvirt 只在 OVS 下
  才会打 VLAN tag，使用 Linux bridge 时不交给它；将来换成 OVS 后，`<vlan><tag>` 可以直接写在
  network XML 中。
- IPAM 暂不做：沿用现有 DHCP 或在 cloud-init 中写静态 IP。domain 中固定 MAC，冷启动换节点后
  IP 不变；实际 IP 通过 qemu-guest-agent 或 DHCP 租约回写 status。

### 自动冷启动：节点自治，不依赖控制面

参照 vSphere HA：故障判定和重启由宿主机上的 agent 完成，vCenter 只负责下发配置；真正防止
双开的是 VMFS 文件锁，不需要全集群 quorum。

- **DRBD 的单 Primary 相当于 VMFS 锁。** 使用 drbd-reactor 的 promoter 插件：
  - 候选节点竞争 promote，赢的节点启动 `start` 中列出的单元；
  - 失去 quorum 的节点停止服务。
- **DRBD 参数**：`quorum majority`、`auto-promote no`、`on-no-quorum io-error`，
  `on-no-data-accessible` 与 `on-no-quorum` 取相同的值。至少 3 个副本，或者 2 个副本加
  diskless tiebreaker。drbd-reactor 不做节点级 STONITH，完全依赖 DRBD quorum。
- **promoter 的 `start` 内容：**
  - VM：`ayatori-vm@<name>.service`，用 Agent 预先放好的定义启动。
  - LXC：`ocf:heartbeat:Filesystem` 挂载 rootfs，然后是 `lxc@<name>.service`。
- **fencing 的粒度是单个卷**，而不是整台宿主机。网络分区最多让个别实例停止，不会重启宿主机。
- **谁来启动实例：**
  - DRBD 上 `ha: Restart` 的实例只由 promoter 启动，Agent 不得直接启动，否则会绕过单 Primary
    互斥。
  - 其他实例由 Agent 根据本地状态中的 `runState` 启动，包括本地存储实例在宿主机重启后的原地
    拉起。
- **控制面只观察**实例实际跑在哪个节点，并写入 status。实例换了节点不算漂移，控制面不得把它
  "纠正"回去。
- **按存储类型的能力：**
  - DRBD：自动冷启动。
  - 本地：只在原节点自动拉起。
  - Ceph：RBD 的 exclusive-lock 是协作式的，不能防止两个节点同时写，fencing 需要加入黑名单
    （blocklist），drbd-reactor 也覆盖不到。出现真实需求时再设计。
- **容量预留**在创建时由控制面检查（对应 vSphere 的 admission control）。运行时不检查容量，接受
  偶尔超卖的风险。

### 与 kubelet 共用宿主机

- **两个 Node 对象分属不同的 apiserver。** Agent 写到 Ayatori，kubelet 写到 workload 集群，
  两者互不相关。
- **容量第一版静态切分：**
  - 内存用 `systemReserved`/`kubeReserved` 扣掉留给 VM/LXC 的部分。
  - CPU 用 `reservedSystemCPUs`，配合 libvirt 的 `<vcpupin>`/`<emulatorpin>`。
- **cgroup：** libvirt 在 `machine.slice`，LXC 在自己的 payload cgroup，kubelet 在
  `kubepods.slice`，统一使用 systemd 驱动。
- **网络：** k8s 要求打开 `net.bridge.bridge-nf-call-iptables=1`，打开后 VM/LXC 走 bridge 的流量
  也会经过宿主机的 iptables/nftables，可能被 CNI 或 kube-proxy 的规则误伤。必须先验证，再在
  macvtap 和 nft 排除规则之间做选择。

### 自举与 break-glass

etcd、DNS，甚至 Ayatori 控制面本身，都可能迁到本后端管理的 LXC 中。这种"控制面运行在它管理的
资源上"的做法成立，前提是：

- 运行中的实例和自动冷启动都不依赖控制面，靠的是 Agent 的本地状态、libvirt/LXC 的持久配置和
  drbd-reactor。
- 控制面完全丢失时，在节点上用 `virsh` 或 `lxc-*` 手工拉起控制面所在的实例；Agent 恢复后按
  名字认领已有实例，不能重复创建。这条路径必须写成 runbook 并实际演练过。
- Agent、LINSTOR 和 apiserver 的连接地址不得依赖被迁入本后端的 DNS。

## 待验证（Accepted 之前必须完成）

1. 两台以上节点上，用 drbd-reactor promoter 分别跑一个 libvirt VM 和一个 LXC 容器。各测一次
   拔网线和断电，确认不会两边同时运行，记录冷启动所需时间。
2. 确认 DRBD 参数（`auto-promote no`、quorum 等）能否和 PVE 现有的 LINSTOR resource group
   共存，或者是否需要单独的 resource group。
3. 各节点下发同名 `rbd`、`logical` pool 和 bridge 型 network 后，domain 使用原生引用，冷启动换
   节点仍能正常解析；LXC 能通过 Agent 解析同一份 pool/network。
4. Windows 和至少一个 retro 系统（例如 Win9x/DOS，i440fx + IDE + SB16）能否通过 domain XML 透传
   运行。
5. `go-lxc` 的 cgo 构建方式和 Agent 的发布形态。
6. 宿主机同时运行 kubelet 和 CNI 时，`bridge-nf-call-iptables` 对 VM/LXC 流量的实际影响。
7. 用 ValidatingAdmissionPolicy 按节点身份限制 Agent 写 status 是否可行。
8. 控制面完全丢失后的手工恢复演练。
9. 控制面不可用时重启宿主机：Agent 按本地状态原地拉起本地存储实例，DRBD 实例由 promoter
   拉起；控制面恢复后，离线期间被删除的对象能被正确清理，不会误删其他对象。

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

以下能力目前没有真实需求，出现需求时参照对应的 vSphere 设计：

- **DRS 亲和/反亲和规则**：例如 etcd 成员不得落在同一宿主机。作为候选节点的过滤条件实现，
  不做 DRS 式的自动负载均衡迁移。
- **维护模式**：Node 设为 unschedulable，并按实例的存储类型冷迁或停机，对应 vSphere 进入维护
  模式时的 evacuation。
- **VMCP（APD/PDL）**：存储路径失效但主机存活时如何处理实例。对应 DRBD 失去 quorum 后
  `io-error` 与 `suspend-io` 的取舍，PoC 时一并观察。
- **按故障切换容量的 admission control**：目前只在创建时检查容量。需要保证"任一宿主机故障后
  剩余容量仍够用"时，参照 vSphere 的槽位或百分比策略。
- **重启优先级与顺序**：基础服务（etcd、DNS）先于依赖它们的实例启动。

明确不参照的部分：vMotion 式热迁移和 Fault Tolerance 不是本后端的目标（ADR-0007）。
