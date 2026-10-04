# 安全模型

| 项目 | 内容 |
| --- | --- |
| 状态 | Review |
| 最后更新 | 2026-09-25 |

## 保护目标

- 应用密码只存在于 OpenBao、消费方集群自行投射的 Secret 和需要使用它的进程内存中；Ayatori 控制面不保存其副本。
- controller 只能在 Database 已确认的管理范围和当前绑定/操作授权内修改资源；未知同名对象报冲突。
- namespace 租户不能越权管理 Instance、Database 导入/回收、其他 namespace 或 controller 配置。
- PostgreSQL 和 OpenBao 的网络身份使用受信 CA 验证，不因 DNS 不可用而降级 TLS。

## 信任边界

Kubernetes 管理员、OpenBao 管理员和 PostgreSQL 管理员是平台信任主体。能读取 Tenant
目标 Secret 或对应 OpenBao path 的主体等同于持有数据库账号。database owner 可以
改变自己 database 内的对象，因此 COMMENT 不能作为 controller 所有权依据。

数据库备份包含业务数据；Kubernetes 保存资源与绑定记录；OpenBao 保存应用凭据。
完整灾难恢复必须分别保护三者并核对恢复点，不依靠数据库内 registry 重建绑定。

## 凭据处理

- controller 使用 Kubernetes auth 获取短期 OpenBao token，不配置长期静态 token。
- Kubernetes auth 不等于部署在 Kubernetes 内：复用 manager 的 kubeconfig/in-cluster 身份，
  通过最小 RBAC 的指定 ServiceAccount TokenRequest 获取 JWT，不依赖 Pod 投射文件。
  kubeconfig 的签发、更新与撤销由部署管理负责；申请失败不回退其他机器身份。
- 管理凭据只从 Instance 引用的 controller namespace Secret 读取，不复制到
  CR/status/Event/metric/trace；管理员维护 ExternalSecret，由 ESO 同步该 Secret。
- 动态供应密码使用密码学安全随机源；已有可靠关联时复用 OpenBao 现值，结果不确定时停止并报冲突。
- controller 不创建 ExternalSecret 或含 data/stringData 的 Secret；只在 Tenant status 公布凭据位置。
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

2026-09-25 维护者确认第一版使用原生非 superuser 管理 role，具有 CREATEDB/CREATEROLE，
不引入 SECURITY DEFINER 接口。Instance 检查拒绝 superuser；具体已有资源的 owner 和
membership 仍需逐资源验证，不能把基础能力用于接管他人资源。扩展按实际权限安装，
不因可用列表包含某个扩展就默认能安装它。controller 不调用 shell 或 `psql` 拼接用户输入。
当前检查与真实权限矩阵见 [Instance 原生管理观测](README.md#instance-原生管理观测)。

Kubernetes RBAC 应把 Instance 管理、Database 导入（含删除 Released 记录后重新导入）和回收限制给平台管理员。
有权创建 Tenant 的申请者可显式申请未绑定且可用的 Database，不增加资源侧允许绑定名单
或逐 Tenant 审批。Released 必须先由管理员处理旧访问并重新开放。Tenant editor
不自动获得 Secret read；是否读取目标 Secret 由 namespace 内独立 RBAC 决定。

## 删除保护

资源侧 Delete 是明确的数据销毁授权，但仍必须在每一步校验 Instance/Database UID、
绑定、实际目标及角色/凭据管理范围；CR 中记录了意图不等于外部对象由本系统创建。
禁止对未知对象使用 `CASCADE`。删除 finalizer 卡住时只能按
[`operations.md`](operations.md) 核实外部状态后人工移除；该操作可能遗留资源。

## 发布前安全验收

- 使用错误 CA、错误 DNS 名和错误 IP 时连接失败；正确 DNS/IP SAN 均成功。
- namespace 用户不能修改 Instance 或跨 namespace Tenant。
- controller 与消费方集群读取身份的 OpenBao policy 互相隔离，越权请求被拒绝。
- 应用 login 不能创建 role/database，也不能连接其他租户 database。
- 日志、Event、Condition、metrics、CR 导出和测试 artifact 不含 canary password/token。
- 伪造 COMMENT、同名 database/role 或错误 UID metadata 均不能绕过 Conflict。
- Delete 只销毁 Database 已确认管理范围内且获得删除授权的资源。
- 导入检查不改密码/owner，Released 不自动授权新使用者；旧访问处理后才能重新交付。
- 导入默认 Retain；角色/凭据的管理与删除范围未明确时不得扩大操作范围。
