# 运维与故障处理

| 项目 | 内容 |
| --- | --- |
| 状态 | Review；命令待实现后演练 |
| 最后更新 | 2026-09-10 |

## 日常检查

先看 API 合同，而不是从日志猜状态：

```sh
kubectl get postgresqlinstances
kubectl get postgresqltenants -A
kubectl get postgresqltenant -n <namespace> <name> -o yaml
kubectl describe postgresqltenant -n <namespace> <name>
```

随后检查 controller 日志、ExternalSecret/Secret、OpenBao metadata、registry 和
PostgreSQL catalog。排障时不得把 Secret data 或带 Token 的请求粘贴到 issue/日志。
`status.phase` 是 controller 状态机 checkpoint，也用于定位当前步骤；`Ready`
Condition/Reason 用于判断对外结果。phase 不能替代外部事实，清空或不一致时应由
controller 自动重建/纠正。

## 常见 Reason

| Reason | 首要检查 |
| --- | --- |
| `InvalidSpec` / `ImmutableField` | API 字段、identifier、不可变/只追加约束 |
| `DependencyUnavailable` | 网络、DNS、服务状态和超时 |
| `AuthenticationFailed` | 管理凭据、CA、DNS/IP SAN、OpenBao auth |
| `InsufficientPrivileges` | PostgreSQL grants、OpenBao policy、Kubernetes RBAC |
| `InstanceNotReady` | 先恢复所引用 Instance |
| `Conflict` | registry UID、同名 DB/role、OpenBao metadata；禁止直接覆盖 |
| `CredentialProjectionFailed` | ClusterSecretStore、ExternalSecret Condition、目标 Secret |
| `ProvisioningFailed` | `status.phase` 及对应外部资源的回读结果 |

修复依赖后让正常 reconcile 自动重试。不要通过删除/重建 CR 规避 Conflict；新 UID 只会
使已有保留资源继续冲突。

## Retain 后的资源

Retain 删除完成后，database、role、OpenBao record 和 registry 所有权记录仍存在但标记
unmanaged。v1alpha1 不支持重新关联。需要恢复管理时，使用 [`migration.md`](migration.md)
把数据迁移到一个全新受管名称；不要手工把 registry UID 改成新 CR UID。

## Delete 卡住

1. 暂停应用写入并记录 Tenant UID、Instance UID、database、role 和 Bao path。
2. 从 registry 和 OpenBao metadata 独立确认所有权。
3. 检查删除阶段，修复 PostgreSQL/OpenBao/ESO 依赖，让 controller 继续。
4. 若依赖永久丢失，列出每个可能残留的 database、role、KV metadata 和 Secret。
5. 只有确认接受这些残留后，才人工移除 finalizer。

最终 finalizer 名称由 API 实现固定后补入命令。人工移除 finalizer不会执行剩余清理，
也不会把外部资源变成可由新 CR 接管的资源。

## 备份与恢复

- PostgreSQL VM/磁盘备份必须与数据库一致性策略配套；仅复制在线磁盘不自动等于有效
  PostgreSQL 备份。
- PostgreSQL 备份必须包含管理 database 中的 controller registry。
- OpenBao 使用独立的受支持备份/快照流程，且恢复点应与 PostgreSQL 尽量接近。
- Kubernetes 侧备份 CR、controller 配置、ClusterSecretStore 和公开 CA bundle，不备份
  明文 Secret 作为凭据事实来源。
- 定期在隔离环境执行恢复演练，验证 registry、KV metadata、应用登录及 Retain/Delete。

恢复后先停止 controller，核对 PostgreSQL/OpenBao 时间点与 UID 映射，再启动单副本
controller 观察；出现一侧存在、一侧缺失时不得手工生成新密码或改 registry，应先按
Conflict 处理并决定恢复哪一侧。

## 升级与紧急停止

有疑似越权删除或凭据泄漏时，先把 controller Deployment scale 到 0，保留 CR、registry
和日志证据，再撤销 OpenBao token/role 并限制 PostgreSQL 管理 role。恢复前在隔离环境
复现并确认不会扩大破坏。一般依赖故障无需 scale down，最终一致性会自动重试。
