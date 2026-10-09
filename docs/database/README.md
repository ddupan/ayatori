# Database 模块

Database 是 Ayatori 首批实际产品领域之一。当前已包含三资源 API、分层绑定与 Instance 原生
管理能力观测、凭据准备及显式启用的角色/数据库创建、扩展安装、应用登录验证、凭据位置公布、Retain/Delete 回收与显式导入。

## 当前设计（2026-09-24）

采用 Instance → Database → Tenant 的资源与申请模型；Database 独立存在，支持显式导入、
Retain 后人工重新绑定与资源侧 Delete。撤销 PostgreSQL ownership registry 及任意 status
丢失自动恢复所有权的要求。未知同名资源或创建结果不确定时，清楚报告 Conflict 并人工处理。

依据 [ADR-0009](../decisions/0009-database-resource-and-claim.md)，当前合同见
[系统规格](specification.md)。下面的迁移来源与已存在代码不反向约束新设计。
registry adapter、专属迁移/测试及 Instance 的 registry 判定现已撤除；Instance 根据完整管理
能力观察直接判定 Ready。Database 资源与绑定、角色/凭据供应已接入，导入及回收仍未完成。wiki 同步位置见
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
- Instance checkpoint 不是外部事实；实际能力必须由 Reconciler 经 adapter 观察后交给领域对象判断。
- Instance 观察只检查供应前置条件，不单独授予 Tenant 所有权或外部写入权限；凭据准备
  另行校验双向绑定和保护，尚不表示完整 Database API 已经可用。

## 管理凭据与连接

`InstanceReconciler` 与 `DatabaseReconciler` 每轮通过 `ManagementSecrets` 直接从固定
controller namespace 的 Kubernetes Secret 读取管理用户名和密码（直连 API reader，Secret 内容
不进入共享 cache），再把当前凭据随每次调用传给 `postgresql.Instances`。adapter 不读取
Secret、不缓存观察结果，只按 Instance 名称复用 pgxpool：Instance UID、endpoint、凭据引用
或有效用户名/密码变化即关闭旧池并重新建连；Secret metadata 和无关字段变化不重建连接。
Secret 读取失败或字段缺失时释放旧连接，不继续使用缓存池。Forget/Close 只释放本地资源。

pgxpool 本身支持并发；登记锁只保护池的登记表，建连、查询和关闭都在锁外执行，不同
Instance 与 Database 的操作不会互相串行。并发建连时只保留一个登记的池。不在观察后再次
读取 Secret 比较：中途轮换由下一轮按新凭据重新建连处理，不构成跨 Kubernetes/PostgreSQL
的原子事务。

运行 `make test-database-integration` 验证真实 API server + 一次性 PostgreSQL；fixture 不接受外部
DSN，镜像固定摘要，使用随机本机回环端口并在退出时删除测试容器。覆盖缺失/错误凭据、RBAC、
namespace 边界、有效值轮换、metadata 无关变化、重建/重试、并发读取、Forget/Close
与 TLS DNS/IP SAN、错误 CA/主机名和禁止明文降级。CI 使用 Pod runner 执行，由 runner 提供
可用的 Docker，workflow 只做预检、不自行启动 daemon；不依赖 VM。普通 lint 之外还检查
integration 标签代码。

## 服务器 metadata 与扩展观测

