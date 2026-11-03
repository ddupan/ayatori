# ADR-0022：LB agent 以客户端证书加入控制面，自行注册 Node 与 LoadBalancerHost

- 状态：Proposed（依赖 [ADR-0021](0021-client-certificates-via-csr.md)，并须先完成下文「待验证」的事项）
- 日期：2026-11-03
- 依据：维护者讨论（compute 节点改为以 CSR 加入控制面之后，LB 也对齐同一做法）。
- 部分取代：[ADR-0018](0018-lb-backend-and-host-resources.md)「agent 状态接口」中「controller
  周期读取 `GET /status`」一条，LoadBalancerHost「由部署方创建」一条，以及 `spec.agentURL`。

## 问题

ADR-0018 规定由 controller 去读 agent 的状态，结果 LB 与 compute 的节点模型方向相反：

- **控制面要能连到每台 LB 主机。** manager 周期请求 `agentURL` 的 `GET /status`（:9641），因此 LB
  主机必须对控制面开放入站端口，部署方也要为每台主机维护 `agentURL`。compute 的 Agent 主动 watch
  API（[ADR-0015](0015-libvirt-lxc-compute-backend.md)），节点不开放入站端口。
- **状态接口没有身份。** `/status` 是明文、无鉴权的 HTTP。ADR-0018 当时接受这一点，理由是伪造的应答
  只会让 status 失真；可是 status 就是 LoadBalancer 就绪的依据，消费方读到的是谁的话无从确认。
- **两套加入方式。** ADR-0021 让控制面组件和集群外客户端统一用 CSR 签发、自行续签的客户端证书，
  compute Agent 按这套流程加入。LB agent 如果继续另走一套，签发、轮换和权限收窄就要维护两遍。

## 决策

LB agent 按 ADR-0021 获得客户端证书，**由 agent 把本机事实写进 API**；controller 不再读取
`/status`。

### 身份

| 项 | 值 |
| --- | --- |
| 用户名 | `system:ayatori:node:<主机名>`，与 compute 节点同一前缀；主机名即 Node 与 LoadBalancerHost 的名称 |
| 组 | `system:ayatori:nodes`（所有节点）、`system:ayatori:lb-hosts`（LB 角色）、`system:ayatori:client-certificates` |
| 私钥 | 每个 agent 一份：同一台主机上的 compute Agent 与 LB agent 用户名相同，但证书、私钥和角色组各自独立 |
| 证书文件 | `/etc/ayatori/lb-agent/client.pem`（证书与私钥合一，原子替换），`kubeconfig` 按相对路径引用 |
| 续签 | agent 进程内使用 `internal/infra/clientcert`，由 ayatori-manager 的批准者按 ADR-0021 批准 |

- **一台主机可以同时承担多个角色。** 架构不限制 compute 与 LB 部署在同一台物理机上，所以身份按主机命名，
  角色由组表达；同一台主机上的每个 agent 只持有自己那个角色的组。一个 agent 被攻破，拿不到同机另一个
  角色的权限。
- 节点通用的权限（写自己的 Node）授予 `system:ayatori:nodes`；角色相关的权限只授予角色组。
  compute 的 `LibvirtDomain` 等权限也应授予 compute 自己的角色组，不要授予 `system:ayatori:nodes`，
  否则 LB 主机也能读到计算后端的对象。
- **首次签发**由 Ayatori 部署 role 完成：
  1. 为每台主机创建 15 分钟有效的 bootstrap token；
  2. 把 `bootstrap-kubeconfig` 写到主机上，文件只属于 root；
  3. 等 agent 提交 CSR，核对申请的正是这台主机的身份、并且角色组正确后批准，不符合的拒绝。

  **这次批准就是准入：** 批准之后，这台主机就能以 LB 角色注册并承接流量。部署方不再另外登记主机。

  bootstrap token 只经过部署机，有效期很短，而且只能用来提交 CSR。私钥只在 LB 主机上生成。
- **Cinc 不碰凭据。** cookbook 只负责安装 agent、写 unit 和本地配置；`/etc/ayatori/lb-agent/` 下的
  凭据文件由部署 role 和 agent 管理。这和 ADR-0021「私钥只在使用证书的机器上生成」一致，也让
  cookbook 不必依赖控制面。

