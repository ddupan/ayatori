# 部署与配置

> 本页区分已实现的 Instance 观测、认证、凭据准备和资源创建配置，与尚未接入的完整供应/交付合同。
> 完整 Database 服务仍不可部署使用；当前可执行入口见 [模块说明](README.md)。

| 项目 | 内容 |
| --- | --- |
| 状态 | Review |
| 环境 | homelab Kubernetes + 外部 PostgreSQL/OpenBao |
| 最后更新 | 2026-09-25 |

本文定义 v1alpha1 的运行依赖、启动顺序和部署级配置。Instance 观测已接入 manager；
OpenBao 认证可显式启用；ESO 与完整供应装配仍是后续实现合同。

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

当前 manager 支持 `--database-secret-namespace`（默认 `POD_NAMESPACE`，为空则停用
Instance 观测）与 `--database-root-cert`（公开 PostgreSQL CA PEM 路径）。Deployment
通过 downward API 获取 namespace，Secret 权限由该 namespace 的 Role 授予。

以下表格区分已实现的认证参数与尚待实现的供应/交付参数。
必填项缺失、路径无效或 duration 不为正数时，进程必须在启动 manager 前失败；
不得等到 reconcile 时才逐个资源报告配置错误。

| CLI flag | 必填/默认 | 说明 |
| --- | --- | --- |
| `--openbao-address` | 已实现，默认空 | HTTPS API 地址；为空时关闭认证会话 |
| `--openbao-consumer-address` | 默认同 `--openbao-address` | 写入 Tenant status，必须能被预期外部消费者解析 |
| `--openbao-auth-mount` | 已实现，`kubernetes` | Kubernetes auth mount 名称 |
| `--openbao-auth-role` | 已实现，启用时必填 | OpenBao 登录 role |
| `--openbao-ca-cert` | 已实现，默认系统信任根 | OpenBao 公开 CA PEM 路径 |
| `--database-credential-mount` | 已实现，默认空 | 显式设置后启用应用凭据准备，要求已配置 OpenBao 认证 |
| `--database-provision-resources` | 已实现，默认 false | 启用角色/数据库及 ACL 创建；要求管理 Secret namespace、凭据 mount 和认证配置，先升级 CRD |
| `--openbao-service-account-namespace` | 已实现，启用时必填 | TokenRequest 目标 SA 的固定 namespace |
| `--openbao-service-account-name` | 已实现，启用时必填 | TokenRequest 目标 SA 名称 |
| `--openbao-token-audience` | 已实现，`openbao` | SA JWT audience，必须匹配 OpenBao role |
| `--database-credential-prefix` | 已实现，默认 `applications` | Database UID 路径的 mount-relative 前缀；不迁移已有位置 |
| `--external-secret-store-name` | 必填 | controller 创建的 ExternalSecret 固定引用 |
| `--database-root-cert` | 已实现 | 只读 PEM trust bundle，不含私钥；沿用 Instance 连接配置 |
| `--reconcile-timeout` | `30s` | 单轮 reconcile 中外部操作的总期限，必须大于零 |

address 必须是绝对 `http` 或 `https` URL，不允许 userinfo、query 或 fragment，末尾 `/`
在规范化后移除。mount、auth mount 和 base path 都使用 mount-relative path 语义，不以
`/` 开头，不含空段、`.` 或 `..`；base path 还不得编码 KV v2 的 `data`/`metadata`
API 层。生产环境的 `--openbao-address` 必须使用 HTTPS；HTTP 只用于明确的开发 fixture。

### 集群内与 systemd 共用 Kubernetes 认证

controller 是 API 客户端，不要求部署为 Pod。OpenBao 认证直接复用 manager 已加载的
Kubernetes 配置：集群外可用标准 `--kubeconfig`（或 `KUBECONFIG`），集群内可使用
in-cluster 配置。禁止另要求 `/var/run/secrets/.../token` 文件或解析 kubeconfig 中的 bearer token。
每次 Bao 登录前通过 TokenRequest 申请新的短期 SA JWT，Kubernetes JWT 与 Bao token 的
生命周期分别由 API 签发和官方 SDK 续期管理；不把 kubeconfig 本身当成永久有效凭据。

管理员为 controller 的实际 Kubernetes 身份授予目标 namespace 内
`create serviceaccounts/token`，用 `resourceNames` 限定目标 SA；示例见
[最小 RBAC](../../config/samples/database_openbao_auth_rbac.yaml)。集群外 RoleBinding subject
对应 kubeconfig 的用户/组，集群内可绑定 manager SA；登录目标 SA 可以独立于调用者身份。
controller 不创建 SA、Role/RoleBinding，也不向自己授予权限。不要求 `get secrets` 来获取 JWT。
OpenBao role 还需限制 SA 名称、namespace 与 audience，TokenReview reviewer 身份由管理员配置。

示例启动参数（仅示意，不包含真实 kubeconfig 或凭据）：

```sh
manager --kubeconfig=/etc/ayatori/controller.kubeconfig \
  --database-secret-namespace=ayatori-system \
  --openbao-address=https://bao.example:8200 \
  --openbao-auth-role=ayatori-database \
  --openbao-service-account-namespace=ayatori-system \
  --openbao-service-account-name=database-openbao-login
```

