# Ayatori Database 系统规格

| 项目 | 内容 |
| --- | --- |
| 状态 | 已批准并已实现；字段见 [API 合同](api-reference.md) |
| 目标 API | `database.ayatori.ddupan.top/v1alpha1` |
| 最后更新 | 2026-09-25 |
| 决策 | [ADR-0009](../decisions/0009-database-resource-and-claim.md) |

本文是当前行为合同，替代旧的 Tenant 同时承担申请与资源生命周期、PostgreSQL registry
持久所有权、任意 status 丢失自动恢复的设计。

## 1. 范围

在已存在的 PostgreSQL 实例上供应独立数据库、一个兼任 owner 的 login role、申请的扩展及
应用凭据；支持管理员显式导入已有数据库。Kubernetes API 管理声明与绑定，OpenBao 保存
应用凭据，并在 Tenant status 公布其位置；投射由消费方集群自行完成（[ADR-0010](../decisions/0010-credential-delivery-ends-at-openbao.md)）。

不运行 PostgreSQL、VM、存储、备份或 OpenBao；不提供跨实例数据迁移、自动密码轮换、
多角色权限产品或跨系统事务。备份与数据恢复仍由管理员负责。

## 2. 资源与职责

```text
Instance
  └─ Database × N          独立持久资源
       └─ Tenant × 0..1    排他绑定的用户申请
```

| 资源 | 职责 | 不承担的职责 |
| --- | --- | --- |
| Instance | 登记实例、管理连接、能力与供应前置条件 | 持有租户集合、保存所有权表 |
| Database | 描述外部数据库、管理范围、绑定与回收策略 | 充当第二套 registry 或通用资源框架 |
| Tenant | 声明需求或显式选择资源，申请使用并交付凭据 | 删除时隐式销毁独立资源记录 |

Instance 与 Database 为 cluster-scoped，Tenant 为 namespaced。Database 的 Kind 是
PostgreSQLDatabase。
Database 由平台管理员管理，不属于应用 namespace，不引入资源专用 namespace。
Tenant 按名称引用 Database；Database 绑定记录包含 Tenant 的 namespace/name/UID。
普通申请者通过 Tenant 申请使用，不能自行修改 Database 回收策略或将 Released 资源重新开放。

2026-09-25 确认：第一版以一个 database、一个兼任 owner 的 login role 及其应用凭据
作为 Database 的生命周期边界，Tenant 负责申请与交付，不单独拥有账号或凭据生命周期。
不预留多账号字段，不新增独立 Role、Credential 或 Claim CRD。一库多账号若出现实际需求，
通过后续 API 版本演进处理，不纳入 v1alpha1。此边界不扩大导入资源的管理授权。

Database 自身必须声明 `instanceRef`，手工登记时同时指定实际数据库名；无需先存在 Tenant，
即可通过 Instance 验证目标。动态申请由 Tenant 选择 Instance，供应时把该引用写入 Database；
选择已有 Database 的 Tenant 从资源获取 Instance，不重复指定另一份来源。资源与实例的归属
独立于使用绑定，Tenant 删除后仍保留；修改引用不是数据库迁移。

