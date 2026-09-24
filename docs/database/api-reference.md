# v1alpha1 API 合同

| 项目 | 内容 |
| --- | --- |
| 状态 | 三资源模型已批准；新增字段与绑定协议待评审 |
| API group/version | `database.ayatori.ddupan.top/v1alpha1` |
| 最后更新 | 2026-09-24 |

以 [系统规格](specification.md) 与
[ADR-0009](../decisions/0009-database-resource-and-claim.md) 为准。尚未实现新 API，
本页不提供可直接 apply 的三资源 YAML，以免把工作名称和未决字段当作已发布合同。

## 通用约定

- Instance 是 cluster-scoped；Tenant 是 namespaced；Database scope 待字段评审。
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

以下是字段职责，不是已批准的 JSON schema：

| 内容 | 合同 |
| --- | --- |
| `instanceRef` | Database 自身必填；定位来源并记录绑定的 Instance UID，不依赖 Tenant 补齐 |
| 外部目标 | 实际 database 名称及已确认的管理范围；操作开始后不可隐式改目标 |
| 来源 | 区分动态供应与管理员显式导入；不能从同名存在推断导入授权 |
| 申请预留/绑定 | 至多一个 Tenant，含 namespace/name/UID；Released 保留旧身份 |
| 回收策略 | 资源侧 Retain（默认）或显式 Delete；普通 Tenant editor 不得扩大授权 |
| 观察与进度 | 实际目标、当前阶段、条件和安全诊断，不保存秘密 |

概念生命周期包含供应/验证、可绑定、已绑定、Released 和删除；最终 phase 枚举待协议评审。
Released 不自动变成可绑定。Database 不以 Tenant 为 GC owner；使用中的资源受删除保护。

导入的初始检查只读；存在不等于 Ready，也不等于有权交付给任意 Tenant。
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

## 绑定协议评审要求

实现前必须明确：

1. 资源 scope 与 typed reference 格式，管理员预留/导入与普通申请者的 RBAC 边界。
2. 资源侧排他记录的写入点、双向绑定顺序、API 冲突重试与单边完成恢复。
3. Tenant UID 变化、对象删除、Released 旧引用与管理员重新授权的判断。
4. 同一物理数据库重复登记的冲突处理；列表查询不是原子认领。
5. Database 的 role/凭据管理范围、稳定凭据定位、旧访问处置与投射清理顺序。
6. 删除开始后的策略固定点和 Tenant/Database finalizer 配合。

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
