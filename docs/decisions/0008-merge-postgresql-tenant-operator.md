# ADR-0008：将 PostgreSQL Tenant Operator 合并为 Ayatori Database 模块

- 状态：Accepted
- 日期：2026-09-20

## 背景

独立仓库 `postgresql-tenant-operator` 已经为 homelab 共享 PostgreSQL 设计了
`PostgreSQLInstance` 与 `PostgreSQLTenant` API，并包含批准的行为规格、领域值对象、状态机、
PostgreSQL ownership registry、OpenBao/External Secrets 边界、迁移与恢复文档及测试。

Database 是 Ayatori 当前最优先的真实管理缺口之一。继续把该 controller 作为独立产品，会重复
维护 manager、API machinery、发布、认证、可观测性和通用 controller 约定，也会使后续应用组合
必须跨两个控制平面理解状态。

源仓库当前实现仍在按规格逐片完成，部分生成的 CRD/API 代码落后于批准规范；迁移不能把当前
工作树或全部脚手架原样复制到 Ayatori。

## 决策

PostgreSQL Tenant Operator 合并为 Ayatori 的 Database 领域模块。保留已经批准且仍适用的外部
行为，不重新发明 database、role、credential、ownership 和删除语义。

目标结构遵守 Ayatori 的模块化单体边界：

```text
api/database/v1alpha1/
internal/database/domain/
internal/database/controller/
internal/database/adapter/postgresql/
internal/database/adapter/openbao/
internal/database/adapter/externalsecrets/
docs/database/
```

最终目录可按 Kubebuilder 与现有模块约定微调，但 Database 不依赖 execution/Job 模块，也不把
PostgreSQL、OpenBao 或 External Secrets 客户端放入共享万能 service/repository 层。

首轮迁移保留现有 `database.ddupan.top/v1alpha1` API group，避免仅为仓库归属制造无收益的 API
重命名。迁移前逐项核对批准规格与当前 Go types；冲突时以批准规格为基线，并在 Ayatori 中记录
任何有意改变。数据库尚未实际由该 operator 纳管，因此不需要执行已部署 CRD 的在线 conversion。

## 迁移方式

1. 等源仓库当前未提交的 Instance 工作形成可引用 commit；迁移期间不读取或复制脏工作树作为
   权威实现。
2. 记录 source commit，并先迁移规范、领域模型和纯单元测试；保持行为与测试可追溯。
3. 在 Ayatori multi-group 项目中用 Kubebuilder 注册 Database API，按批准规格迁移 types，重新
   生成 CRD、DeepCopy 与 RBAC；不直接复制旧生成文件或旧 `PROJECT`。
4. 将依赖升级到 Ayatori 当前 Go、Kubernetes 与 controller-runtime 版本，并先迁移 PostgreSQL
   registry/adapter contract tests。
5. 逐片迁移 Instance observe、Tenant provisioning、OpenBao、ExternalSecret、删除与恢复流程；
   每片必须包含对应单元、envtest 和真实 PostgreSQL/OpenBao 集成测试。
6. Ayatori 中的 Database 模块达到原项目验收标准并完成迁移演练后，冻结旧仓库并将其 README
   指向 Ayatori；不同时运行两个 controller 管理同一组 CR。

不通过一次性 unrelated-history merge 或整仓复制保留表面上的 Git 历史。旧仓库和 source commit
保留完整来源历史；Ayatori 迁移提交按可审阅行为切片记录 provenance。

## 结果

- Ayatori 获得第一个真实产品领域，而不是继续围绕实验性 Job 扩张。
- 已批准的 DBaaS 设计与测试投资得到保留。
- 单一 manager/release 不意味着领域耦合；Database 仍保持独立 package、adapter 和测试边界。
- 在源仓库当前并行工作提交前，只进行文档与迁移准备，不移动其代码。
