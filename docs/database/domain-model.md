# Database 领域模型

状态：资源模型已确认，字段与绑定协议待细化。日期：2026-09-25。
行为以 [系统规格](specification.md) 为准；决策依据见
[ADR-0009](../decisions/0009-database-resource-and-claim.md)。

## 统一语言与关系

| 术语 | 含义 |
| --- | --- |
| Instance | 平台登记的 PostgreSQL 资源来源与管理入口 |
| Database | 独立存在的数据库资源，保存目标、管理范围、绑定与回收策略 |
| Tenant | 用户对数据库的申请与使用合同 |
| Binding | Database 与 Tenant 的排他关联，不是独立 Claim 或 registry |
| LoginRole | 当前单数据库场景中兼任 owner 的登录角色 |
| CredentialLocation | 与资源生命周期一致的凭据定位，不是密码 |
| CredentialProjection | 面向当前使用者的凭据投射要求与观察 |

Instance 一对多 Database；每个 Database 同时零或一个 Tenant，Tenant 最多一个 Database。
Instance 不持有全部资源的内存集合。三者以引用关联，操作一个资源无需加载整个实例集合。

Instance 与 Database 是集群级资源，Tenant 位于 namespace。Tenant 按名称引用 Database，
Database 记录所绑定 Tenant 的 namespace/name/UID。Database 不属于应用 namespace；
平台管理员管理资源及回收策略，普通申请者不能自行将 Released 资源重新开放。

Database 的 instanceRef 表达资源归属，手工登记时也必须提供，不从 Tenant 反推。
Tenant 动态申请才选择 Instance；引用已有 Database 时使用资源声明的 Instance。
释放使用绑定不改变 Database 的实例归属，修改引用不能实现外部数据库迁移。

Database 不再是 Tenant 内的无独立生命周期描述。原 OwnershipClaim 不再作为独立领域能力：
排他绑定是资源自身的不变量，Kubernetes 保存记录，不另建 PostgreSQL 所有权存储。

## 职责

Instance 只接收观察并判断当前连接、metadata、管理权限和扩展支持，不访问 IO、不初始化
registry。管理凭据来源和连接刷新由应用层协调，连接池由 pgxpool 实现。见
[Instance 规格](domain-instance.md)。

Database 保护目标和管理范围、排他绑定、导入验证与 Retain/Delete 规则。资源首次外部操作前
必须已有持久记录；完成记录与外部存在性分别检查。数据库名称不是归属证明。

Tenant 表达申请与交付要求；可显式申请未绑定且可用的资源，不增加反向授权名单。
绑定后检查数据库满足要求、
应用凭据可登录且 ESO 投射完成，才可 Ready。Tenant 删除意味着释放使用关系。

第一版 Database 的生命周期边界包含一个 database、一个兼任 owner 的 LoginRole 及其
应用凭据。LoginRole 与 CredentialLocation 随 Database 保留，不能因为 Tenant 消失就
失去定位或未经授权被删除；导入时的管理授权与具体字段仍需评审。不新增 Role、Credential
或 Claim CRD，也不预留多账号集合；若出现一库多账号的实际需求，再通过后续 API 版本演进。

## 生命周期与恢复

- 动态创建与显式导入最终形成同一种 Database 资源，但导入本身不允许改密、改 owner 或删除。
- Retain 后 Database 保持 Released 与旧绑定身份，人工确认数据、权限和凭据后才可重新绑定。
- 回收策略属于资源侧；Tenant 与 Database 不是可随申请级联 GC 的父子关系。
- 回收策略默认 Retain，进入删除流程前可由资源管理者修改，进入后固定；Delete 无额外审批。
- 动态凭据位置按 Database UID 确定，导入显式关联已有凭据；Released 不自动改密。
- 绑定 UID 防止同名新申请继承权限。双向记录的单边写入不代表绑定完成。
- 动态 Database 名称由 Tenant UID 确定；先持久化资源侧 Tenant 引用，再更新 Tenant status
  的 Database 引用。后一写入失败由 reconcile 核对身份后补齐，不回滚资源侧记录；
  其他 Tenant 已占用则报冲突。双向一致后才供应或交付，绑定不等于 Ready。
