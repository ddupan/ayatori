# 部署与配置

> 本页迁入作为 Database 模块的目标部署合同。Ayatori manager flags、manifests 与发布装配尚未
> 实现；原设计行为保持有效，但当前仓库不能直接按本页完成部署。

| 项目 | 内容 |
| --- | --- |
| 状态 | Review |
| 环境 | homelab Kubernetes + 外部 PostgreSQL/OpenBao |
| 最后更新 | 2026-09-10 |

本文定义 v1alpha1 的运行依赖、启动顺序和部署级配置。当前 manifests 尚未实现这些
配置，示例是后续实现合同，不可直接用于现有脚手架。

## 依赖与顺序

1. 准备 PostgreSQL VM、持久盘、备份和网络入口。
2. 用 OpenBao PKI 签发 PostgreSQL server 证书，包含 Instance `host` 的 DNS SAN 与
   `hostaddr` 的 IP SAN；配置 PostgreSQL 强制 TLS。
3. 创建 PostgreSQL controller 管理 role 和管理 database 连接权限。
4. 在 OpenBao KV v2 写入管理 role 凭据。
5. 配置 OpenBao Kubernetes auth、controller policy 和面向 ESO 的读取 policy。
6. 安装 ESO，配置独立的管理凭据同步身份和租户凭据读取身份。管理员在 controller
   namespace 创建管理 ExternalSecret，确认管理 Secret 已同步；另创建供租户使用的
   `ClusterSecretStore`。
7. 创建公开 CA bundle ConfigMap，并挂载到 controller 和需要直接验证数据库的应用。
8. 部署 controller，再创建 Instance；等待 Ready 后才创建 Tenant。

任何一步都不得把真实密码、Token、kubeconfig 或 CA 私钥提交进 Git。

## Controller 配置合同

controller 使用以下 CLI flags。必填项缺失、路径无效或 duration 不为正数时，进程必须
在启动 manager 前失败；不得等到 reconcile 时才逐个资源报告配置错误。

| CLI flag | 必填/默认 | 说明 |
| --- | --- | --- |
| `--openbao-address` | 必填 | controller 可访问的 OpenBao API address |
| `--openbao-consumer-address` | 默认同 `--openbao-address` | 写入 Tenant status，必须能被预期外部消费者解析 |
| `--openbao-auth-mount` | `kubernetes` | Kubernetes auth mount 名称 |
| `--openbao-auth-role` | 必填 | controller ServiceAccount 对应 role |
| `--openbao-kv-mount` | `kv` | KV v2 mount；开发可显式用 `secret` |
| `--openbao-service-account-token-path` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Kubernetes auth 使用的投射 token 文件 |
| `--openbao-tenant-base-path` | 默认 `postgresql-tenants` | controller 专属 mount-relative 前缀 |
| `--external-secret-store-name` | 必填 | controller 创建的 ExternalSecret 固定引用 |
| `--postgresql-ca-bundle-path` | PostgreSQL TLS 模式必填 | 只读 PEM trust bundle，不含私钥 |
| `--reconcile-timeout` | `30s` | 单轮 reconcile 中外部操作的总期限，必须大于零 |

address 必须是绝对 `http` 或 `https` URL，不允许 userinfo、query 或 fragment，末尾 `/`
在规范化后移除。mount、auth mount 和 base path 都使用 mount-relative path 语义，不以
`/` 开头，不含空段、`.` 或 `..`；base path 还不得编码 KV v2 的 `data`/`metadata`
API 层。生产环境的 `--openbao-address` 必须使用 HTTPS；HTTP 只用于明确的开发 fixture。

Tenant 路径固定推导为 `<base-path>/<namespace>/<metadata.name>`。namespace/name 都已通过
Kubernetes 名称校验，因此不再允许 CR 提供任意路径。KV v2 API URL 使用 consumer
address 拼为 `<address>/v1/<mount>/data/<base-path>/<namespace>/<metadata.name>`。

