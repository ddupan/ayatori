# Instance 领域对象规格

状态：Draft，含已确认决策。日期：2026-09-13。

上层合并边界见 [ADR-0008](../decisions/0008-merge-Ayatori Database controller.md)。本文只展开 Instance，不包含 Tenant 的供应
实现，也不新增 CRD 字段。设计签名用于评审职责与行为，不是待复制的 Go 接口代码。

## 1. 对象职责与生命周期

Instance 表示一次登记的 PostgreSQL 管理对象，是聚合根。它负责字段与策略校验、
根据观察结果判断能力是否满足要求、保护状态转换规则；不登录数据库，不读取 Bao，
不查询权限或初始化 registry。

本草案选择：**领域对象只接收数据并做业务决策，不直接或通过端口、回调访问外部。**
应用层调用适配器获取事实、执行被允许的操作，并将观察结果交回对象。Instance 不接收
context、客户端或 IO 接口。领域行为不是公共 SetReady：调用方提供事实，不能指定结论。

每轮从 CR 重建一个 Instance；对象不跨 reconcile 缓存，也不是线程共享单例。
管理连接可由装配层跨轮次复用，但连接复用不代表上次能力验证仍然成立。

身份与 endpoint 以管理员声明为准。改变 endpoint 不验证是否同一物理服务器或
registry，不增加安装身份连续性检查；只使旧观察失效，按新配置重验管理能力。
新 CR 是新 Instance，不自动获得旧 UID 资源的所有权，也不迁移或清理旧目标。
下文“观察绑定匹配”仅指结果属于本轮身份／配置，不是物理服务器身份认证协议。

## 2. 字段与值对象

所有可变状态封装在对象内部。构造后身份和本轮 definition 不可变；配置变更通过
下一轮装载新的 definition 处理，不提供任意 SetPhase/SetReady/SetEndpoint。

| 字段 | 类型与内容 | 来源／持久化 | 修改规则 |
| --- | --- | --- | --- |
| identity | InstanceIdentity：UID、name | CR metadata | 本次对象身份内不可变；同名新 UID 是新对象 |
| revision | 正整数，期望配置版本 | metadata.generation | 本轮不可变；不是物理服务器版本 |
| definition.endpoint | Endpoint：host、hostaddr、port、managementDatabase、tlsMode | CR spec | 本轮不可变；新配置重验 |
| definition.adminCredential | CredentialReference：name、usernameKey、passwordKey | CR spec | 只引用 controller namespace 的管理 Secret，不存明文 |
| availableExtensions | 可选的实际可安装扩展集合 | 应用层从目标 PostgreSQL 查询；本轮观察，不新增 status 字段 | 未观察与已观察的空集合不同；目标变化后旧结果失效 |
| checkpoint | Pending/Validating/InitializingRegistry/Ready/Deleting | CR status.phase | 只能由领域动作变更，应用层负责持久化 |
| observedRevision | 最近完成有结论协调的版本 | CR status.observedGeneration | 成功或已知失败时更新，单纯记录意图不更新 |
| readiness | Unknown/Ready/NotReady，加安全失败类别和操作说明 | 由 status Ready Condition 重建，结果再映射回 Condition | 方法更新；不是第二套持久化状态 |
| reportedVersion | 可选服务器版本字符串 | status.postgresqlVersion；验证后从服务器更新 | 仅供展示，不能证明连接成功 |
| deleting | 是否已请求删除 | metadata.deletionTimestamp 映射 | 本轮不可变；优先于其他动作 |
| evidence | 可选 CapabilityEvidence | 本轮外部回读；不新增 status 字段 | 重建时始终为空，不能从 Ready Condition 伪造 |

Endpoint 的构造约束沿用 API：非空 host、合法 IP、1–65535 端口、合法 PostgreSQL
identifier、显式 TLS mode，禁止隐式降级。CredentialReference 包含合法 Secret 名称
及非空字段名，不包含 namespace 或 Bao path；namespace 由应用层固定为 controller
自身 namespace。这里校验领域值，不在对象里校验整个 controller 部署配置。

CapabilityEvidence 包含本轮目标绑定（Instance UID、revision、endpoint、凭据引用）、
server version、管理能力检查结果、registry 观察结果。registry 结果区分
Absent/NeedsMigration/Usable；连接失败不能当作 Absent。它不包含密码、token 或 DSN。

管理能力要求来自规格中的 role/database/grant/extension 操作，不等价于“能执行
SHOW server_version”。具体权限探测矩阵需在 PostgreSQL 适配器规格中定义，不能
让一个没有定义检查内容的布尔值承担验收。