- 普通失败按 reconcile 重试；可靠确认的步骤幂等继续；不确定创建/未知同名对象报告 Conflict。
- Kubernetes status 是持久进度和观察，不是外部事实，也不是 controller 内存。
  不引入“status 任意丢失后自动恢复所有权”的附加要求。

## 分层

| 层 | 责任 |
| --- | --- |
| 领域 | 聚合与值对象：身份、允许动作、不变量、下一步与完成/冲突判定；不做 IO |
| controller | 每个 Kind 一个 Reconciler，即 Kubernetes adapter 兼用例：直接读写 CR、CR 与聚合映射、编排外部操作、呈现 conditions/status/finalizer |
| 后端 adapter | PostgreSQL、OpenBao 等外部系统的薄访问层：只返回事实，把后端错误翻译成领域结果分类 |
| infra / 装配 | 客户端与成熟连接池的生命周期，不是领域状态 |

判断放在哪里：

- 不需要 IO、只凭值就能决定的（资格、下一步、状态迁移、冲突与否）放在聚合方法里。
  Reconciler 中出现直接读领域字段做分支（如 `if state.RoleOID == 0`）即应移入聚合。
- 读写顺序、外部操作前后回查、把结果交回聚合由 Reconciler 负责。
- 外部系统错误码分类（确定未执行 / 结果不确定 / 冲突）和 SQL/CR 字段映射属于 adapter。
- 输入合法性由 CRD schema/CEL 保证，必要时用 webhook；代码不重复校验 API 已拒绝的输入。

只为可能出现新实现的后端定义接口，接口在 controller 中按使用方需要声明；
Kubernetes 侧不再包一层 Repository/Presenter 接口，`client.Client` 本身就是端口。
不引入通用 Repository CRUD、跨系统 Unit of Work、事务队列或第二套 phase 存储。
resourceVersion 解决 API 对象并发更新，不宣称 PostgreSQL 与 Kubernetes 原子提交。

### 命名

| 对象 | 规则 | 例子 |
| --- | --- | --- |
| Reconciler | `<Kind>Reconciler`，文件 `<kind>_controller.go`；只写本 Kind 的 status | `DatabaseReconciler` |
| 领域聚合 | 包名为名词，类型与包同名；`New…` 新建，`Reconstitute` 从持久状态加载 | `instance.Instance`、`database.Database` |
| 后端接口 | 在 controller 中声明，以 `Backend` 结尾，只列使用方需要的方法 | `CredentialBackend`、`PostgreSQLBackend` |
| 后端 adapter | 按所代表的外部对象命名 | `openbao.Credentials`、`postgresql.Connector` |
| 结果分类 | 影响状态迁移，放在领域包中，adapter 只依赖领域 | `database.ErrUnavailable`、`ErrUncertain`、`ErrConflict` |

不使用 `Service`、`Reconciliation`、`Preparation`、`Resources`、`Record`、`Presenter`、
`Result` 等含义模糊的后缀。

### 迁移状态

Database 与 Instance 链路已按上述规则实现：`domain/database` 聚合合并供应资格、凭据准备与
资源供应状态；`DatabaseReconciler`、`InstanceReconciler` 直接读写 CR 并读取管理 Secret；
`postgresql.Instances` 按 Instance 复用连接池，只返回事实。Tenant 绑定链路仍是旧的
`application.BindingService` + `adapter/kubernetes` Resources/Presenter 结构，将在后续切片按同一
规则迁移，迁移前不要在旧结构上扩展新能力。

## API 切片前需明确

- 引用与绑定字段的最终格式及校验、管理员与 controller 的权限落实。
- 资源侧回收策略与 Tenant/Database finalizer 配合。
- 导入时角色/凭据的管理范围及关联字段、旧使用者撤权及投射清理。
- 绑定/导入同一实际目标的重复声明如何拒绝，且不引入 registry。
- 管理员确认冲突、解除旧绑定的具体可审计操作入口。

这些细节不阻止已确认的三资源设计，但必须先于对应 API 与生命周期实现获得评审。
