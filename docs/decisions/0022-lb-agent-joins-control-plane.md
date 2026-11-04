# ADR-0022：LB agent 加入控制面，watch 资源并在本机收敛

- 状态：Proposed（已实现于 `dev/lb-agent`，Dev 已切换；Accepted 之前还剩下文「待验证」中未勾选的三项）
- 日期：2026-11-03
- 依据：维护者讨论（compute 节点改为以 CSR 加入控制面之后，LB 也对齐同一做法）。
- 部分取代：[ADR-0018](0018-lb-backend-and-host-resources.md) 中以下几点：
  - controller 经 Data Plane API 逐主机收敛代理配置；
  - controller 周期读取 agent 的 `GET /status`；
  - LoadBalancerHost 由部署方创建，并带有 `dataPlaneURL`、`agentURL`；
  - Data Plane API 的凭据放在 controller 的凭据 Secret 中。

  [ADR-0017](0017-lb-host-local-announcement.md) 的宣告规则不变。

## 问题

LB 与 compute 的节点模型方向相反：

- **控制面要能连到每台 LB 主机。** manager 通过 `:5555` 的 Data Plane API 推送配置，并周期请求
  `:9641` 的 `/status`。LB 主机因此要对控制面开放两个入站端口，部署方还要为每台主机维护两个 URL。
  compute 的 Agent 是主动 watch API 的（[ADR-0015](0015-libvirt-lxc-compute-backend.md)），节点不开放
  入站端口。
- **控制面持有能改写所有 LB 主机的凭据。** `pki-loadbalancer-<env>` 签发的客户端证书和 Basic 凭据按环境
  共用，拿到它就能改写该环境中任何一台 LB 主机的代理配置。
- **逐主机的状态机放错了地方。** 期望配置、事务、通过 runtime 更新 endpoint、drain、reload 校验，都只
  关心一台主机，却在 manager 里远程执行：中间隔着网络，控制面不在线时主机也无法自行收敛。真正需要
  全局一致的只有 VIP 分配和 DNS。
- **`/status` 没有身份。** 明文、无鉴权，但 LoadBalancer 是否就绪就以它为依据。
- **两套加入方式。** ADR-0021 让集群外客户端统一用 CSR 签发、自行续签的证书，compute Agent 也按这套
  流程加入。LB 如果另走一套，签发、轮换和权限收窄就要维护两遍。

## 决策

LB agent 按 ADR-0021 获得客户端证书，自行注册 Node 和 LoadBalancerHost，**watch LoadBalancer 与
Backend，在本机计算期望配置并收敛 HAProxy**，再把结果写回 API。控制面不再连接 LB 主机。

### 职责划分

| | 中央 controller（manager） | LB agent（每台主机） |
| --- | --- | --- |
| VIP | 从地址池分配，写 IPAddress 与 `status.address` | 只读 `status.address`；地址未分配前不下发该 LoadBalancer |
| DNS | 维护独占 zone 中的 A 记录 | — |
| 引用 | 写 `ResolvedRefs` | 引用缺失时按领域规则处理，与现在相同 |
| 代理配置 | — | 计算本机期望配置，经本机 Data Plane API 收敛（事务、runtime 更新 endpoint、drain、reload 校验） |
| 宣告 | — | ADR-0017、ADR-0018 的三层自检，不变 |
| status | 把各主机的上报汇总为 LoadBalancer 的 `status.hosts[]`、`Programmed`、`Degraded` | 写自己那台 LoadBalancerHost 的 status |
| 删除 | 持有 finalizer，等所有主机都报告已移除再放行 | 看到 deletionTimestamp 后移除本机配置并上报 |

- 期望配置的计算仍是 domain 包中的纯函数，由 manager 移到 agent 调用。现有的 HAProxy adapter 原样
  搬到 agent，目标从远程 `:5555` 改为本机的 Data Plane API，只监听 loopback 或 unix socket。
- 每个 LoadBalancer 仍落地到全部 LB 主机（ADR-0018），因此 agent watch 全部 LoadBalancer 与 Backend，
  不需要按主机打 label。出现多组主机时，再按 label 分组。
- **本地状态就是 HAProxy 的配置文件。** 控制面不可达时，agent 照常按已有配置服务、宣告，并在本机
  继续做健康检查与 drain。
- **只有 API 可达时才删除**（沿用 ADR-0015）：只有 watch 收到删除事件，或者重新 list 后确认对象已不
  存在，agent 才会移除对应的 frontend/backend。API 不可达期间绝不清理。