参考 [Kubernetes PV/PVC](https://kubernetes.io/docs/concepts/storage/persistent-volumes/) 的
资源/申请分离与绑定生命周期，不复制存储调度和 CSI 协议。资源与申请的关系不是 GC 所有关系。

## 3. 身份与事实来源

- Kubernetes spec 保存声明；受保护的资源绑定记录与 status 保存身份关联、操作进度和观察。
  它们通过 API 持久化，不因 controller 重启而消失。
- PostgreSQL catalog 是 database、role、grant、extension 实际状态的来源。
- OpenBao 是应用凭据的事实来源；Ayatori 不在控制面投射 Secret（[ADR-0010](../decisions/0010-credential-delivery-ends-at-openbao.md)）。
- Instance、Database、Tenant 以 UID 区分对象身份；namespace/name 用于定位，
  同名新 UID 不继承旧绑定。Database `status.roleOID`/`databaseOID` 是创建或导入后回读确认的
  PostgreSQL 身份，冲突判定、Delete 回收与重新开放都按它核对；它不跨集群或备份恢复保持稳定，
  也不能凭同名补记。
- 不新增 PostgreSQL 所有权表、安装身份表或 Retain 墓碑；COMMENT 也不能授权认领。
- 记录操作意图不等于外部操作成功，phase 不等于外部所有权；执行前后仍须观察实际状态。

## 4. Instance 合同

Instance 声明 host、hostaddr、port（默认 5432）、管理 database（默认 postgres）、
TLS mode（默认 verify-full）及 controller namespace 的管理 Secret 名称和字段映射。
禁止隐式 TLS 降级。凭据引用不接受自选 namespace 或 OpenBao path。

管理 Secret 由 ESO 从 Bao 同步（ExternalSecret 随 release 由 Flux 下发）；controller 只读，不修改管理密码，
不直接从 Bao 取管理凭据。有效用户名或密码变化时释放旧连接并重验；仅 metadata 或无关
字段变化不重建。中途凭据变化必须丢弃旧观察。已有有效管理凭据时，Bao/ESO 故障本身
不撤销 Instance Ready；首次缺少有效 Secret 时不能 Ready。

Ready 要求当前目标的连接、服务器 metadata 和所需管理能力检查通过，不要求创建、
迁移或读取 registry，也不证明备份或高可用。阶段简化为 Pending → Validating → Ready，
删除进入 Deleting。实际扩展可用列表不等于安装权限；查询失败不等于不支持。

endpoint 变更由管理员负责评估，不验证物理服务器连续性，不迁移或清理旧目标；
旧观察失效。新 UID 不接管旧资源。Instance 开始受管前保存 finalizer；删除时停止新供应，
只要有引用它的 Database（包括 Released/删除中）或尚未绑定的 Tenant 就等待。
查询失败不视为无引用；无引用才解除 finalizer，不级联删除任何业务资源。
引用检查不是跨对象事务，正在删除或不存在的 Instance 不允许开始新的供应/绑定。

## 5. 动态供应与排他绑定

1. 校验 Tenant 请求、Instance 能力、名称和扩展要求。
   绑定 controller 在触及资源侧绑定前将 Tenant 进度记为 Binding，固定申请目标，
   避免两次绑定写入之间修改引用占用第二个资源；该进度不是已绑定的声明。
2. 在首次外部写入前持久化独立 Database 记录、确定目标与管理范围。
   动态创建的 Database 名称由 Tenant UID 确定；重试复用同一记录，不重复创建。
3. 先在 Database 写入 Tenant namespace/name/UID，再在 Tenant status 写入 Database
   name/UID；双向记录一致后才允许供应或交付。
4. 按已确认步骤建立凭据、role、database、授权和扩展，逐步回读。
5. 验证应用登录、公布已确认凭据位置后，Tenant 才可 Ready。

绑定前固定有效目标；绑定或开始外部供应后不得通过修改名称或引用实施隐式迁移。
每个 Database 最多一个使用者，每个 Tenant 最多一个 Database。
绑定 API 写入采用 resourceVersion 并发控制；双向记录不原子，单边完成不得授予使用权限。
采用 Kubernetes PV/PVC 的资源侧先写模式，参考
[官方 bind 实现](https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/volume/persistentvolume/pv_controller.go)。
Database 已绑定其他 Tenant 时报告 Conflict，不抢占；API 更新版本冲突时重新读取并判断，
不能盲目覆盖。资源侧成功而 Tenant status 写入失败时，下一次 reconcile 核对双方身份后
补写，不因单次失败撤销资源侧绑定。普通 controller 重启沿用这些持久记录继续协调。
这只处理 Kubernetes 绑定记录的部分完成，不提供外部数据库不确定创建结果的自动认领。
绑定成功不代表 Ready，具体字段及并发、重启、单边写入恢复必须由真实 API server 测试验证。

不同 Database 记录请求同一外部名称仍可能竞争，不能仅靠 Kubernetes 中的列表检查保证
PostgreSQL 名称唯一。后端创建时的重名失败报告 Conflict，失败方不得接管胜方资源。
不为此新增跨系统锁或 registry。管理员也不得把同一物理数据库登记成多个可绑定资源。

## 6. 显式导入

管理员创建资源声明，明确 Instance、已有数据库和允许管理的范围，构成导入授权。
初始检查只读验证存在性、owner、角色权限和扩展等是否匹配；不匹配报告清楚的差异，
不得通过重置密码、改变 owner 或撤销现有访问来“完成导入”。

未显式导入的同名数据库一律 Conflict。导入资源默认 Retain，不隐含 Delete 授权。
有权创建 Tenant 的申请者可以显式引用已登记、未绑定且可用的 Database；不增加资源侧
允许绑定名单或逐 Tenant 的管理员审批。绑定仍检查目标、可用状态与排他关系。
Released 不在可申请范围，必须由管理员处理旧访问并重新开放。导入时以 `spec.credentialRef`
显式关联已有凭据，不通过隐式改密生成替代凭据。

## 7. Retain、重新绑定与 Delete

回收策略属于 Database，默认 Retain；Tenant 删除是释放申请，不是独立资源的 GC 授权。
有资源管理权限的主体可在进入删除流程前修改 Retain/Delete；进入删除流程后策略固定。
显式设置 Delete 就是删除授权，不增加第二次审批或确认字段。
Database 不得设置会让它随 Tenant 消失的 ownerReference。

### Retain

- 保留 Database 对象、外部数据以及与资源关联的角色和凭据，不自动删除或重置。
- Tenant 释放后 Database 进入 Released，保留旧绑定身份用于诊断和防止自动复用。
  Released 不是 Available，不再向原申请交付新状态，也不自动分配给同名新 Tenant。
- 保留策略不要求 PostgreSQL/Bao 在线才能完成申请释放，但必须先将释放关系安全记录到
  Kubernetes；API 写入失败时不能宣称释放完成。仍有在途操作时不得跳过必要协调。
- 管理员检查数据、旧账号访问与凭据后，显式授权重新绑定。保留数据的复用可以不清空数据，
  但必须由管理员确认新使用者应获得这些数据及旧使用者的权限处置。
- 解除绑定不等于撤销 PostgreSQL 访问；Retain 不承诺自动撤权。消费方集群中的投射由其自行清理。

### Delete

必须由有权限的主体在资源侧明确授权，并核对 Database 身份、绑定、实际对象和管理范围。
在 finalizer 保护下按已确认 OID 依次：`NOLOGIN` 并终止应用会话 → `DROP DATABASE` →
`DROP ROLE`（无 CASCADE/REASSIGN）→ 永久删除 OpenBao metadata → 移除 finalizer。共享或
未纳管的对象不得删除。Ayatori 不投射凭据，没有投射清理步骤。

每步回读，失败保持进度与 finalizer；确认已删除的对象可幂等跳过，未知同名对象不能继续删除。
数据库被使用时，直接删除 Database 不得绕过绑定保护。资源已释放后才按策略处理。

## 8. 幂等、失败与人工处理

普通依赖故障退避重试。已持久确认且仍与观察一致的步骤可以幂等继续；controller 重启
不重新生成密码，不重复创建已确认资源。

外部创建成功但记录尚未保存，或超时导致结果不确定时，若不能可靠确认归属，报告
Conflict 并停止相关写入；不得仅凭名称相同、曾记录意图或字段相似自动认领。
失败恢复不承诺全部自动完成，也不实现队列模拟事务。

status 缺失不假定发生于正常重启。Instance 可重新探测能力；Database/Tenant 缺少绑定或
操作确认时不能从外部同名对象推导所有权。按冲突/灾难恢复处理，不自动重建所有权表。

人工处理必须能看到：请求与资源身份、目标 Instance/database/role、失败步骤、已确认完成
与结果不确定的操作、冲突原因、下一步核实建议。保留现场，不自动删除疑似残留或改密。
管理员核实后可清理确定的残留再重试，或走显式导入；不能通过伪造 Ready/清空 status 强行继续。

## 9. 权限、凭据与扩展

2026-09-25 确认第一版管理账号使用原生非 superuser + CREATEDB/CREATEROLE 方案，
不引入 SECURITY DEFINER 接口；权限检查与限制见 [安全合同](security.md#最小权限)。

动态供应继续使用一个兼任 database owner 的 LOGIN role；应用角色不得具备 superuser、
CREATEDB、CREATEROLE 或 replication 权限。撤销 PUBLIC CONNECT，再授予目标角色；
不修改无关数据库和角色。identifier 匹配 `^[a-z][a-z0-9_]{0,62}$`，SQL 安全引用。

请求扩展按实例实际可用集合判断，成功后只追加，不自动 DROP EXTENSION。
可用列表查询失败按依赖错误处理；实际安装仍检查权限与结果。

应用密码使用安全随机源，只写 OpenBao；已有可信凭据可复用，不因失败生成第二份密码。
新建时先安全保存并回读凭据，再创建角色；凭据写入本身结果不确定也适用人工冲突规则。
Ayatori 不创建 ExternalSecret 或含凭据的 Secret；消费方按 Tenant status 公布的位置与版本自行投射（[ADR-0010](../decisions/0010-credential-delivery-ends-at-openbao.md)）。

凭据仍输出 username/password/database/host/hostaddr/port/sslmode 七键，不生成带密码 URI。
Tenant status 提供 `credentialRef`、`credentialVersion` 与无认证信息的 `credentialURL`。
mount/base path 属部署配置，Tenant 不得自选任意路径；原按 Tenant namespace/name 固定
推导路径的规则撤除。动态供应的凭据路径按 Database UID 确定；导入时显式关联已有凭据
位置，不要求搬迁已有凭据。Released 不自动改密，管理员处理旧访问后才重新开放资源。
不得因换 Tenant、改部署参数或重新绑定就隐式搬迁凭据或改密。

2026-09-27 确认最小凭据记录：Database `status.credentialRef` 在首次外部写入前固定
mount/path；`status.credentialVersion` 仅在成功创建并回读后保存 KV 版本。位置和确认版本
不得自动更改或清空，Conditions 只描述当前可用性。已有值但无确认版本时报告 Conflict，
不能靠读取成功认领；已确认凭据消失或最新版本不一致同样停止，等待人工核实。
部署参数变化不得搬迁旧位置；确认记录写入的 resourceVersion 冲突不能通过盲目重试覆盖。

TLS、OpenBao Kubernetes auth、controller/ESO 身份隔离、Secret 读取范围和防泄漏要求
见 [安全模型](security.md)。这些安全约束继续适用。

## 10. Conditions 与可观测性

三类资源均提供唯一的 Ready Condition 及 observedGeneration，phase 只辅助表示阶段。
Ready=True 必须有当前目标的实际验证；Database 已绑定不等于 Tenant 凭据交付已经完成。

实际 Reason 与阶段枚举见 [API 合同](api-reference.md#conditions)。Released 不可直接重新绑定，
不假定仅靠 phase 判断授权。

使用结构化日志、Events 和低基数失败分类 metrics；Condition 是面向使用者的主要诊断入口。
禁止在任何 CR、Event、日志、metric、trace 或测试输出中出现密码、Token、完整秘密响应。

## 11. 验收

| 场景 | 必须验证的结果 |
| --- | --- |
| Instance 登记与重验 | 无 registry 依赖；真实凭据/TLS/管理权限检查 |
| 动态供应与重复 reconcile | 独立资源记录、排他绑定、密码不变、实际登录成功、凭据位置已公布 |
| 显式导入 | 无数据/密码/owner 隐式修改；错误目标、已占用或 Released 资源的申请被拒绝 |
| 同名未知资源 | Conflict，原数据库/角色/凭据不变 |
| 并发申请与单边绑定 | 最多一个使用者；失败方不能开始危险外部操作 |
| controller 重启 | 已确认步骤正常继续；不确定创建报告人工可诊断冲突 |
| Retain 与 namespace/Tenant 删除 | Database 不被 GC，外部数据保留，Released 不自动复用 |
| 人工重新绑定 | 旧 UID 不继承使用权；确认数据及凭据/旧访问处置后才能交付 |
| Delete 每步中断 | finalizer 保留，可重试，不误删未知/共享/未纳管对象 |
| 依赖稍后出现/权限恢复 | 安全重试，过期观察不授权写入 |
| status/备份恢复不一致 | 不凭同名推导归属，明确人工处理范围 |
| 泄漏与权限 | canary 不出现在输出；namespace 用户不能导入/回收他人资源 |

纯规则用单元测试；schema/CEL/status/watch/resourceVersion/绑定事件链用 envtest；
真实 PostgreSQL/OpenBao 验证后端行为；GC 用具备相应控制器的测试集群，envtest 不运行 GC。
Database 模块中 ESO 只同步 Instance 的管理 Secret（[postgresql.yaml](../../deploy/flux/control-plane/postgresql.yaml)），
不参与应用凭据交付。详细测试与实现差距见
[开发文档](development.md)。导入不是数据迁移，dump/restore 仍是可选路径，见
[迁移文档](migration.md)。
