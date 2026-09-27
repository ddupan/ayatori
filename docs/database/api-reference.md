# v1alpha1 API 合同

| 项目 | 内容 |
| --- | --- |
| 状态 | API schema 与绑定 controller 已实现；供应、交付与删除清理未接入 |
| API group/version | `database.ayatori.ddupan.top/v1alpha1` |
| 最后更新 | 2026-09-27 |

以 [系统规格](specification.md) 与
[ADR-0009](../decisions/0009-database-resource-and-claim.md) 为准。类型与生成的 CRD 已纳入源码，
尚未发布为可用 DBaaS。示例可进入绑定协调，但不代表创建对象后会供应数据库或交付凭据。

## 当前 API 切片

Go 类型位于 `api/database/v1alpha1`，CRD 随 `config/crd` 发布；manager 已注册 Scheme 和
`internal/database/controller` 的绑定 controller。以下字段是本切片的具体实现：

| 资源 | 字段 | 含义 |
| --- | --- | --- |
| Database | `spec.instanceRef.name` | 所属集群级 Instance |
| Database | `spec.database`、`spec.loginRole` | 实际数据库与唯一登录 owner，必填 |
| Database | `spec.source` | 必填 `Provision` 或 `Import`，不隐式认领 |
| Database | `spec.credentialRef.mount/path` | Import 必填的已有 KV v2 凭据位置；Provision 禁止指定 |
| Database | `spec.reclaimPolicy` | Retain 默认或 Delete |
| Database | `spec.tenantRef.namespace/name/uid` | controller 写入的完整绑定身份，不是允许名单 |
| Database | `status.instanceUID` | 观察时的 Instance 身份 |
| Database | `status.credentialRef.mount/path` | 首次写入前固定的 KV v2 位置，不随部署配置迁移 |
| Database | `status.credentialVersion` | 创建并回读成功后确认的正整数版本；省略表示未确认 |
| Tenant | `spec.provision.instanceRef.name` | 动态申请来源，与 `spec.databaseRef` 互斥且必须二选一 |
| Tenant | `spec.provision.database/loginRole` | 可省略，语义默认值由 controller 解析，不由 CRD 推导 |
| Tenant | `spec.databaseRef.name` | 显式申请已有 Database，不额外指定 Instance |
| Tenant | `spec.extensions`、`spec.secretName` | 扩展集合与同 namespace 的交付目标 |
| Tenant | `status.databaseRef.name/uid` | 资源侧绑定成功后写入 |
| Tenant | `status.secretName`、`status.credentialURL` | 交付观察，不包含密码或认证信息 |

三资源均有 status subresource、observedGeneration 和按 type 唯一的 Conditions。
Instance phase 沿用已批准枚举；Database/Tenant phase 暂不冻结供应子阶段枚举。
`credentialRef.path` 是 mount 内逻辑路径，不包含 KV v2 的 `data/` 前缀。
其部署允许范围、实际凭据读取和 URL 安全构造仍由后续 adapter/controller 验证。

凭据位置与确认版本一旦写入便不可更改或移除；确认版本必须有对应位置。Conditions 描述
当前可用性，不替代确认记录，也不能因读取暂时失败而清空记录。上述 schema 已有真实 API
server 校验；负责持久化它们的供应用例尚未接入，现有绑定 controller 不会填写这些字段。
未确认的已有值必须报 Conflict，不能用读取成功补记版本；已确认版本的恢复读取检查最新
KV 版本，删除或版本漂移均需人工处理，不回退旧版本或生成替代密码。第一版不提供轮换入口。

示例：[Instance](../../config/samples/database_v1alpha1_postgresqlinstance.yaml)、
[导入 Database](../../config/samples/database_v1alpha1_postgresqldatabase.yaml)、
[动态/已有资源申请](../../config/samples/database_v1alpha1_postgresqltenant.yaml)。

当前 schema 验证名称、端口、IP、TLS 枚举、申请互斥、导入凭据要求和绑定 UID 完整性。
API 接受两个 Tenant 引用同一 Database 不表示允许双重绑定；排他绑定由 controller 协调。
绑定 controller 解析动态 database/loginRole 的 Tenant 名称默认值；动态资源 CR 名称为
`tenant-<Tenant UID>`，首次创建即包含资源侧绑定与 finalizer，不带 Tenant ownerReference。
已有资源必须有当前版本 Ready 观察、匹配的 Instance UID，并处于未绑定的 Available 状态。
同一 Tenant 的资源侧记录已写入时，允许回读后补齐申请侧，不重新争抢资源。

