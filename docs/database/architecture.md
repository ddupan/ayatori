# 系统架构

本文是已批准 [`specification.md`](specification.md) 的架构视图。规范定义外部行为，
本文解释组件边界；二者冲突时以规范为准。Ayatori Database 模块目前只有首批领域模型，尚未
注册 API 或接入运行链路。

## 组件与数据流

```text
GitOps / kubectl / Terraform / Backstage
                    |
                    v
          Kubernetes API (CRD)
                    |
                    v
       Ayatori Database controller
          |          |          |
          v          v          v
 PostgreSQL DBMS  OpenBao KV  ExternalSecret
 catalog+registry              |
                               v
                         Kubernetes Secret
```

- Kubernetes `spec` 保存期望状态；`status.phase` 保存 controller 状态机 checkpoint，
  其他 status 字段保存可重建的观察结果。整个 status 都必须能由外部事实保守恢复。
- PostgreSQL catalog 保存 database、role、grant 和 extension 的实际状态。
- 两个 CR 的 `status.phase` 是 controller 状态机的权威 checkpoint。
- PostgreSQL 管理 database 中的 controller registry 只负责所有权、安装身份和保留标记。
- OpenBao KV v2 是应用凭据的事实来源。
- External Secrets Operator（ESO）读取 OpenBao，并创建应用使用的 Kubernetes Secret。

controller 不运行 PostgreSQL/OpenBao，不管理 VM、存储、备份或 OpenBao PKI，也不直接
把明文凭据写入 Kubernetes API。

## 资源模型

`PostgreSQLInstance` 是 cluster-scoped，由平台管理员创建，描述外部 PostgreSQL 的
DNS host、IP host address、端口、管理 database、TLS 模式和管理 Secret 引用。
实际可安装扩展由应用层查询后交给领域对象判定，v1alpha1 不实现管理员 allowlist。

管理连接使用管理员维护的 ExternalSecret 经 ESO 同步到 controller namespace 的
Secret；Instance 只选择 Secret 名称与字段，controller 只读，不直接从 Bao 获取
管理凭据。Tenant 凭据的创建、读取与销毁仍由 controller 直接访问 Bao。

`PostgreSQLTenant` 是 namespaced。一个 Tenant 对应一个 database、一个同时作为 owner
的 login role、一组只允许追加的 extension、一个由 controller 推导的 OpenBao KV
记录，以及同 namespace 的 ExternalSecret 和目标 Secret。

Tenant namespace 只提供 Kubernetes RBAC 和身份边界。database 与 role 名称在一个
Instance 内仍然全局唯一。

## Reconcile 与所有权

系统采用最终一致性，不在 Kubernetes、PostgreSQL、OpenBao 和 ESO 之间假装存在分布式
事务。每个外部写入前在 CR status 记录阶段，执行幂等操作，回读验证，再推进阶段：

```text
Planned -> CredentialCreated -> RoleCreated -> DatabaseCreated
        -> ExternalSecretCreated -> CredentialProjected -> Ready
```

controller 每轮同时读取 CR、registry、PostgreSQL catalog、OpenBao metadata 和 ESO
投射状态。`status.phase` 是状态机 checkpoint，但不能替代外部回读；丢失或与事实冲突
时必须保守重建/纠正。`metadata.generation` 只表示 spec 修改；Condition 的
`observedGeneration` 表示该版本是否已经完成一次有结论的协调。

所有权使用 Instance UID、Tenant UID 与 namespace/name 验证。database/role COMMENT
可以辅助排障，但不能代替 registry。未知资源只报告 `Conflict`，不得修改、接管或
删除。Retain 后用相同名称重建 CR 会获得新 UID，因此仍然冲突。

## 创建与删除边界

创建时先校验全部输入和冲突，再生成一次密码并写入 OpenBao，随后创建 role、database、
extension 和 ExternalSecret。只有 ESO 已投射 Secret 且应用凭据实际登录成功，Tenant
才可 Ready。

`Retain` 是默认删除策略，只移除 Kubernetes 管理关系并保留外部资源。显式 `Delete`
使用 finalizer，在重新验证所有权后依次删除 ExternalSecret/Secret、连接、database、
role、OpenBao KV 历史和 registry。详细恢复与逃生步骤见
[`operations.md`](operations.md)。

## 网络与 TLS

Instance 同时公布 DNS `host` 和 IP `hostaddr`。PostgreSQL server 证书必须包含对应的
DNS SAN 和 IP SAN，消费者自行选择可达目标，并可使用 `verify-full` 验证。OpenBao PKI
持有 CA 私钥并签发服务端证书；controller 只挂载公开 CA bundle。

OpenBao 的 controller 内部地址和外部消费者地址可以不同。Tenant status 同时提供目标
Kubernetes Secret reference 和不含认证信息的 OpenBao KV v2 API URL。

## 文档入口

- API 字段与 Condition：[`api-reference.md`](api-reference.md)
- 安装、依赖和配置：[`deployment.md`](deployment.md)
- 本地与 CI 测试：[`development.md`](development.md)
- 安全模型与最小权限：[`security.md`](security.md)
- 现有数据库迁移：[`migration.md`](migration.md)
- 日常排障和删除逃生：[`operations.md`](operations.md)
