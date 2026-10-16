# ADR-0012：以独立 L4 LoadBalancer 统一 VIP 与入口

- 状态：Accepted；VIP 宣告由谁决定（南向第 2、3、5 步、故障撤回、gobgpd 进程与 API 边界）已被
  [ADR-0014](0014-lb-host-local-announcement.md) 取代；`LoadBalancer` 内嵌后端的形状与主机清单来源被
  [ADR-0015](0015-lb-backend-and-host-resources.md) 取代
- 日期：2026-10-04
- 依据：维护者讨论、homelab-infra/wiki 现状盘点与成熟方案调研；落实
  [ADR-0006](0006-demand-driven-resource-scope.md) 中的 `LoadBalancer` 方向。
- 领域设计：[LoadBalancer 领域模型](../loadbalancer/domain-model.md)。

## 问题

当前 L4 入口与 VIP 分散在两处，且都难以继续扩展：

- **VyOS 承担 LB 数据面。** sandbox k3s API（10.60.0.13:6443）、OpenSandbox API、sandbox
  datastore PostgreSQL（10.60.0.1:5432）由 VyOS 内置 HAProxy 提供；pg-prod（192.168.10.2:5432）
  由 VyOS 上另一份 HAProxy systemd unit 提供。配置均为 Ansible 模板化的 VyOS `set` 行，IaC 体验
  差；VyOS 2025.11 生成的 pgsql check 有缺陷，sandbox PostgreSQL 只能手工指定 primary，故障
  切换依赖 runbook。
- **Kubernetes 集群没有真正的 VIP。** 每个集群用 k3s ServiceLB（klipper）共享节点 IP：同一
  端口只能有一个 LoadBalancer，客户端地址被 SNAT，地址没有统一分配。新集群随时可能出现，
  每个集群各自部署 MetalLB 只会再分散一次地址池与状态。

LB 的消费者不只是 Kubernetes Service：集群 API server、数据库主库入口等非 Kubernetes 后端
同样需要稳定 VIP 与健康检查。Ayatori 自己的 `PostgreSQLInstance` 生产 endpoint 也在等待一个
能识别 primary 的入口。

## 评估

- **每集群 MetalLB + 独立 HAProxy 主机（不进 Ayatori）**：地址池按集群人工划分，k8s 与非 k8s
  入口是两套系统，新集群接入需要重复部署与分配。
- **LoxiLB**：VIP 分配在各集群 kube-loxilb 内独立进行，中心多集群 IPAM 未实现（loxilb#371）；
  HTTP probe 仅为 HEAD 且只认 200；REST API 为 0.x；需要特权 eBPF，不适合 LXC。
- **Envoy + 自写 xDS 控制面与主机 agent**：endpoint 集合只在 Service、节点或迁移变化时改变，
  频率低，xDS 增量推送的收益不足以抵消额外控制面服务与 agent 的维护成本。
- **整段 VIP 前缀静态路由到 LB 主机**：Ayatori 逐台收敛配置期间，新 VIP 只在部分主机就绪，
  ECMP 会把流量哈希到尚未配置的主机形成黑洞。需要按 VIP 宣告。
- **经 Ansible executor 下发配置**：Job/Run 执行切片尚未合并，且远程执行 playbook 不如直接
  调用后端稳定 API 可观察；Ansible 仍用于 LB 主机的一次性部署。
- **路由型（BGP 以后端为 next-hop）保留源地址**：需要 VyOS 接受第三方 next-hop、后端在
  loopback 配置 VIP，当前没有确需保留源地址的消费者（现有 PostgreSQL 入口即为 SNAT），不做。

## 决策

Ayatori 提供独立的 L4 `LoadBalancer` API，持有中心 VIP 池；数据面为专用 LB 主机上的 HAProxy，
VIP 由各主机上的 GoBGP 按 VIP 向 VyOS 宣告 /32，VyOS 只保留路由职责。首版只做 TCP 代理；HTTP
路由继续由各集群内的 Envoy Gateway 等负责。

### 职责边界

