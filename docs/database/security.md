# 安全模型

| 项目 | 内容 |
| --- | --- |
| 状态 | Review |
| 最后更新 | 2026-09-10 |

## 保护目标

- 应用密码只存在于 OpenBao、ESO 投射的目标 Secret 和需要使用它的进程内存中。
- controller 只能修改其 registry 能证明归属当前 Tenant UID 的资源。
- namespace 租户不能越权管理 Instance、其他 namespace 或 controller 配置。
- PostgreSQL 和 OpenBao 的网络身份使用受信 CA 验证，不因 DNS 不可用而降级 TLS。

## 信任边界

Kubernetes 管理员、OpenBao 管理员和 PostgreSQL 管理员是平台信任主体。能读取 Tenant
目标 Secret 或对应 OpenBao path 的主体等同于持有数据库账号。database owner 可以
改变自己 database 内的对象，因此 COMMENT 不能作为 controller 所有权依据。

VM/磁盘备份会包含 PostgreSQL registry 和租户数据，但不应包含 OpenBao 中的密码；完整
灾难恢复必须同时保护 PostgreSQL 与 OpenBao，并控制两份备份的访问权限。

## 凭据处理

- controller 使用 Kubernetes auth 获取短期 OpenBao token，不配置长期静态 token。
- 管理凭据只从 Instance 引用的 controller namespace Secret 读取，不复制到
  CR/status/Event/metric/trace；管理员维护 ExternalSecret，由 ESO 同步该 Secret。
- 租户密码使用密码学安全随机源生成一次；中断恢复必须复用 OpenBao 现值。
- controller 创建 ExternalSecret，不直接创建含 data/stringData 的 Secret。
- 日志字段允许 namespace/name、UID、generation、阶段和错误类别；禁止记录请求/响应体、
  DSN、Authorization header、密码或完整 OpenBao URL path 作为 metric label。
- panic、错误包装和测试失败输出必须经过凭据泄漏测试。

## TLS

- homelab 默认 `verify-full`，`disable` 只允许显式开发配置。
- server 证书同时覆盖 DNS `host` 和 IP `hostaddr`；消费者自行选择连接目标。
- OpenBao PKI 保管 CA 私钥并负责签发/续期。controller Deployment 只挂载公开 CA
  bundle，挂载只读且使用最小文件权限。
- 证书轮换必须先发布同时信任新旧 CA 的 bundle，再轮换服务端证书，最后移除旧 CA。

## 最小权限

OpenBao controller identity 只管理固定 tenant base path，不读取管理凭据。管理凭据
ESO 身份只读管理路径，租户 ESO 身份只读 tenant base path，二者隔离，Tenant 不得
使用管理凭据 Store。controller 对管理 Secret 的读取限于自身 namespace，Instance
不能指定其他 namespace；controller 不创建或修改管理 Secret／ExternalSecret。

PostgreSQL 管理 role 不应是 superuser。若平台选择 SECURITY DEFINER 函数承载创建或
删除操作，函数必须固定 `search_path`、严格校验 identifier、拒绝任意 SQL，并仅向
controller role 授予 EXECUTE。controller 不调用 shell 或 `psql` 拼接用户输入。

Kubernetes RBAC 应把 cluster-scoped Instance 管理限制给平台管理员。Tenant editor
不自动获得 Secret read；是否读取目标 Secret 由 namespace 内独立 RBAC 决定。

## 删除保护

Delete 是明确的数据销毁授权，但仍必须在每一步校验 Instance UID、Tenant UID、名称和
OpenBao metadata。禁止对未知对象使用 `CASCADE`。删除 finalizer 卡住时只能按
[`operations.md`](operations.md) 核实外部状态后人工移除；该操作可能遗留资源。

## 发布前安全验收

- 使用错误 CA、错误 DNS 名和错误 IP 时连接失败；正确 DNS/IP SAN 均成功。
- namespace 用户不能修改 Instance 或跨 namespace Tenant/ExternalSecret。
- controller/ESO 的 OpenBao policy 互相隔离，越权请求被拒绝。
- 应用 login 不能创建 role/database，也不能连接其他租户 database。
- 日志、Event、Condition、metrics、CR 导出和测试 artifact 不含 canary password/token。
- 伪造 COMMENT、同名 database/role 或错误 UID metadata 均不能绕过 Conflict。
- Delete 只销毁 registry 可证明归属当前 Tenant 的资源。
