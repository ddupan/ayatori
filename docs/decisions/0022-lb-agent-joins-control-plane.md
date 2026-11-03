# ADR-0022：LB agent 以客户端证书加入控制面，注册 Node 并自己报告状态

- 状态：Proposed（依赖 [ADR-0021](0021-client-certificates-via-csr.md)，并须先完成下文「待验证」的事项）
- 日期：2026-11-03
- 依据：维护者讨论（compute 节点改为以 CSR 加入控制面之后，LB 也对齐同一做法）。
- 部分取代：[ADR-0018](0018-lb-backend-and-host-resources.md)「agent 状态接口」中「controller
  周期读取 `GET /status`」一条，以及 LoadBalancerHost 的 `spec.agentURL`。

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
| 证书文件 | `/etc/ayatori/lb-agent/client.pem`（证书与私钥合一，原子替换），`kubeconfig` 按相对路径引用 |
| 续签 | agent 进程内使用 `internal/infra/clientcert`，由 ayatori-manager 的批准者按 ADR-0021 批准 |

- 节点通用的权限（写自己的 Node 与 Lease）授予 `system:ayatori:nodes`；角色相关的权限只授予角色组。
  compute 的 `LibvirtDomain` 等权限也应授予 compute 自己的角色组，不要授予 `system:ayatori:nodes`，
  否则 LB 主机也能读到计算后端的对象。
- **首次签发**由 Ayatori 部署 role 完成，它本来就按 inventory 创建 LoadBalancerHost：
  1. 为每台主机创建 15 分钟有效的 bootstrap token；
  2. 把 `bootstrap-kubeconfig` 写到主机上，文件只属于 root；
  3. 等 agent 提交 CSR，核对申请的正是这台主机的身份后批准，不符合的拒绝。

  bootstrap token 只经过部署机，有效期很短，而且只能用来提交 CSR。私钥只在 LB 主机上生成。
- **Cinc 不碰凭据。** cookbook 只负责安装 agent、写 unit 和本地配置；`/etc/ayatori/lb-agent/` 下的
  凭据文件由部署 role 和 agent 管理。这和 ADR-0021「私钥只在使用证书的机器上生成」一致，也让
  cookbook 不必依赖控制面。

### 注册为 Node

与 compute Agent（ADR-0007、ADR-0015）使用同一套节点注册逻辑：

- agent 启动后以 SSA 创建并维护自己的 Node，带上 Ayatori ownership label 和角色 label
  `node-role.kubernetes.io/loadbalancer`；地址与 `nodeInfo` 由 agent 填写。
- 心跳是 `kube-node-lease` 中与 Node 同名的 Lease，与 compute 节点相同。
- Node 只表示「这台主机加入了控制面、仍然活着」。LB 自己的事实（各 VIP 是否宣告）仍写在
  LoadBalancerHost 中，见下节。
- 选择计算节点时，必须按 compute 角色 label 正向挑选，不能用「排除 LB」的方式；控制面没有
  kube-scheduler，Node 上的 taint 不会自动生效。
- Node 由 agent 注册，但 LB 主机列表仍以部署方创建的 LoadBalancerHost 为准：已经加入、却没有
  对应 LoadBalancerHost 的主机不会收到任何 LoadBalancer 配置。

### 状态由 agent 写入

- agent 以 server-side apply（field manager `lb-agent`）写自己那台 LoadBalancerHost 的
  `status.vips` 和 `AgentReady` condition。内容与语义沿用 ADR-0018：三层自检、未宣告的原因、
  「至少一个会话建立」等。只要本机事实有变化，或者距上次写入已满一个周期，agent 就会写入。
- controller 照搬 Node lifecycle controller 的做法：Node 的 Lease 超过 `AgentStaleAfter` 未续约时，把
  `AgentReady` 置为 `Unknown`（reason `AgentStale`），并清空 `status.vips`。agent 恢复后写回
  `True`/`False`。`AgentReady` 因此有两个写入方，和 kubelet 与 node lifecycle controller 共写 Node
  `Ready` 一样；controller 以 force apply 覆盖。
