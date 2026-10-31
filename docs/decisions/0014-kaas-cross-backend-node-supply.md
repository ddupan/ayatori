# ADR-0014：KaaS 以 CAPI 管理集群生命周期，Ayatori 供应跨后端节点

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-10-15
- 依据：维护者讨论；细化 [ADR-0006](0006-demand-driven-resource-scope.md) 中 KaaS 候选方向，
  与 [ADR-0007](0007-compute-node-and-vm-boundary.md)、[ADR-0010](0010-credential-delivery-ends-at-openbao.md)、
  [ADR-0012](0012-l4-loadbalancer.md) 的边界保持一致。

## 问题

homelab 中的 Kubernetes 集群越来越多，而且**一个集群的节点会分布在多个平台上**（Proxmox、
Incus，以后可能还有 libvirt）。目前每个集群都是手工建的，节点替换、控制面端点、凭据交付和
GitOps 接入每次都要重复做一遍。

如果把这件事当成通用 VM 调度来做，就会碰到跨平台迁移：南向接口多，没有统一的 vnet，存储
也互不相通，跨平台冷迁移很贵，热迁移基本不可能。

## 评估

- **通用 VM 调度加跨平台迁移**：要统一磁盘格式、网络和存储抽象，homelab 里没有对应的反复
  需求。同一平台内的迁移各后端已经提供（`qm migrate`、`incus move`、`virsh migrate`）。
- **只把 CAPI 当作 KaaS**：CAPI 擅长"一个集群一种 infra provider"。一个 `Cluster` 只有一个
  `infrastructureRef`，多个 provider 混用不是主流路径；libvirt 也没有维护中的 provider。
- **k0smotron `RemoteMachine` 接入 IaC 起好的机器**：任何后端都能接，但它只负责 join/reset，
  不创建也不销毁 VM，节点替换流程会被拆在两处，和不可变节点对不上。
- **Ayatori 直接管理子集群（drain、删除 Node、持有 admin 凭据）**：违反 ADR-0010/0012 中
  "Ayatori 不访问 workload 集群"的边界，而且需要重新实现 CAPI 已有的 Machine 生命周期。
- **集群内复制存储（Longhorn 等）**：数据放在节点盘上，每次重建节点都要同步副本，和不可变
  节点冲突。PVE 三节点跑 Ceph 实测太慢，不予考虑。

## 决策

### 迁移放在 Kubernetes 层，不放在 VM 层

- KaaS 节点是 **cattle**：不可变、随时可以销毁重建，**不做 VM 迁移**。
- workload 跨平台的迁移就是 Kubernetes 的 Pod 重新调度。节点所在平台通过
  `topology.kubernetes.io/zone` 等 label 表达，平台就是故障域。
- 跨平台的网络差异交给 CNI（overlay，或者 BGP 模式和现有路由器对等），只要求节点之间三层
  可达。不追求统一的底层 vnet。
- pet 型 VM 继续由各平台的原生 API 或 IaC 创建。本 ADR 不改变 ADR-0007 中
  `VirtualMachine` 的定位；是否还需要单独的北向 `VirtualMachine`，留待出现真实需求时再评估。

### 分层

```text
Ayatori（API-only，ADR-0013）
  Machine：选平台 → 后端 adapter 建/删 VM（PVE / Incus / libvirt）
  LoadBalancer、PowerDNS Hostname、Database、Bao 交付
        ▲
        │ 消费方（与 ADR-0012 中 CCM 的定位相同）
托管集群（单独部署，是 Ayatori 的一个 backend）
  CAPI core：Machine、drain、MachineHealthCheck、滚动替换
  k0smotron：子集群 hosted control plane、k0s bootstrap
  ayatori-capi-provider：AyatoriMachine ⇄ Ayatori Machine 的薄 shim
        │
子集群 worker（PVE / Incus / libvirt 上的不可变节点）
```

| 层 | 负责 |
|---|---|
| CAPI + k0smotron（托管集群） | 集群生命周期；节点 join、drain、替换的时机；持有子集群 kubeconfig |
| Ayatori | 节点放在哪个平台、怎么建 VM；和 LB/DNS/Bao/Database 的集成 |
| 各后端 | 运行 VM |