不属于 Instance 的字段：Tenant 清单、客户端、连接池、token TTL、CA 文件句柄、
Kubernetes resourceVersion。resourceVersion 留在应用层作为乐观并发保存的前提。

### 扩展支持判定（2026-09-14 已确认方向）

v1alpha1 按目标 PostgreSQL 实际可安装的扩展列表判断请求，不实现管理员 allowlist。
allowlist 仅保留为后续可选策略，不接受一个看似生效、实际被忽略的策略字段；现有
CRD 的 allowedExtensions 应在对应 API 改动中移除，本次只修订文档。

应用层查询实际可用扩展并提供与本轮目标绑定的观察；Instance 只做集合判断，不
访问数据库。不沿用之前提议的字符正则，不自动改大小写或名称；SQL 适配器仍须
安全引用 identifier。可用列表不是已安装列表，也不保证权限或其他安装前提满足。

未观察／查询失败不得当作空集合或不支持；不得用旧目标的列表授权新目标的操作。
非空请求须属于已观察的可用集合，返回不支持的名称；空请求无需扩展支持判定，
但不绕过 Instance 的其他就绪要求。安装后仍需回读，不能以集合匹配代替安装验证。
列表变化不触发自动卸载；已有扩展的漂移处理留到 Tenant 用例细化。

## 3. 设计签名

```text
Reconstitute(identity, revision, definition, checkpointSnapshot, deleting)
    -> Instance | InvalidDefinition

Instance.BeginValidation() -> Outcome
Instance.AssessManagement(observation: CapabilityObservation) -> Outcome
Instance.PlanRegistryPreparation(observation: CapabilityObservation)
    -> AlreadyUsable | PreparationAllowed | PreparationDenied
Instance.AssessRegistryResult(result: RegistryPreparationResult) -> Outcome
Instance.AssessReadiness(observation: CapabilityObservation) -> Outcome
Instance.CheckExtensions(requested: ExtensionSet)
    -> Accepted | ExtensionsUnsupported | ExtensionSupportUnobserved
Instance.RequireProvisioningReady() -> Accepted | InstanceNotReady
Instance.BeginDeletion() -> Outcome
Instance.Snapshot() -> InstanceSnapshot
```

Outcome 是正常推进、已知失败或方法前提不成立，不包含重试秒数、Kubernetes patch
或原始驱动错误。InstanceSnapshot 只包含 checkpoint、observedRevision、readiness、
reportedVersion，不能序列化 evidence。快照与集合访问返回值副本。

CapabilityObservation 是不可变的事实输入：目标绑定、服务器版本、管理能力检查项和
registry 观察结果；各检查项区分成功、失败、未观察，未观察不视为成功。失败只含安全
类别，不含驱动异常或凭据。对象校验目标绑定与当前身份／配置一致，拒绝不匹配输入，
不改变状态；完整性不足不能产生 Ready。观察结果由应用层收集，对象不能自行证明
这些事实的真实性或实时性；采集来源、同轮次关联和并发检查由应用层保证。

RegistryPreparationResult 为操作失败（目标绑定、安全失败类别）或操作后的完整回读
观察。单独的“迁移调用成功”不是就绪证据。CapabilityEvidence 是对象接受并判定满足
要求的观察值，不是调用方传入的 Ready 布尔值。

### 构造与恢复

Reconstitute 校验期望 definition；无效输入不构造一个可参与用例决策的 Instance。
入口把 InvalidDefinition 映射成 InvalidSpec，不必为了报告坏 CR 而制造非法领域对象。
checkpoint 缺失或未知时保守使用 Pending；reportedVersion 和 Ready 都只是旧观察，
evidence 为空。若 observedRevision 与 revision 不一致，旧 Ready 不得通过供应检查。

### 方法合同