- agent 启动时先按现有配置恢复宣告，再连接 API、完成 list，然后才开始收敛。

### 身份

| 项 | 值 |
| --- | --- |
| 用户名 | `system:ayatori:node:<主机名>`，与 compute 节点同一前缀；主机名即 Node 与 LoadBalancerHost 的名称 |
| 组 | `system:ayatori:nodes`（所有节点）、`system:ayatori:role:loadbalancer`（LB 角色）、`system:ayatori:client-certificates` |
| 私钥 | 每个 agent 一份。同一台主机上的 compute Agent 与 LB agent 用户名相同，证书、私钥和角色组各自独立 |
| 证书文件 | `/etc/ayatori/lb-agent/client.pem`（证书与私钥合一，原子替换），`kubeconfig` 按相对路径引用 |
| 续签 | agent 进程内使用 `internal/infra/clientcert`，由 ayatori-manager 的批准者按 ADR-0021 批准；身份由共用库 `node.Identity(dir, node, "loadbalancer")` 管理 |

- **一台主机可以同时承担多个角色。** 架构不限制 compute 与 LB 部署在同一台物理机上，所以身份按主机
  命名，角色由组表达。同一台主机上的每个 agent 只持有自己那个角色的组，一个 agent 被攻破，拿不到同机
  另一个角色的权限。
- 节点通用的权限授予 `system:ayatori:nodes`，角色相关的权限只授予角色组。compute 的 `LibvirtDomain`
  等权限也应只授予 compute 自己的角色组。
- **首次签发（join）由部署方完成**，实现为 homelab-infra 的一次性 playbook
  `infrastructure/chef/ansible/join.yml`（与 SPIRE 首次 enrollment 同类，不从 CI 运行），compute 与 LB 共用：
  1. 在控制面为这台主机创建 15 分钟有效的 bootstrap token；
  2. 把 `bootstrap-kubeconfig` 写进 cookbook 建的凭据目录，文件只属于 agent 的运行用户（LB agent
     不以 root 运行）；
  3. 用控制面正在运行的 manager 镜像执行 `client-credentials approve-bootstrap`：只批准以这个 token
     提交、申请的恰好是这台主机身份与角色组的 CSR。核对逻辑在 Ayatori（`clientcert.ApproveBootstrap`）。

  join 之前 LB 主机就要宣告：agent 第一次需要 API 时才启动证书申请，失败则在后台重试，宣告不受影响。

  bootstrap token 只经过部署机，有效期很短，只能用来提交 CSR。私钥只在 LB 主机上生成。

  **这次批准就是准入：** 批准之后，这台主机就能以 LB 角色注册并承接流量，部署方不再另外登记主机。
- **Cinc 不碰凭据。** cookbook 只安装 agent 和 dataplaneapi，并写入 unit 与本地配置；
  `/etc/ayatori/lb-agent/` 下的凭据由部署 role 和 agent 管理。

### 注册为 Node

LB agent 与 compute Agent（ADR-0007、ADR-0015）使用同一套节点注册逻辑，即共用库 `internal/infra/node`
（LB agent 用 `node.Registrar{Role: "loadbalancer"}`）。各角色的命名都从角色名推出：

| | 规则 | LB |
| --- | --- | --- |
| 证书组 | `system:ayatori:nodes` + `system:ayatori:role:<role>` | `system:ayatori:role:loadbalancer` |
| Node label | `node-role.kubernetes.io/<role>` | `node-role.kubernetes.io/loadbalancer` |
| condition | `<role>.ayatori.ddupan.top/Ready` | `loadbalancer.ayatori.ddupan.top/Ready` |
| Lease | `kube-node-lease/<node>-<role>` | `<node>-loadbalancer` |
| field manager | `<role>-agent` | `loadbalancer-agent` |

因为都能从角色名推出，准入策略只需一条规则约束「只改自己角色的 label、condition 和 Lease」，不必每个
角色另写（见「权限」）。

- **一台主机一个 Node，可以有多个角色。** 同一台主机上的各个 agent 用各自的 field manager（如
  `loadbalancer-agent`、`compute-agent`）对同一个 Node 做 SSA，每个 agent 只 apply 自己的字段：
  - 自己的角色 label，例如 `node-role.kubernetes.io/loadbalancer`；
  - 自己角色的 condition，例如 `loadbalancer.ayatori.ddupan.top/Ready`；
  - 地址、`nodeInfo` 等主机级字段。各 agent 写入相同的值，SSA 允许多个 field manager 共同持有。

  Node 由先启动的 agent 创建。agent 不删除 Node，主机退出由部署方处理。
