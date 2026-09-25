# Database 模块

Database 是 Ayatori 首批实际产品领域之一。当前已包含三资源 API、分层绑定与 Instance 原生
管理能力观测；尚未完成 Database 供应/导入、Tenant 凭据交付与资源回收链路。

## 当前设计（2026-09-24）

采用 Instance → Database → Tenant 的资源与申请模型；Database 独立存在，支持显式导入、
Retain 后人工重新绑定与资源侧 Delete。撤销 PostgreSQL ownership registry 及任意 status
丢失自动恢复所有权的要求。未知同名资源或创建结果不确定时，清楚报告 Conflict 并人工处理。

依据 [ADR-0009](../decisions/0009-database-resource-and-claim.md)，当前合同见
[系统规格](specification.md)。下面的迁移来源与已存在代码不反向约束新设计。
registry adapter、专属迁移/测试及 Instance 的 registry 判定现已撤除；Instance 根据完整管理
能力观察直接判定 Ready。Database 资源与绑定已接入，导入、角色/凭据供应及回收仍未完成。wiki 同步位置见
`homelab-wiki/services/postgresql-tenant-operator.md`，跨仓库发布状态由 wiki 的同步记录维护。

## 来源基线

完整设计合同及首批领域模型与测试提取自原 PostgreSQL Tenant Operator：

- 仓库：`git.ddupan.top/panxiao81/postgresql-tenant-operator`
- source commit：`dae546e58efa1be81e930861c87f7fb13bb12113`
- 原路径：`internal/domain/instance/`、`docs/domain-instance.md`
- 迁移日期：2026-09-20

本目录迁入该基线的 specification、architecture、API、领域、部署、安全、开发、迁移与运维
文档，最初只进行 Ayatori 产品归属、API group、目录和链接适配；2026-09-24 经维护者
批准的资源/申请分离修订明确替代 registry、自动恢复与原 Retain 合同，其余适用的安全约束保留。

迁移只使用该 commit 中已提交的文件。源仓库
`feature/instance-extension-observations` 工作树中的 `instance.go` 修改与
`instance_extensions_test.go` 未进入本切片。

代码被移动到 Ayatori 的 `internal/database/domain/instance`，测试 import 和文档链接相应更新；
首个后续切片按已批准合同增加 Instance extension observation：观测与当前 target 绑定，进入重新
验证或删除时失效，且支持判定不授权 Tenant provisioning。已有 Ready 切片曾按原合同加入
registry 准备决策；这一依赖现已从代码移除，不能把旧运行链路接回模型。
各层验证边界见 [Instance 领域规格](domain-instance.md)。

## 边界

- 领域层不依赖 Kubernetes types、数据库 driver 或凭据 provider。
- CredentialReference 只携带管理 Secret 的名称与字段映射，不包含 Secret 内容或 OpenBao path。
- Instance checkpoint 不是外部事实；实际能力必须由 application/adapter 观察后交给领域对象判断。
- 当前代码只检查 Instance 供应前置条件，不授予 Tenant 所有权或外部写入权限，也不表示
  Database API 已经可用。

## 管理凭据与连接切片