- `DataPlaneReachable` 仍由 controller 写入。本 ADR 不改配置下发，manager 依旧推送到 Data Plane API。
- LoadBalancer 的 Attachment 仍以 `status.vips` 为准，规则不变。只是数据来源从 controller 读取
  `/status` 变成了 agent 写入。

### 权限

- RBAC：
  - `system:ayatori:nodes` 可以 `get/create/patch/update` Node 和 `nodes/status`，在 `kube-node-lease`
    中可以 `get/create/update` Lease。与 compute 共用。
  - `system:ayatori:lb-hosts` 可以 `get/list/watch` LoadBalancerHost，`patch/update`
    `loadbalancerhosts/status`。
- ValidatingAdmissionPolicy（与 dev/compute 上 compute 的 PoC 09 同一做法）按 `request.userInfo` 收窄：
  - 节点通用规则，与 compute 共用：Node 与 Lease 只能是与自己同名的那个；Node 的角色 label 一旦写入
    就不能更改。LB 主机不能把自己改成计算节点，反之亦然。
  - LB 规则：只能写与自己同名的 LoadBalancerHost 的 status，而且只能改 `vips` 和 type 为
    `AgentReady` 的 condition。
- 读权限不按主机限制：LoadBalancerHost 不含凭据，每台主机读到其他主机的 status 也无妨。

### API 变化

- `spec.agentURL` 改为可选，并标为弃用。controller 不再读它，下一个不兼容版本移除。部署 role 停止
  生成它。
- agent 的 `/status` 保留在指标端口上，供本机排障与 Prometheus 之外的人工检查，不再是控制面的依赖。

## 已接受的代价

- **LB 主机多了一个对控制面的依赖：证书。** agent 停机超过证书剩余有效期（正常为数月）后，需要重跑
  部署 role 重新首次签发。路由不受影响：宣告与否仍只由本机决定（ADR-0017），控制面不可达时 agent
  照常宣告，只是不再上报。
- **部署 role 要能 ssh 到 LB 主机**，用于写入 `bootstrap-kubeconfig`。目前只有 Cinc 的 converge
  ssh 到这些主机。
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
  地址也只能放进 annotation。而且主机加入（Node）和主机被允许承接流量（部署方创建的
  LoadBalancerHost）是两件事，删除时「先清理该主机上的配置再放行」的语义也属于后者。
- **LB 主机不注册 Node，单独用 `ayatori-system` 中的 Lease**：节点身份、心跳和 VAP 就得为 LB 再做一套，
  而用 role label 区分 LB 与计算节点并不难。

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

1. envtest 中：VAP 只允许 `system:ayatori:node:<名称>` 写自己那台 LoadBalancerHost 的 `vips` 与
   `AgentReady`，写其他主机、其他 condition 或 spec 一律拒绝；Node 与 Lease 只能写自己那份，
   角色 label 不能更改。
2. envtest 中：agent 的 SSA 与 controller 的 force apply 共写 `AgentReady` 时，Lease 过期置为
   `Unknown`，agent 恢复后回到 `True`，`DataPlaneReachable` 不受影响。
3. Dev 上：部署 role 为 lb-dev-1/2 完成首次签发，agent 写入 status，并按 ADR-0021 自行续签；
   manager 不再连接 :9641（LB 主机上可以关掉该端口对控制面的放行来确认）。

## 测试合同

- agent 单测：本机事实（沿用 `/status` 的结构）到 status apply 配置的转换；事实不变时不重复写入。
- controller 单测：Lease 新鲜与过期时的 `AgentReady`；过期时清空 `vips`。
- 集成测试：envtest 中，以 impersonation 模拟两台 LB 主机和一台计算节点的 agent，覆盖上面的 VAP
  用例；
  `internal/infra/clientcert` 已有的集成测试覆盖签发与续签，这里不重复。
