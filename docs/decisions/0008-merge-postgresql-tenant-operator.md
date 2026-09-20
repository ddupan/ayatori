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

截至 2026-09-20，源仓库已经合并 Instance 的 Endpoint、凭据引用、身份/版本、定义与观测目标
等值对象，以及扩展支持模型和最小生命周期/checkpoint。它们尚未接入实际运行链路。完整 Ready
判定、Kubernetes Secret 管理凭据与连接刷新、应用层/数据库 adapter/controller 接入、CRD 规格
对齐及集成验证仍未完成；Tenant 的创建、凭据交付与 Retain/Delete 生命周期也未落地。

现有运行链路仍是直接读取 OpenBao 管理凭据的旧实现，不能作为新设计已经可用的证据。源仓库
本地 `feature/instance-extension-observations` 还保留两个未提交文件，用于 Instance 接受扩展观测
及测试；该工作已暂停，不能作为已合并能力或迁移基线。部分生成的 CRD/API 代码也仍落后于批准
规范，因此迁移不能把当前工作树或全部脚手架原样复制到 Ayatori。

## 决策

PostgreSQL Tenant Operator 合并为 Ayatori 的 Database 领域模块。保留已经批准且仍适用的安全、
所有权、幂等与删除行为，不重新发明 database、role、credential 和 registry 语义。

当前没有可用发布版本、没有被该 operator 托管的 PostgreSQL 实例或 Tenant，也没有需要在线
转换的已部署 CR。因此此次合并不承担旧实现兼容性：旧运行链路可以直接撤销，不保留直接读取
OpenBao 管理凭据的路径，不兼容旧 status checkpoint、samples 或落后于规范的 CRD。API 字段若
妨碍清晰领域模型、恢复行为或测试，可以在 `v1alpha1` 阶段修改并重新生成。

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

Database API 直接重构为 Ayatori 统一结构：API group 使用
`database.ayatori.ddupan.top/v1alpha1`，Go package 使用 `api/database/v1alpha1`，controller、
domain 与 adapter 放入 Ayatori 对应 Database 模块。原 `database.ddupan.top/v1alpha1` 不保留
别名、conversion 或兼容入口。

迁移前逐项核对批准规格、领域模型与当前 Go types；冲突时以批准规格和代码质量为基线，并在
Ayatori 中记录有意改变。无需实现在线 CRD conversion 或数据迁移。

## 迁移方式

1. 以包含已合并 Instance 领域基础和 CI #14 的最新 `main` commit 作为 source reference；记录
   commit，并先提取规范、领域模型和纯单元测试中仍然成立的部分。目标是保留知识与验证，不是
   逐文件复制旧实现。
2. 保留源仓库暂停中的脏工作树，不移动、提交或复制两个 extension observation 文件。以后可以
   先在源仓库形成独立 commit，或在 Ayatori 根据批准合同重新实现，但不得把未提交内容描述为来源。
3. 在 Ayatori multi-group 项目中用 Kubebuilder 注册 Database API，按批准规格迁移 types，重新
   生成 `database.ayatori.ddupan.top` CRD、DeepCopy 与 RBAC；不直接复制旧生成文件或旧 `PROJECT`。
4. 删除旧运行链路假设，以 Ayatori 当前 Go、Kubernetes 与 controller-runtime 版本重新建立
   application ports 和 adapter contract；先恢复 PostgreSQL registry/adapter contract tests。
5. 逐片实现 Instance observe、Kubernetes Secret 管理凭据与连接刷新、Tenant provisioning、
   OpenBao、ExternalSecret、删除与恢复流程；
   每片必须包含对应单元、envtest 和真实 PostgreSQL/OpenBao 集成测试。
6. Ayatori 中的 Database 模块达到原项目验收标准并完成迁移演练后，冻结旧仓库并将其 README
   指向 Ayatori；不同时运行两个 controller 管理同一组 CR。

不通过一次性 unrelated-history merge 或整仓复制保留表面上的 Git 历史。旧仓库和 source commit
保留完整来源历史；Ayatori 迁移提交按可审阅行为切片记录 provenance。

## 结果

- Ayatori 获得第一个真实产品领域，而不是继续围绕实验性 Job 扩张。
- 已批准的 DBaaS 设计与测试投资得到保留。
- 单一 manager/release 不意味着领域耦合；Database 仍保持独立 package、adapter 和测试边界。
- 可以从已合并的领域基础开始迁移；旧运行链路和未提交 extension observation 不进入首个切片。
- 无部署兼容负担允许优先修正 API 和架构，不为尚未使用的旧代码保留技术债。
- Database 使用 Ayatori 统一 API group 与目录结构，不为未投入使用的旧 group 保留入口。