Ayatori 只交付 API 与其背后的 VIP 分配和数据面收敛。消费方如何把自己的对象映射为
`LoadBalancer` 不属于 Ayatori：例如 workload 集群中把 `Service type=LoadBalancer` 翻译为
Ayatori `LoadBalancer` 并回写 Service status 的 CCM 类组件，由消费方自行提供与部署，Ayatori
不访问 workload 集群（与 [ADR-0010](0010-credential-delivery-ends-at-openbao.md) 一致）。API 必须
足以支撑这类组件：可整体更新 endpoint 集合，status 公布 VIP 与就绪状态。

### 北向资源

- `LoadBalancer`（namespaced，API group `network.ayatori.ddupan.top`）：表达 VIP（自动分配或指定）、listener、backend endpoint 与健康
  检查；status 公布已分配 VIP、按 LB 主机的数据面就绪与各 endpoint 健康 conditions。namespace
  是消费方与 RBAC 边界（例如一个 workload 集群或一个平台组件），不表示共置。
- VIP 分配采用上游 `networking.k8s.io/v1 IPAddress` 的模式：每个已分配 VIP 是一个集群级对象，
  对象名即规范化 IP，以 Create 的名字唯一性原子占位，AlreadyExists 即视为已占用；分配事实
  以 API 对象为准，不在内存中重建。孤儿对象由回收循环在宽限期后删除。复用内置 `IPAddress`
  还是自定义 CRD 在 API 评审中决定；复用时 VIP 池不得与 Ayatori API server 的 Service CIDR
  重叠，并使用非 `ipallocator.k8s.io` 的 managed-by 标签。
- VIP 池首版为单一池，使用不属于任何二层网段、只经路由可达的专用地址段，登记于
  homelab-infra 地址清单。按消费方隔离地址段的需求出现后再引入池选择（参照 MetalLB/Cilium
  的 selector、priority 与重叠池 Conflicting 语义）。
- 共享 VIP（多个 LoadBalancer 使用同一 IP 不同端口）首版不支持。
- 每个 LoadBalancer 获得 Ayatori 维护的域名，A 记录指向 VIP，位于 Ayatori 独占的 PowerDNS
  zone（Prod `lb.lab.ddupan.top`，Dev `dev-lb.lab.ddupan.top`）；详见领域模型。
- 健康检查需支持探测端口与流量端口不同（如代理 :5432、探测 Patroni `:8008/primary`）。按角色
  只选一个后端时，无健康后端即拒绝连接，不回退到全部后端；后端被判失败时断开其现有连接
  （HAProxy `on-marked-down shutdown-sessions`）。
- 后端看到 LB 主机地址；需要客户端地址的后端可使用 PROXY protocol（以后端支持为前提）。

### 南向与收敛

每台 LB 主机运行 HAProxy、HAProxy Data Plane API（v3）与 gobgpd。Ayatori adapter 对每台主机
独立收敛，按观察事实逐步推进，不跨主机做事务：

1. 经 Data Plane API 确保该 VIP 的 frontend/backend 存在。写操作携带当前配置 `version`，
   409 时重新观察；配置变更返回 `202` 与 `Reload-Id`，以该 reload 成功为生效依据。
2. reload 成功后，经 gobgpd gRPC `AddPath` 宣告 VIP /32。
3. 删除顺序相反：先 `DeletePath`，再删除配置并等待 reload。
4. 只有 endpoint 集合变化时走 runtime server 接口，无需 reload；VIP 与 listener 变化走配置
   变更。
5. observe 阶段以 Data Plane API（配置与 runtime server 状态）和 gobgpd `ListPath` 为事实来源，
   按主机写入 status。

故障时的路由撤回不依赖自写 agent：

- 主机宕机：BGP 会话断开，VyOS 撤回该主机全部路径。
- HAProxy 停止：gobgpd 以 systemd `BindsTo=haproxy.service` 随之停止，效果同上。
- gobgpd 不持久化路径；重启后由 Ayatori 观察到缺失再补宣告，补齐前该主机不承接流量。
- 单个 VIP 的后端全部不健康时不撤回 VIP，由 HAProxy 拒绝连接，与现状一致。

