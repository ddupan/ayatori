# PostgreSQL Tenant Operator 系统规格说明书

| 项目 | 内容 |
| --- | --- |
| 状态 | Approved |
| 目标 API | `database.ayatori.ddupan.top/v1alpha1` |
| 最后更新 | 2026-09-13 |
| 批准日期 | 2026-09-10 |
| 规范范围 | 首次注册外部 PostgreSQL 实例并创建一个应用租户 |

本文档定义系统对用户和外部依赖呈现的行为，是 API、测试和实现共同遵守的合同。
实现若需要改变本文合同，必须先修改规格并重新获得批准。

文中的“必须”“禁止”“应当”“可以”分别对应强制要求、强制限制、推荐行为和可选
行为。

## 1. 背景

homelab 中的大部分应用共享一个运行在独立 VM 上的 PostgreSQL DBMS。应用需要各自
独立的 database、作为 owner 的 login role 和密码，但不需要独立 PostgreSQL 实例。
目前这些资源依靠人工 SQL 和人工 Secret 管理，难以重复、审计和检测漂移。

本系统使用 Kubernetes CRD 作为声明式 API，持续协调外部 PostgreSQL 与 OpenBao：

```text
PostgreSQLInstance / PostgreSQLTenant
                  |
                  v
      Ayatori Database controller
          |                 |
          v                 v
 PostgreSQL catalog    OpenBao KV v2
```

## 2. 目标

v1alpha1 必须实现以下目标：

1. 注册一个已经存在的外部 PostgreSQL 实例并报告连接状态。
2. 为一个应用租户创建独立 database 和一个同时作为 database owner 的 login role。
3. 根据实例实际可安装扩展列表检查并安装租户申请的 PostgreSQL extension。
4. 首次生成高强度长期密码，并只把凭据明文写入 OpenBao KV v2。
5. 为 Kubernetes 应用创建 ExternalSecret，由 ESO 将凭据投射到同 namespace Secret。
6. 同时输出 OpenBao API URL，使 Kubernetes 外的应用可以直接读取凭据。
7. 同时输出 PostgreSQL DNS hostname 和 IP address，不假定所有消费者都能使用集群内
   DNS。
8. 持续检测并修正由本系统管理的非破坏性漂移。
9. 通过 Kubernetes Condition 报告进度、成功和可操作的失败原因。
10. 重复 reconcile、controller 重启及外部依赖暂时失败不得重复创建或破坏资源。
11. 删除 Tenant CR 时默认保留外部资源；显式选择 `Delete` 时提供完整清理路径。

## 3. 非目标

v1alpha1 不负责：

- 创建、升级、备份或高可用运行 PostgreSQL DBMS/VM；
- 创建或运维 OpenBao；
- 直接写入包含凭据明文的 Kubernetes Secret；Secret 必须由 ESO 投射；
- 动态凭据、定时或自动密码轮换；
- Web UI、独立 REST API 或 Backstage 插件；
- 跨实例迁移 database；
- schema/table 级别租户、多 login role 或跨租户 grant；
- 删除不属于本系统管理的 database、role、extension 或 OpenBao Secret；
- 接管不是由本系统创建的外部资源；
- 提供生产环境 SLA。

## 4. 参与者与事实来源

| 对象 | 事实来源 | 说明 |
| --- | --- | --- |
| 期望状态 | Kubernetes CR `spec` | 用户声明的合同 |
| 最近观察结果与当前阶段 | Kubernetes CR `status` | 可以丢失并重建，不是外部事实来源 |
| database/role/grant/extension | PostgreSQL catalog | 每轮 reconcile 必须重新读取 |
| 受管资源所有权与保留标记 | PostgreSQL controller registry | 与受管 DBMS 一起备份和恢复 |
| controller 工作流阶段 | Kubernetes CR `status.phase` | 状态机 checkpoint；可由外部事实保守重建 |
| 应用凭据 | OpenBao KV v2 | Kubernetes API 中不得出现明文 |
| Kubernetes 凭据投射 | External Secrets Operator | ExternalSecret 由本 controller 管理 |
| PostgreSQL 管理凭据 | controller namespace 的 Kubernetes Secret | 管理员维护 ExternalSecret，由 ESO 同步；Instance 只引用 Secret |