### 注册为 Node

与 compute Agent（ADR-0007、ADR-0015）使用同一套节点注册逻辑：

- **一台主机一个 Node，角色可以有多个。** 同一台主机上的各个 agent 以各自的 field manager（如
  `lb-agent`、`compute-agent`）对同一个 Node 做 SSA，每个 agent 只 apply 自己的字段：
  - 自己的角色 label，例如 `node-role.kubernetes.io/loadbalancer`；
  - 自己角色的 condition，例如 `ayatori.ddupan.top/LoadBalancerReady`；
  - 地址、`nodeInfo` 等主机级字段：各 agent 写入相同的值，SSA 允许共同持有。

  先启动的 agent 负责创建 Node；agent 不删除 Node，主机退出由部署方处理。
- **心跳按角色分开。** LB agent 续约 `kube-node-lease` 中的 `<node>-loadbalancer`，不碰与 Node 同名的
  Lease。原因是 ADR-0015 的自动冷启动和 fencing 要从 Lease 判断 compute Agent 是否存活：如果同机的
  LB agent 替它续约，compute Agent 挂掉后 Lease 仍然新鲜，VM 就得不到恢复。与 Node 同名的 Lease
  留给 compute 决定怎么用；建议 compute 也改用按角色命名的 Lease，让语义对称。
- Node 只表示「这台主机加入了控制面」以及各角色是否就绪。LB 自己的事实（各 VIP 是否宣告）仍写在
  LoadBalancerHost 中，见下节。
- 选择计算节点时，必须按 compute 角色 label 正向挑选，不能用「排除 LB」的方式；控制面没有
  kube-scheduler，Node 上的 taint 不会自动生效。

### LoadBalancerHost 由 agent 注册

- agent 以 SSA（field manager `lb-agent`）创建并维护与自己同名的 LoadBalancerHost。`spec.dataPlaneURL`
  取自本机配置（cookbook 写入的地址与端口）。controller 照常对它推送配置。
- 部署方的 inventory 只保留 `address_pool`，不再列出主机。部署 role 不再创建 LoadBalancerHost，
  也不再按 label 删除。
- **删除仍由人或部署方发起**，语义与 ADR-0018 相同：finalizer 先清理该主机上的配置，确认后再放行。
  agent 不能删除对象，也不能修改 finalizer。
- **退役顺序：** 先在主机上停止 agent 并删除凭据，然后删除 LoadBalancerHost。Kubernetes 的客户端证书
  无法吊销，如果 agent 还在运行，删掉的对象会被它重新注册回来。
- 以后加入的维护字段（例如 ADR-0018 后续提到的 `spec.drain`）由管理员持有。agent 只 apply
  `dataPlaneURL`，SSA 的字段归属保证两者互不覆盖。

### 状态由 agent 写入

- agent 以 server-side apply（field manager `lb-agent`）写自己那台 LoadBalancerHost 的
  `status.vips` 和 `AgentReady` condition。内容与语义沿用 ADR-0018：三层自检、未宣告的原因、
  「至少一个会话建立」等。只要本机事实有变化，或者距上次写入已满一个周期，agent 就会写入。
- controller 照搬 Node lifecycle controller 的做法：Lease `<node>-loadbalancer` 超过 `AgentStaleAfter` 未续约时，把
  `AgentReady` 置为 `Unknown`（reason `AgentStale`），并清空 `status.vips`。agent 恢复后写回
  `True`/`False`。`AgentReady` 因此有两个写入方，和 kubelet 与 node lifecycle controller 共写 Node
  `Ready` 一样；controller 以 force apply 覆盖。
- `DataPlaneReachable` 仍由 controller 写入。本 ADR 不改配置下发，manager 依旧推送到 Data Plane API。
- LoadBalancer 的 Attachment 仍以 `status.vips` 为准，规则不变。只是数据来源从 controller 读取
  `/status` 变成了 agent 写入。

### 权限