`InspectManagement` 的同一条只读语句读取 `pg_catalog.current_setting('server_version')` 和
`pg_catalog.pg_available_extensions`，避免从已安装列表推断可用列表，且不依赖可修改的
`search_path`。查询失败丢弃整份结果并释放连接；成功返回空列表与尚未观察严格区分。
参考 PostgreSQL 的 [pg_available_extensions](https://www.postgresql.org/docs/18/view-pg-available-extensions.html)
与 [CREATE EXTENSION](https://www.postgresql.org/docs/18/sql-createextension.html) 合同：可用列表
表示服务器提供的扩展，不证明管理账号有安装权限，也不保证依赖和其他安装前提满足。

扩展列表作为原始事实返回，可交给 `instance.ObserveExtensionSupport` 和 Instance 的
`ObserveExtensions` 同轮次使用，不能持久化或跨轮缓存。metadata 读取不安装扩展、不初始化
registry、不设置 Ready，也不授予 Tenant 写权限。真实测试验证未安装扩展可被观察、名称保持
大小写、search_path 遮蔽不改变查询来源、低权限账号读取以及权限撤回失败与恢复。

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
superuser 属性、服务器可写状态、版本和扩展列表，作为原始事实返回；是否满足管理要求由
`instance.ManagementFacts.Checks` 判断。不创建探针数据库/角色，不初始化 schema。
只有非 superuser、具备两项原生属性且当前服务器/会话可写时，基础管理能力才通过。
供应操作不再逐次重查这些权限：权限丢失时 Instance 转为 NotReady，Database 供应随之停止。
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
固定位置、只在创建并回读成功后确认版本；位置写入后不可清空或修改，版本不可清空或回退，只随
连接信息改写前进。
`ReadConfirmed` 按已确认版本检查最新 KV 值，删除或版本漂移报 Conflict，不读取旧版本掩盖变化。
已确认凭据只有连接信息与 Instance 不一致时，`Update` 以确认版本为 CAS 写入下一版本并回读核对。
测试 token 只用于临时 fixture，不是生产静态 token 配置接口。

### 凭据准备闭环

配置认证后，显式设置 `--database-credential-mount` 启用准备；路径默认
`applications/<Database UID>`，前缀由 `--database-credential-prefix` 配置。
`DatabaseReconciler` 是 Database status 的唯一写入者。`domain/database` 的 Database 聚合
检查 Provision 来源、双向绑定 UID、申请目标、finalizer、删除/Released 状态及当前 Instance Ready，
并决定下一步；导入资源不执行本流程。顺序为固定位置 → 确认后端尚无凭据 → 创建并回读
→ 保存确认版本。每轮最多执行一次外部写入，保存后由状态变化或 30 秒依赖重查推动下一轮。
Reconciler 直接读写 API（resourceVersion 保护），负责 CR 与聚合之间的映射。领域阶段持久化在
`status.credentialPhase` 与 `status.resourcePhase`，下一轮只从这两个字段还原；Condition 的
Reason/Message 只用于呈现，保留既有拼写。后端依赖在 bootstrap 显式组装，不在 Reconcile 中构造。

不使用持久化的"创建中"检查点。创建结果丢失或确认保存失败时，下一轮会读到固定位置已有
值而 CR 没有确认版本，按 Conflict 停止，不认领、不重新生成密码；位置仍为空则正常重试。
外部写入成功后立即记录确认版本；记录遇到 resourceVersion 冲突时重新读取后再写，
之后若 Tenant/Instance 已变化，由资格检查停止后续供应。Conflict 保留首次原因，普通依赖
错误保留已确认版本；配置变化停止在原位置，不搬迁或覆盖。同一对象的协调由
controller-runtime 串行执行，多副本部署依靠既有 leader election；Bao 写入另有 CAS=0 保护。

成功只设置 `CredentialsReady=True`；只准备凭据时 Database Ready 保持 False/ProvisioningIncomplete，
Tenant 仍未完成交付。凭据准备本身不创建 PostgreSQL 资源；后续创建需另行启用下述用例。
回收见下文“释放与回收”，导入见“显式导入与重新开放”。

单元测试穷举前置条件；真实 API server + 隔离 Bao 验证创建、状态确认、幂等、重启、
依赖恢复、固定位置、不确定结果、确认保存失败和删除边界；实际 manager 验证 watch 驱动
及重启。该 fixture 只声明 Instance 前置 Ready，真实 PostgreSQL 管理能力由既有 Instance
集成测试覆盖，不把凭据准备验收当成实际建库或应用登录验收。

## PostgreSQL 资源创建闭环

在管理 Secret、OpenBao 认证和凭据准备配置之上，显式设置 `--database-provision-resources`
才启用外部角色/数据库写入；默认关闭。升级时先安装新 CRD，供应字段见
[API 合同](api-reference.md#当前-api-切片)。只处理 Provision 来源，导入资源不进入创建流程。

启用后同一个 `DatabaseReconciler` 在凭据确认可用之后继续资源供应，不另设 controller，
避免两个 worker 争写同一资源状态。所有 SQL 操作复用 `postgresql.Instances` 中 Instance 的
管理连接池，不再开一个管理连接池。执行前重新查询非 superuser 管理能力。
PostgreSQL adapter 只返回 catalog 事实；下一步由 Database 聚合按“CR 记录的 OID + 本轮
观察”决定。每一步都可重复执行，不需要事务或持久检查点：

1. 数据库不存在：以管理账号为临时 owner 建库，记录 `status.databaseOID`。空库在下一步
   之前没有可泄露的数据。
2. PUBLIC 仍有 CONNECT：撤销。
3. 角色不存在：最后创建无管理特权的 LOGIN 角色，记录 `status.roleOID`。此时 PUBLIC 已
   撤销且角色尚非 owner，应用账号无法连接。
4. owner 不是该角色或管理账号缺少 CONNECT：授予管理账号对角色的 SET 权限、移交 owner，
   再以新 owner 身份授予管理账号 CONNECT（移交会把原 owner 的 ACL 条目一并转给新 owner）。
5. Tenant `spec.extensions` 中服务器可用但本库未安装的扩展：管理账号连接目标库，以 owner
   身份 `CREATE EXTENSION`，扩展对象归应用角色。服务器不提供的扩展或非 trusted 扩展被拒绝时
   报 Unavailable 并写明原因；扩展只追加（CRD 拒绝删减），不卸载已安装的扩展。
6. 以上均满足后，用已确认的应用凭据实际登录目标库；成功即 Available，失败为 Unavailable。

OID 只保存在 CR，表示本次成功创建的回读身份，不是 registry 或认领机制。未知同名对象、
已确认对象消失/被重建、owner 既非管理账号也非已确认角色均报 Conflict。创建结果不确定时
不在当轮判定：下一轮看到未记录的同名对象即 Conflict，对象不存在则重试。controller 前提是
除不可恢复情况外不人工登录改库；人工授予的特权或成员关系无法区分有意或异常，因此不检测、
不修正。诊断包含 Database、Instance、实际 database/role、失败步骤；确认 OID 独立保留。
OID 不是跨集群/备份恢复的稳定身份，恢复后须人工核对，不能靠匹配 OID 推导管理权。

`ResourcesReady=True` 与 Database `Ready=True/DatabaseReady` 表示数据库、角色、权限与扩展
已收敛且应用凭据本轮登录成功；每轮（约 30 秒）重新验证，凭据失效即撤销就绪。Tenant 随之
可用，见下节。当前不轮换密码，也不撤销其他数据库的 PUBLIC 权限。管理员须保证共享实例中其他数据库的接入策略满足隔离要求。

真实 API server + PostgreSQL + OpenBao 测试覆盖实际密码登录、owner 建表、应用管理权限拒绝、
无关账号连接拒绝、重试/重启、权限恢复、同名冲突、角色重建、并发授权、创建结果丢失、
确认持久化失败、删除停止与实际 manager 的观察链路。保留外部操作/API 写入间的非原子边界，
不承诺控制面与数据库之间的事务或对任意管理员并发 DDL 的无损恢复。

遵循 PostgreSQL 官方的
[CREATE DATABASE 非事务与 owner 权限合同](https://www.postgresql.org/docs/17/sql-createdatabase.html)
和 [CREATEROLE 的成员授权](https://www.postgresql.org/docs/17/role-attributes.html)。
不新增 controller 通用生命周期框架，继续采用既有直接观察、resourceVersion 保护和条件报告。

## Tenant 凭据位置公布

Ayatori 的凭据交付止于 OpenBao（[ADR-0010](../decisions/0010-credential-delivery-ends-at-openbao.md)）：
控制面可能不运行 workload，Tenant namespace 只是租户边界，因此不创建 ExternalSecret 或 Secret。
`TenantReconciler` 在绑定完成后公布 Database 已确认凭据的 `credentialRef`、`credentialVersion`，
以及设置 `--database-credential-url-base` 时不含认证信息的 `credentialURL`。

Tenant 依次报告 `CredentialsPending`（凭据未确认）、`DatabaseNotReady`（已公布位置，等待资源、
扩展与应用登录验证），最后 `Ready=True/CredentialsAvailable`。消费方集群按公布的位置与版本自行
投射（例如固定该版本的 ExternalSecret）；其读取 policy 在接入时单独授予。

## 释放与回收

对照 PV/PVC：Tenant 删除是释放申请，回收策略属于 Database（默认 Retain）。

1. 删除 Tenant 后，`TenantReconciler` 先把绑定的 Database `status.phase` 记为 `Released`（保留
   `spec.tenantRef` 旧身份），再移除 Tenant finalizer。释放只需要 API 写入，不依赖 PostgreSQL 或
   OpenBao 在线。尚未产生 Database 的 Tenant 直接删除。
2. `Released` 之后 `reclaimPolicy` 由 CEL 固定；Released 不再供应，也不自动分配给新 Tenant。
3. **Retain**：保留数据库、角色与凭据，不撤销访问。管理员删除 Database CR 时只移除 finalizer，
   外部对象原样保留。重新开放见下节。
4. **Delete**（仅 Provision 来源；导入资源不隐含删除授权）：`DatabaseReconciler` 按已确认 OID
   逐步清理，每轮一步，对象已不存在则跳过，同名但身份不符则 Conflict 并保留 finalizer：
   - 阻止应用登录（`NOLOGIN`）、关闭连接入口、以 owner 身份终止该角色在本库的会话，再
     `DROP DATABASE`；终止的只是应用角色自己的会话，不需要 `pg_signal_backend`；
   - `DROP ROLE`，不使用 CASCADE/REASSIGN，角色仍拥有其他对象时转 Conflict 交给人工；
   - 永久删除 OpenBao 中该位置的全部版本与 metadata 并回读确认；
   - 删除 Database CR 并移除 finalizer。
5. 绑定中的 Database 被直接删除时保持 finalizer，报告需先删除 Tenant。

`DatabaseReconciler` 在未启用凭据准备时也会注册，只处理删除保护与 Retain 放行；Delete 回收需要
启用资源供应。真实 PostgreSQL/OpenBao 集成测试覆盖：应用会话被终止、库与角色删除、凭据永久
删除、Database 删除；Retain 保留外部对象；清理中途同名角色被重建时停止。

## 显式导入与重新开放

管理员以 `source: Import` 登记尚无记录的已有对象：`instanceRef`、`database`、`loginRole` 与
`spec.credentialRef`（已有七键凭据的位置，必须位于部署配置的 mount 与前缀内）。

Released 资源已具备导入成功的全部确认（OID、凭据位置与版本、Instance UID），不需要删除后重新
导入。对照 PV 清除 `claimRef`：管理员处置旧访问后清除 Released Database 的 `spec.tenantRef`，
这就是重新开放的显式授权；controller 按确认身份重新验证后回到 `Available`，可被新 Tenant 以
`databaseRef` 申请，新绑定写入新 Tenant 的 UID。仍保留旧 `tenantRef` 的 Released 记录只报告等待。
来源不变：动态供应的资源重新开放后仍可使用 Delete 回收。旧使用者已持有的密码不会因此失效；
如需改密，由管理员在清除 `tenantRef` 前人工处理（Bao 版本变化会被视为漂移并报 Conflict）。

导入与重新开放都由 `DatabaseReconciler`（需启用凭据准备与资源供应）验证，不再创建对象。
导入在全部通过后首次登记：

1. 读取 `spec.credentialRef` 最新版本，内容须与 database、loginRole 一致；
2. 数据库与角色存在，owner 就是 loginRole，管理账号对该库有 CONNECT；
3. Tenant 申请的扩展已安装（导入不安装扩展）；
4. 用该凭据实际登录目标库。

登记把本轮观察到的 OID、凭据版本与 Instance UID 写入 status，`status.phase=Available`、
`Ready=True`；导入本身就是管理员对该名称的显式授权。之后每轮按确认身份重新验证：对象重建、
owner 变化或凭据版本漂移报 Conflict。导入不改密码、owner 或权限，不装扩展；回收只支持 Retain。
管理员显式声明凭据位置即把该凭据交由本资源管理：登记后连接信息与 Instance 不一致时，与动态
供应相同以同一密码改写连接信息（首次登记仍只读，下一轮改写）。
Tenant 以 `databaseRef` 申请 Available 的导入资源，绑定后同样公布凭据位置。

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
