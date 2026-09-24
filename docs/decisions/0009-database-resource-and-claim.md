# ADR-0009：分离 Database 资源与 Tenant 申请

- 状态：Accepted（资源模型与生命周期）；API 字段细节待评审
- 日期：2026-09-24
- 依据：维护者本轮设计讨论；本决定部分替代 [ADR-0008](0008-merge-postgresql-tenant-operator.md)
  对 ownership registry、自动恢复与 Retain 的沿用要求，不改变 Database 合并归属。

## 问题

原模型让 Tenant 同时表示用户申请和外部资源生命周期，又用 PostgreSQL registry 在 Tenant
删除后保留所有权。它把简单的一对多关系扩展成额外持久化协议，并为“外部创建成功但 status
未写入”承诺自动认领恢复。这里并没有自动恢复所有不确定结果的产品要求；清楚报告冲突、
保留现场并允许人工处理是可接受的合同。

Kubernetes status 存储在 API/etcd 中，不是 controller 重启即丢失的内存。外部操作与 API
写入之间确实存在失败窗口，但不因此引入第二套所有权数据库或模拟跨系统事务。

## 参考与取舍

参考 Kubernetes 官方 [Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)：
资源独立于申请存在，绑定排他；Retain 释放后需要人工处理；已有资源可以静态登记。
同时参考 [owner references](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/)
与 [finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/) 的生命周期边界。

采用这些模式，不直接使用 PV/PVC 类型，不实现 CSI 协议，不引入 StorageClass、容量匹配、
调度器、通用 Claim、事件总线或额外 registry。Instance 是资源来源与管理入口，PostgreSQL
adapter 承担类似驱动的访问职责；Instance 本身不是 CSI 驱动。

## 决策

- Instance 对应多个独立 Database；每个 Database 同时最多绑定一个 Tenant，Tenant 最多绑定
  一个 Database。Tenant 是用户申请，不再直接承担持久资源的全部生命周期。
- Database 是新增 Kubernetes 资源；本文采用 PostgreSQLDatabase 作为工作名称，具体字段、
  scope 与短名称在 API 评审中确定，不从类比自动推导。
- 动态供应先建立资源记录；已有数据库只能由管理员显式登记导入。仅发现同名数据库不是授权。
- Database 自带 instanceRef，手工登记不依赖 Tenant。动态申请由 Tenant 选择 Instance；
  引用已有 Database 的 Tenant 从资源获取 Instance，不重复声明另一份来源。
- Retain 默认保留 Database 与外部资源；Tenant 删除后资源进入 Released，保留旧绑定身份，
  不自动重新分配。管理员处理数据、账号权限与凭据后，才可授权重新绑定。
- 回收策略在 Database 一侧。Delete 必须具备明确管理范围、删除授权、finalizer 和回读；
  导入不隐含授权改密码、改 owner、撤权或删除。
- Kubernetes CR 保存资源身份、绑定与操作进度，PostgreSQL catalog 保存实际数据库状态；
  不再维护 PostgreSQL ownership registry，不把 registry 初始化或回读作为 Instance Ready 条件。
- 普通依赖失败继续 reconcile；同名未知资源、创建结果无法确认时报告 Conflict，停止对相关
  资源的危险操作，提供人工诊断。已有可靠记录支持的幂等步骤可以继续，不将每次重启都变成冲突。
- status 不承诺在任意删除后自动重建所有权。灾难恢复按备份与人工核实处理。
- Database 不受 Tenant 的级联 GC 控制；Instance 删除也不能级联删除 Database 或业务数据。

## 保留的合同与未决项

管理 Secret 来源、凭据变化后刷新连接、TLS、实际扩展观察、OpenBao 应用凭据与 ESO 投射、
最小权限和分层集成验证继续适用。单 database、单 login owner 的首版使用场景不变。

Database 对 role/凭据的具体管理边界、持久凭据定位与重新授权方式、资源 scope、绑定字段、
预留及并发绑定的 API 更新协议需在 API/实现切片前细化。旧的 Tenant namespace/name 固定
凭据路径不能未经评估直接用于跨 Tenant 重新绑定；不为填满字段表而默认授权搬迁或改密。

## 实施边界

本次只修订设计。现有 registry adapter、Instance 中的 registry 判定及相关测试是待撤换的旧实现，
不是新合同的前提；未提交的 registry inspection 不继续接入。后续按新规格撤除这些依赖，
再实现 Database 资源、绑定、导入与回收的纵向切片，不保留未部署实现的兼容层。

详见 [系统规格](../database/specification.md)、[领域模型](../database/domain-model.md)、
[API 设计状态](../database/api-reference.md)和[测试合同](../database/development.md)。
