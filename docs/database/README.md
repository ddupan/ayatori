# Database 模块

Database 是 Ayatori 首批实际产品领域之一。第一个迁移切片只建立 PostgreSQL Instance 的纯领域
模型，不注册 CRD、不启动 controller，也不访问 PostgreSQL、OpenBao 或 Kubernetes Secret。

## 来源基线

完整设计合同及首批领域模型与测试提取自原 PostgreSQL Tenant Operator：

- 仓库：`git.ddupan.top/panxiao81/postgresql-tenant-operator`
- source commit：`dae546e58efa1be81e930861c87f7fb13bb12113`
- 原路径：`internal/domain/instance/`、`docs/domain-instance.md`
- 迁移日期：2026-09-20

本目录迁入该基线的 specification、architecture、API、领域、部署、安全、开发、迁移与运维
文档，并只进行 Ayatori 产品归属、API group、目录和链接适配；其余已批准行为保持不变。

迁移只使用该 commit 中已提交的文件。源仓库
`feature/instance-extension-observations` 工作树中的 `instance.go` 修改与
`instance_extensions_test.go` 未进入本切片。

代码被移动到 Ayatori 的 `internal/database/domain/instance`，测试 import 和文档链接相应更新；
首个后续切片按已批准合同增加 Instance extension observation：观测与当前 target 绑定，进入重新
验证或删除时失效，且支持判定不授权 Tenant provisioning。后续 Ready 切片实现管理能力判定、
registry 准备决策与完整回读、Ready 重验及本轮 evidence 前置检查；沿用已批准合同，不能把旧运行
链路接回该模型。各层验证边界见 [Instance 领域规格](domain-instance.md)。

## 边界

- 领域层不依赖 Kubernetes types、数据库 driver 或凭据 provider。
- CredentialReference 只携带管理 Secret 的名称与字段映射，不包含 Secret 内容或 OpenBao path。
- Instance checkpoint 不是外部事实；实际能力必须由 application/adapter 观察后交给领域对象判断。
- 当前代码只检查 Instance 供应前置条件，不授予 Tenant 所有权或外部写入权限，也不表示
  Database API 已经可用。

## 管理凭据与连接切片

`application.InstanceService` 适配自原项目固定基线
[`internal/instance/service.go`](https://git.ddupan.top/panxiao81/postgresql-tenant-operator/src/commit/dae546e58efa1be81e930861c87f7fb13bb12113/internal/instance/service.go)，
保留 CredentialReader、Connector、Database 的装配边界及串行操作/释放规则。
具体连接池完全由 pgxpool v5.11.0 提供，PostgreSQL adapter 不拥有凭据缓存、轮换流程或任意观测回调。

相对源基线有两项按已批准合同作出的必要修改：管理凭据从固定 controller namespace 的
Kubernetes Secret 直接读取；每轮比较有效用户名、密码，检测到变化即关闭旧连接并重新装配。
Secret metadata 和无关字段变化不重建连接。观测后再次读取 Secret，中途有效值变化则丢弃结果，
不把旧连接的成功作为新凭据有效的证据；这不构成跨 Kubernetes/PostgreSQL 的原子事务。
Instance UID、endpoint 或凭据引用变化也会释放旧连接；Forget/Close 只释放本地资源。

当前只有 `ObserveVersion` 版本查询用例，不能产生完整 CapabilityObservation 或 Ready。
controller 接入、Secret watch、finalizer、registry 与真实权限检查仍待后续切片；并发 CR 更新
必须由调用者通过 resourceVersion 校验。应用层沿用源实现的串行处理，本阶段未引入新的调度框架。

运行 `make test-database-integration` 验证真实 API server + 一次性 PostgreSQL；fixture 不接受外部
DSN，镜像固定摘要，使用随机本机回环端口并在退出时删除测试容器。覆盖缺失/错误凭据、RBAC、
namespace 边界、有效值轮换、metadata 无关变化、中途轮换、重建/重试、并发读取、Forget/Close
与 TLS DNS/IP SAN、错误 CA/主机名和禁止明文降级。CI 使用 Pod runner 执行，workflow 显式启动
本 job 独占的 Docker daemon，随 Pod 销毁；不依赖 VM 或共享宿主 Docker。普通 lint 之外还检查
integration 标签代码。领域单测、真实 API 行为与真实 PostgreSQL 行为分别验收，不以本切片
替代整个 Instance controller 的集成验收。

## 设计入口

- [系统规格](specification.md)：规范性行为与验收标准；
- [架构](architecture.md)与[API 合同](api-reference.md)；
- [领域模型](domain-model.md)与[Instance 领域规格](domain-instance.md)；
- [安全](security.md)、[迁移](migration.md)与[运维](operations.md)；
- [部署合同](deployment.md)与[开发测试合同](development.md)。

部署和开发文档描述目标合同，其中旧仓库专属的 Make target、脚手架版本和目录尚未接入 Ayatori；
在对应实现切片完成前，不应把其中命令理解为当前仓库已经可执行的入口。