| 方法 | 前置条件／输入 | 行为与状态变化 | 失败语义 |
| --- | --- | --- | --- |
| BeginValidation | 未删除；初次登记、配置变更或需重建 checkpoint | 转 Validating，readiness=Unknown，清空 evidence；不做外部 IO，不推进 observedRevision | deleting 时不启动验证 |
| AssessManagement | 未删除；Validating；目标匹配的观察 | 判定管理访问、metadata、权限是否满足；registry 可用或可安全准备时转 InitializingRegistry，仍为 Unknown；不执行探测 | 失败保持 Validating，NotReady，observedRevision=当前版本 |
| PlanRegistryPreparation | 未删除；InitializingRegistry；本轮前置观察 | 根据管理能力及 registry 现状决定无需写入、允许准备或禁止准备；返回决策，不执行迁移、不标 Ready | 访问失败、不兼容或证据不足时禁止写入，NotReady；保持阶段，更新 observedRevision |
| AssessRegistryResult | 未删除；InitializingRegistry；准备结果或无需写入时的完整回读 | 按全部就绪条件判断回读结果；全满足才 Ready，并更新 observedRevision/version/evidence | 操作失败或回读不满足时保持 InitializingRegistry、NotReady；不得提前 Ready |
| AssessReadiness | 未删除；Ready；本轮观察 | 配置版本不一致时仅 BeginValidation；否则根据全部观察判断是否仍满足就绪条件 | 访问失败转 Validating/NotReady；registry 缺失或需迁移时转 InitializingRegistry，保存后下一轮修复 |
| CheckExtensions | 请求集合；本轮实际可用扩展观察 | 判断请求是否为实际可用集合的子集，返回不支持的名称；无 IO、无状态修改 | ExtensionsUnsupported 或 ExtensionSupportUnobserved；不卸载已存在扩展 |
| RequireProvisioningReady | 供 Tenant 用例使用 | 要求未删除、Ready、observedRevision 匹配，并有本次调用链的新鲜完整 evidence | 不满足即 InstanceNotReady；持久化 Ready 本身不构成授权 |
| BeginDeletion | deleting=true | 转 Deleting，清除供应能力，Unknown；不执行任何数据库或凭据删除 | 引用检查／finalizer 处理失败不得恢复成可供应 |
| Snapshot | 任意合法对象状态 | 返回可安全持久化的结果值 | 不触发 IO，也不改变状态 |

领域方法只检查对象状态，不知道 checkpoint 是否已落盘。“已持久化 checkpoint”是
应用用例执行外部写入的前提。内存字段变成 InitializingRegistry 不代表已保存成功；不能
在同一轮无条件接着执行迁移。通过用例测试验证此约束，而不是伪造一个内存事务。

AssessManagement 成功只是中间步骤，observedRevision 不前移；完成就绪判定或
明确失败才产生相应有结论结果。旧版本字符串可供诊断，但失败会清空 evidence。

Instance 不在本轮暴露 CreateDatabase/DeleteDatabase：Tenant 的供应／销毁授权来自
Tenant 和 OwnershipClaim，不是从 Instance.Ready 推导。数据库执行能力如何承接
已授权动作，留到 Tenant 对象规格，不在这里设计第二个万能 service。

## 4. 应用层与外部访问边界

```text
应用层依赖的适配器能力（不传入 Instance）：
InspectManagement(context, target) -> ManagementObservation | AccessFailure
InspectRegistry(context, target) -> RegistryObservation | AccessFailure
EnsureRegistry(context, target) -> Completed | AccessFailure
```

应用层在 IO 前绑定目标并关联结果，领域对象在接受观察时检查身份和配置匹配；旧
endpoint 的成功结果不得用于新 endpoint。Inspect 是只读；EnsureRegistry 是幂等初始化／迁移，
不能顺带建立 Tenant 数据库或接管未知 schema。Completed 不足以推进 Ready，必须回读。

适配器由装配层绑定管理连接；Secret 读取与连接池释放留在该边界之后，Instance
管理连接不涉及 Bao token。适配器不得把基础设施异常转换成 Ready。失败区分依赖不可用、
认证失败、权限不足和 registry 不兼容；不兼容属于不可安全继续，不自动覆写。
registry 不兼容的具体 Condition 映射须在接口规格中确定，不能统一误报权限不足。

管理连接由应用层从 controller namespace 的 Secret 装配；管理员维护 ExternalSecret，
ESO 负责同步。Instance 路径不直接访问 Bao，也不以 Bao／ESO 当前可用性作为就绪条件。
首次装配缺少有效 Secret 时失败；已有凭据可正常访问 PG 时继续按 PG 能力判定。
检测到所引用 Secret 的有效用户名或密码变化时，应用／基础设施层使用新值重建连接池
并重新采集管理能力观察；metadata 或无关字段变化不重建。不要求 Instance generation
变化，也不能复用旧连接的成功观察来证明新凭据有效。Secret 变化监听、连接释放和
刷新均不进入领域对象；应用层保证旧连接观察不混入刷新后的调用链。
controller 不修改 PostgreSQL 密码、不回写 Secret 或 Bao 管理凭据。

## 5. 状态转换与初始化走查

