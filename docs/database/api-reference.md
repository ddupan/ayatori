# v1alpha1 API 合同

| 项目 | 内容 |
| --- | --- |
| 状态 | Review |
| API group | `database.ayatori.ddupan.top` |
| version | `v1alpha1` |
| 最后更新 | 2026-09-10 |

本文把已批准的系统规格映射为 CRD 字段合同。API types、生成 CRD、sample 和测试必须与本文
一致。Ayatori 尚未注册这些 API，本页是后续实现的规范来源。

## 通用约定

- PostgreSQL identifier 匹配 `^[a-z][a-z0-9_]{0,62}$`。
- 所有引用名称使用 Kubernetes DNS label/name 的相应校验。
- Tenant 的 `spec.instanceRef` 与 `metadata.name` 长度合计不超过 241 个字符，确保派生的
  `<instanceRef>-<metadata.name>-postgresql` 不超过 Kubernetes DNS subdomain 的
  253 字符限制。
- port、TLS mode、deletion policy 等固定默认值由 CRD defaulting 提供。database、
  loginRole、Secret 名称等依赖其他字段的值是 controller 语义默认值：字段保持省略，
  controller 计算 effective value 并通过 status/受管资源展示，不引入 mutating webhook。
- 需要读取旧值或跨字段的校验由 CEL 或 controller 完成。
- `status` 由 controller 独占写入，禁止出现密码、Token、管理用户名或完整连接串。
- 两个 Kind 都只承诺一个 `Ready` Condition；调用方不得依赖内部协调阶段。

## PostgreSQLInstance

cluster-scoped，short name 为 `pginstance`。

### Spec

| JSON path | 类型 | 必填/默认 | 合同 |
| --- | --- | --- | --- |
| `spec.endpoint.host` | string | 必填 | PostgreSQL DNS 名；必须被服务端证书 DNS SAN 覆盖 |
| `spec.endpoint.hostaddr` | string | 必填 | 单个 IPv4/IPv6；必须被服务端证书 IP SAN 覆盖 |
| `spec.endpoint.port` | int32 | `5432` | 1–65535 |
| `spec.endpoint.database` | string | `postgres` | 管理连接 database；合法 PostgreSQL identifier |
| `spec.endpoint.sslMode` | enum | `verify-full` | `disable`、`require`、`verify-ca`、`verify-full` |
| `spec.adminCredentialRef.name` | string | 必填 | controller namespace 内的管理 Secret 名称 |
| `spec.adminCredentialRef.usernameKey` | string | `username` | Secret data 中的键名 |
| `spec.adminCredentialRef.passwordKey` | string | `password` | Secret data 中的键名 |

`adminCredentialRef` 不接受 namespace 或 Bao path。管理 Secret 固定在 controller
namespace，名称须合法，两个字段须存在且非空。管理员维护 ExternalSecret，由 ESO
同步；controller 只读管理 Secret，不创建或修改它。此为 2026-09-13 批准的修订，
现有 API types、生成 CRD 和 samples 尚未更新。

Instance endpoint、管理凭据引用可以修改。修改后 controller 重新验证。
2026-09-14 修订：v1alpha1 不实现 allowedExtensions；现有 API types、生成 CRD 和
samples 中的字段待后续移除，不作为一个可配置但被忽略的策略保留。
扩展请求按目标 PostgreSQL 实际可安装列表判断，可用列表由应用层查询。

endpoint 由管理员负责，不校验变更前后是否同一物理服务器／registry，只重验新配置
的连接与管理能力。新 UID 按新 Instance 处理，不授权接管旧 UID 的 Tenant 资源。

### Status

| JSON path | 类型 | 含义 |
| --- | --- | --- |
| `status.observedGeneration` | int64 | 最近完成有结论协调的 generation |
| `status.phase` | enum | `Pending`、`Validating`、`InitializingRegistry`、`Ready`、`Deleting` |
| `status.postgresqlVersion` | string | 从 server 回读的版本，不用于客户端解析 |
| `status.conditions[]` | `metav1.Condition` | 至少包含唯一的 `Ready` |

print columns：`Endpoint=.spec.endpoint.host`、`Phase`、`Ready`、`Age`。

Instance `Ready=True` 要求管理凭据可读、TLS/认证成功、server metadata 可读、registry
可访问且权限预检成功。它不代表数据库已经备份或高可用。

管理凭据从 Kubernetes Secret 装配；已有有效凭据可访问 PostgreSQL 时，Bao／ESO
暂时不可用不单独撤销 Instance Ready。Tenant 凭据操作仍依赖 Bao。

## PostgreSQLTenant

namespaced，short name 为 `pgtenant`。

### Spec

| JSON path | 类型 | 必填/默认 | 合同 |
| --- | --- | --- | --- |
| `spec.instanceRef` | string | 必填 | cluster-scoped Instance 名称 |
| `spec.database` | string | `metadata.name` | 合法 PostgreSQL identifier |
| `spec.loginRole` | string | `metadata.name` | database owner 兼应用 login |
| `spec.extensions` | set[string] | 空集合 | 必须属于目标实例实际可安装的扩展列表；成功创建后只允许追加 |
| `spec.credential.secretName` | string | `<instance>-<name>-postgresql` | 合法的同 namespace ESO target Secret 名称 |
| `spec.deletionPolicy` | enum | `Retain` | `Retain` 或 `Delete` |