- **没有整机的就绪状态。** Node 的 `Ready` condition 不写入，准入策略也禁止 agent 写它：compute Agent
  不就绪不能让同机的 LB 被当成不可用。控制面不运行 node lifecycle controller，也不打 taint；
  各 controller 只看自己角色的 label、condition 和 Lease。
- **心跳按角色分开。** LB agent 续约 `kube-node-lease` 中的 `<node>-loadbalancer`，不碰与 Node 同名的
  Lease。ADR-0015 的自动冷启动和 fencing 要根据 Lease 判断 compute Agent 是否存活：如果同机的 LB agent
  替它续约，compute Agent 挂掉后 Lease 仍然是新的，VM 就得不到恢复。建议 compute 也改用按角色命名的
  Lease。
- 选择计算节点时，必须按 compute 角色 label 正向挑选，不能用「排除 LB」的方式。控制面没有
  kube-scheduler，taint 不会自动生效。

### LoadBalancerHost 由 agent 注册

- agent 以 SSA（field manager `loadbalancer-agent`）创建并维护与自己同名、不带 spec 的 LoadBalancerHost。
  `agentURL` 已删除；`dataPlaneURL` 改为可选，只在迁移期间供仍远程收敛的环境使用，迁移第 3 步删除。
- 部署方的 inventory 只保留 `address_pool`，部署 role 不再创建或删除 LoadBalancerHost。
- **删除仍由人或部署方发起：** controller 持有 finalizer，等 agent 报告本机配置已全部清空（condition
  `Purged=True`，agent 看到自己的 LoadBalancerHost 删除中时清除本机全部配置后写入）后才放行。
  主机不可达时，删除会一直等待，由管理员移除 finalizer 强制完成。agent 不能删除对象，也不能修改
  finalizer。
- **退役顺序：** 先在主机上停止 agent 并删除凭据，再删除 LoadBalancerHost。Kubernetes 的客户端证书
  无法吊销，agent 只要还在运行，删掉的对象就会被它重新注册回来。
- 以后加入的维护字段（例如 ADR-0018 后续提到的 `spec.drain`）由管理员持有，agent 只读不写。

### 状态由 agent 写入

agent 以 SSA 写自己那台 LoadBalancerHost 的 status：

- `vips` 与 `AgentReady`：内容和语义沿用 ADR-0018。
- `loadBalancers[]`：每个 LoadBalancer 在本机的代理事实：namespace/name 与 `uid`；`digest`——本机配置与之
  完全一致的期望配置摘要（`Snapshot.Digest`，controller 用同一函数计算后比较；不用 generation，因为
  Backend 变化不改变 LoadBalancer 的 generation）；`present`；`reload`（Succeeded、Pending、Failed）与
  `message`；各 server 的健康检查结论（ADR-0023）。对象删除完成后移除该项。
- controller 把每台主机的这一项还原为代理事实，交给与远程收敛相同的领域规则（`PlanHost`）判定
  Attachment、`Programmed` 与 endpoint 健康；不另设每主机的 `Programmed` condition，失败原因在
  `reload` 与 `message` 中。

只要本机事实有变化，或者距上次写入已满一个周期，agent 就写入一次。

中央 controller：

- 只读各 LoadBalancerHost 的 status，汇总成 LoadBalancer 的 `status.hosts[]`、`Programmed` 与
  `Degraded`。Attachment 规则不变：配置一致，且 agent 报告已宣告该 VIP。
- 照搬 Node lifecycle controller 的做法：Lease `<node>-loadbalancer` 超过 `AgentStaleAfter` 未续约时，
  把 `AgentReady` 置为 `Unknown`（reason `AgentStale`），并清空 `vips`。`AgentReady` 因此有两个写入方，
  和 kubelet 与 node lifecycle controller 共写 Node `Ready` 一样，controller 以 force apply 覆盖。
- 不再有 `DataPlaneReachable`。控制面不连接主机，主机是否可达由 Lease 体现。

### 权限

- **节点通用部分已在 `deploy/flux/nodes`，与 compute 共用：**
  - RBAC：`system:ayatori:nodes` 可以 `get/create/patch/update` Node 和 `nodes/status`，在
    `kube-node-lease` 中 `get/create/update` Lease；
  - ValidatingAdmissionPolicy（`deploy/flux/nodes/admission.yaml`）按 `request.userInfo` 收窄：只能写与
    自己同名的 Node；角色 label、角色 condition 和 Lease 只能是从自己的 `system:ayatori:role:<role>`
    组推出的那一个。