平台管理员管理 `PostgreSQLInstance`、controller 部署配置、OpenBao policy 和
PostgreSQL 管理 role。应用或 GitOps 流程在获得 namespace RBAC 后管理
`PostgreSQLTenant`。

## 5. 资源模型

### 5.1 PostgreSQLInstance

`PostgreSQLInstance` 是 cluster-scoped 资源，表示一个已经存在、可由 controller
管理的 PostgreSQL server。

它必须声明：

- PostgreSQL host、port 和管理连接使用的 database；
- PostgreSQL host address，供无法解析 DNS 的消费者使用；
- TLS mode；
- controller namespace 中 PostgreSQL 管理 Secret 的名称和字段名。

可安装的 extension 集合由应用层从目标 PostgreSQL 查询，不由管理员在 Instance
中声明。v1alpha1 不实现 allowlist；该概念保留为后续可选策略。实际可用不代表安装
权限及前置条件已满足，安装仍需执行并回读；查询失败不得被解释为扩展不支持。

实例 Ready 不代表 PostgreSQL 数据有备份或高可用，只表示 controller 当前可以安全
建立管理连接、读取 server metadata、访问 controller registry 并使用所需管理能力。

Instance 身份和 endpoint 以管理员声明为准。修改 endpoint 不验证是否仍是原物理
服务器或原 registry，不增加服务器／安装身份绑定检查；但旧配置观察失效，必须按
新配置重新检查连接和管理能力。新 CR 按新 Instance 处理，不自动接管旧 UID 的租户
资源。controller 不迁移旧服务器上的数据，也不清理旧目标，影响由管理员负责评估。

Instance 开始受管时即添加 finalizer，成功保存后才参与供应，不等发现 Tenant 后
再补加。删除期间停止新供应；仍有引用它的 Tenant（包括正在删除的 Tenant）时保留
finalizer，无引用后才移除。不级联删除 Tenant 或任何外部数据库、角色、凭据。
引用检查失败不得当作无引用。首版不引入跨对象锁或准入控制：finalizer 不禁止同时
创建 Tenant CR，新 Tenant 遇到正在删除或已不存在的 Instance 时不得开始供应。
这不保证列表检查、CR 创建和在途外部操作之间的原子性；不是严格的跨对象事务。

### 5.2 PostgreSQLTenant

`PostgreSQLTenant` 是 namespaced 资源。v1alpha1 中，一个 Tenant 精确对应：

- 一个 `PostgreSQLInstance`；
- 一个 database；
- 一个同时作为 database owner、供应用使用的 `LOGIN` role；
- 零个或多个 extension；
- 一个 OpenBao KV v2 凭据位置；
- 一个同 namespace ExternalSecret 及其目标 Kubernetes Secret。

Tenant 的 namespace 用于 Kubernetes RBAC 和身份识别，不代表 PostgreSQL schema。
同一 Instance 中的 database 和 role 名称全局唯一。

## 6. 标识与默认值

以下是 v1alpha1 的标识合同：

| 字段 | 默认值 | 约束 |
| --- | --- | --- |
| Instance port | `5432` | 1–65535 |
| Instance host address | 无 | 必须是合法 IPv4 或 IPv6 address |
| 管理 database | `postgres` | 合法 PostgreSQL identifier |
| TLS mode | `verify-full` | 禁止隐式降级 |
| Tenant database | `metadata.name` | 同一 Instance 全局唯一 |
| login role | `metadata.name` | 同一 Instance 全局唯一 |
| deletion policy | `Retain` | `Retain` 或 `Delete` |

Tenant 的 `spec.instanceRef` 与 `metadata.name` 长度合计不得超过 241 个字符，确保
派生的 ExternalSecret/Secret 默认名称
`<instanceRef>-<metadata.name>-postgresql` 不超过 Kubernetes 253 字符限制。