`application.InstanceService` 适配自原项目固定基线
[`internal/instance/service.go`](https://git.ddupan.top/panxiao81/postgresql-tenant-operator/src/commit/dae546e58efa1be81e930861c87f7fb13bb12113/internal/instance/service.go)，
保留 CredentialReader、Connector、Database 的装配边界及串行操作/释放规则。
具体连接池完全由 pgxpool v5.11.0 提供，PostgreSQL adapter 不拥有凭据缓存、轮换流程或任意观测回调。

相对源基线有两项按已批准合同作出的必要修改：管理凭据从固定 controller namespace 的
Kubernetes Secret 直接读取；每轮比较有效用户名、密码，检测到变化即关闭旧连接并重新装配。
Secret metadata 和无关字段变化不重建连接。观测后再次读取 Secret，中途有效值变化则丢弃结果，
不把旧连接的成功作为新凭据有效的证据；这不构成跨 Kubernetes/PostgreSQL 的原子事务。
Instance UID、endpoint 或凭据引用变化也会释放旧连接；Forget/Close 只释放本地资源。

`ObserveMetadata` 读取服务器版本和可用扩展，`ObserveVersion` 是版本读取便捷入口；两者
不会填充管理检查，不能产生 Ready。`ObserveManagement` 使用同一凭据/连接边界读取完整
原生管理检查，返回绑定当前 target 的 `InstanceObservation`。controller 的资源呈现适配器
使用 resourceVersion 拒绝过期写入。应用层沿用串行处理，不引入新的调度框架。

运行 `make test-database-integration` 验证真实 API server + 一次性 PostgreSQL；fixture 不接受外部
DSN，镜像固定摘要，使用随机本机回环端口并在退出时删除测试容器。覆盖缺失/错误凭据、RBAC、
namespace 边界、有效值轮换、metadata 无关变化、中途轮换、重建/重试、并发读取、Forget/Close
与 TLS DNS/IP SAN、错误 CA/主机名和禁止明文降级。CI 使用 Pod runner 执行，由 runner 提供
可用的 Docker，workflow 只做预检、不自行启动 daemon；不依赖 VM。普通 lint 之外还检查
integration 标签代码。领域单测、真实 API 行为与真实 PostgreSQL 行为分别验收，不以本切片
替代整个 Instance controller 的集成验收。

## 服务器 metadata 与扩展观测

SQL adapter 通过一条只读语句读取 `pg_catalog.current_setting('server_version')` 和
`pg_catalog.pg_available_extensions`，避免从已安装列表推断可用列表，且不依赖可修改的
`search_path`。查询失败丢弃整份结果；成功返回空列表与尚未观察严格区分。
参考 PostgreSQL 的 [pg_available_extensions](https://www.postgresql.org/docs/18/view-pg-available-extensions.html)
与 [CREATE EXTENSION](https://www.postgresql.org/docs/18/sql-createextension.html) 合同：可用列表
表示服务器提供的扩展，不证明管理账号有安装权限，也不保证依赖和其他安装前提满足。

`InstanceService` 保留原有 CredentialReader → Connector → Database 边界，复用同一个
凭据读取、连接刷新和串行释放流程，不新增连接池封装或任意查询回调。每次重新查询 metadata，
并在 Secret 有效值回读一致后生成不可变的 `InstanceObservation`，绑定本次 target（含当前
generation），不绑定建池时的旧 target。结果不包含凭据，扩展集合不与 driver 的可变 slice 共享。
凭据中途变化、读取失败或查询失败时，返回零值观察并释放连接，不复用旧的扩展列表。

调用方可将 `Target()` 与 `Extensions()` 交给 Instance 的 `ObserveExtensions`；应用调用链
仍负责同轮次使用，不能持久化或跨轮缓存这份证据。metadata 读取不安装扩展、不初始化 registry、
不设置 Ready，也不授予 Tenant 写权限。管理权限检查使用下面的独立入口。

真实 API server + PostgreSQL 测试验证未安装扩展可被观察、名称保持大小写、search_path 遮蔽
不改变查询来源、低权限账号读取、权限撤回失败与恢复、Secret 中途变化丢弃扩展结果。
单元测试补充成功空列表、查询附带部分数据时丢弃、结果与可变 slice 隔离、每轮重新读取和
generation 变化时的目标绑定；原凭据/TLS/超时/并发测试沿同一 metadata 路径继续运行。

## Registry 撤除

原 `adapter/postgresql/registry`、SQL 迁移、registry 专属集成测试及 tern 依赖已删除；
未提交的 inspection 实验也已撤除。来源仍可在 Git 历史追溯，没有删除外部 PostgreSQL 对象。
Instance 不再具有 InitializingRegistry 阶段、RegistryState、准备决策或回读方法；
首次完整管理能力观察即可完成验证，重验失败则撤销本轮就绪证据。

保留凭据读取与连接刷新、TLS、metadata/扩展观察及其真实后端测试。领域测试覆盖每项能力
在初次验证和 Ready 重验时失败、依赖恢复、重启后重新取证、错误目标/阶段及删除保护。
这不等于三资源供应、交付和回收已实现。

## Instance 原生管理观测

2026-09-25 维护者确认先使用原生非 superuser 方案，不引入 SECURITY DEFINER 接口。
`InspectManagement` 通过同一条只读语句读取当前执行角色的 `CREATEROLE`、`CREATEDB`、
superuser 属性、服务器可写状态、版本和扩展列表。不创建探针数据库/角色，不初始化 schema。
只有非 superuser、具备两项原生属性且当前服务器/会话可写时，基础管理能力才通过。
角色属性不可从继承成员关系推导；具体已有资源仍须检查 owner、membership 与授权范围。

权限依据和真实测试对应：

- role：当前角色具有 CREATEROLE，可创建普通登录角色；
- database：当前角色具有 CREATEDB，且会话非只读、服务器不在 recovery；
- grant：使用自己新建角色的管理权限，显式建立 SET membership，再以 owner 管理数据库 ACL；
- extension：新建数据库 owner 可安装 trusted 扩展；可用列表不是安装授权，非 trusted 或
  其他前提不满足的扩展仍可能失败，必须逐请求执行和回读。导入不继承此动态供应授权。

参考 PostgreSQL 官方 [CREATE ROLE](https://www.postgresql.org/docs/18/sql-createrole.html)、
[CREATE DATABASE](https://www.postgresql.org/docs/18/sql-createdatabase.html) 与
[CREATE EXTENSION](https://www.postgresql.org/docs/18/sql-createextension.html)。这些检查是基础
能力观察，不是未来操作必然成功的保证；权限、容量、连接数等仍可能在执行时变化。

`InstanceReconciliation` 协调 finalizer、观察、领域判定和删除引用检查，controller 只连接
事件、用例、状态呈现与重试。每轮重建无证据的领域对象；旧 Ready 不授权新一轮操作。
失败清除当前版本结果并撤销 Ready；CR 在观察期间被修改则拒绝旧结果，下一轮重新读取。
连接/权限变化由 Secret watch 和 30 秒重查驱动，单轮 IO 最长 15 秒；不持续写入相同状态。

manager 通过 `--database-secret-namespace`（默认 `POD_NAMESPACE`）启用 Instance 观测；
为空时不启用。本地运行需显式提供该参数。Deployment 使用 downward API 获取自身 namespace；
Secret 的 get/list/watch 权限由该 namespace 的 Role 单独授予，不放入 ClusterRole。
watch 使用 controller-runtime 的 metadata-only cache，读取有效凭据仍直连 API server。
TLS 使用既有 endpoint 合同，公开 CA bundle 可由 `--database-root-cert` 指定；不会自动挂载
生产证书或创建管理 Secret。manager worker 停止后统一关闭 pgxpool。

Instance 删除首先释放本地连接并撤销 Ready。任何引用它的 Database（含 Released、删除中）
或动态 Tenant 申请都会阻止 finalizer 解除；列表查询失败也等待。仅在引用全部解除后移除
`database.ayatori.ddupan.top/instance-protection`，不删除 PostgreSQL、账号或凭据。
引用查询与删除不是跨对象事务；后续供应仍必须拒绝已删除/删除中的 Instance。

验收使用真实 PostgreSQL + API server：原生管理账号实际建库、owner 授权、trusted 扩展
安装/回读，拒绝非 trusted 扩展；权限撤回/恢复、只读会话、superuser 拒绝和中途轮换。
实际 manager 在生成的资源 RBAC 和 namespaced Secret Role 下验证缺失 Secret 后出现、
轮换、删除、跨 namespace 拒绝和 watch。API 测试另覆盖写入版本冲突、幂等、新 reconciler
恢复与引用删除保护。完整 DBaaS 仍需供应/导入、OpenBao/ESO、Retain/Delete 集成验收。

## 设计入口

- [系统规格](specification.md)：规范性行为与验收标准；
- [架构](architecture.md)与[API 合同](api-reference.md)；
- [领域模型](domain-model.md)与[Instance 领域规格](domain-instance.md)；
- [安全](security.md)、[迁移](migration.md)与[运维](operations.md)；
- [部署合同](deployment.md)与[开发测试合同](development.md)。

部署和开发文档描述目标合同，其中旧仓库专属的 Make target、脚手架版本和目录尚未接入 Ayatori；
在对应实现切片完成前，不应把其中命令理解为当前仓库已经可执行的入口。
