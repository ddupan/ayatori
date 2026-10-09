# LoadBalancer 领域模型

状态：已评审，API 字段随切片 1 落地。日期：2026-10-05。决策依据见
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
| Hostname | Ayatori 为 LoadBalancer 生成并维护的域名，A 记录指向其 VIP |

LoadBalancer 一对一 Address、一对一 Hostname；首版每个 LoadBalancer 落地到全部 Host，每台 Host 一个 Attachment。
Host 不是 API 资源，也不持有 LoadBalancer 集合；Attachment 只作为 LoadBalancer status 的一部分
存在。

## 聚合与职责

### LoadBalancer 聚合（`domain/loadbalancer`）

唯一的聚合，保护以下不变量：

- **VIP 固定**：Address 一经确认写入 status 即不再改变；spec 请求的 VIP 由 CEL 设为不可变。
  需要换地址就新建 LoadBalancer。
- **先配置后宣告**：某 Host 只有在本轮观察到配置与期望一致且 reload 已成功后，才允许宣告。
- **先撤回后删除**：删除或从 Host 摘除时，先撤回宣告，确认撤回后才删除配置。
- **先删记录后释放**：Hostname 的记录删除确认后才释放 Address，避免名字指向已分给他人的 VIP。
- **删除完成条件**：所有 Host 均已撤回并删除配置、记录已删除后才释放 Address、移除 finalizer。

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
      drainTimeout: 30s        # endpoint 被移出后保留已有连接的上限
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
DNS 名称，由数据面经 LAN resolver（Blocky，`lab.ddupan.top` 条件转发至 PowerDNS）周期解析
（HAProxy `resolvers`）；解析结果只影响
数据面转发，不进入 Address 分配或宣告。健康检查端口覆盖作用于该 port 下的全部 endpoint。

固定行为（各家不一致，此处明确定义并由所有 adapter 以共同 contract test 保证）：

- **fail-closed**：某 port 没有健康 endpoint 时拒绝新连接，不回退到全部 endpoint。AWS/GCP
  默认 fail-open，与按角色选主（如 PostgreSQL primary）冲突。
- **不健康即断开**：endpoint 被健康检查判为失败时断开其已有连接（同 AWS 默认；HAProxy
  `on-marked-down shutdown-sessions`）。
- **移出即排空**：endpoint 从集合移除时转为 drain，停止新连接；会话归零或超过端口的
  `drainTimeout`（默认 30s）后删除。超时仍有会话时删除触发 reload，剩余连接由旧 worker
  服务至 LB 主机的 `hard-stop-after`。
- **新 server 先验证后接流量**：server 以 `init-state fully-down` 起始，连续通过检查才接收流量，
  避免新加入的副本在首次检查前收到连接。

后续特性：上述行为确有需求时再开放为字段，参照 OCI `isFailOpen`、GCP
`connectionPersistenceOnUnhealthyBackends` 与 AWS `connection_termination`。例如健康检查抖动
会断开 k8s API 的长连接 watch，当前接受由客户端重连。

不采纳：权重、backup 成员、会话保持、负载算法选择（平台固定，适合长连接的 leastconn）、
`allowed_cidrs`、连接数上限、cross-zone、floating IP。连接超时等机械参数由平台配置。

健康检查使用 PROXY protocol 时探测也需携带 PROXY 头（AWS PPv2 同此行为），由 adapter 处理。

### Hostname

参照 AWS NLB 的 DNS 名称：每个 LoadBalancer 获得 `<name>.<namespace>.<zone>`，A 记录指向
VIP，公布在 `status.hostname`。消费方需要可读名称时，在自己的记录中以 CNAME 指向它。
不由 Ayatori 维护 DNS 的消费方（包括未部署 external-dns 的 k8s 集群）因此也能获得稳定名称。

- zone 由 Ayatori 独占写入，经 PowerDNS HTTP API 管理：Prod `lb.lab.ddupan.top`，Dev
  `dev-lb.lab.ddupan.top`。独占 zone 使归属无需 TXT 标记，也不与 Terraform、external-dns、
  Poweradmin 管理的名字冲突。zone 本身由 homelab-infra 创建并委派。
- 首版不支持在 spec 中指定任意名称。
- Address 确认后即发布记录，不等待 `Programmed`：VIP 固定，记录不随健康变化。
- 名称由 namespace/name 决定；同名重建的 LoadBalancer 覆盖记录为自己的 VIP。CRD 要求
  LoadBalancer 名称为 DNS label。记录 TTL 为 60 秒；status 只在回读确认指向 VIP 后写入
  `hostname`，并以 `HostnamePublished` condition 呈现。
