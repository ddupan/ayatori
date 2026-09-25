# Instance 领域对象规格

日期：2026-09-25。资源模型修订依据
[ADR-0009](../decisions/0009-database-resource-and-claim.md)，行为以
[系统规格](specification.md) 为准。本页替代原 registry 准备与恢复合同；领域依赖已撤除，Instance 应用/controller 观测链路已接入。

## 职责

Instance 是登记的 PostgreSQL 管理入口，只接收观察、判断规则，不直接或通过回调执行 IO。
应用层读 Secret、调用 adapter、关联目标与观察，再将结果交给领域判定。
领域不持有客户端、连接池、context 或完整 Database/Tenant 集合。

每轮由 CR 重建；连接可由应用层复用，但旧连接、旧 Ready 不是本轮能力证据。
身份与 endpoint 以管理员声明为准，变更使观察失效，不验证物理服务器连续性，
不迁移旧数据，不自动授权旧 UID 资源的操作。

## 字段与观察

| 内容 | 合同 |
| --- | --- |
| identity | Instance UID/name；同名新 UID 是新对象 |
| revision | 当前 generation，不能与旧观察混用 |
| definition | endpoint 与管理 Secret 引用，不含明文 |
| checkpoint | Pending、Validating、Ready、Deleting |
| observedRevision/readiness/version | 映射 CR status，只表示进度或最近结果 |
| availableExtensions | 本轮实际可用集合；未观察与空集合不同 |
| evidence | 本轮目标、版本与管理能力检查结果，不持久化为永久授权 |
| deleting | 删除请求；禁止新的供应 |

CapabilityObservation 包含目标（UID、generation、endpoint、凭据引用）、server version、
连接/metadata/role/database/grant/extension 管理检查项。各项区分成功、失败和未观察，
不包含 registry 状态、凭据或驱动错误。缺项或目标不匹配不得 Ready。

实际可用扩展不等于安装权限，不做 allowlist，不改大小写；查询失败不当作不支持，
列表变化不自动卸载。安装必须由实际操作及回读验证。
resourceVersion 留在应用层处理 API 并发，不是领域版本或物理数据库身份。

## 行为

- Reconstitute 校验 definition，重建 checkpoint，丢弃旧 evidence。
- BeginValidation 清空能力证据并进入 Validating。
- AssessManagement/AssessReadiness 根据本轮完整观察判断 Ready 或安全失败；
  方法的具体合并方式在实现重构时决定，不保留无意义的中间初始化阶段。
- CheckExtensions 判定请求集合，不能单独授权供应。
- RequireProvisioningReady 检查本轮能力与删除状态；不授予 Database 所有权。
- BeginDeletion 禁止新供应，不执行外部删除。
- Snapshot 返回值副本，不序列化 evidence 或秘密。

撤销 PlanRegistryPreparation、AssessRegistryResult、RegistryPreparationResult、
RegistryState 与 InitializingRegistry 的设计需求。不能用始终返回 Usable 的兼容层绕过旧逻辑。

## 应用与连接边界

管理凭据来自 controller namespace 的 Secret；管理员维护 ExternalSecret，ESO 同步。
Instance 不直接访问 Bao。有效用户名/密码变化时应用层释放旧连接、用新值装配并重验；
metadata/无关字段变化不重建。采集途中有效值变化必须丢弃观察，不因 generation 没变而复用。
controller 不修改 PostgreSQL 密码、管理 Secret 或 Bao 管理凭据。

InspectManagement 是观察能力，不包含 schema 初始化/迁移。只读 metadata 查询仍不足以
证明管理权限；真实权限矩阵由 adapter 定义和测试。连接复用由 pgxpool 提供，不自建池。
已可用管理凭据下，Bao/ESO 当前故障不单独撤销 Instance Ready。

## 状态与删除

```text
Pending → Validating → Ready
Ready → Validating（配置/凭据变更或能力失效）
任意阶段 → Deleting
```

应用层保存意图、获取观察、领域判定、按 resourceVersion 保存结果；
保存冲突重新装载，不能覆盖较新配置。Instance 观察本身不创建外部资源，
初始 status 缺失可重新探测，不由此推出 Database 所有权可自动重建。

受管前先保存 finalizer。删除期间查询 Database 引用（包括 Released/删除中）及未绑定
Tenant；存在引用或查询失败都等待，无引用才解除。不得级联删除数据库或凭据。
finalizer 不阻止并发申请 CR 创建；新请求见 Instance 删除中/不存在时不得供应。
不引入跨对象锁，不声称列表与删除之间存在原子事务。

## 验收与实现差距

领域单测覆盖缺项、旧配置、错误目标、扩展空集合与未观察、重验与删除禁用。
真实 API server 验证 Secret、resourceVersion、watch、finalizer；真实 PostgreSQL 验证
权限、TLS、凭据更新和查询失败，不以领域布尔值或 server_version 查询代替管理权限验收。

Instance 领域代码、adapter 与测试的 registry 依赖已撤除。AssessManagement 根据完整观察
直接完成验证；AssessReadiness 失败进入 Validating，依赖恢复后重新验证。领域测试覆盖各检查项
在这两个入口的失败与恢复。原生权限检查、Instance controller、metadata-only Secret watch
和引用删除保护已接入，验证矩阵见 [模块说明](README.md#instance-原生管理观测)；
Database/Tenant 的供应、交付和回收仍未完成。
