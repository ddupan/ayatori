# 运维与故障处理

依据 [系统规格](specification.md)，不再查询或维护 PostgreSQL registry。

## 运行边界

源码已接入绑定、Instance 观测、Bao 凭据准备、PostgreSQL 供应与 Retain/Delete 回收与显式导入（供应相关需显式启用）。
绑定后 Tenant 依次报告 CredentialsPending、DatabaseNotReady，最后 CredentialsAvailable 并公布凭据位置；
Ayatori 不投射 Secret，消费方集群自行读取。
Tenant 删除先把 Database 记为 Released 再移除 finalizer；Database 按回收策略保留或清理，见
[释放与回收](README.md#释放与回收)。Delete 清理遇 Conflict 时保留 finalizer，核实同名对象后
再处理；不要为了消除等待状态直接移除 finalizer。Released 重新开放：处置旧访问后清除 Database 的 `spec.tenantRef`，见[显式导入与重新开放](README.md#显式导入与重新开放)。

凭据准备现在可单独启用，见[部署参数](deployment.md)。观察 Database 的
`CredentialsReady`、固定 `status.credentialRef` 和 `status.credentialVersion`，不要导出
凭据内容。Prepared 只代表 Bao 凭据可用，不代表已建库或 Tenant Ready。
创建结果丢失或确认保存失败后，下一轮读到未确认的已有值即报告 Conflict；不要因为想重试
就清空固定位置或生成新密码。
无确认版本的 Conflict 不会自行消失，后端恢复也不自动重入；应暂停该 controller、等待
在途请求结束后核实版本历史、残留与绑定，再决定清理残留后重试或以 `source: Import` 显式导入。不要清空整个 status
或更改固定引用来绕过保护。

## 日常检查

Instance 观察已实现：先确认 manager 配置了 `--database-secret-namespace` 或 `POD_NAMESPACE`，
再检查 Ready Reason、observedGeneration 与管理 Secret 名称/字段映射，切勿导出其 data。
`InsufficientPrivileges` 表示当前原生方案要求的非 superuser、CREATEDB/CREATEROLE 不满足；
`InstanceInUse` 消息定位阻塞删除的资源。
Secret 事件立即入队，30 秒重查覆盖 PostgreSQL 权限等没有 Kubernetes 事件的外部变化。
Instance 删除不要求 PostgreSQL 可达，但必须可读取所有 Database/Tenant 引用。

先看 Instance、Database、Tenant 的 Ready Condition、绑定 UID、阶段与 observedGeneration，
再核对 PostgreSQL catalog、OpenBao metadata 与 Tenant 公布的凭据位置；消费方投射在其集群排查。
资源为 `postgresqlinstances`、`postgresqldatabases`（均为集群级）与 `postgresqltenants`；finalizer 为
`database.ayatori.ddupan.top/{instance,database,tenant}-protection`。
不得把 Secret data、密码或带 Token 的请求粘贴到 issue/日志。

## 故障分类

完整 Reason 列表见 [API 合同](api-reference.md#conditions)。

| Reason/状态 | 首要检查 |
| --- | --- |
| InvalidRequest（Tenant） | 申请互斥、名称与只追加扩展约束 |
| InvalidCredentials | 管理 Secret 的字段映射 |
| DependencyUnavailable | 网络、DNS、Secret 是否存在、服务状态和超时；恢复后退避重试 |
| AuthenticationFailed | 管理 Secret、TLS、OpenBao auth |
| InsufficientPrivileges | 管理 role 是否非 superuser 且具备 CREATEDB/CREATEROLE |
| ObservationIncomplete | 管理能力检查缺项；不能凭 metadata 查询成功判定 Ready |
| Conflict | 绑定 UID、未知同名资源、失败步骤与外部结果确定性 |
| `status.phase=Released` | 资源已保留，不代表可直接交给另一个 Tenant |

## 创建不确定或同名冲突

1. 保留 CR、绑定与安全诊断，不清空 status、不反复删除重建申请。
2. 核对确切 Instance/database/role 和凭据位置；区分已确认完成与结果不确定的操作。
3. 使用只读检查确认资源内容、使用者和权限，不通过重设密码来“验证归属”。
4. 管理员决定清理确定的残留后重试，或显式导入保留资源；涉及删除需另有明确授权。
5. 记录处理依据，再恢复协调。

普通依赖故障可以自动继续，未知归属不得因后端恢复就自动认领。
controller 重启保留 Kubernetes 中的记录，不需要恢复第二套 registry。

## Retain 与重新绑定

Tenant 删除后 Database 及实际资源保留，进入 Released，保存旧绑定身份。
不要删除 Database 对象来“释放名称”，也不要只修改 UID 或 Ready 强行交付。

管理员先确认数据是否允许交给新使用者、旧角色是否共享、旧账号访问如何撤销或保留、
新使用者如何获得凭据，以及消费方集群中原投射的处置。删除 Secret 不会撤销已持有密码
的 PostgreSQL 访问。完成这些处置后清除 Database 的 `spec.tenantRef` 即重新开放，controller
按确认身份重新验证后回到 Available，见[显式导入与重新开放](README.md#显式导入与重新开放)。

## Delete 卡住

核对 Database/Instance/绑定身份、资源侧 Delete 授权及实际管理范围，修复相关依赖，
让 controller 从已确认的步骤继续。不要删除共享角色或未纳管凭据，不使用扩大范围的 CASCADE。

依赖永久丢失时列出每个可能残留的数据库、角色与凭据。只有管理员接受残留与后续处置
责任后才人工移除确切对象的 finalizer。该操作不会完成清理，也不会授权新申请使用残留资源。

## 备份与恢复

分别备份 PostgreSQL 数据、Kubernetes 资源与绑定记录、OpenBao 数据及必要配置。
不再要求备份专用 registry。只复制在线磁盘不等于有效数据库备份；秘密备份必须加密并限制访问。

控制面由 `ayatori-backup.timer` 每日 `k0s backup`（etcd、PKI、k0s.yaml，本机卷，非离机）；恢复步骤
及恢复后对快照前后新建/删除的 Database 以 Import 或 Retain 删除记录人工对账，见
[deploy/README.md 恢复](../../deploy/README.md#恢复)。PostgreSQL 与 OpenBao 的备份不由 Ayatori 负责。

灾难恢复先暂停 controller，核对三者恢复点、UID、外部目标与凭据的一致性，再恢复协调。
不一致时按 Conflict 人工处理，不承诺仅凭外部同名数据库重建丢失绑定。

## 监控

kube-state-metrics（`:8085`）导出 `ayatori_resource_info`（Instance/Database/Tenant 的 Ready、原因与
阶段），manager 的 controller-runtime 指标在 `:8080`；端口与告警位置见
[deploy/README.md 监控](../../deploy/README.md#监控)。

## 紧急停止

疑似越权删除或秘密泄漏时暂停 controller，保留 CR 与脱敏证据，限制相关管理身份权限，
在隔离环境复现并确认修复后恢复。一般依赖失败不需要停机。
