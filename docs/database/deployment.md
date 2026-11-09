# 部署与配置

> 本页是 manager 的配置合同。Dev/Prod 的实际部署由 Chef cookbook `homelab_ayatori` 完成
> （环境声明为 homelab-infra `infrastructure/chef/policies/ayatori-<env>.rb`），见
> [deploy/README.md](../../deploy/README.md) 与 [ADR-0013](../decisions/0013-api-only-control-plane-on-lxc.md)。

| 项目 | 内容 |
| --- | --- |
| 状态 | Accepted |
| 环境 | LXC 上的 API-only k0s + 外部 PostgreSQL/OpenBao |
| 最后更新 | 2026-10-04 |

本文定义 v1alpha1 的运行依赖、启动顺序和部署级配置。Instance 观测随 manager 启用；
OpenBao 认证、凭据准备与资源供应需显式启用；Ayatori 不投射凭据，消费方集群自行投射（[ADR-0010](../decisions/0010-credential-delivery-ends-at-openbao.md)）。

## 依赖与顺序

1. 准备 PostgreSQL VM、持久盘、备份和网络入口。
2. 用 OpenBao PKI 签发 PostgreSQL server 证书，包含 Instance `host` 的 DNS SAN；配置
   PostgreSQL 强制 TLS。消费者若直接以 `hostaddr` 作为连接主机，证书还需包含该 IP SAN。
3. 创建 PostgreSQL controller 管理 role 和管理 database 连接权限。
4. 在 OpenBao KV v2 写入管理 role 凭据。
5. 配置 OpenBao Kubernetes auth 与 controller policy；消费方集群读取应用凭据的 policy 由其接入时单独授予。
6. Chef converge 控制面：k0s、ESO、Flux 与 manager（podman quadlet，无 Pod）。Flux 从 release tag
   同步 CRD（单独的 Kustomization，关闭 prune）、RBAC 以及管理凭据的 SecretStore/ExternalSecret
   与 Instance（[postgresql.yaml](../../deploy/flux/control-plane/postgresql.yaml)）；Tenant/Database
   由部署方 `gitops` 目录经 Kustomization `flux-system/gitops-tenants` 声明。
7. manager 以 `--database-root-cert` 读取公开 CA；Instance Ready 后才创建 Tenant。

任何一步都不得把真实密码、Token、kubeconfig 或 CA 私钥提交进 Git。

## Controller 配置合同

`--database-secret-namespace`（默认取 `POD_NAMESPACE` 环境变量，为空则停用 Instance 观测）
在生产中显式设置，因为 manager 以 quadlet 运行、没有 Pod；Secret 权限由该 namespace 的 Role 授予。

依赖参数缺失、组合不完整（如启用供应而未配置凭据 mount）或地址/路径非法时，manager 在启动前
失败，不等到 reconcile 时才逐个资源报告。

| CLI flag | 必填/默认 | 说明 |
| --- | --- | --- |
| `--openbao-address` | 默认空 | HTTPS API 地址；为空时关闭认证会话 |
| `--openbao-auth-mount` | `kubernetes` | Kubernetes auth mount 名称 |
| `--openbao-auth-role` | 启用时必填 | OpenBao 登录 role |
| `--openbao-ca-cert` | 默认系统信任根 | OpenBao 公开 CA PEM 路径 |
| `--database-credential-mount` | 默认空 | 显式设置后启用应用凭据准备，要求已配置 OpenBao 认证 |
| `--database-provision-resources` | 默认 false | 启用角色/数据库及 ACL 创建；要求管理 Secret namespace、凭据 mount 和认证配置，先升级 CRD |
| `--openbao-service-account-namespace` | 启用时必填 | TokenRequest 目标 SA 的固定 namespace |
| `--openbao-service-account-name` | 启用时必填 | TokenRequest 目标 SA 名称 |
| `--openbao-token-audience` | `openbao` | SA JWT audience。OpenBao 不持有 reviewer JWT 时登录 JWT 兼作 TokenReview 凭据，必须设为 API audience（k0s 为 `https://kubernetes.default.svc`），cookbook 已如此配置 |
| `--leader-election-namespace` | 默认空 | 集群外运行时选主 Lease 所在 namespace，与 `--leader-elect` 一起使用 |
| `--database-credential-prefix` | 默认 `applications` | Database UID 路径的 mount-relative 前缀；不迁移已有位置 |
| `--database-credential-url-base` | 默认空 | OpenBao API 地址，只用于 Tenant status 中不含认证信息的 `credentialURL` |
| `--database-root-cert` | 默认空（libpq 默认信任） | 只读 PEM trust bundle，不含私钥；作为管理连接的 `sslrootcert` |

`--openbao-address` 必须是绝对 `https` URL，不允许 userinfo、path、query 或 fragment，末尾 `/`
被移除（`internal/infra/openbao`）。凭据 mount 与前缀的每段只允许 `[A-Za-z0-9_-]`，且不得是
KV v2 的 `data`/`metadata` API 层。`--database-credential-url-base` 原样拼成
`<base>/v1/<mount>/data/<path>`，不应带结尾 `/` 或认证信息。