固定默认值由 CRD defaulting 写入。依赖 `metadata.name` 或 `instanceRef` 的 database、
login role、ExternalSecret/Secret 名称属于 controller 语义默认值：省略字段不会被 admission
回写，controller 必须始终计算同一个 effective value，并通过 status 的 database、
loginRole、credential reference 以及实际资源展示。
v1alpha1 不为此引入 mutating webhook。

database 和 role 名称必须作为 PostgreSQL identifier 参数安全引用，禁止通过字符串
拼接执行。名称校验必须拒绝空字符串、NUL 和超过 PostgreSQL identifier 长度限制的
值，并统一限制为小写字母、数字和下划线。

Tenant 首次成功后，`instanceRef`、database、login role 和凭据位置必须
不可变。修改这些字段不是 rename 或 migration，API 必须拒绝或报告明确的
`ImmutableField`。

## 7. PostgreSQL 权限合同

建议的 v1alpha1 权限模型如下：

1. database 必须由 login role 拥有。
2. login role 必须是 `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION`。
3. 必须撤销 `PUBLIC` 对租户 database 的连接权限，再显式允许 login role 连接。
4. controller 不得修改其他 database 或无关 role 的权限。
5. controller 只保证请求的 extension 存在；移除 extension 不得自动执行
   `DROP EXTENSION`。

这意味着应用可以在自己的 database 内执行 schema migration，但不能创建其他
database、role 或访问其他租户。v1alpha1 不创建只有形式意义、却未隔离运行时权限的
额外 `NOLOGIN` owner。若以后应用能分别使用 migration 和 runtime 凭据，再通过新的
权限 profile 引入 owner/migrator/runtime 角色模型。

## 8. OpenBao 凭据合同

### 8.1 Controller 自身认证

controller 必须使用 Kubernetes auth 登录 OpenBao。controller 使用的 OpenBao API
address、提供给消费者的 OpenBao API address、auth mount、auth role 和 KV v2 mount
属于部署配置，不属于任何 CR。两个 API address 可以相同；若 controller 使用集群内
地址而外部消费者不能解析，则必须单独配置 consumer address。生产部署的 KV mount
默认为 `kv`；开发环境可以配置为 OpenBao dev server 默认的 `secret`。长期 OpenBao
Token 禁止写入 Deployment、CR 或镜像。

### 8.2 管理凭据

`PostgreSQLInstance` 只引用 controller 自身 namespace 中 Kubernetes Secret 的名称及
用户名、密码字段名，不允许指定 namespace 或 Bao path。endpoint 仍由 Instance 声明。
平台管理员维护 ExternalSecret，将 OpenBao 管理凭据同步到该 Secret；controller 只读
Secret，不创建或修改管理 Secret、其 ExternalSecret 或上游管理凭据。

Instance 管理连接不直接访问 Bao，不负责管理密码轮换。已装配的凭据仍可访问
PostgreSQL 且满足 registry／权限要求时，Bao 或 ESO 暂时不可用不使 Instance NotReady。
首次装配无法取得有效 Secret 时不能 Ready。检测到所引用 Secret 的有效用户名或密码
变化时，controller 使用新值重建管理连接池并重新检查管理能力；仅 metadata 或无关
字段变化不触发重建。刷新不依赖 Instance generation 变化，新连接验证失败按实际
故障报告，不能用旧连接的成功结果证明新凭据可用。
controller 不修改 PostgreSQL 密码、不回写 Secret，也不修改 Bao 管理凭据；数据库侧
凭据变更由管理员负责。这是跟随已提供凭据的连接刷新，不是自动密码轮换。
Tenant 凭据管理仍直接依赖 Bao。

### 8.3 租户凭据

Tenant 不声明凭据 path。controller 根据部署级 KV mount、base path 和 Tenant 的
namespace/name 推导唯一的 mount-relative path。base path 来自 controller 启动参数
`--openbao-tenant-base-path`，默认 `postgresql-tenants`。最终路径为
`<base-path>/<namespace>/<metadata.name>`。推导结果禁止以 `/` 开头，禁止包含空路径段、
`.`、`..`，也禁止把 KV v2 HTTP API 的 `data` 或 `metadata` 层编码进路径。