- 删除时在全部 Host 清理后删除记录，回读确认不存在才释放 Address。

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

Address 复用内置 `networking.k8s.io/v1 IPAddress`（envtest 1.37 验证：Service CIDR 外的地址
可以创建，名称强制规范化，重复创建返回 AlreadyExists，非 `ipallocator.k8s.io` managed-by 标签的
对象不被 apiserver repair loop 回收；parentRef 可指向 CRD）。parentRef 不含 UID，以
annotation 记录 LoadBalancer UID。VIP 池不得与 Ayatori API server 的 Service CIDR 重叠。

后端接口在 controller 中声明，只列使用方需要的方法：

- `ProxyBackend`（实现：`haproxy.Hosts`）：`Observe(host, lb)` 返回 `HostFacts` 的配置部分；
  `Apply(host, lb)`、`Remove(host, lb)` 执行变更。adapter 自行决定走 runtime server 接口
  （仅 endpoint 变化，无需 reload）还是配置变更 + reload，并处理 version/409 重试。
- `RouteBackend`（实现：`gobgp.Speakers`）：`Announced(host, vip)`、`Announce`、`Withdraw`。
- `DNSBackend`（实现：`powerdns.Zone`）：`Record(name)`、`Publish(name, vip)`、`Remove(name)`。

两者独立：代理与路由宣告都可能被替换，且故障相互独立。Kubernetes 侧不加接口。

每轮对每台 Host 至多执行一次外部写入；Host 之间相互独立，一台 Host 不可达只影响它自己的
Attachment，不阻塞其他 Host。外部系统没有 watch，Reconciler 以固定周期重新观察，以发现
gobgpd 重启后丢失的宣告与配置漂移。

## 状态

LoadBalancer status：