- **托管集群和 PVE 一样，只是 Ayatori 的一个 backend。** Ayatori 不运行任何子集群的控制面。
- **Ayatori 是唯一的 CAPI infra provider。** 所有 MachineDeployment 只引用 `AyatoriMachine`，
  这样就避开了 CAPI 混用 provider 的问题，也补上了 libvirt。新增后端只需要在 Ayatori 加一个
  adapter，CAPI 那一侧不用动。
- shim 跑在托管集群，用 namespace 范围的凭据访问 Ayatori API，把 InfraMachine 契约
  （providerID、addresses、ready）翻译成 Ayatori `Machine`。
- drain 和删除 Node 由托管集群上的 CAPI 执行。Ayatori 不访问 workload 集群，ADR-0010/0012
  不需要开例外。
- 放置先保持简单：按 zone 分散，检查各平台容量够不够。不做通用 Placement。

### 控制面

- 子集群用 k0smotron hosted control plane，以 Pod 形式跑在托管集群里。worker 池里只有
  worker。
- datastore 用 kine + PostgreSQL，每个子集群一个 Ayatori `Database`，沿用现有的备份恢复。
- apiserver 端点用 Ayatori `LoadBalancer` 的 VIP 和 PowerDNS Hostname；控制面到 kubelet、
  webhook 的连接走 konnectivity。
- 人访问子集群走 OIDC：apiserver 信任 iam-login（issuer `https://sso.ddupan.top`），用 `groups`
  claim 绑定 RBAC。每个子集群在 iam-login 有一个独立的客户端，ID token 的 `aud` 就是这个
  集群的 client_id，因此一个集群的 token 不能拿到另一个集群重放。客户端和集群一起在
  homelab-infra 里声明，由 `apps/iam-login/terraform` 管理，不进 Ayatori。group 到
  ClusterRoleBinding 的映射放在子集群自己的 GitOps 里。
- 机器身份不经过 iam-login：apiserver 通过 structured authentication configuration 把 SPIRE 的
  OIDC discovery 当作第二个 issuer 信任，以 JWT-SVID 认证。
- apiserver 只依赖 iam-login 的 JWKS（会缓存）。iam-login 暂时不可用时，已签发的 token 仍然
  有效，但新登录和刷新会失败。iam-login 目前是单副本、experimental，在它的可用性提高之前，
  break-glass 路径必须能用。
- admin kubeconfig 只给 CAPI/k0smotron 和 break-glass 使用，交付到 Bao，status 里只公布位置
  （ADR-0010），不分发给人。
- 托管集群是所有子集群的共同依赖，它的可用性和恢复要求要高于任何子集群。

### 存储

- 复用 PVE 上现有的 LINSTOR：子集群通过 linstor-csi（Piraeus）连接这个**外部 LINSTOR
  controller**。k8s 节点只作为 **diskless satellite**，通过 DRBD 走网络挂卷，不在节点上存
  数据副本。
- 子集群使用独立的 resource group，开启 LINSTOR SSL/认证，不能操作 PVE 自己的卷。
- 销毁节点时要在 LINSTOR 里清理对应的 satellite 和 diskless 资源，放在 Machine 删除流程中，
  必须先 detach，再删 LINSTOR node，最后删 VM。
- 能移到集群外的状态尽量移出去：关系型数据走 `Database`，对象走 Bucket（ADR-0011）。

### 系统镜像

不可变镜像必须包含 DRBD 9 内核模块，并且能在 PVE、Incus、libvirt 上启动。倾向 **k0s +
Flatcar**，与 k0smotron hosted control plane 衔接。Talos 官方提供 drbd extension，但它自带
Kubernetes 发行版，和 hosted control plane 不是主流组合，所以作为备选。Fedora CoreOS 因为
kmod 维护成本高，不考虑。

### 子集群内的 Service LoadBalancer

- 子集群需要的 cloud provider 只有 Service LB 这一部分：用 `k8s.io/cloud-provider` 写，把
  `Service type=LoadBalancer` 翻译成 Ayatori `LoadBalancer`，并回写 `ip` 和 `hostname`。
  由消费方部署（ADR-0012）。
- Node 地址、providerID 和 zone label 在 join 时通过 kubelet/k0s 参数写入。Node 删除由节点
  生命周期流程负责。完整 CCM 等出现缺口再做。