新 Tenant 的凭据建立顺序必须可从任意中断点恢复：

1. 验证 Instance、名称、extension 和目标 OpenBao 路径；
2. 确认目标 database、role 和 OpenBao 记录不存在，或能够验证为同一 Tenant
   已创建的部分状态；
3. 生成密码；
4. 先创建带 controller 所有权 metadata 的 OpenBao KV v2 记录；
5. 从 OpenBao 重新读取凭据；
6. 使用该凭据创建作为 database owner 的 login role 和其他 PostgreSQL 资源；
7. 用 login role 实际连接目标 database；
8. 全部验证成功后将 Tenant 标记 Ready。

若第 4 步成功、后续 PostgreSQL 操作失败，下一轮必须读取同一份 OpenBao 凭据继续，
不得生成第二个密码。若 PostgreSQL 先存在而 OpenBao 记录不存在，controller 必须报告
Conflict，不得擅自重置已有 role 密码。

controller 必须在 PostgreSQL 管理 database 的专用 registry schema 中持久保存可验证的
Instance UID、Tenant UID 与 namespace/name 关联，不能只依赖会丢失的 CR status 判断
资源所有权。registry 必须可回读且不得改变数据库授权语义；database 或 role COMMENT
不能作为权威所有权记录。

默认写入字段固定为：

```text
username
password
database
host
hostaddr
port
sslmode
```

这些字段是 controller 的规范化输出合同。controller 不生成包含密码的 URI、JDBC URL
或应用专用键名。应用通过 ExternalSecret template、Helm values 或自身配置把原子字段
映射为 `DATABASE_URL`、独立环境变量或配置文件；因此 URI escaping 和应用特有格式也
由消费方负责。`host` 是 DNS 名称，`hostaddr` 是可直接连接的 IP；消费者自行选择其
支持且可达的连接目标。PostgreSQL server 证书必须同时包含与 `host` 匹配的 DNS SAN
和与 `hostaddr` 匹配的 IP SAN，使两种目标都能在 `verify-full` 下独立完成身份验证。

### 8.4 凭据输出与 ExternalSecret

controller 必须根据部署级 base path 推导 Tenant 的 KV path，Tenant 不能选择 mount 或
任意远端路径。ExternalSecret 固定命名为
`<instanceRef>-<metadata.name>-postgresql`。Tenant 可以通过
`spec.credential.secretName` 指定目标 Kubernetes Secret 名称；省略时使用同一默认名。
自定义名称只需是合法 Kubernetes Secret 名称，不限制命名内容；两者均与 Tenant 位于
同一 namespace。

controller 必须创建同 namespace ExternalSecret，从固定的 ClusterSecretStore 读取七个
原子字段。ExternalSecret 及目标 Secret 的名称通过 Tenant status 暴露。controller
不得直接读取 OpenBao 密码后写入 Kubernetes Secret。

Tenant status 还必须提供完整、可由外部消费者使用的 OpenBao KV v2 API URL。URL 可以
包含 consumer API address、mount 和 secret path，但不得包含 Token、密码或其他认证
信息。默认 `kubectl get` 表格显示目标 Secret 名称；完整 OpenBao URL 通过
`kubectl get postgresqltenant <name> -o yaml` 获取，避免表格列过长。

OpenBao metadata 必须能够标识 Tenant UID、namespace/name 和 Instance，使 controller
区分自己的残留记录与外部记录。任何凭据值都不得进入日志、Event、Condition、metric
label、trace、CR spec/status 或测试快照。

## 9. Reconcile 行为

系统采用最终一致性模型。Kubernetes、PostgreSQL、OpenBao 和 ESO 可以短暂处于不同
阶段；controller 不尝试实现跨系统事务，而是以 Kubernetes CR `status.phase` 作为
工作流 checkpoint，通过幂等外部操作和每轮回读验证最终收敛。

