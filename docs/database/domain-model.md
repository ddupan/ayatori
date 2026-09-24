# Database 领域模型

状态：资源模型已确认，字段与绑定协议待细化。日期：2026-09-24。
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

Tenant 表达申请与交付要求；显式选择已有资源不自动授权使用。绑定后检查数据库满足要求、
应用凭据可登录且 ESO 投射完成，才可 Ready。Tenant 删除意味着释放使用关系。

LoginRole 与 CredentialLocation 的生命周期必须随独立资源保留，不能因为 Tenant 消失就
失去定位或未经授权被删除；具体字段归属及导入时的管理范围需继续评审。不要因此新增 Role、
Credential 或 Claim CRD。当前首版仍是单 database + 单 login owner。

## 生命周期与恢复

- 动态创建与显式导入最终形成同一种 Database 资源，但导入本身不允许改密、改 owner 或删除。
- Retain 后 Database 保持 Released 与旧绑定身份，人工确认数据、权限和凭据后才可重新绑定。
- 回收策略属于资源侧；Tenant 与 Database 不是可随申请级联 GC 的父子关系。
- 绑定 UID 防止同名新申请继承权限。双向记录的单边写入不代表绑定完成。
- 普通失败按 reconcile 重试；可靠确认的步骤幂等继续；不确定创建/未知同名对象报告 Conflict。
- Kubernetes status 是持久进度和观察，不是外部事实，也不是 controller 内存。
  不引入“status 任意丢失后自动恢复所有权”的附加要求。

## 分层

| 层 | 责任 |
| --- | --- |
| 领域 | 值、身份、允许动作、不变量、完成与冲突判定；不做 IO |
| 应用 | 装载记录与事实、协调 API 更新和 adapter、回读、交回领域判定 |
| controller | watch/调度、映射、conditions/status/finalizer；不重写领域规则 |
| adapter | Kubernetes、PostgreSQL、OpenBao、ESO 的具体访问与安全错误分类 |
| 装配 | 客户端与成熟连接池的生命周期，不是领域状态 |

不引入通用 Repository CRUD、跨系统 Unit of Work、事务队列或第二套 phase 存储。
resourceVersion 解决 API 对象并发更新，不宣称 PostgreSQL 与 Kubernetes 原子提交。

## API 切片前需明确

- Database 的 scope、引用格式、谁可以预留/绑定/释放，以及绑定字段和更新顺序。
- 资源侧回收策略与 Tenant/Database finalizer 配合。
- 导入时角色/凭据的管理范围，稳定凭据定位、旧使用者撤权及新投射授权。
- 绑定/导入同一实际目标的重复声明如何拒绝，且不引入 registry。
- 管理员确认冲突、解除旧绑定的具体可审计操作入口。

这些细节不阻止已确认的三资源设计，但必须先于对应 API 与生命周期实现获得评审。