### 部署与凭据边界

- homelab-infra 以 Ansible 一次性部署 LB 主机：HAProxy、dataplaneapi、gobgpd 及其 systemd 依赖，
  以及 VyOS 侧的 BGP 邻居、`maximum-paths` 与向 OSPF 的再分发。部署后不再模板化
  `haproxy.cfg`：该文件只由 Ayatori 经 Data Plane API 写入。
- dataplaneapi 与 gobgpd 的 API 均只监听管理地址并启用 mTLS（dataplaneapi `tls_ca`，gobgpd
  `--tls-client-ca-file`）；Ayatori 的客户端证书与 Basic 凭据由 OpenBao 管理。
- GoBGP 与 dataplaneapi 版本在部署中固定，升级经 Dev 验证后 promotion。GoBGP BFD（v4.6 引入）
  首版不依赖，使用较短 hold timer。

### 环境与隔离

- 每组 LB 主机只有一个 Ayatori 写入方。Dev 与 Prod 各自拥有独立的 LB 主机：Prod 2 台，承载
  真实入口；Dev 1 台，承载测试 VIP，用于验证 adapter 生命周期后再 promotion。ECMP 与主机故障
  切换在 Prod 首次上线时验证，确需反复验证时再临时为 Dev 增加主机。
- Dev 与 Prod 使用不重叠的 VIP 地址段。VyOS 对每个 BGP 邻居以 prefix-list 只接受其所属环境
  地址段内的 /32，由路由器保证 Dev 的错误宣告不影响 Prod。
- 两个环境的 mTLS 客户端证书分别签发，Dev Ayatori 无法访问 Prod LB 主机的管理 API。
- 组件级 adapter 集成测试不依赖 VyOS：以容器运行 HAProxy、dataplaneapi、gobgpd 与作为对端的
  gobgp。

### 已接受的代价

- ECMP 成员变化会重新哈希，中断部分现有连接；LB 主机之间不做连接同步。客户端需能重连。
- 两台主机分别收敛期间配置可能短暂不同；按 VIP 宣告保证流量只进入已就绪的主机。

## 后续模式（不在首版）

- **DNS 型（无 VIP，保留源地址）**：适用于 SSH 等需要客户端源地址的入口。PowerDNS 已成为
  `lab.ddupan.top` 的权威服务并提供 HTTP API；向 Ayatori 控制面开放 API 后即可实现，按健康
  endpoint 维护独占 zone 中的记录，不写 Samba。

## 未决项（API 评审与首个切片前确定）

- Prod LB 主机形态（非特权 LXC 或 VM）与网段；VyOS BGP 配置在 Dev 网络以测试 /32 验证。
- Dev/Prod VIP 地址段；PowerDNS API 向控制面开放与两个 zone 的创建委派。
- Data Plane API runtime/reload 端点的具体路径与字段：以部署版本自带的 `/v3/docs` 核对。
- 消费方访问 Ayatori API 的身份与按 namespace 的 RBAC 签发方式。
- LoadBalancer 删除与 VIP 回收语义（释放即可复用，或保留期）；指定 VIP 的授权方式。
- 迁移顺序：建议先迁 sandbox PostgreSQL 与 sandbox k3s API（替换 VyOS HAProxy），再由一个
  集群试点 Service 接入，最后迁 pg-prod。

## 测试合同

- VIP 选择、LoadBalancer 到 HAProxy 配置的翻译、按主机收敛步骤的判定：单元测试。
- LoadBalancer/IPAddress 的 schema、并发分配冲突、finalizer、回收循环与 controller 重启：envtest。
- Data Plane API 与 gobgpd adapter：对真实 dataplaneapi/HAProxy 与 gobgpd 的集成测试，覆盖 409
  重试、reload 失败、宣告与撤回顺序、gobgpd 重启后的补宣告。
- VyOS ECMP、primary 切换与断连、主机或 HAProxy 故障后的路由撤回：Dev 环境端到端测试。