- `address`：已确认的 VIP。
- `hostname`：已发布的域名。
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
全部 Host 完成 → 删除 Hostname 记录并确认 → 删除 Address → 移除 finalizer
```

Host 不可达时删除等待，不跳过；需要强制完成时由管理员先将该 Host 移出配置。

## HAProxy 映射与部署契约

adapter（`adapter/haproxy`）经 Data Plane API v3 的 `full_section` 读写，客户端由
`make generate-dataplaneapi` 从固定版本的 OpenAPI spec 生成（去掉 `x-go-type`，不依赖
client-native）。每个端口对应 frontend `lb_<uid>_<port>` 与 backend `lb_<uid>_<port>_backend`
（HAProxy 3.3 起二者不能同名）；同一 LoadBalancer 的全部端口在一个事务中提交，只触发一次
reload。Observe 将期望配置翻译后与回读结果逐字段比较，并以主机最近一次 reload 结果判断是否
已生效。

LB 主机部署（homelab-infra）须保证：

- `haproxy.cfg` 中 `lb_` 前缀 section 只由 Ayatori 写入；
- `net.ipv4.ip_nonlocal_bind=1`，frontend 可在宣告前绑定 VIP；
- reload 前以 `server-state-file` 保存 server 状态，否则每次 reload 所有 server 回到
  `fully-down`，主机上全部 LB 中断约 rise × interval；
- `hard-stop-after` 限定旧 worker 排空时长；
- DNS endpoint 所用的 `resolvers` section 指向 LAN resolver。

## Controller 配置

| 参数 | 含义 |
| --- | --- |
| `--loadbalancer-address-pool` | VIP 池；为空时不启用 LoadBalancer |
| `--loadbalancer-host=<名称>,<Data Plane API URL>,<gobgpd 地址>` | LB 主机，可重复；为空时只分配 VIP |
| `--loadbalancer-credentials=<namespace>/<name>` | 凭据 Secret：`username`/`password`（Data Plane API Basic）与 `tls.crt`/`tls.key`/`ca.crt`（两个 API 的 mTLS） |
| `--loadbalancer-connect-timeout`、`--loadbalancer-resolvers` | 代理连接后端超时；DNS endpoint 的 resolvers section |
| `--loadbalancer-dns-url`、`--loadbalancer-dns-zone` | PowerDNS API 与独占 zone；同时设置时维护 Hostname，API key 取凭据 Secret 的 `powerdns-api-key` |

凭据 Secret 由部署方以 ExternalSecret 从 OpenBao 同步到 controller namespace，与 PostgreSQL 管理
凭据相同。客户端证书由各环境独立的根 `pki-loadbalancer-<env>` 签发（3 天寿命，ESO 每天重签），
manager 每分钟从 Secret 刷新、每次 TLS 握手取最新证书，轮换无需重启；刷新失败时沿用上一张。
服务端 CA 与 Basic 凭据只在启动时读取：CA 是寿命 10 年的内部根，轮换时重启 manager。
LB 主机由 homelab-cookbooks 的 `homelab_loadbalancer` 部署，须以 `<hostname>.lab.ddupan.top`
访问（ACME 证书只有 DNS SAN）。

## 待评审

1. **HAProxy 对象归属**：以 LoadBalancer UID 命名 frontend/backend（见上节），使 Observe 无需额外索引。
   Ayatori 是 `haproxy.cfg` 的唯一写入方，因此可以按主机清扫无主对象与无主宣告；首版只依赖
   finalizer 正常删除，清扫留作后续，是否接受？
2. **指定 VIP 的授权**：首版任何有权创建 LoadBalancer 的 namespace 都可请求 Pool 内的空闲 VIP；
   按 namespace 限定可请求范围留作后续。
3. **Pool、Host 与 DNS zone 的配置来源**：首版放在 controller 配置，变更需重启 controller；出现频繁变更
   或多池需求时再改为 API 资源。

## 测试分层

- 领域单测：VIP 候选与认领判定、孤儿判定、`PlanHost` 在各 `HostFacts` 组合下的下一步（含
  reload 失败、宣告先于配置的拒绝、删除顺序）、Programmed/Degraded/Available 判定。
- envtest：Address 并发认领冲突、同名重建、finalizer、controller 重启后从 status 与 Address 恢复。
- adapter 集成测试：容器中的真实 HAProxy + dataplaneapi、gobgpd + 对端 gobgp、PowerDNS。

## 实现进度

- 切片 1（API 与 VIP 分配）：`network.ayatori.ddupan.top/v1alpha1 LoadBalancer`、Address 认领/释放与
  孤儿回收已实现；`--loadbalancer-address-pool` 为空时不启用。数据面、Hostname 尚未接入，
  `Programmed` 恒为 False（Reason `DataPlanePending`）。CRD 暂不加入 `deploy/flux/crds`，
  待数据面切片可用后再部署。
- 切片 2（HAProxy adapter）：`haproxy.Hosts` 的 Observe/Apply/Remove 已实现，以真实 HAProxy 3.4 +
  dataplaneapi v3.4.3 容器做 contract test（回读一致、端口增删、不影响其他 LoadBalancer、
  经 VIP 的健康转发与无健康后端时拒绝）。尚未接入 Reconciler。
- 切片 3a（gobgpd adapter）：`gobgp.Speakers` 的 Announced/Announce/Withdraw 已实现，使用官方
  `github.com/osrg/gobgp/v4/api`；contract test 在进程内运行两个真实 GoBGP 并建立 eBGP 会话，
  验证对端收到与撤回 /32、重复操作幂等。尚未接入 Reconciler（3b）。
- 切片 4（endpoint 走 runtime）：section 集合与 server 以外的参数不变时，adapter 逐个 server
  收敛：新增经 server 接口写入（runtime 生效、不 reload）后显式设为 ready（dataplaneapi 动态
  添加的 server 停在 maint）；移除先 drain，会话归零或超过 `drainTimeout` 后转 maint 并删除。
  排空开始时刻只记在 controller 内存，重启后重新计时（只会延长排空）。新建、端口增减或检查、
  超时等变化仍整段替换并 reload。Observe 对 IP endpoint 额外要求 runtime 为 ready；运行中的
  进程尚未加载 backend 时视为 reload 未生效。contract test 验证新增与无会话移除不 reload、
  有会话时在 `drainTimeout` 内保留。
- 切片 3b（接入 Reconciler）：`PlanHost` 按主机收敛（配置一致且 reload 成功后宣告；配置变化时
  保留宣告重写；删除先撤回再删配置，任一主机不可达则等待），status 写 `hosts[]`、`Programmed`
  与 `Degraded`；bootstrap 读取主机清单与凭据 Secret。Hostname/DNS 记录尚未接入。
- Hostname：`powerdns.Zone`（`github.com/joeig/go-powerdns/v3`）维护 A 记录，以 PowerDNS
  Authoritative 5.0.7 容器做 contract test；Reconciler 在 VIP 确认后发布、删除时主机清理后
  先删记录再释放 VIP。