这里的认证成功只开放 manager readiness，不代表 Database 已具备供应或交付能力。
实例管理 Secret 的 namespace 同样由参数指定，systemd 模式不依赖 `POD_NAMESPACE` 环境变量。

Tenant 不能选择任意凭据路径。凭据必须能随 Database 保留并安全交付给被授权的新 Tenant；
原 `<base-path>/<namespace>/<metadata.name>` 定位规则不再直接作为新 API 合同。
动态供应位置使用 `<base-path>/<Database UID>`；导入使用 Database 的显式 credentialRef，
不要求搬迁已有凭据。供应流程须先记录原 mount/path，不能在配置变化后重新推导位置。
consumer URL 仍使用无认证信息的 KV v2 API URL。

base path 必须是合法 mount-relative path，不以 `/` 开头且不包含空段、`.`、`..`、
`data`/`metadata` API 层。ExternalSecret 固定命名为
`<instanceRef>-<metadata.name>-postgresql`；目标 Secret 可由 Tenant 指定，但名称必须
满足 Kubernetes Secret 名称校验，不限制命名内容，默认与 ExternalSecret 同名。

配置变化不得隐式迁移既有凭据。修改 KV mount/base path 或 consumer address 前必须
停止 controller、评估现有 Database 与绑定，并走明确迁移。资源记录应能定位原凭据，
不能根据新部署参数静默切换；不再使用 registry 保存安装身份。

## PostgreSQL 管理 role

生产部署禁止使用 superuser。管理 role 至少需要：

- 连接管理 database、读取必要 catalog；
- 创建/修改受管 login role；
- 创建 database 并指定 owner；
- 撤销 `PUBLIC` CONNECT、授予租户 role CONNECT；
- 连接租户 database 并创建实例实际支持、租户申请的 extension；
- `Delete` 时禁止连接、终止目标 database session、删除已验证归属的 database/role。

部分 PostgreSQL 操作天然要求较高权限，尤其终止其他 session 和安装某些 extension。
第一版使用原生非 superuser 的 CREATEDB/CREATEROLE 方案，不引入 SECURITY DEFINER
管理接口。对自行创建的 owner 显式建立 SET membership，再以 owner 管理 ACL 与扩展；
已有对象仍须逐资源核实授权，不能凭基础属性接管。需要 superuser 的扩展不能扩大 controller
权限。真实权限矩阵见 [Instance 原生管理观测](README.md#instance-原生管理观测)。

## OpenBao 与 ESO

当前凭据准备只需固定前缀下 KV v2 data 的 create/read/update 权限，不需要 metadata 或
delete 权限，也不读取管理凭据路径。示例（`secret` 为测试 mount，需替换为实际配置）：

```hcl
path "secret/data/applications/*" {
  capabilities = ["create", "read", "update"]
}
```

在上面的认证启动参数基础上增加 `--database-credential-mount=secret` 即启用凭据准备；
需要不同前缀时同时配置 `--database-credential-prefix` 和对应 policy。仅设置
`--openbao-address` 不会启用凭据创建。缺少认证配置或非法 mount/prefix 在启动时失败。
新字段 CRD 必须先升级再启用 controller，否则 API pruning 会使确认记录无法保存。
未来 Delete 清理才需要永久删除全部版本及 metadata 的权限，本切片不提前授予。

管理凭据由管理员维护的 ExternalSecret 同步到 controller namespace；其 ESO 身份
只读对应管理路径，不能供 Tenant 使用。租户 ESO 身份只读 tenant base path，不得
读取 PostgreSQL 管理凭据。controller 不创建或修改管理 ExternalSecret／Secret。
`ClusterSecretStore` 由平台管理员创建，controller 只引用，不创建或修改 Store。
controller 创建的 ExternalSecret 与 Tenant 同 namespace；其 ownerReference 和 Retain 时的
保留/清理须与凭据交付协议一起确定，不能把投射关系等同于 Database 的 GC 关系。目标
Secret 包含固定七键：`username`、`password`、`database`、`host`、`hostaddr`、`port`、
`sslmode`。

## Kubernetes RBAC

- controller 按用例读/写 Instance、Database、Tenant 及其 status/finalizer 和 Event。
- Database 不设置随 Tenant 级联删除的 ownerReference；导入、预留、回收和重新绑定授权限管理员。
- controller 可在 Tenant namespace 创建、读取、更新、删除 ExternalSecret，并只读检查
  对应 Secret 是否完成投射。
- namespace 用户可以管理本 namespace Tenant，但不能管理 Instance、Store、controller
  配置或其他 namespace 的 ExternalSecret。
- controller 只在自身 namespace 读取所引用管理 Secret 的 data，不获得跨 namespace
  的管理 Secret 读取权限。Instance 不允许自选 Secret namespace。
- 对应用目标 Secret，controller 无需读取 data；验证登录使用从 OpenBao 读取的应用
  凭据，只检查 Secret 存在性和 ESO 状态。

## 升级与回滚

v1alpha1 尚不承诺跨版本转换。升级前备份 CR/绑定、PostgreSQL 数据与 OpenBao，
先在隔离 Kind 环境运行 E2E。禁止在同一组 CR 上同时运行两个 controller 版本。若新版本
在执行任何破坏性迁移前失败，可回滚镜像；涉及 API/storage 或凭据定位迁移时，
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