### 集群内与 systemd 共用 Kubernetes 认证

controller 是 API 客户端，不要求部署为 Pod。OpenBao 认证直接复用 manager 已加载的
Kubernetes 配置：集群外可用标准 `--kubeconfig`（或 `KUBECONFIG`），集群内可使用
in-cluster 配置。禁止另要求 `/var/run/secrets/.../token` 文件或解析 kubeconfig 中的 bearer token。
每次 Bao 登录前通过 TokenRequest 申请新的短期 SA JWT，Kubernetes JWT 与 Bao token 的
生命周期分别由 API 签发和官方 SDK 续期管理；不把 kubeconfig 本身当成永久有效凭据。

生产中 manager 以 CSR 签发、自行续签的客户端证书作为 User `ayatori-controller` 访问本机 API
（[ADR-0021](../decisions/0021-client-certificates-via-csr.md)）。其 RBAC 见
[rbac.yaml](../../deploy/flux/control-plane/rbac.yaml)：绑定生成的 `manager-role`，另在 `ayatori-system`
授予 Secret 读取、Lease 与 `database-openbao-login` 的 `create serviceaccounts/token`（`resourceNames`
限定）。通用最小示例见 [最小 RBAC](../../config/samples/database_openbao_auth_rbac.yaml)。
controller 不创建 SA、Role/RoleBinding，也不向自己授予权限。
OpenBao role 限制 SA 名称与 namespace。homelab 的 mount 不持有 reviewer JWT，以登录 JWT 自身调用
TokenReview（两个登录 SA 绑定 `system:auth-delegator`），因此 role 不校验 audience，JWT 必须是
API audience。

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

Tenant 不能选择任意凭据路径。动态供应位置为 `<prefix>/<Database UID>`；导入使用 Database 的
显式 `spec.credentialRef`，不搬迁已有凭据。位置在首次外部写入前固定到 status，不随配置重新推导。

配置变化不得隐式迁移既有凭据。修改 KV mount/prefix 或 `--database-credential-url-base` 前必须
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

## OpenBao

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
使用 Delete 回收时，还需对同一前缀的 metadata 授予 `delete`（永久删除全部版本），例如：

```hcl
path "secret/metadata/applications/*" {
  capabilities = ["delete"]
}
```

只用 Retain 时不需要该权限。

管理凭据由管理员维护并放入 controller namespace 的 Secret；controller 只读，不创建或修改。
应用凭据的键：`username`、`password`、`database`、`host`、`port`、`sslmode`；Instance 声明了 `hostaddr` 时还有 `hostaddr`。
Ayatori 不投射应用凭据：消费方集群按 Tenant status 的 `credentialRef` 自行读取最新版本
（例如其 GitOps 中的 ExternalSecret 不写 `version`）。该路径只由 Ayatori 写入，每个版本都可用；
Instance endpoint 变化时以相同密码改写为新版本，固定版本会让消费方停在旧 endpoint，且 KV v2
只保留有限版本。`credentialVersion` 用于观察。读取所需的 Bao policy 在接入时单独授予，不使用
能读取整个 tenant base path 的共享身份。

## Kubernetes RBAC

- controller 按用例读/写 Instance、Database、Tenant 及其 status/finalizer 和 Event。
- Database 不设置随 Tenant 级联删除的 ownerReference；导入、预留、回收和重新绑定授权限管理员。
- controller 不需要 ExternalSecret 权限，也不读取 Tenant namespace 的 Secret。
- namespace 用户可以管理本 namespace Tenant，但不能管理 Instance 或 controller 配置。
- controller 只在自身 namespace 读取所引用管理 Secret 的 data，不获得跨 namespace
  的管理 Secret 读取权限。Instance 不允许自选 Secret namespace。
- 验证应用登录使用从 OpenBao 读取的应用凭据，不经过任何 Kubernetes Secret。

## 升级与回滚

v1alpha1 尚不承诺跨版本转换。升级与回滚就是修改 policy 的 `release` 与 `image`（先在 Dev
验收再 promotion 到 Prod），流程见 [deploy/README.md](../../deploy/README.md#release)；CRD 随 release
同步且不 prune。禁止在同一组 CR 上同时运行两个 controller 版本。涉及 API/storage 或凭据定位
迁移时，必须先写独立升级规格和回滚步骤。

## 上线验证

```text
PostgreSQL TLS 与备份验证
  -> OpenBao auth/policy 验证
  -> SecretStore Valid / 管理 Secret 已同步
  -> controller Ready/leader elected
  -> Instance Ready
  -> 测试 Tenant Ready
  -> 用 DNS host 登录（声明了 hostaddr 时也用它登录）
  -> 删除测试 Tenant 并验证所选策略
```

生产 homelab 上线前还必须完成 [`security.md`](security.md) 的权限检查和
[`operations.md`](operations.md) 的备份/逃生检查。