- RBAC：
  - `system:ayatori:nodes` 可以 `get/create/patch/update` Node 和 `nodes/status`，在 `kube-node-lease`
    中可以 `get/create/update` Lease。与 compute 共用；具体能写哪些字段由下面的 VAP 收窄。
  - `system:ayatori:lb-hosts` 可以 `get/list/watch/create/patch` LoadBalancerHost，`patch/update`
    `loadbalancerhosts/status`；不能 `delete`。
- ValidatingAdmissionPolicy（与 dev/compute 上 compute 的 PoC 09 同一做法）按 `request.userInfo` 收窄：
  - 节点通用规则，与 compute 共用：
    - 只能写与自己同名的 Node；
    - 角色 label 与角色 condition 只能增删、修改自己角色组对应的那一个，不能动同机其他角色的；
    - Lease 只能是 `<自己>-<自己的角色>`。
  - LB 规则：
    - 只能创建、修改与自己同名的 LoadBalancerHost；
    - spec 中只能改 `dataPlaneURL`，不能动 finalizer 和其他字段；
    - status 中只能改 `vips` 和 type 为 `AgentReady` 的 condition。
- 读权限不按主机限制：LoadBalancerHost 不含凭据，每台主机读到其他主机的 status 也无妨。

### API 变化

- `spec.agentURL` 改为可选，并标为弃用。controller 不再读它，下一个不兼容版本移除。
- LoadBalancerHost 改由 agent 注册。部署 role 的 `ayatori_loadbalancer.hosts` 弃用；迁移期间两者可以
  并存，SSA 的字段归属会处理同一个 `dataPlaneURL`。所有主机都换成新 agent 之后，再从 inventory 中删除
  hosts。
- agent 的 `/status` 保留在指标端口上，供本机排障与 Prometheus 之外的人工检查，不再是控制面的依赖。

## 已接受的代价

- **LB 主机多了一个对控制面的依赖：证书。** agent 停机超过证书剩余有效期（正常为数月）后，需要重跑
  部署 role 重新首次签发。路由不受影响：宣告与否仍只由本机决定（ADR-0017），控制面不可达时 agent
  照常宣告，只是不再上报。
- **agent 可以自己写 `dataPlaneURL`。** 被攻破的主机可以把它指向别处，诱使 controller 把 Data Plane API 的
  Basic 凭据发出去。但这份凭据按环境共用，攻破一台主机本来就能在本机的 dataplaneapi 上截获它，
  这样做没有扩大暴露面。「配置由 agent 在本机应用」（后续）落地后，这份凭据会整个撤掉。
- **退役必须先停 agent。** 证书无法吊销，在有效期内还在运行的 agent 会把主机重新注册回来。
- **部署 role 要能 ssh 到 LB 主机**，用于写入 `bootstrap-kubeconfig`。目前只有 Cinc 的 converge
  ssh 到这些主机。同一台主机上的每个角色各做一次首次签发。
- **controller 判断不了 agent 是否「活着但写不进来」**，例如证书过期：这时只看得到 Lease 过期，即
  `AgentStale`。原因要到主机日志里查。为了提前告警，agent 要在 `/metrics` 中导出自身客户端证书的
  到期时间；控制面的 `ayatori_certificate_not_after_timestamp_seconds` 只覆盖控制面上的证书。

## 不选的方案

- **维持 controller 轮询 `/status`，只给接口加 mTLS**：ADR-0018 已经否决过。而且即使加上 mTLS，控制面
  仍要能连到每台主机，节点模型照旧与 compute 相反。
- **以 SPIFFE JWT-SVID 登录**：理由同 ADR-0021。apiserver 会依赖运行在 Kubernetes 中的 SPIRE；LB 主机
  的身份是静态的，用不上短期 token。
- **由 Cinc 签发并分发证书**：私钥会经过部署机或 Bao。cookbook 还要依赖控制面，Kitchen 也就没法独立测试。
- **用 Node 取代 LoadBalancerHost**：Node 的 status 没有地方放每个 VIP 是否宣告及其原因，Data Plane
  地址也只能放进 annotation；删除时「先清理该主机上的配置再放行」的语义也不属于 Node。
- **LoadBalancerHost 继续由部署方创建**：主机能否承担 LB 角色，在首次签发时部署方核对角色组那一刻就
  已经决定了。再让部署方单独维护一份主机列表，只会多出一处可能与实际不一致的地方。
