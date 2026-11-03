# 开发与测试环境

本页是 Database 模块的测试分层与 fixture 合同。入口都是仓库 Makefile 的 target，没有
单独的开发 VM、devcontainer 或 Compose 环境。

## 当前设计验收（2026-09-24）

[ADR-0009](../decisions/0009-database-resource-and-claim.md) 将资源生命周期从 Tenant 中分离。
新增验收矩阵见 [系统规格](specification.md#11-验收)。registry 实现、专属测试与迁移依赖已撤除，不继续 schema 审计或自动所有权恢复切片。

| 层次 | 本次设计要求 |
| --- | --- |
| 纯领域 | Instance 无 registry 就绪判定、排他绑定、Released 不自动复用、管理范围与冲突规则 |
| envtest | 三资源 schema/status、RBAC、resourceVersion 并发、绑定单边更新/重启、依赖 watch、finalizer |
| 真实 PostgreSQL/OpenBao | 同名不修改、显式导入只读验证、创建不确定报冲突、可靠步骤幂等、Delete 故障重试 |
| 测试集群 | Tenant/namespace 删除不 GC Database、人工重新绑定前的旧访问处置 |

需故障注入外部成功而 API 写入失败、后端响应丢失、双 Tenant 竞争、同名新 UID、依赖稍后出现、
Instance 删除与 Released 引用。冲突必须给出可操作而不泄密的诊断；不要求自动认领不确定结果。
envtest 不运行 GC；这类行为必须由测试集群验证。Ayatori 不投射凭据，交付验收止于 OpenBao 与 Tenant status。
三资源 API 类型与 CRD 已实现，`make test` 包含真实 API server 验证：作用域、
静态默认值、非法声明、status 写入隔离、Condition 唯一性、resourceVersion 冲突与绑定记录回读。
另有绑定 controller 的真实 API server 测试：动态记录幂等、资源侧写入后故障注入、
新 reconciler 回读补齐、双 Tenant 竞争、Released/旧 UID 拒绝、目标固定、陈旧观察、
删除期间 finalizer 保留。实际 manager 在生成的 RBAC 角色下通过 watch/cache 处理依赖
稍后出现，绑定角色不具备 Secret 读取权限。没有 PostgreSQL/OpenBao 写入；后端 Ready
在测试中由 fixture 提供，不能把它当成完整 Instance/Database 观察验证。

Retain 释放与 Delete 清理已由 envtest 与真实 PostgreSQL/OpenBao 集成测试覆盖；GC、namespace
删除仍需测试集群验收；导入与 Released 重新开放由真实后端集成测试覆盖。

绑定规则另有不依赖 Kubernetes 的单元测试；service 测试只验证操作顺序、失败停止与最终
身份回读，不模拟 API server。原真实 API controller 测试覆盖完整分层调用，另验证 service
返回后发生并发修改时，资源呈现拒绝过期结果，重试完成绑定且保留其他字段。

## 命令入口

```sh
make test                        # 单元测试 + envtest（真实 API server）
make lint
make test-database-integration   # hack/integration-test.sh：API server + PostgreSQL + OpenBao
make lint-database-integration   # integration 构建标签下的 Database/infra/bootstrap 代码
make run                         # 本机运行 manager，参数见 deployment.md
```

`test-database-integration` 要求本机 Docker 可用。`hack/integration-test.sh` 先编译全部集成测试二进制，同时在
go test 之外启动一次 envtest 的真实 API server/etcd（`hack/apiserver`）和固定镜像摘要的临时
PostgreSQL、OpenBao dev 容器，随机绑定回环端口；再逐包串行运行测试，结束时删除这些容器与进程。
测试只认脚本给出的 kubeconfig 与容器 ID（`AYATORI_IT_*`，见 `internal/testenv`），不读取默认
kubeconfig，也不接受指向现有数据库的 DSN。每个场景独占这些实例：开始前复位 PostgreSQL 与
OpenBao，使用自己的 namespace，结束时清除 cluster 级对象。直接 `go test -tags=integration`
不会启动这些后端。TLS 测试在临时目录生成一次性证书与私钥，不使用生产 CA。

当前覆盖固定 namespace 的 Secret 读取与 RBAC、缺失/无效凭据恢复、有效凭据变化后的重连、
无关字段更新不重连、中途轮换时丢弃观察、会话重建、并发读取、本地连接释放和 TLS 验证。
快速测试、lint 和 Database 集成测试均使用 Pod runner。按维护者于 2026-09-21 更新的接口
约定，runner 提供默认可用的 Docker；workflow 通过 `docker version` 和 `docker info` 预检，
不自行启动 daemon、不强制 storage driver 或覆盖 Docker endpoint。该约定的 CI 验收依赖
runner 后端修复上线，不能从本地测试通过推断远端已经可用。
fixture 启动失败会保留退出错误与 stderr，并遮蔽测试密码，
以区分缺少命令、daemon 不可达、权限和镜像拉取失败。

metadata 测试验证版本与可用扩展的只读查询，包括未安装扩展、大小写保持、search_path 遮蔽、
低权限账号读取、catalog 访问被撤回后的失败与恢复，以及凭据中途变化时同时丢弃版本和扩展。
权限撤回只修改每个场景自建 PostgreSQL 容器的 ACL；不连接现有服务。
可用列表不等于安装权限，这些检查不替代后续的完整管理权限矩阵或 Instance Ready 验收。

Instance 领域测试不再提供 registry 状态，初次验证与 Ready 重验分别覆盖所有管理检查项的
未观察、不可用、认证失败、权限不足及未知值，并验证依赖恢复；完整管理观察可直接 Ready。

CRD 基础语义由前述 API 测试覆盖；版本查询成功不意味着 Instance Ready。