```text
Pending --BeginValidation/保存--> Validating
Validating --AssessManagement(观察)/保存--> InitializingRegistry
InitializingRegistry --AssessRegistryResult(回读结果)/保存--> Ready
Ready --配置变化或访问失败/保存--> Validating
Ready --registry 需修复/保存--> InitializingRegistry
任意阶段 --删除请求/保存--> Deleting
```

1. 入口读取 CR，装配 definition、checkpointSnapshot；客户端不注入领域对象。
2. 应用层按 checkpoint 协调用例；首次调用 BeginValidation，没有 IO。
3. 保存 Validating。若保存失败，结束本轮，不执行 registry 写入。
4. 下一轮应用层调用适配器探测实例，将观察交给 AssessManagement；领域判定通过后
   保存 InitializingRegistry，保存失败则停止，不进行迁移。
5. 再下一轮应用层采集前置观察，调用 PlanRegistryPreparation。仅在意图已持久化且
   领域允许时调用 EnsureRegistry；AlreadyUsable 则跳过写入，PreparationDenied 则
   保存失败结果并停止。允许的操作完成后回读，交给 AssessRegistryResult 决定能否
   Ready；操作失败也用安全结果交回，不在应用层直接修改 phase。
6. 入口用原 resourceVersion 前提保存快照；并发变更导致冲突时重新装载，不覆盖新状态。
7. 后续 Ready 检查先由应用层探测，再调用 AssessReadiness；Tenant 用例同样获取当前事实，不能
   仅凭另一个 CR 的 Ready Condition 永久缓存授权。实际资源写入仍须处理并发变化。

阶段调度和外部操作顺序在应用层；“观察是否满足业务要求、是否允许准备 registry、
哪些结果算完成、失败退到哪里”在 Instance 方法内。controller 不重复这些规则，
也不直接把 phase 设置成 Ready。领域允许操作并不锁住外部世界，适配器仍须保障幂等
和并发安全；禁止把旧观察当成永久授权。

## 6. 不变量与恢复验收

- UID 不随名称复用；不同 UID 的 evidence/结果不可互用。
- 未完成当前配置的能力回读，不能新产生 Ready，也不能通过供应检查。
- checkpoint 可以落后或被伪造；每次初始化／供应前都核对事实。status 清空只需重新
  验证和幂等准备，不删除 registry，更不能重新生成 Tenant 密码。
- 迁移成功而 status 保存失败：重试回读已存在 registry，安全完成，不重复破坏性写入。
- registry 在 Ready 后消失：下一次回读撤销 Ready，保存修复意图后才能重新准备。
- 外部 IO 超时：产生安全失败结果；保存 status 使用仍有效的外层上下文，不能复用
  已超时的 IO 上下文而丢失失败状态。
- 已请求删除的 Instance 不允许新供应；BeginDeletion 不删除 PostgreSQL、Tenant 或
  Bao。应用层在开始受管时添加并保存 finalizer，而非出现 Tenant 后再添加。
  删除时查询所有引用它的 Tenant（含删除中的对象）；有引用或查询失败就保留
  finalizer，确认无引用才移除。引用查询、finalizer 写入和本地连接释放均不属于
  领域 IO，Instance 只根据删除请求禁用供应能力。
- 首版不为 Instance 删除增加跨对象锁或准入控制。并发创建的 Tenant CR 不被
  finalizer 拦截，但遇到删除中／不存在的 Instance 不得开始供应；不承诺取消
  已在途的外部操作，也不声称引用查询与移除 finalizer 是跨对象原子事务。
- CheckExtensions 失败不能授权扩展安装；可用列表变化不会自行卸载已有扩展。
- Snapshot、错误、日志和领域对象格式化不输出明文凭据或 token。
- 领域测试只提供观察值，无需数据库、网络、context 或 IO mock；相同状态和输入
  得到相同决策。缺少检查项、目标不匹配和旧配置结果不得产生 Ready。

上述每条都对应领域或用例测试；真实权限检查、迁移与并发保障由适配器集成测试
验证。本文为设计文档，未执行或宣称通过这些测试。

## 7. 本轮待评审与后续阻塞项

本轮请先确认字段归属、应用层采集事实／Instance 纯决策的分工、方法与状态转换合同。
管理 Secret 来源、Bao 故障不单独撤销 Instance Ready，以及管理用户名／密码变化时
重建连接池，以及管理员声明的 Instance 身份／endpoint 和简化 finalizer 删除规则
均已确认。其他决策及未决项见总体草案，不增加后台清扫器或状态字段。

批准本对象结构不等于批准这些未决行为，也不意味着立刻实现完整供应链路。