- **LB 只需给角色组 `system:ayatori:role:loadbalancer` 授予自己的资源：**
  - `get/list/watch` LoadBalancer、Backend；
  - `get/list/watch/create/patch` LoadBalancerHost，`patch/update` `loadbalancerhosts/status`；
  - 不能 `delete`，也不能写 LoadBalancer 和 Backend；
  - LB 自己的 VAP 规则：只能创建、修改与自己同名的 LoadBalancerHost，不能动 spec 和 finalizer，只能写
    status。
- 读权限无法按主机限制：每台 LB 主机都能读到所有 LoadBalancer 与 Backend。这些对象只有地址和端口，
  不得包含密钥；PROXY protocol、TLS 等以后如果需要密钥，要另行设计交付方式。

### 下线与迁移

- 控制面撤掉 Data Plane API 的客户端证书与 Basic 凭据，包括 `pki-loadbalancer-<env>`、
  `kv/infra/loadbalancer/<env>`，以及 `deploy/flux/loadbalancer` 中的 ESO 对象。
- cookbook 改为让 dataplaneapi 只监听 loopback 或 unix socket，本机凭据由 cookbook 在本机生成，不离开
  主机；`:5555` 与 vip-guard 无关，直接关闭。
- `/status` 保留在指标端口上，只用于本机排障，不再是控制面的依赖。
- 迁移顺序：
  1. 发布能 watch、能收敛的 agent，同时保留 manager 的远程收敛，以开关切换；
  2. 每台主机确认 agent 收敛结果与 manager 一致后，关闭 manager 的远程收敛；
  3. 删除 Data Plane API 凭据，关闭 `:5555`；
  4. 不再在 GitOps 中声明 LoadBalancerHost（原部署 role 的 `ayatori_loadbalancer.hosts` 已随控制面迁入
     Chef 改为 GitOps 声明）：先给对象加 `kustomize.toolkit.fluxcd.io/prune: disabled` 并确认生效，再从
     仓库删除——否则 Flux 删除对象，触发整机清理。

  切换期间，同一台主机不能同时有两个写入方。开关按环境整体切换，不按主机切换。
  开关是 manager 的 `--loadbalancer-agent-convergence` 与 LB 主机 cookbook 的 `agent.converge`。先开 manager
  （远程写入停止，主机显示 Pending），再逐台开主机：中间没有写入方，HAProxy 按已有配置照常服务。

## 已接受的代价

- **LB 主机依赖控制面签发的证书。** agent 停机超过证书剩余有效期（正常为数月）后，需要重跑部署 role
  重新首次签发。路由与已有配置不受影响，只是不再接收变更。
- **配置变更要等 agent 自己 watch 到。** controller 无法替某台主机「推一下」。agent 卡住时，只能从
  `AgentStale` 或 `Programmed=False` 看出来，要到主机上排查。
- **退役必须先停 agent。** 证书无法吊销。
- **部署 role 要能 ssh 到 LB 主机**，用于写 `bootstrap-kubeconfig`。同一台主机上的每个角色各做一次
  首次签发。
- **agent 需要导出自身证书的到期时间指标**，用于提前告警。控制面的
  `ayatori_certificate_not_after_timestamp_seconds` 只覆盖控制面上的证书。
- **ADR-0018 的实现结构要大改：** controller 拆为中央与 agent 两部分，集成测试从「envtest + 远程
  HAProxy」改为「envtest + agent + 本机 HAProxy」。

## 不选的方案

- **只把状态改为 agent 上报，配置仍由 manager 推送**：控制面仍要连到每台主机，仍持有能改写所有 LB 主机
  的凭据，逐主机的状态机也仍然隔着网络执行。
- **维持 controller 轮询 `/status`，只给接口加 mTLS**：ADR-0018 已经否决；而且控制面仍要连到每台主机。
- **以 SPIFFE JWT-SVID 登录**：理由同 ADR-0021。
- **由 Cinc 签发并分发证书**：私钥会经过部署机或 Bao，cookbook 还要依赖控制面。
- **用 Node 取代 LoadBalancerHost**：Node 的 status 放不下各 VIP 与各 LoadBalancer 的落地进度，也没有
  「先清理该主机上的配置再放行」的删除语义。