base path 必须是合法 mount-relative path，不以 `/` 开头且不包含空段、`.`、`..`、
`data`/`metadata` API 层。ExternalSecret 固定命名为
`<instanceRef>-<metadata.name>-postgresql`；目标 Secret 可由 Tenant 指定，但名称必须
满足 Kubernetes Secret 名称校验，不限制命名内容，默认与 ExternalSecret 同名。

配置变化不得隐式迁移既有凭据。修改 KV mount/base path 或 consumer address 前必须
停止 controller、评估现有 Tenant，并走明确迁移；实现应把 mount/base path 视为安装
身份的一部分并在 registry 留存，以便检测错误配置。

## PostgreSQL 管理 role

生产部署禁止使用 superuser。管理 role 至少需要：

- 连接管理 database、读取必要 catalog；
- 创建/修改受管 login role；
- 创建 database 并指定 owner；
- 撤销 `PUBLIC` CONNECT、授予租户 role CONNECT；
- 连接租户 database 并创建实例实际支持、租户申请的 extension；
- 创建和维护 controller 专属 registry schema/table；
- `Delete` 时禁止连接、终止目标 database session、删除已验证归属的 database/role。

部分 PostgreSQL 操作天然要求较高权限，尤其终止其他 session 和安装某些 extension。
应优先使用 PostgreSQL 预定义角色、受控 SECURITY DEFINER 管理函数或限定数据库的
授权；任何不得不使用 superuser 的 extension 都必须按实例单独记录，不得扩大默认
controller 权限。最终可执行 SQL grant 将随 PostgreSQL adapter 集成测试固化。

## OpenBao 与 ESO

controller policy 仅允许在固定 tenant base path 下 create/read/update/delete KV v2
data 和 metadata，Delete 必须能永久删除全部版本及 metadata；不读取管理凭据路径。

管理凭据由管理员维护的 ExternalSecret 同步到 controller namespace；其 ESO 身份
只读对应管理路径，不能供 Tenant 使用。租户 ESO 身份只读 tenant base path，不得
读取 PostgreSQL 管理凭据。controller 不创建或修改管理 ExternalSecret／Secret。
`ClusterSecretStore` 由平台管理员创建，controller 只引用，不创建或修改 Store。
controller 创建的 ExternalSecret 与 Tenant 同 namespace，并设置 ownerReference；目标
Secret 包含固定七键：`username`、`password`、`database`、`host`、`hostaddr`、`port`、
`sslmode`。

## Kubernetes RBAC

- controller 可读/写 Instance、Tenant 的 status/finalizer 和 Event。
- controller 可在 Tenant namespace 创建、读取、更新、删除 ExternalSecret，并只读检查
  对应 Secret 是否完成投射。
- namespace 用户可以管理本 namespace Tenant，但不能管理 Instance、Store、controller
  配置或其他 namespace 的 ExternalSecret。
- controller 只在自身 namespace 读取所引用管理 Secret 的 data，不获得跨 namespace
  的管理 Secret 读取权限。Instance 不允许自选 Secret namespace。
- 对应用目标 Secret，controller 无需读取 data；验证登录使用从 OpenBao 读取的应用
  凭据，只检查 Secret 存在性和 ESO 状态。

## 升级与回滚

v1alpha1 尚不承诺跨版本转换。升级前备份 CR、PostgreSQL registry 和 OpenBao metadata，
先在隔离 Kind 环境运行 E2E。禁止在同一组 CR 上同时运行两个 controller 版本。若新版本
在执行任何破坏性迁移前失败，可回滚镜像；涉及 API/storage 或 registry schema 迁移时，
必须先写独立升级规格和回滚步骤。

## 上线验证

```text
PostgreSQL TLS 与备份验证
  -> OpenBao auth/policy 验证
  -> ClusterSecretStore Ready
  -> controller Ready/leader elected
  -> Instance Ready
  -> 测试 Tenant Ready
  -> DNS host 与 IP hostaddr 分别登录
  -> 删除测试 Tenant 并验证所选策略
```

生产 homelab 上线前还必须完成 [`security.md`](security.md) 的权限检查和
[`operations.md`](operations.md) 的备份/逃生检查。