两个 CR 的状态机权威记录都在 `status.phase`。controller 根据 phase 选择下一项候选
动作，但 phase 不能替代外部状态检查：执行前后仍须回读 PostgreSQL catalog、registry、
OpenBao 和 Kubernetes/ESO。外部写入成功但 status 更新失败时，下一轮必须识别已完成
事实并推进 phase，不得重复生成密码或报告虚假冲突。

status 丢失时，controller 必须从 registry 的所有权记录和各外部系统实际状态保守重建
phase。若 status 被伪造或领先于实际状态，controller 必须纠正到安全阶段并补齐资源，
不能跳过验证。registry 不保存或驱动协调 phase。

Instance phase 按当前 generation 表示连接与初始化进度：

```text
Pending -> Validating -> InitializingRegistry -> Ready
(any phase) --------------------------------> Deleting
```

spec generation 改变后可以从 `Ready` 回到 `Validating`。Tenant phase 如下：

```text
Pending -> Planned -> CredentialCreated -> RoleCreated -> DatabaseCreated
        -> ExternalSecretCreated -> CredentialProjected -> Ready -> Deleting
```

失败不增加 `Failed` phase；phase 保留在无法推进的步骤，由 `Ready=False` 的 Reason 和
message 表达 `Conflict`、认证失败或依赖不可用。Retain 删除完成后 CR 已不存在，因此
没有持久的 `Retained` phase。

每轮 Tenant reconcile 必须按以下逻辑执行：

```text
读取 Tenant
  -> 读取 Instance
  -> 校验不可变字段与输入
  -> 检查 Instance Ready
  -> 读取 OpenBao 与 PostgreSQL 实际状态
  -> 检测冲突或部分完成状态
  -> 执行非破坏性补齐
  -> 使用应用凭据验证登录
  -> 创建并验证 ExternalSecret/Secret 投射
  -> 回读实际状态
  -> 更新 status
```

要求：

- 所有步骤必须幂等；
- 每个外部写入前必须先在 CR status 持久化足够的操作意图，写入后必须回读并推进
  `status.phase`；
- 暂时性网络、锁和依赖错误必须重试；
- 输入错误、资源冲突和禁止操作不得忙循环重试，只在 generation 或依赖状态变化后
  重试；
- 未知外部资源不得被修改、接管或删除；
- 用户从 `spec.extensions` 移除 extension 时不得执行卸载，必须报告该字段在 v1alpha1
  中只允许追加；
- controller 重启不得影响已经签发的应用密码；
- `status` 丢失后必须可以从 registry、PostgreSQL、OpenBao 和 Kubernetes/ESO 重建。

## 10. Condition 合同

两个资源都必须提供唯一的 `Ready` Condition。可以增加辅助 Condition，但调用方只需
依赖 `Ready`。

| 状态 | 含义 |
| --- | --- |
| `Ready=Unknown` | 正在首次观察或 reconcile，尚无结论 |
| `Ready=False` | 当前 generation 未达到合同要求 |
| `Ready=True` | 当前 generation 已回读验证成功 |

Condition 必须带正确的 `observedGeneration`。资源自身的
`status.observedGeneration` 只在当前 generation 完成一次有结论的 reconcile 后更新。

最低 Reason 集合：

| Reason | 适用资源 | 含义 |
| --- | --- | --- |
| `Reconciling` | 两者 | 尚在处理 |
| `Ready` | 两者 | 当前 generation 已验证 |
| `InvalidSpec` | 两者 | 输入不满足规格 |
| `DependencyUnavailable` | 两者 | PostgreSQL 或 OpenBao 暂时不可用 |
| `AuthenticationFailed` | Instance | 管理凭据或 TLS 验证失败 |
| `InsufficientPrivileges` | Instance | 管理 role 缺少必要权限 |
| `InstanceNotReady` | Tenant | 引用的 Instance 未 Ready |
| `Conflict` | Tenant | 目标名称或 OpenBao 路径已被其他主体占用 |
| `ProvisioningFailed` | Tenant | 可重试的创建/验证失败 |
| `CredentialProjectionFailed` | Tenant | ESO 或目标 Secret 未达到期望状态 |

Condition message 必须适合人类排障，但禁止包含连接串密码、Token 或完整 Secret 数据。

