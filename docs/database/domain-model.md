# 领域模型设计草案

状态：Draft，含已确认决策。日期：2026-09-13。

本文定义领域职责、身份与一致性边界，并用对象规格细化字段和方法合同；方法使用
设计签名，不固定 Go 目录、SDK 或框架，也不批准实现。外部行为以
[系统规格](specification.md) 为准；下列未决问题不能由实现自行决定。
PR #6 的代码和已有 registry 表结构是可评估的实现素材，不反向决定领域模型。

## 1. 领域与统一语言

本系统的领域是“在共享 PostgreSQL 上供应并管理应用租户”，不是数据库服务器运维。
v1alpha1 先采用一个限界上下文，不把 PostgreSQL、Bao、Kubernetes 各自当成业务上下文。

| 术语 | 含义 | 不是什么 |
| --- | --- | --- |
| Instance | 平台登记的外部 PostgreSQL 管理对象及其供应策略 | 连接池、VM 或 controller 单例 |
| Tenant | 一个应用的数据库使用合同及受管资源生命周期 | PostgreSQL database 的别名 |
| Database | 租户数据库的名称、owner、扩展等期望描述与实际观察 | 包含 Bao 登录与连接关闭的操作接口 |
| LoginRole | 同时作为 database owner 和应用登录身份的角色 | 额外的 NOLOGIN owner |
| OwnershipClaim | 某个 Tenant 身份对一组资源名称与凭据位置的所有权声明 | 工作流阶段或仅凭名称推断的归属 |
| CredentialLocation | 固定推导的凭据位置及所有权关联 | 密码本身或用户可任意选择的 KV path |
| CredentialProjection | 把既定凭据交付到目标 Secret 的要求与观察结果 | controller 直接写入明文 Secret |

UID 表示一次 Kubernetes 对象身份；namespace/name 用于定位，不足以证明归属。
database OID 是诊断观察值，不充当本系统的租户身份。

## 2. 候选聚合边界

### Instance：实例能力与供应策略

Instance 是候选聚合根，持有自身身份、endpoint、管理凭据引用、实际可用扩展观察，
以及用于判断当前能力的观察结果。它不持有所有 Tenant 对象的集合。

其行为包括：

- 判断租户申请的 extension 是否在本实例实际可安装列表中；v1alpha1 暂不实现 allowlist。
- 根据管理连接、服务器信息、registry 和权限检查结果判断是否具备供应能力。
- 判断配置变化使哪些能力观察过期，禁止以旧 generation 的 Ready 证明新配置可用。
- 在 registry 初始化完成并回读验证后，接受新的就绪结果。

“探测实例”“准备管理 registry”是应用用例协调的外部操作，不是 Instance 的 IO 方法。
领域对象只接收观察值，负责前提、规则和状态决策；应用层调用适配器获取事实与执行
获准操作。领域对象不持有或调用外部访问端口、客户端或回调。具体选择见
[Instance 字段与行为](domain-instance.md)，仍处于待评审状态。

### Tenant：供应合同与资源生命周期

Tenant 是另一个候选聚合根，通过身份引用 Instance，而不是 Instance 的聚合成员。
操作一个 Tenant 不应要求装载、锁定或保存整个实例的租户集合。

Tenant 持有有效的 database/role 名称、请求的扩展、凭据交付目标、删除策略，以及
已建立的资源绑定。它负责：

- 检查绑定后的不可变字段、extension 只追加规则。
- 判断外部部分状态属于本 Tenant、尚不存在，还是与未知资源冲突。
- 决定是否允许继续供应、何时达到 Ready、是否允许释放受管资源。
- 按 Retain/Delete 合同限制行为，禁止把保留资源自动认领给同名新 UID。

Database、LoginRole 和 CredentialProjection 暂不设独立聚合根或独立 CRUD 用例。
它们可作为 Tenant 内的资源描述与观察值；有规则才增加行为，不为了“充血”添加方法。
真实 PostgreSQL database/role 的存在不意味着内存中必须各有一个有身份的实体。

聚合边界是业务规则的保护边界，不表示 Tenant 对应的 PostgreSQL、Bao、ESO 资源
能够一次事务提交。跨系统供应必须允许部分完成。

### OwnershipClaim：跨租户唯一性与持久证据

名称唯一性不可能只靠某个 Tenant 的内存检查保证。需要一项领域能力，在持久化边界
原子认领资源；已有 registry 是其适配器候选，仍需结合 catalog 和 Bao metadata 检查。

Claim 与 Tenant 关联，但不随 Tenant CR 消失：Retain 后证据必须继续存在。因此不能
把它仅视为 CR 的附属 status。是否作为独立的小聚合，先以“可独立持久化、保留并保护
归属不变量的声明”建模；不因此引入新的 CRD。

- 同一身份、同一绑定的重复认领可以成功；不同 UID 或不同绑定不能覆盖。
- Claim 预留名称不等于证明同名外部资源由本 controller 创建。
- 实际写入仍须核对所有权，不能把先查后建当成并发安全保证。
- 当前 registry 的数据库事务不能覆盖 Bao；跨实例的凭据路径竞争也不能靠单个
  registry 的唯一约束解决。写入前提与条件创建协议需单独设计和验收。

## 3. 领域、用例与适配器的分工