Tenant 进入 `status.phase=Binding` 后由 CEL 固定申请目标；Database 有实例身份观察、凭据位置或
绑定后固定实际 database、loginRole、来源和凭据引用，回收策略仍可修改。
读取绑定判断使用 APIReader，写入依靠 resourceVersion；watch/cache 负责触发协调。
绑定顺序由 application service 协调，纯资格规则在领域层；Kubernetes adapter 负责快照
映射、finalizer 和状态呈现。呈现前若资源版本已变化，返回冲突供下一轮重读，不覆盖其他修改。
双向记录完成后 Tenant 为 Bound，Ready=False/BindingComplete，明确尚未供应或交付。
生成的 manager ClusterRole 授予资源读写，不包含 Secret 读取；Instance 观测的管理 Secret
权限由固定 namespace 的独立 Role 授予。Instance controller 已接入原生管理观察与引用删除
保护，启用方式及 Ready 边界见 [模块说明](README.md#instance-原生管理观测)。

当前有 Tenant/Database finalizer 保护，但**删除清理尚未实现**：Tenant 删除报告
Ready=False/DeletionPending 并保留绑定与 finalizer，Database 的保护也不会被自动移除。
还未实现删除流程开始后的回收策略固定、Retain 释放、Released 重新开放、外部清理、
扩展只追加、Secret 默认名称解析与凭据交付。在后续清理协议和真实后端验收完成前，
不能作为可用 DBaaS 部署，也不能通过强行移除 finalizer 把它视为已完成清理。

## 通用约定

- Instance 与 Database 是 cluster-scoped；Tenant 是 namespaced。
- Tenant 按名称引用 Database，不提供 Database namespace；Database 绑定记录包含
  Tenant namespace/name/UID。字段见当前 API 切片；绑定采用下述资源侧先写顺序。
- 每类资源提供唯一的 Ready Condition、observedGeneration；phase 用于进度展示，
  不能单独作为写权限或所有权证明。
- 引用必须区分定位名称与已绑定 UID；同名新对象不继承绑定。
- controller 管理 status；资源侧管理与回收权限不得随 Tenant editor 权限自动授予。
- 固定默认值使用 CRD defaulting，跨字段/不可变校验使用 CEL 或 controller；
  并发更新使用 resourceVersion，不增加 mutating webhook 或跨系统事务。
- database/role identifier 继续匹配 `^[a-z][a-z0-9_]{0,62}$`。
- 动态申请的 database/loginRole 省略时继续以 Tenant metadata.name 为语义默认值；
  显式导入资源使用实际目标，不从新 Tenant 名称重新推导。最终互斥字段须经 API 评审。
- 投射仍位于 Tenant namespace，ExternalSecret 默认命名沿用
  `<instanceRef>-<metadata.name>-postgresql`；目标 Secret 可由申请指定，省略时同名。
  有效 Instance 名称与 Tenant 名称合计不超过 241 字符；完整名称须满足 API 名称校验。
- 任何 spec/status/错误不得出现密码、Token 或完整秘密响应。

## PostgreSQLInstance

保留管理入口字段：

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


管理 Secret 固定在 controller namespace，不接受 namespace 或 Bao path。
管理员维护其 ExternalSecret，controller 只读；有效用户名/密码变化时重验连接。
endpoint 变化使旧观察失效，不迁移旧服务器的数据，不自动接管旧 UID 的资源。

Status 保留 observedGeneration、postgresqlVersion 和 conditions；
phase 为 Pending、Validating、Ready、Deleting，移除 InitializingRegistry。
Ready 要求连接、metadata 与所需管理权限，不要求 registry。
实例实际可用扩展来自查询，不提供 allowedExtensions 配置。

开始受管时保存 finalizer；删除时阻止新供应，并检查 Database（含 Released）及未绑定
Tenant 引用。无引用才解除，不级联删除资源；查询失败不能视为无引用。

## Database 资源（工作 Kind：PostgreSQLDatabase）

v1alpha1 以一个 database、一个兼任 owner 的 login role 及其应用凭据作为 Database 的
生命周期边界；不提供多账号字段或独立 Role/Credential CRD。多账号需求留待后续 API 版本。
此决定不改变导入只读验证和显式管理授权的要求。

Database 是平台管理的集群级资源，不归属于应用 namespace，也不需要资源专用 namespace。
普通申请者通过 Tenant 申请使用，不能自行修改 Database 回收策略或将 Released 资源重新开放；
这些资源管理操作由平台管理员授权。controller 的绑定协调权限与用户申请权限分别配置。

以下是行为合同，具体 schema 见当前 API 切片与生成的 CRD；后端行为尚未实现：

| 内容 | 合同 |
| --- | --- |
| `instanceRef` | Database 自身必填；定位来源并记录绑定的 Instance UID，不依赖 Tenant 补齐 |
| 外部目标 | 实际 database 名称及已确认的管理范围；操作开始后不可隐式改目标 |
| 来源 | 区分动态供应与管理员显式导入；不能从同名存在推断导入授权 |
| 绑定 | 至多一个 Tenant，含 namespace/name/UID；Released 保留旧身份；无允许绑定名单 |
| 回收策略 | 资源侧 Retain（默认）或显式 Delete；普通 Tenant editor 不得扩大授权 |
| 观察与进度 | 实际目标、当前阶段、条件和安全诊断，不保存秘密 |

概念生命周期包含供应/验证、可绑定、已绑定、Released 和删除；最终 phase 枚举待协议评审。
Released 不自动变成可绑定。Database 不以 Tenant 为 GC owner；使用中的资源受删除保护。

导入的初始检查只读；存在不等于 Ready。未绑定且可用的资源允许 Tenant 显式申请，
不要求管理员逐 Tenant 授权；已占用或 Released 的资源不能直接绑定。
默认保留不隐含密码、owner、授权或删除的变更许可。

## PostgreSQLTenant

保留用户申请与交付职责：

- 动态申请描述 Instance、所需数据库/登录角色、扩展与目标 Secret；
  或显式引用管理员已登记的 Database。后者从 Database 获取 Instance，不重复指定来源；
  两条路径互斥，具体 schema 在 API 切片固定。
- 绑定前固定有效需求，绑定/开始供应后不能通过改引用或名称迁移资源。
- extensions 成功后只允许追加，不自动卸载。
- status 展示绑定 Database 身份、Ready、交付 Secret 引用及不含认证信息的 OpenBao URL。
- Tenant 删除释放申请，按照 Database 的回收策略处理，不独立持有最终删除授权。

旧 Tenant `spec.deletionPolicy` 不再作为最终资源回收策略；旧字段表、供应 phase 枚举及
仅按 Tenant namespace/name 派生凭据路径的规则不再是实现合同。已有代码没有兼容负担，
不保留两套相互覆盖的策略字段。

## 已确认的绑定顺序

1. 动态申请按 Tenant UID 确定 Database 名称并创建记录；已有资源申请使用指定的 Database，
   检查资源未绑定且可用，不增加反向授权名单。动态创建重试遇到同名记录时需核对身份与目标。
2. 使用 resourceVersion 并发控制，先在 Database 记录 Tenant namespace/name/UID。
   已绑定其他 Tenant 时报告 Conflict，不抢占；版本冲突后重新读取、重新判断。
3. 在 Tenant status 记录 Database name/UID。若上一步成功、本步失败，后续 reconcile
   核对身份后补齐，不回滚已经成功的资源侧绑定。
4. 双向记录一致才允许动态供应或凭据交付；实际数据库与凭据验证通过后才可 Ready。

此顺序借鉴 PV/PVC 的资源侧先写模式，行为依据见
[系统规格](specification.md#5-动态供应与排他绑定)。Retain 释放时保留旧绑定身份并进入
Released，不自动清空后重新分配。API 记录部分写入由 reconcile 重试；外部创建结果
无法确认时仍报告 Conflict，交给人工，不新增事务队列或 registry。

## 绑定协议剩余评审要求

实现前必须明确：

1. 引用字段的最终格式，以及落实管理员资源管理、controller 协调与普通申请权限的 RBAC 规则。
2. 绑定记录的最终字段及校验规则，落实上述写入顺序与恢复行为。
3. Tenant UID 变化、对象删除、Released 旧引用与管理员重新授权的判断。
4. 同一物理数据库重复登记的冲突处理；列表查询不是原子认领。
5. 已有凭据关联字段、旧访问处置与投射清理顺序；动态凭据路径已确定按 Database UID 定位。
6. Tenant/Database finalizer 配合；回收策略默认 Retain，删除流程前可改，进入后固定。

资源管理者设置 Delete 即表示删除授权，不增加额外审批字段。导入显式关联已有凭据；
Released 不自动改密，管理员处理旧访问后才重新开放。Tenant 不得自选任意 OpenBao 路径。

该协议使用 Kubernetes API 持久化，不为它新增 PostgreSQL registry。
未完成一致绑定不得供应或交付；外部创建结果不确定按 Conflict 人工处理。

## Conditions

每类资源至少提供唯一的 Ready；同类型 Condition 不重复，维护 lastTransitionTime 与
observedGeneration。最低安全错误分类见 [系统规格](specification.md#10-conditions-与可观测性)。

Conflict 必须说明目标、步骤、已确认与不确定部分及人工核实建议；不能建议清空 status、
伪造 Ready 或改密码绕过。可恢复依赖故障退避重试，冲突不忙循环。
具体 Reason 在 API 切片固定，Released 不得误报为可立即交付。

## 实现验收

CRD defaulting、CEL、status subresource、resourceVersion、并发绑定、重启与依赖 watch
使用真实 API server 验证；ownerReference/namespace 删除的实际 GC 使用测试集群。
类型、CRD、sample 和 contract tests 必须一起对齐，不以 fake client 替代 API 语义。