- 生成的 LoadBalancer 名称必须从来源 Service 的 namespace/name **确定性推导**（超过 63
  字符时截断并加 hash），不能用随机后缀，也不能用默认的 `"a" + UID`。这样重建后 Hostname
  不变。

### 声明的对象与派生的对象

GitOps 不再是所有 Ayatori 对象的来源：CCM、CAPI shim 会动态创建对象。**实际存在的资源以
Ayatori 状态为准**；git 是顶层意图的来源。每个 Ayatori 对象必须属于下列两类之一：

- **声明的**：在 homelab-infra/GitOps 中，由 Flux 管理。
- **派生的**：由 controller 根据上游意图创建，必须满足：
  - 带有 `app.kubernetes.io/managed-by`、来源集群和来源对象的标识；
  - 正常删除跟随上游（Service 上 `service.kubernetes.io/load-balancer-cleanup` finalizer）；
  - 上游整体消失时，通过 Ayatori 内部的 ownerReference（指向代表该集群的对象）级联回收；
  - 丢失后能由消费方 resync 重建，并认领已有的后端资源（例如 Machine 通过 providerID
    observe 已有 VM），不能重复创建。

手工 `kubectl create`、既不在 git 又没有 owner 的对象，不允许存在。

**下游必须优先通过 DNS 调用 LoadBalancer。** 派生的 LB 只保证**名字稳定**：VIP 重建后可能
变化，Hostname 会随之更新（TTL 60 秒）。需要稳定 IP 的场景（路由器 NAT、防火墙、
NetworkPolicy `ipBlock`、证书 IP SAN）必须使用**声明的** LB 并指定 VIP。CCM 重建时可以从
`Service.status.loadBalancer.ingress` 读回原来的 VIP 去申请指定 VIP，作为尽力而为的优化，
不作为保证。Ayatori etcd 照常备份。

## 待验证（Accepted 之前必须完成）

1. 在 Incus 上起一个 k0s worker，作为 diskless satellite 接上 PVE 的外部 LINSTOR，通过
   linstor-csi 挂载 PVC；销毁节点，记录需要清理的 LINSTOR 对象；测一下跨主机 diskless 的
   延迟。
2. Piraeus 外部 controller 的配置方式和版本要求，以及 Flatcar 上 DRBD 9 模块的提供方式。
3. k0smotron hosted control plane + kine/PostgreSQL + konnectivity 能否以 Ayatori LB VIP 作为
   端点正常运行。
4. 一个最小的 `AyatoriMachine` shim 能否走完 CAPI 的创建、drain、删除顺序，finalizer 不会
   卡死；CAPI v1beta1 → v1beta2 的契约变化。
5. 盘点现有集群里真正需要 PVC 的 workload。
6. k0smotron 托管的 apiserver 能否使用 structured authentication configuration（多 issuer、
   CEL claim 映射）；托管集群的 Pod 能否经 split horizon 访问 `sso.ddupan.top` 的 discovery
   和 JWKS，并信任它的 TLS 证书。
7. 请求 `groups` scope 时，iam-login 的 ID token 里是否确实带有 `groups` claim（apiserver 只看
   ID token，不调用 userinfo）。discovery 已经声明了 `groups` claim/scope、RS256 和
   `offline_access`，但还没有用实际签发的 token 验证过。kubectl
   登录（kubelogin）可以用的两种方式 iam-login 都已经实现：public client + PKCE 的 loopback
   redirect，以及 device authorization grant。

## 结果

- 跨平台的复杂度被限制在节点供应上：不做 VM 迁移，不统一 vnet，不统一存储抽象。
- VM 能力以 `Machine` 的形式进入 Ayatori，只用于 KaaS 节点；新的北向资源有明确的缺口作为
  依据（CAPI 没有跨后端放置，也没有 libvirt）。
- 复用 CAPI 的 Machine 生命周期和 drain，不自己设计替换循环；代价是要维护一个 CAPI infra
  provider，并跟进 CAPI API 版本。
- 托管集群和 LINSTOR controller 成为子集群的共同依赖：前者影响所有控制面，后者影响 PVC 的
  provision/attach（不影响已挂载卷的数据路径）。
- "声明的 / 派生的"分类同样适用于现在的 LoadBalancer 和以后的消费方组件。