Tenant 不声明 OpenBao mount 或 path。controller 使用部署级 mount/base path 和
`namespace/name` 推导稳定路径，并用 UID metadata 验证所有权。ExternalSecret 固定为
`<instanceRef>-<metadata.name>-postgresql`；目标 Secret 可以由用户指定，只需
满足 Kubernetes Secret 名称校验，不限制命名内容；省略时使用相同默认名。

`instanceRef`、`database`、`loginRole` 和 `credential.secretName` 在首次成功创建外部
状态后不可变。
`extensions` 只允许集合不变或追加；移除返回 `ImmutableField`，不会执行
`DROP EXTENSION`。`deletionPolicy` 在对象进入删除前可以修改；删除开始后以 finalizer
首次观察到的值为准，避免清理过程中改变授权范围。

### Status

| JSON path | 类型 | 含义 |
| --- | --- | --- |
| `status.observedGeneration` | int64 | 最近完成有结论协调的 generation |
| `status.phase` | enum | controller 状态机的权威 checkpoint |
| `status.database` | string | 应用语义默认值后的实际 database 名称 |
| `status.loginRole` | string | 应用语义默认值后的实际 owner/login role 名称 |
| `status.databaseOID` | uint32 | 回读的 database OID，仅供诊断 |
| `status.credential.secretRef.name` | string | 同 namespace 目标 Secret 名称 |
| `status.credential.openBaoURL` | string | 完整 KV v2 API URL，不含认证信息 |
| `status.conditions[]` | `metav1.Condition` | 至少包含唯一的 `Ready` |

Secret reference 不重复 namespace，因为它必定与 Tenant 同 namespace。OpenBao URL 格式
为 `<consumer-address>/v1/<mount>/data/<derived-path>`；不得包含 Token、用户名、密码或
query credential。

Tenant phase 枚举为 `Pending`、`Planned`、`CredentialCreated`、`RoleCreated`、
`DatabaseCreated`、`ExternalSecretCreated`、`CredentialProjected`、`Ready`、`Deleting`。
它不包含 `Failed` 或 `Retained`；失败类型由 Condition Reason 表达。

print columns：`Instance`、`Database`、`Phase`、`Secret`、`Ready`、`Age`。完整 OpenBao URL 只在
YAML/JSON status 中输出。

两个 Kind 的 `status.phase` 都是 controller 状态机的权威 checkpoint。controller 用它
选择下一候选动作，但必须在动作前后核对外部事实，不能仅凭 phase 跳过幂等检查。status
丢失或领先于实际状态时必须保守重建/纠正。自动化就绪判断仍应读取 `Ready` Condition；
phase 用于进度展示、恢复和排障。

## Condition

每种类型最多一个 Condition；更新必须保留正确的 `lastTransitionTime` 语义。

| Reason | Kind | 可重试性 |
| --- | --- | --- |
| `Reconciling` | 两者 | 正常进行中 |
| `Ready` | 两者 | 已收敛 |
| `InvalidSpec` | 两者 | 修改 spec 前不会恢复 |
| `ImmutableField` | Tenant | 恢复原值或重新迁移 |
| `DependencyUnavailable` | 两者 | 自动重试 |
| `AuthenticationFailed` | Instance | 修复凭据/TLS 后重试 |
| `InsufficientPrivileges` | Instance | 修复管理 role 后重试 |
| `InstanceNotReady` | Tenant | Instance 恢复后重试 |
| `Conflict` | Tenant | 人工解除名称/所有权冲突 |
| `ProvisioningFailed` | Tenant | 按错误类别退避重试 |
| `CredentialProjectionFailed` | Tenant | ESO/Secret 恢复后重试 |

`Ready=True` 必须使用 Reason `Ready`。处理中为 `Unknown/Reconciling`；已知未满足合同为
`False`。Condition message 可以包含资源名和错误类别，禁止包含凭据值或完整 Secret。

## 删除语义

- `Retain` 不需要等待外部依赖；删除 CR 后外部记录保留原 UID 并标记 unmanaged。
- `Delete` 添加 finalizer，严格按规格的所有权验证和清理顺序执行；失败保持 finalizer。
- Instance 开始受管时即添加并保存 finalizer；删除时停止新供应，存在 Tenant 引用
  （包括正在删除的 Tenant）就保留 finalizer，无引用才移除。引用查询失败时继续等待。
  不级联删除 Tenant 或外部资源；管理员可使用运维逃生流程。
- finalizer 不禁止创建 Tenant CR；并发创建者遇到删除中或不存在的 Instance 不得
  开始供应。首版不增加跨对象锁或准入控制，不承诺跨对象原子删除。

## 示例

```yaml
apiVersion: database.ayatori.ddupan.top/v1alpha1
kind: PostgreSQLInstance
metadata:
  name: shared
spec:
  endpoint:
    host: postgresql.home.arpa
    hostaddr: 192.0.2.10
    port: 5432
    database: postgres
    sslMode: verify-full
  adminCredentialRef:
    name: shared-postgresql-admin
---
apiVersion: database.ayatori.ddupan.top/v1alpha1
kind: PostgreSQLTenant
metadata:
  name: netbox
  namespace: netbox
spec:
  instanceRef: shared
  database: netbox
  loginRole: netbox
  extensions: [pg_trgm]
  credential:
    secretName: shared-netbox-database-credentials
  deletionPolicy: Retain
```