| 层 | 承担的职责 | 禁止承揽的职责 |
| --- | --- | --- |
| 领域对象／策略 | 身份、有效合同、归属判断、允许的动作、完成条件 | 外部 IO（包括通过接口间接调用）、解析 CLI、生成 Kubernetes Condition |
| 应用用例 | 装载模型与事实、持久化意图、调用能力、回读、提交结果 | 另写一套绕过领域规则的判断流程 |
| controller 入口 | CR 映射、调度、watch、重试、status/finalizer 写入 | 在 reconcile 中重新定义业务规则 |
| 基础设施适配器 | PostgreSQL、registry、Bao、ESO 的实际读写与并发保障 | 自行决定接管、改密码或扩大删除范围 |
| 启动装配 | 校验部署配置，创建共享客户端、连接管理器及用例依赖 | 把连接生命周期当成 Instance 的业务状态 |

领域可使用独立的身份、endpoint、identifier、extension 集合等值对象，不依赖 CRD
类型、pgx pool 或 Bao SDK。Kubernetes 对象的存取与 registry 的存取不是一个通用
`Save(Tenant)` 可以原子完成的事情；不虚构跨系统 Unit of Work。

暂不引入事件总线、事件溯源、通用聚合框架或全套 Repository CRUD。领域建模的依据
是业务规则，而不是接口和目录数量。

## 4. 状态与恢复

CR `status.phase` 仍是已批准的工作流 checkpoint，不在内存对象或 registry 再建一套
权威 phase。领域对象可以由 CR 的期望状态、checkpoint 和外部观察重新构造。

phase 只决定候选步骤，外部证据决定该步骤是否允许执行、是否已经完成。应用层在
写操作前保存意图，调用幂等操作后回读，再保存下一 checkpoint。status 写入失败时，
下次从外部事实识别完成结果；不能重发密码，也不能相信伪造的 Ready。

业务失败区分 InvalidSpec、ImmutableField、Conflict 等；依赖故障由适配器转换成
安全的能力失败，应用层决定重试并映射 Condition。凭据不进入模型序列化、status、
事件或错误明细；只能在实际需要它的执行边界短暂传递。

## 5. 用例走查与验收方向

| 场景 | 领域判定 | 应用与适配器执行／恢复 |
| --- | --- | --- |
| 登记 Instance | 当前配置的能力要求是否满足 | 读取管理凭据，验证连接与权限，准备并回读 registry；完成后才 Ready |
| 供应 Tenant | Instance 策略、绑定与归属允许供应 | 保存意图，认领资源，先写并回读 Bao 凭据，再创建 role/database，登录验证和 ESO 投射 |
| Bao 写入后进程中断 | 同一身份的部分状态可继续 | 回读原凭据继续，不生成第二份密码 |
| 两个 Tenant 竞争名称 | 只有匹配所有权的一方可继续 | 持久化认领和条件写入裁决竞争，失败方 Conflict，不覆盖资源 |
| Delete 中断 | 已消失资源可视为完成；剩余资源仍须归属正确 | 按规格顺序继续删除，全部回读不存在后才清 registry 和 finalizer |
| Retain 后同名 CR 重建 | 新 UID 不等于原所有者 | Conflict，不恢复管理、不改密码 |

领域测试验证规则与决策；adapter 测试验证锁、条件写入、SQL 与协议行为；controller
测试验证 checkpoint 持久化和重启恢复；E2E 验证最终合同。不能只验证一串 mock 调用
就声称实现了最终一致性。

## 6. 决策记录与待细化边界

1. **Instance 身份与物理目标（已确认）**：以管理员声明为准，endpoint 变更不验证
   物理服务器／registry 连续性，不增加安装身份绑定检查；旧观察失效，重验新配置
   的连接与管理能力。新 CR 视为新 Instance，不自动接管旧 UID 资源或迁移数据。
2. **Retain 完成条件**：外部依赖不可用不能永久阻止 CR 删除，但 registry 又需标记
   unmanaged。需定义 CR 消失后的补偿／清扫入口及所需身份依据，不能承诺同时原子
   完成两者，也不能在没有回读时声称已写入保留标记。
3. **管理凭据来源与 Ready（已确认）**：Instance 引用 controller namespace 内的
   管理 Secret 名称和字段；管理员维护 ExternalSecret，ESO 同步。controller 不直接
   从 Bao 读取管理凭据。已有凭据仍可访问 PG 时，Bao／ESO 故障不撤销 Instance Ready；
   首次装配无有效 Secret 则失败。Secret 的有效用户名／密码变化时重建管理连接池并
   重验，不因无关字段变化重建；controller 不修改 PG 密码，不回写 Secret 或 Bao。
4. **绑定时机**：系统规格写“首次成功后不可变”，API 文档写“首次创建外部状态后
   不可变”。应明确绑定在认领、首次外部写入还是 Ready 时固定，及如何在 status 丢失
   后恢复；否则供应中途改名称可能产生无人管理的资源。

5. **Instance 删除（已确认）**：开始受管即添加 finalizer；删除期间停止新供应，
   有 Tenant 引用就等待，无引用才解除，不级联删除外部资源。首版采用 finalizer
   与引用检查，不引入跨对象锁／准入控制；不保证并发创建与删除的原子性。

Tenant 的 Retain 等待决行为留到 Tenant 设计，不属于本轮 Instance 设计范围。
相关用例在决策批准前不进入实现，不同时实现整套模型。