## 11. 删除与保留

### 11.1 Retain

`Retain` 是默认策略：

- 删除 Tenant CR 不得删除 database、role、extension 或 OpenBao 记录；
- controller 不得因外部依赖不可用而永久阻止 Retain CR 删除；
- 保留资源必须继续携带原 Tenant UID 和 namespace/name 的所有权记录，但在 CR 删除后
  明确处于 unmanaged 状态；
- 重新创建同名 Tenant 会产生新的 UID，必须因已有资源不属于新 UID 而报告 Conflict；
- v1alpha1 不提供重新关联、import 或 adoption；恢复管理必须使用第 12 节的迁移流程，
  或等待后续版本定义显式纳管协议。

### 11.2 Delete

用户在创建 Tenant 时显式设置 `deletionPolicy: Delete`，表示删除 CR 时授权永久清理
该 Tenant 的外部资源。controller 必须使用 finalizer，并按以下顺序处理：

1. 再次验证 database、role 和 OpenBao 记录都属于当前 Tenant UID；
2. 删除 ExternalSecret，并确认目标 Kubernetes Secret 已删除；
3. 禁止该 login role 建立新连接；
4. 终止该 database 的现有连接；
5. 删除 database，database 内 extension 随之删除；
6. 删除 login role；
7. 删除 OpenBao KV 记录及其可恢复版本；
8. 回读确认外部资源均不存在；
9. 删除 controller registry 记录；
10. 移除 finalizer，允许 Kubernetes 删除 CR。

任一步失败都必须保持 finalizer 并从安全检查开始重试。controller 禁止使用
`CASCADE` 删除无法证明属于该 Tenant 的依赖对象。若 Instance 或 OpenBao 永久丢失，
管理员可以在核实外部状态后手工移除 finalizer；该逃生操作必须在运维 runbook 中明确
标记为可能遗留资源。

v1alpha1 不自动检查备份，也不承诺恢复被 `Delete` 删除的数据。显式选择 Delete 的
用户承担数据销毁语义；默认 Retain 用于避免普通误删。

## 12. 现有环境迁移

v1alpha1 不接管现有 database 或 role，但必须提供可重复、可回滚的迁移 runbook。对每
个现有应用租户，推荐的停机迁移顺序是：

1. 盘点 database、role、owner、grant 和 extension，并完成可恢复备份；
2. 创建逻辑备份，必须使用可映射到新 owner 的格式，避免恢复旧 role ownership；
3. 停止应用写入并确认没有活动写事务；
4. 完成最终逻辑备份；
5. 将旧 database 和 role 重命名为带迁移时间戳的保留名称，释放最终名称；
6. 创建 `PostgreSQLTenant`，由 controller 创建最终 database、role 和 OpenBao 凭据；
7. 等待 Tenant Ready；
8. 以新 owner 恢复逻辑备份，并验证 row count、schema、extension 和应用权限；
9. 让 ESO 投射新凭据，重启或重新部署应用；
10. 验证应用读写后结束维护窗口；
11. 保留旧 database、role 和备份直到回滚窗口结束，再由管理员手工清理。

回滚时停止新应用写入、恢复原名称或连接配置，并重新使用旧凭据。迁移工具不得把旧
密码、管理凭据或 dump 文件提交到 Git。真实命令、锁定方式和各现有应用验证项见
[`migration.md`](migration.md)，并必须在实现首个可用版本前通过临时 PostgreSQL 实例
演练。

## 13. 安全要求

1. 所有 PostgreSQL 与 OpenBao 网络访问必须支持超时和 context cancellation。
2. homelab 部署必须通过 Deployment 挂载的共享 CA bundle 验证 TLS server identity；
   该 bundle 的信任根来自 OpenBao PKI，但不得包含 CA 私钥。Instance 默认使用
   `verify-full`，其 host 必须与服务器证书名称匹配。开发环境可以显式使用 `disable`
   明文连接。
3. PostgreSQL 管理 role 应使用满足本规格的最小权限，不应使用 PostgreSQL
   superuser；若 extension 安装需要额外权限，必须单独记录例外。
