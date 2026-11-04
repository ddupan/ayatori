# Database 系统架构

本页解释 [系统规格](specification.md) 的组件边界；资源与申请分离的依据见
[ADR-0009](../decisions/0009-database-resource-and-claim.md)。三条链路均已实现，见[模块说明](README.md)。

## 资源与后端

```text
Kubernetes API
  Instance ──引用── Database ──排他绑定── Tenant
                         |
              Ayatori Database controllers
                   |          |          |
              PostgreSQL   OpenBao
                catalog      ↑ 消费方集群按 Tenant status 公布的位置自行读取
```

Kubernetes 保存声明、绑定与操作进度；PostgreSQL 保存实际数据库状态；OpenBao 保存应用凭据。
不在 PostgreSQL 中再建立管理 registry。Instance 是管理入口而非 CSI 协议实现，adapter 是薄访问层。

Instance 验证当前目标的管理能力，不供应 Tenant 数据库，不因 registry 缺失初始化任何 schema。
Database 用例负责独立资源的供应、显式导入、保留与回收；Tenant 用例负责申请、绑定与凭据交付。
这是用例职责，不要求为每一步新增一个 controller 或通用控制循环。

## 协调与恢复

通过 Kubernetes API 的 resourceVersion 保护并发更新，watch 推动依赖恢复；外部操作前资源
记录及其固定目标已持久化，执行后观察并记录结果。不另存"创建中"检查点：下一步由已记录的
确认与本轮观察决定。数据库实际操作仍须处理后端竞态，列表检查不是唯一约束。

可靠确认的步骤允许幂等继续；外部创建与进度写入之间的失败若导致归属不确定，则停止写入并
报告 Conflict。不创建第二套所有权存储，不承诺跨系统事务或任意 status 丢失自动认领。
错误必须提供人工可用的步骤、资源与结果确定性信息，但不泄漏秘密。

## 绑定、导入与回收

Database 独立于 Tenant 存在，不能以会导致级联删除的 ownerReference 连接两者。
Instance 删除检查 Database 引用，包括 Released 资源，不直接清理数据库。

动态供应和管理员导入使用同一种资源记录。导入验证初始只读；未知同名数据库仍为冲突。
Retain 保留资源及旧绑定身份；重新绑定必须经过人工数据和访问权限处置，不自动分配。
Delete 由资源侧明确授权，在 finalizer 保护下按管理范围清理并逐步回读。

Delete 清理顺序为 NOLOGIN → DROP DATABASE → DROP ROLE → 删除 OpenBao metadata → 移除 finalizer，
见[释放与回收](README.md#释放与回收)；Ayatori 不投射凭据，没有投射清理步骤。

## 保持的访问边界

管理 Secret 固定在 controller namespace，由 ESO 从 Bao 同步（ExternalSecret 随 release 由 Flux 下发）；controller 只读。
有效值变化刷新管理连接，不直接访问 Bao 获取管理凭据，不自行实现连接池。
应用凭据写 OpenBao 并在 Tenant status 公布位置，不在控制面投射 Secret（[ADR-0010](../decisions/0010-credential-delivery-ends-at-openbao.md)）。
TLS、DNS/IP SAN、七键凭据输出和最小权限合同继续适用。

## 导航

- [API 合同](api-reference.md)
- [领域模型](domain-model.md)与[Instance 规格](domain-instance.md)
- [部署](deployment.md)、[安全](security.md)、[开发测试](development.md)
- [dump/restore 迁移](migration.md)、[运维](operations.md)