- **LB 主机不注册 Node，单独用 `ayatori-system` 中的 Lease**：节点身份、心跳和 VAP 就得为 LB 再做一套，
  而用 role label 区分 LB 与计算节点并不难。更重要的是，compute 与 LB 可能在同一台主机上，各自注册
  主机会让一台物理机在控制面里变成两个互不相关的对象。
- **每台主机只跑一个 node agent，由它代各角色注册并持有全部权限**：一个角色的漏洞就能拿到同机所有角色的
  权限，各角色的发布节奏也会被绑在一起。
- **各角色共用与 Node 同名的 Lease**：会掩盖单个 agent 的死亡，理由见「注册为 Node」。

## 待定问题

- **同机的 BGP speaker。** ADR-0017 规定 LB agent 内嵌的 GoBGP 是本机唯一的 BGP speaker。compute 如果也
  要向路由器宣告（例如 VM 或实例的地址），同一台主机上就会有两个 speaker 用同一个源地址与路由器建立
  会话，二者只能留一个。届时要决定是由一个进程统一宣告、各角色把路径交给它，还是各角色用不同的
  本机地址分别建立会话。在 compute 确定是否需要 BGP 之前，本 ADR 不处理。
- **同机部署时的 vip-guard。** homelab_loadbalancer 的 nft 规则会拒绝发往 VIP 池、却落到监听通配地址的
  socket 的流量。这条规则只针对 VIP 池，不影响计算节点上的其他服务，但两种角色同机部署前，仍要在
  Kitchen 中验证一次。

## 后续（另立 ADR）

**配置改由 agent 在本机应用。** 也就是 ADR-0015 中 compute Agent 的模型：controller 给 LoadBalancer
打主机 label，agent 只 watch 与自己相关的 LoadBalancer 和 Backend，用现有的 HAProxy adapter 写
本机的 Data Plane API（只监听 loopback 或 unix socket）。完成后：

- LB 主机不再开放 `:5555`；
- 控制面撤掉 `pki-loadbalancer-<env>` 客户端证书与 Basic 凭据；
- HAProxy 的配置文件本身就是本地状态。「API 不可达期间不删除」这一规则，对 agent 删除 frontend
  同样适用。

这一步要把收敛逻辑从 manager 迁到 agent，ADR-0018 和集成测试的结构都会跟着改动，所以等本 ADR 落地后
另行评估。本 ADR 的身份、Lease 与 VAP 届时可以直接沿用。

## 待验证（Accepted 之前必须完成）

1. envtest 中：VAP 只允许 `system:ayatori:node:<名称>` 创建与自己同名的 LoadBalancerHost、改它的
   `dataPlaneURL`、写它的 `vips` 与 `AgentReady`；写其他主机、其他 condition、其他 spec 字段、改
   finalizer 或删除对象一律拒绝；Node 只能写与自己同名的那个，Lease
   只能写 `<自己>-loadbalancer`。
2. envtest 中，模拟同一台主机：LB 与 compute 两个身份用户名相同、角色组不同，对同一个 Node 做 SSA；
   各自只能增删自己的角色 label 和 condition，动不了对方的，也续约不了对方的 Lease。
3. envtest 中：agent 的 SSA 与 controller 的 force apply 共写 `AgentReady` 时，Lease 过期置为
   `Unknown`，agent 恢复后回到 `True`，`DataPlaneReachable` 不受影响。
4. Dev 上：部署 role 为 lb-dev-1/2 完成首次签发，agent 注册 LoadBalancerHost 并写入 status，并按 ADR-0021 自行续签；
   manager 不再连接 :9641（LB 主机上可以关掉该端口对控制面的放行来确认）。

## 测试合同

- agent 单测：本机事实（沿用 `/status` 的结构）到 status apply 配置的转换；事实不变时不重复写入。
- controller 单测：Lease 新鲜与过期时的 `AgentReady`；过期时清空 `vips`。
- 集成测试：envtest 中，以 impersonation 模拟两台 LB 主机和一台计算节点的 agent，其中一台主机同时
  承担两种角色，覆盖上面的 VAP 用例；`internal/infra/clientcert` 已有的集成测试覆盖签发与续签，这里不重复。