4. controller 的 OpenBao policy 仅覆盖受管租户 KV 操作，不授予管理凭据路径权限。
   管理凭据的 ESO 同步身份与应用凭据的 ESO 读取身份隔离。controller 只在自身
   namespace 获得管理 Secret 读取权限，不因此扩大跨 namespace Secret data 访问范围。
5. namespace 用户不得修改 cluster-scoped Instance。
6. 所有 identifier、extension name 和引用字段必须在发起外部调用前校验。
7. controller 不得通过 shell 或 `psql` 子进程执行用户输入。
8. 错误包装、结构化日志和 tracing 必须经过 Secret 泄露测试。

详细威胁模型和部署 policy 见 [`security.md`](security.md)。

## 14. 可观测性要求

v1alpha1 至少必须提供：

- Kubernetes Events：开始 provisioning、成功及需要人工处理的失败；
- 结构化日志：resource namespace/name、Instance、generation、阶段和错误类别；
- controller-runtime 默认 reconcile metrics；
- 不包含 database、role、OpenBao path 等无界用户输入的低基数失败分类 metric。

日志和 metrics 的存在不能代替 Condition；Condition 是 API 使用者判断状态的主要方式。

## 15. 验收标准

实现 v1alpha1 第一条完整纵向切片前，测试必须覆盖：

1. 有效 Instance 可以建立 TLS 管理连接并变为 Ready。
2. PostgreSQL 管理能力不可用时 Instance Ready=False，恢复后自动变为 Ready；已有
   管理凭据可正常使用时，Bao／ESO 故障不单独影响 Instance Ready。首次装配缺少有效
   管理 Secret 时不能 Ready；Tenant 的 Bao 操作失败按其自身依赖故障报告。
3. 有效 Tenant 创建 database、作为 owner 的 login、grant、extension 和 OpenBao
   记录。
4. 应用凭据可以实际连接且不能创建其他 database/role。
5. 相同 generation 重复 reconcile 不改变密码、不重复创建资源。
6. controller 在每个外部写入步骤后中断，重启后都能继续并得到相同最终状态。
7. 预先存在且不属于当前 Tenant UID 的 database、role 或 OpenBao path 导致
   Conflict，且不修改已有资源。
8. 目标实例实际不支持的 extension 在供应外部写入前被拒绝；扩展列表查询失败时
   按依赖故障处理，不报告为不支持。安装结果仍须回读验证。
9. status 被清空后可以从两个外部事实来源重建。
10. 删除 Retain Tenant 后外部资源仍存在且不再受管；重新创建同名 Tenant 报告
    Conflict。
11. 日志、Event、Condition、metric 和 CR 中不存在生成的密码或管理凭据。
12. 两个 namespace 对同一 Instance 申请相同名称时，只有第一个成功，第二个报告
    Conflict。
13. 删除 Delete Tenant 时，任一步骤失败都可重试，且最终删除 database、login role、
    OpenBao KV 历史和 finalizer。
14. 使用迁移 runbook 可以把一个现有 database 转移到新建的受管 database，并在回滚
    窗口内恢复旧服务。
15. Tenant 只有在 ExternalSecret Ready、目标 Secret 存在且应用凭据实际可登录后才
    Ready。
16. Tenant status 同时提供 Kubernetes Secret reference 和不含认证信息的 OpenBao API
    URL。
17. DNS 不可用时，使用输出的 `hostaddr` 可以连接 PostgreSQL；server 证书同时覆盖
    `host` 的 DNS SAN 和 `hostaddr` 的 IP SAN，两种连接目标均可通过 `verify-full`。
18. 两个 CR 的 `status.phase` 都能反映当前协调步骤；清空 status 后可以从外部事实重建，
    且伪造或过期 phase 不会使 controller 跳过验证或外部操作。

单元测试验证纯决策逻辑，adapter 集成测试使用 Docker PostgreSQL/OpenBao，controller
集成测试使用 envtest，完整网络路径使用 Kind E2E。

## 16. 已确认决策

