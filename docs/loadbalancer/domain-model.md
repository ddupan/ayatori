# LoadBalancer 领域模型

状态：草案，待评审。日期：2026-10-04。决策依据见
[ADR-0012](../decisions/0012-l4-loadbalancer.md)。分层与命名沿用
[Database 领域模型](../database/domain-model.md#分层)。

## 统一语言

| 术语 | 含义 |
| --- | --- |
| LoadBalancer | 消费方声明的 L4 入口：VIP、listener、backend endpoint 与健康检查 |
| Address | 已分配给某个 LoadBalancer 的 VIP；以“名字即 IP”的 API 对象记录占用 |
| Pool | 当前环境可分配的 VIP 范围，首版来自 controller 配置 |
| Host | 一台 LB 主机：一个 HAProxy（经 Data Plane API 管理）与一个 gobgpd，首版来自 controller 配置 |
| Attachment | 某个 LoadBalancer 在某台 Host 上的落地进度：配置是否生效、VIP 是否宣告 |
| Announcement | 某台 Host 上的 gobgpd 对某个 VIP /32 的宣告 |

LoadBalancer 一对一 Address；首版每个 LoadBalancer 落地到全部 Host，每台 Host 一个 Attachment。
Host 不是 API 资源，也不持有 LoadBalancer 集合；Attachment 只作为 LoadBalancer status 的一部分
存在。

## 聚合与职责

### LoadBalancer 聚合（`domain/loadbalancer`）

唯一的聚合，保护以下不变量：

- **VIP 固定**：Address 一经确认写入 status 即不再改变；spec 请求的 VIP 由 CEL 设为不可变。
  需要换地址就新建 LoadBalancer。
- **先配置后宣告**：某 Host 只有在本轮观察到配置与期望一致且 reload 已成功后，才允许宣告。
- **先撤回后删除**：删除或从 Host 摘除时，先撤回宣告，确认撤回后才删除配置。
- **删除完成条件**：所有 Host 均已撤回并删除配置后才释放 Address、移除 finalizer。

listener、endpoint 与健康检查是 LoadBalancer 的核心语义，以领域包中的普通结构体表达期望
配置（见下节），也是 controller 与 adapter 的契约。字段合法性由 CRD schema/CEL 保证，聚合
不重复校验，也不为每个字段包值类型；翻译为 HAProxy 对象并与观察到的对象比较是 adapter 的
职责，adapter 只向聚合返回“一致/不一致”的事实。

```go
// 示意，具体签名在实现时确定。
func Reconstitute(snapshot Snapshot, deps Dependencies) *LoadBalancer

func (lb *LoadBalancer) PlanAddress(pool Pool) Action           // Allocate / Save / Wait
func (lb *LoadBalancer) AddressClaimed(ip netip.Addr, err error) // 结果分类见下
func (lb *LoadBalancer) PlanHost(host string, facts HostFacts) Action
// Apply / AwaitReload / Announce / Withdraw / Remove / Wait
func (lb *LoadBalancer) Programmed() bool
```

`HostFacts` 是 adapter 返回的原始事实：配置是否存在、是否与期望一致、最近一次 reload 的状态
（无 / 进行中 / 成功 / 失败）、VIP 是否已宣告。聚合据此给出该 Host 的下一步，并记录 Attachment。

### 期望配置

参照 AWS NLB、GCP、Azure LB、OpenStack Octavia 与 OCI NLB 的 L4 API，只采纳至少三家共有、
且 homelab 消费者确实需要的概念。各家都把 VIP、listener、后端集合、成员与健康检查拆开；
单个 CRD 的规模下，健康检查嵌入 port（同 AWS TargetGroup、Octavia pool、OCI backendSet），
不做 GCP/Azure 式的独立可复用资源。

```yaml
spec:
  address: 10.62.0.10          # 可选；不可变
  ports:
    - name: postgres
      port: 5432               # VIP 端口
      endpoints:               # 后端集合，消费方整组替换（Register/Deregister 的声明式版本）
        - address: 192.168.10.127   # IP 或 DNS 名称
          port: 5432
        - address: pg-lxc150.example.internal
          port: 5432
      proxyProtocol: None      # None | V1 | V2
      idleTimeout: 1h
      drainTimeout: 30s        # endpoint 被移出集合后保留已有连接的上限
      healthCheck:
        type: HTTP             # TCP | HTTP
        port: 8008             # 为空表示使用各 endpoint 自身端口（AWS traffic-port）
        path: /primary
        expectedStatuses: [200]
        interval: 5s
        timeout: 3s
        healthyThreshold: 2
        unhealthyThreshold: 3
```

每个 port 拥有自己的 endpoint 集合，endpoint 为“地址 + 端口”，同 Octavia pool/member 与 AWS
TargetGroup/Target。k8s Service 的每个端口对应一组“节点 IP + NodePort”。endpoint 地址可以是
DNS 名称，由数据面按平台配置的 resolver 周期解析（HAProxy `resolvers`）；解析结果只影响
数据面转发，不进入 Address 分配或宣告。健康检查端口覆盖作用于该 port 下的全部 endpoint。

固定行为（各家不一致，此处明确定义并由所有 adapter 以共同 contract test 保证）：

- **fail-closed**：某 port 没有健康 endpoint 时拒绝新连接，不回退到全部 endpoint。AWS/GCP
  默认 fail-open，与按角色选主（如 PostgreSQL primary）冲突。
- **不健康即断开**：endpoint 被健康检查判为失败时断开其已有连接（同 AWS 默认；HAProxy
  `on-marked-down shutdown-sessions`）。
- **移出即排空**：endpoint 从集合移除时停止新连接，已有连接最多保留 `drainTimeout`。

后续特性：上述行为确有需求时再开放为字段，参照 OCI `isFailOpen`、GCP
`connectionPersistenceOnUnhealthyBackends` 与 AWS `connection_termination`。例如健康检查抖动
会断开 k8s API 的长连接 watch，当前接受由客户端重连。

不采纳：权重、backup 成员、会话保持、负载算法选择（平台固定，适合长连接的 leastconn）、
`allowed_cidrs`、连接数上限、cross-zone、floating IP。连接超时等机械参数由平台配置。

健康检查使用 PROXY protocol 时探测也需携带 PROXY 头（AWS PPv2 同此行为），由 adapter 处理。

### Address 规则（`domain/address`）

VIP 的唯一性由 API server 的对象名唯一性保证，不在领域中实现锁或位图。领域只提供纯规则：

- `Pool.Next(taken)`：按固定顺序给出候选地址，排除已知占用；请求的指定 VIP 必须在 Pool 内。
- 认领结果判定：Create 成功即占用；AlreadyExists 时读取该对象，若其记录的 LoadBalancer 身份
  （namespace/name/UID）就是自己，视为上次创建后 status 未写入，直接采用；否则换下一个候选，
  指定 VIP 则报告冲突。这使“外部已创建、status 未写入”的窗口可以安全恢复，不需要 Conflict。
- 孤儿判定：Address 记录的 LoadBalancer 不存在或 UID 不同，且对象年龄超过宽限期，可删除。

## 控制器与后端

| Reconciler | 职责 |
| --- | --- |
| `LoadBalancerReconciler` | 加 finalizer、认领 Address、逐 Host 收敛与删除、写 status |
| `AddressReconciler` | 按孤儿规则回收无主 Address |

后端接口在 controller 中声明，只列使用方需要的方法：

- `ProxyBackend`（实现：`haproxy.Hosts`）：`Observe(host, lb)` 返回 `HostFacts` 的配置部分；
  `Apply(host, lb)`、`Remove(host, lb)` 执行变更。adapter 自行决定走 runtime server 接口
  （仅 endpoint 变化，无需 reload）还是配置变更 + reload，并处理 version/409 重试。
- `RouteBackend`（实现：`gobgp.Speakers`）：`Announced(host, vip)`、`Announce`、`Withdraw`。

两者独立：代理与路由宣告都可能被替换，且故障相互独立。Kubernetes 侧不加接口。

每轮对每台 Host 至多执行一次外部写入；Host 之间相互独立，一台 Host 不可达只影响它自己的
Attachment，不阻塞其他 Host。外部系统没有 watch，Reconciler 以固定周期重新观察，以发现
gobgpd 重启后丢失的宣告与配置漂移。

## 状态

LoadBalancer status：

- `address`：已确认的 VIP。
- `hosts[]`：每台 Host 的 Attachment（`Pending`、`Configured`、`Announced`、`Withdrawn`、
  `Unavailable`）与说明。
- `ports[].endpoints[]`：各 endpoint 在该 port 健康检查下的观察，只做呈现，不参与状态迁移——
  健康摘除由 HAProxy 在数据面完成。

conditions 参照 Octavia 将控制面下发（provisioning_status）与数据面健康（operating_status）
分开，命名沿用 Gateway API：

- `Programmed`：Address 已确认，且至少一台 Host 配置一致并已宣告。
- `Degraded`：存在未完成配置或宣告的 Host。
- `Available`：每个 port 至少有一个健康 endpoint。PostgreSQL 无 primary 时为 False，但
  `Programmed` 不受影响。

Host 从配置中移除时，其 Attachment 不再参与 Programmed 判定；残留的宣告随该主机下线而失效。

## 删除

```text
任意阶段 → Deleting
Deleting：逐 Host Withdraw → 确认撤回 → Remove → 确认配置已删除且 reload 成功
全部 Host 完成 → 删除 Address → 移除 finalizer
```

Host 不可达时删除等待，不跳过；需要强制完成时由管理员先将该 Host 移出配置。

## 待评审

1. **HAProxy 对象归属**：以 LoadBalancer UID 命名 frontend/backend，使 Observe 无需额外索引。
   Ayatori 是 `haproxy.cfg` 的唯一写入方，因此可以按主机清扫无主对象与无主宣告；首版只依赖
   finalizer 正常删除，清扫留作后续，是否接受？
2. **内置 `IPAddress` 的 parentRef 不含 UID**：同名重建的 LoadBalancer 会采用旧 Address。若复用
   内置类型，需要以 label/annotation 补记 UID；若改用自定义 CRD，可直接在 spec 中记录。由切片 1
   的 envtest 结果决定。
3. **指定 VIP 的授权**：首版任何有权创建 LoadBalancer 的 namespace 都可请求 Pool 内的空闲 VIP；
   按 namespace 限定可请求范围留作后续。
4. **Pool 与 Host 的配置来源**：首版放在 controller 配置，变更需重启 controller；出现频繁变更
   或多池需求时再改为 API 资源。

## 测试分层

- 领域单测：VIP 候选与认领判定、孤儿判定、`PlanHost` 在各 `HostFacts` 组合下的下一步（含
  reload 失败、宣告先于配置的拒绝、删除顺序）、Programmed/Degraded/Available 判定。
- envtest：Address 并发认领冲突、同名重建、finalizer、controller 重启后从 status 与 Address 恢复。
- adapter 集成测试：容器中的真实 HAProxy + dataplaneapi 与 gobgpd + 对端 gobgp。
