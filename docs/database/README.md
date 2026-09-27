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
- Instance 观察只检查供应前置条件，不单独授予 Tenant 所有权或外部写入权限；凭据准备
  另行校验双向绑定和保护，尚不表示完整 Database API 已经可用。

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

## 应用凭据存储切片

`adapter/openbao` 使用官方 Go SDK `api/v2 v2.7.0` 的 KV v2 API，只有创建和读取，
不维护 registry、不覆盖已有密码。动态路径由固定前缀与 Database UID 组成；所有访问都校验
配置前缀，已有导入位置也不能绕过 controller 的凭据权限范围。

创建使用 CAS=0，随后回读七键和版本 1；已有值或软删除历史报冲突。关闭 SDK 自动重试，
写入响应丢失、回读失败或内容变化均返回不确定结果，上层不得生成第二份密码或自动认领。
`Read` 只适用于调用方已确认关联的路径，读取成功本身不是管理权证据。错误不传播 SDK
响应体；内存凭据的普通格式化及 JSON 输出均脱敏，明确的 `SecretData` 才返回明文七键。

依据官方 [KV v2 CAS 合同](https://github.com/openbao/openbao/blob/main/internal/builtin/logical/kv/path_data.go)
与 [Go SDK](https://github.com/openbao/openbao/tree/main/api)。`make test-database-integration`
现包含独立 OpenBao dev 容器，固定摘要、随机回环端口、无持久卷，不接受外部地址。
真实后端覆盖创建/回读、并发唯一创建、重建适配器读取、软删除冲突、固定前缀 token
拒绝管理路径，以及成功写入后丢失响应；HTTP 故障测试补充不重试和错误脱敏。

认证会话和凭据准备用例已接入 manager，但默认不启用外部写入。
Database 已有 `status.credentialRef` 和 `status.credentialVersion` 的字段与 CEL 校验：
固定位置、只在创建并回读成功后确认版本，二者写入后不可清空或修改。
`ReadConfirmed` 按已确认版本检查最新 KV 值，删除或版本漂移报 Conflict，不读取旧版本掩盖变化。
测试 token 只用于临时 fixture，不是生产静态 token 配置接口。

### 凭据准备闭环

配置认证后，显式设置 `--database-credential-mount` 启用准备；路径默认
`applications/<Database UID>`，前缀由 `--database-credential-prefix` 配置。
`CredentialPreparation` 用例检查 Provision 来源、双向绑定 UID、申请目标、finalizer、
删除/Released 状态及当前 Instance Ready；导入资源不执行本流程。
顺序为固定位置 → 确认后端尚无凭据 → 保存 CreationStarted → 创建并回读 → 保存确认版本。
`CredentialReconciler` 只负责 Database/Tenant/Instance watch 和 30 秒依赖重查；
Kubernetes repository 负责直接读取及有 resourceVersion 保护的状态更新。

外部操作前后回查目标：Database 必须仍是同一 UID/resourceVersion，Tenant/Instance 必须
保持绑定、spec generation、删除状态、finalizer 保护与有效 Ready；无关 Conditions 刷新
不构成目标变化。不存在跨 Kubernetes/OpenBao 原子事务：中途发生相关变化时停止确认，
留下可观察状态，交给后续协调或人工核实，不盲目重试过期的确认写入。

`CredentialsReady=False/CreationStarted` 是正在进行而未确认的诊断，不是成功证明。
任何新一轮协调遇到该状态且没有确认版本，都转为 Conflict；即使进程在发出请求前退出
也采用这一保守边界。明确的认证/权限拒绝可等待恢复；响应丢失、回读失败、确认保存失败
及未确认的已有值均不会自动认领或重新生成密码。Conflict 保留首次原因，普通依赖错误
保留已确认版本；配置变化停止在原位置，不搬迁或覆盖。并行实例中途撞上已开始的创建
同样可能保守地要求人工处理，不承诺无损接续；多副本部署应启用既有 leader election。

成功只设置 `CredentialsReady=True`，Database Ready 仍为 False/ProvisioningIncomplete，
Tenant 仍未完成交付。本切片没有 PostgreSQL role/database 创建、扩展安装、ESO 投射、
Retain 释放或 Delete 清理，也不会解除 finalizer。不要作为完整 DBaaS 部署。

单元测试穷举前置条件；真实 API server + 隔离 Bao 验证创建、状态确认、幂等、重启、并发、
依赖恢复、固定位置、不确定结果、确认保存失败和删除边界；实际 manager 验证 watch 驱动
及重启。该 fixture 只声明 Instance 前置 Ready，真实 PostgreSQL 管理能力由既有 Instance
集成测试覆盖，不把凭据准备验收当成实际建库或应用登录验收。

## OpenBao Kubernetes 认证会话

公共 `internal/infra/openbao.KubernetesSession` 复用官方 Kubernetes auth helper 和 `LifetimeWatcher`
（均为 v2.7.0）。认证直接注入 `manager.GetClient()`，与 reconcile 共用已装配的 Kubernetes
client，不从配置另建客户端。标准 `--kubeconfig` /
`KUBECONFIG` 支持 systemd 或其他集群外运行方式，集群内使用 in-cluster 配置，不要求存在 Pod。
Kubernetes 身份的签发和更新由部署管理及 client-go 的认证机制负责，不另建 kubeconfig 读取器。

每次登录前，通过该 client 的 `SubResource("token").Create` 调用固定 namespace/name 的
ServiceAccount TokenRequest；写入直连 API server，不读取 cache 或要求额外的 SA get 权限。申请
audience 匹配 OpenBao role、期望有效期 600 秒的短期 JWT；检查返回值非空且未过期，再交给
官方 Kubernetes auth helper。JWT 不缓存，不读取投射文件，也不回退静态 OpenBao token；
实际 JWT 有效期由 API server 决定。RBAC 拒绝或 TokenRequest 失败时不会继续 Bao 登录。
OpenBao 登录结果必须包含有效 token 和有限 TTL。

续期、到期阈值与等待时间由 SDK 管理；可续期 token 的续期失败就撤下本地 token，不可续期
token 由 SDK 监测剩余寿命。会话结束后最多每 5 秒重新登录一次，并重新申请 Kubernetes JWT。
`Ready()` 仅表示当前 lease 正受 SDK 管理，不授权任何 Database 写入，也不能保证下一次请求
必然成功。认证/续期响应不写日志、不返回给调用方；后端操作仍独立检查并返回脱敏错误。

`Start(ctx)` 退出时清空 client token 并等待续期 goroutine 结束。SDK Stop 不取消已经发出的
续期 HTTP 请求，因此专用 client 的请求期限固定为 15 秒；不增加新连接池或自己的续期算法。
同一会话拒绝并发 Start。manager 使用 Runnable 管理生命周期，并增加 `openbao-auth` readiness
检查；认证故障不影响 liveness。`--openbao-address` 为空时不启用，不自动修改生产 auth/RBAC。
HTTPS 和显式 CA/系统信任根不可通过 BAO 环境变量降级，参数见 [部署合同](deployment.md)。

依据官方 [Kubernetes auth](https://openbao.org/docs/auth/kubernetes/) 与
[token 生命周期](https://openbao.org/docs/concepts/auth/)。真实测试使用 envtest 签发 SA token，
OpenBao 通过专用 reviewer 调用真实 TokenReview；集群外受限 kubeconfig 启动实际 manager，
验证共享 client 申请 JWT、短 TTL 续期、RBAC 撤回/恢复与重新登录、跨 namespace/其他 SA 拒绝、
错误 OpenBao audience 拒绝以及凭据访问恢复。临时 TokenReview 入口仅允许对应 POST，
两段连接均验证 TLS；其 Docker bridge 入口仅为隔离测试，不修改生产 OpenBao 或 Kubernetes。
单元测试补充 TokenRequest 失败/空 token 无回退、重新申请 JWT、无期限 lease 拒绝、
并发生命周期、退出清理及 manager 显式参数不受 BAO 环境身份覆盖。

## 公共基础设施与领域适配

Bao client/TLS 与认证生命周期已移至 `internal/infra/openbao`，与任何产品领域无关。
Kubernetes 读写客户端由 manager 管理，Secret 凭据适配器只接收直连的 `client.Reader`。
`adapter/openbao.Credentials` 仍属于 Database：它直接使用官方 KV v2 API，实现七键凭据、
UID 路径、CAS=0 与回读确认的领域合同，不把这些规则推广为公共存储语义。
后续供应用例需要的 repository 接口由领域侧按实际操作定义，不提前增加通用仓储抽象。
分层约定见[总体架构](../architecture/overview.md#进程内依赖边界)。

认证单元测试归公共 infra；真实认证与 Database 凭据读写的组合测试仍在 Database adapter。
Database 集成测试和 lint 入口同时覆盖 `internal/infra/...`，避免拆包导致 CI 漏测。

## 设计入口

- [系统规格](specification.md)：规范性行为与验收标准；
- [架构](architecture.md)与[API 合同](api-reference.md)；
- [领域模型](domain-model.md)与[Instance 领域规格](domain-instance.md)；
- [安全](security.md)、[迁移](migration.md)与[运维](operations.md)；
- [部署合同](deployment.md)与[开发测试合同](development.md)。

部署和开发文档描述目标合同，其中旧仓库专属的 Make target、脚手架版本和目录尚未接入 Ayatori；
在对应实现切片完成前，不应把其中命令理解为当前仓库已经可执行的入口。