- **LoadBalancerHost 继续由部署方创建**：主机能否承担 LB 角色，在首次签发时核对角色组那一刻就决定了。
  再维护一份主机列表，只会多出一处可能与实际不一致的地方。
- **LB 主机不注册 Node**：节点身份、心跳和 VAP 就得为 LB 再做一套。compute 与 LB 可能在同一台主机上，
  分别注册会让一台物理机在控制面里变成两个互不相关的对象。
- **每台主机只跑一个 node agent，代各角色注册并持有全部权限**：一个角色的漏洞会波及同机所有角色，各
  角色的发布节奏也会被绑在一起。
- **各角色共用与 Node 同名的 Lease**：会掩盖单个 agent 的死亡。
- **agent 直接写 LoadBalancer 的 `status.hosts[]`**：每个 LoadBalancer 会有 N 个写入方，VAP 要按列表项
  收窄。改为各自只写自己的 LoadBalancerHost、由中央汇总，写入方与权限都更简单。

## 已知限制

- **BGP speaker 仍内嵌在 LB agent 中。** 按 ADR-0017，它是本机唯一的 BGP speaker；目前也没有其他角色
  需要宣告路由。将来如果有第二个角色要宣告，就把 speaker 从 LB agent 中独立出来，作为本机共用的服务，
  各角色把各自的路径交给它，宣告规则仍由各角色在本机决定。在那之前不做拆分。

## 待定问题

- **同机部署时的 vip-guard。** 这条 nft 规则只针对 VIP 池，但两种角色同机部署前，仍要在 Kitchen 中验证。

## 待验证（Accepted 之前必须完成）

2026-11-04 的结果附在各项后；未勾选的仍需完成。

1. [x] envtest 中验证 VAP：`system:ayatori:node:<名称>` 只能创建自己的 LoadBalancerHost、只能写它的
   status，不能写其他主机、spec、finalizer，也不能删除；Node 只能写自己的，Lease 只能写
   `<自己>-loadbalancer`。——`agent.TestAgentJoins`（LoadBalancerHost 部分）、`node.TestRoles`（Node、Lease）。
2. [x] envtest 中模拟同一台主机：LB 与 compute 两个身份用户名相同、角色组不同，对同一个 Node 做 SSA，
   各自只能动自己的角色 label、condition 和 Lease。——`node.TestRoles`。
3. agent 集成测试（envtest + 本机 HAProxy）：
   - [x] 新建、修改、删除 LoadBalancer 与 Backend 后，本机收敛并正确上报；删除 LoadBalancerHost 时清除
     本机全部配置并报告 `Purged`。——`agent.TestAgentConverges`。
   - [x] API 不可达期间不删除任何配置，恢复后按 list 结果清理。——`TestAgentConverges`：agent 经可切断的
     代理访问 API，切断期间删除对象后数轮收敛配置仍在，恢复后被清理。
   - [x] agent 重启后，先按已有配置恢复宣告，再继续收敛。前半由 `TestAgentAnnouncesFromHAProxy` 覆盖；
     后半由 `TestAgentConverges`：停机期间删除与新建的 LoadBalancer，重启后分别清理与下发。
4. [x] 中央 controller：按各主机上报汇总 LoadBalancer status；删除 LoadBalancer 与 LoadBalancerHost 时，
   finalizer 等到所有 agent 报告完成才放行；Lease 过期时 `AgentReady` 为 `Unknown`。
   ——`controller.TestAgentConvergence`、`TestLoadBalancerHostLifecycle`。
5. Dev 上：按「下线与迁移」的步骤切换 lb-dev-1/2，切换前后 VIP 不中断；最后关闭 `:5555`，确认控制面
   与 LB 主机之间已没有任何入站连接。
   - [x] 2026-11-04 以本地构建切换（join、manager 开关、主机 `converge`），每秒探测 VIP 共 1550 次无一失败；
     agent 上报的摘要与 controller 一致，临时 LoadBalancer 的新建与删除经 agent 在本机完成。
   - [ ] 关闭 `:5555`：Prod 仍远程收敛，随迁移第 3 步在两个环境都切换后进行。

## 测试合同

- domain 单测：期望配置的计算不变（从 manager 移到 agent 调用）；把各主机上报汇总成 LoadBalancer
  status 的规则。
- agent 单测：本机事实到 status apply 配置的转换；事实不变时不重复写入。
- 集成测试：按上面「待验证」第 1–4 项。HAProxy adapter 现有的集成测试保留，只是目标改为本机。