- v1alpha1 使用一个同时作为 database owner 的 login role，不创建额外 NOLOGIN owner。
- v1alpha1 不接管任意现有资源，但必须提供并演练 dump/restore 迁移路径。
- v1alpha1 同时实现默认 `Retain` 和显式 `Delete`；Delete 必须有 finalizer、所有权验证
  和完整清理路径。
- OpenBao KV v2 mount 和 base path 是 controller 部署配置，mount 默认 `kv`，base path
  由 `--openbao-tenant-base-path` 配置并默认 `postgresql-tenants`；Tenant 不能选择 mount
  或任意远端 path，controller 根据 namespace/name 推导记录路径。
- 租户 KV 记录固定写入 `username/password/database/host/hostaddr/port/sslmode` 七个
  原子字段；
  controller 不生成连接 URI，应用负责映射和拼装自身配置。
- PostgreSQL TLS 使用 controller Deployment 挂载的共享 CA bundle。OpenBao PKI 是
  CA 权威并继续签发、续期 PostgreSQL server 证书；controller 只消费公开 trust
  bundle，不接触 CA 私钥。bundle 可以由 ConfigMap 或现有证书同步机制投射，不允许
  Tenant 或 Instance 选择其他 CA；开发环境可以显式使用 `sslMode: disable`。
- 每个 PostgreSQLInstance 在其管理 database 中维护 controller 专用 registry schema。
  registry 是受管资源所有权、安装身份和 Retain 后 unmanaged 标记的权威记录；两个
  CR 的 `status.phase` 是 controller 状态机的权威 checkpoint，Instance status 不聚合
  Tenant 清单。
- PostgreSQL database 和 role identifier 必须匹配 `^[a-z][a-z0-9_]{0,62}$`，不支持
  需要双引号的大小写或特殊字符名称。
- External Secrets Operator 是 v1alpha1 的运行依赖。controller 管理同 namespace
  ExternalSecret，但不直接写明文 Secret；Tenant status 同时输出目标 Secret reference
  和供非 Kubernetes 消费者使用的 OpenBao API URL。
- PostgreSQLInstance 同时声明 DNS `host` 和 IP `hostaddr`；PostgreSQL server 证书必须
  同时包含对应 DNS SAN 和 IP SAN，消费者自行选择连接目标。

## 17. 批准状态

2026-09-14 确认 extension 判定修订：v1alpha1 使用实例实际可安装列表，不实现管理员
allowlist；后续可按需引入策略。现有 allowedExtensions 字段尚待 API 实现移除。

2026-09-13 已确认管理连接修订：Instance 引用 controller namespace 内的管理 Secret，
管理员维护 ExternalSecret，由 ESO 同步；controller 不再从 Bao 直接读取管理凭据。
此项是已批准行为，现有 API types 与实现尚待后续修改。

具体设计决策和本文整体已于 2026-09-10 获得批准，可以进入 API reference、测试和
实现阶段。同日确认状态机修订：两个 CR 的 `status.phase` 是 controller 工作流的权威
checkpoint；PostgreSQL registry 只承担所有权、安装身份和保留状态。

## 18. 与当前脚手架的已知差异

当前 API skeleton 至少需要以下调整：

- 删除 Tenant 自选 OpenBao path 的能力，改由部署级 mount、base path 和 Tenant
  identity 推导，并修正当前包含 `kv/` 前缀的示例；
- 增加 controller 部署级 OpenBao KV mount 和 TLS 配置；
- 增加部署级 OpenBao consumer address、ClusterSecretStore 和 KV base path 配置；
- 删除独立 `ownerRole` 字段，使 login role 成为 database owner；
- 为 Instance 增加 `hostaddr`，为 Tenant 增加目标 Secret 配置及 Secret/Bao URL 输出
  status；
- 按已确认的 identifier 合同收紧校验；
- 增加 PostgreSQL controller registry，记录基于 UID 的所有权、安装身份和保留状态；
- 修正凭据 type 中遗留的 rotation 注释；
- 使 Condition、不可变字段和 extension 追加语义具备 API 校验或明确的 reconcile
  结果。

这些是规格批准后的实现工作，不属于本规格本身。
