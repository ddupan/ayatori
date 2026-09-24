# 开发与测试环境

> 本页迁入作为 Database 模块的测试分层与 fixture 合同。旧项目的 Make target、devcontainer
> 和脚手架版本尚未适配 Ayatori；实现时应复用 Ayatori 现有工具链，并保持这里定义的测试边界。

## Ayatori 已接入的凭据切片测试

本节命令已在 Ayatori 接入；以下历史 Compose/Kind 操作仍属于迁入的目标合同。

```sh
make test
make lint
make lint-database-integration
make test-database-integration
```

最后一项要求本机 Docker 可用。它启动 envtest 的真实 API server/etcd 和固定镜像摘要的临时
PostgreSQL 容器，随机绑定回环端口，不读取 kubeconfig，也不接受指向现有数据库的 DSN。
每个凭据场景使用独立环境；测试清理仅关闭自己的进程和按确切 ID 删除自己的容器。
TLS 测试在临时目录生成一次性证书与私钥，不使用生产 CA。

当前覆盖固定 namespace 的 Secret 读取与 RBAC、缺失/无效凭据恢复、有效凭据变化后的重连、
无关字段更新不重连、中途轮换时丢弃观察、会话重建、并发读取、本地连接释放和 TLS 验证。
快速测试、lint 和 Database 集成测试均使用 Pod runner。按维护者于 2026-09-21 更新的接口
约定，runner 提供默认可用的 Docker；workflow 通过 `docker version` 和 `docker info` 预检，
不自行启动 daemon、不强制 storage driver 或覆盖 Docker endpoint。该约定的 CI 验收依赖
runner 后端修复上线，不能从本地测试通过推断远端已经可用。
fixture 启动失败会保留退出错误与 stderr，并遮蔽测试密码，
以区分缺少命令、daemon 不可达、权限和镜像拉取失败。

这些测试尚不包含 Instance CRD/controller、Secret watch、status/finalizer 事件链、registry、
权限探测矩阵、ESO 或 Tenant 供应。版本查询成功不意味着 Instance Ready。

本项目同时依赖 Kubernetes API、PostgreSQL、OpenBao 和 ESO。日常开发不连接 homelab
中的真实服务：Kubernetes 使用 envtest 或一次性 Kind，另外两个依赖使用一次性
容器。这样既避免污染真实数据，也能把启动顺序固化为命令。

## 是否需要开发 VM

默认不需要。仓库的 devcontainer 使用独立 Docker-in-Docker daemon，Go 工具链、
Kind 节点和依赖容器都与宿主机环境隔离。宿主机只需要能够运行支持 privileged
container 的 Docker/Dev Container 环境。

只有以下情况才建议增加一台可随时重建的开发 VM：

- 宿主机不允许 privileged devcontainer；
- 无法安全使用 Docker socket 或 Docker-in-Docker；
- 本机地址段与 Kind/Docker 网络持续冲突；
- 需要长期运行、接近 homelab 网络和 TLS 配置的验收环境。

即使使用 VM，也应在 VM 内继续执行本文相同的容器化流程；不要把 VM 配置成第二套
手工维护的开发环境。

## 环境分层

| 层次 | Kubernetes | PostgreSQL / OpenBao | 用途 |
| --- | --- | --- | --- |
| 单元测试 | fake client | fake client | SQL 计划、状态转换和错误分类 |
| controller 集成测试 | envtest | fake adapter | CRD、watch、status、finalizer、ExternalSecret 对象 |
| adapter 集成测试 | 不需要 | Docker Compose | 真实协议、权限和幂等行为 |
| E2E | 一次性 Kind + ESO | Kind 内测试实例 | 凭据投射、TLS、完整网络和删除路径 |

envtest 只启动 API server 和 etcd，没有 kubelet、scheduler、ESO 或 controller-manager，
因此不能用它验证 Deployment、Pod 调度或 Service 网络。此类行为必须留给 Kind
E2E。

## 首次准备

推荐用支持 Dev Containers 的编辑器打开仓库。devcontainer 会提供 Go、Docker、
Kubebuilder、Kind 和 kubectl。脚本固定 Kubebuilder 4.15.0、Kind 0.33.0 和
kubectl 1.36.0，与当前脚手架和 Kubernetes Go module 对齐。容器启动后先确认：

```sh
go version
docker info
kubebuilder version
kind version
kubectl version --client
```

不要在仓库中保存真实 OpenBao Token、数据库密码或 kubeconfig。Compose 中的
`postgres-dev-only` 和 `dev-only-root-token` 是仅绑定回环地址、随容器销毁的公开
测试值，不得复制到其他环境。

## 日常开发的正确顺序

### 1. 生成并验证纯 Go/Kubernetes 部分

```sh
make manifests generate
make test
make lint
```

`make test` 会下载与 `go.mod` 中 Kubernetes minor 版本匹配的 envtest 二进制，
启动临时 API server/etcd，测试结束后自动关闭。

规格实现后，快速测试必须覆盖默认值/校验、Condition `observedGeneration`、两个 CR 的
status 状态机、不可变字段、extension 只追加、registry 所有权和外部错误分类。envtest 只断言 controller 创建了正确
的 ExternalSecret；它不能证明 ESO 已生成 Secret。

### 2. 启动 PostgreSQL/OpenBao adapter 依赖

只有开发 PostgreSQL/OpenBao adapter 或完整 reconcile 时才需要：

```sh
make dev-up
make dev-smoke
```

启动顺序由 Compose healthcheck 保证：

1. 创建独立 Compose 网络；
2. 启动 PostgreSQL 和 OpenBao；
3. 等待 PostgreSQL `pg_isready` 成功；
4. 等待 OpenBao `bao status` 成功；
5. smoke test 执行 `SELECT 1`；
6. smoke test 在 OpenBao dev server 默认的 `secret/` KV v2 mount 写入并读回测试管理
   凭据。

本机进程使用以下端点：

```text
PostgreSQL: postgresql://postgres:postgres-dev-only@127.0.0.1:15432/postgres
OpenBao:    http://127.0.0.1:18200
Token:      dev-only-root-token
```

若端口冲突，可以只对当前命令覆盖：

```sh
POSTGRES_DEV_PORT=25432 OPENBAO_DEV_PORT=28200 make dev-up
```

后续执行 `dev-smoke` 和 controller 时必须使用相同端口变量。

Compose 使用明文 PostgreSQL/OpenBao dev 模式，不覆盖生产 TLS 合同。DNS SAN、IP SAN、
Kubernetes auth、最小 policy 和 ESO 必须在 Kind E2E fixture 中验证。

### 3. 运行针对临时依赖的测试或 controller

adapter 集成测试通过独立 Make target 执行，不默认塞进快速单元测试：

```sh
make test-integration
```

该 target 会启动一次性 Compose 依赖，并通过 `POSTGRES_TEST_DSN` 把测试指向开发
PostgreSQL。registry 测试会删除并重建固定的测试 schema，因此禁止将该变量指向真实
homelab database。测试后运行 `make dev-down` 清理依赖。

Gitea Actions 的 job 本身运行在 Docker container 中，不能通过 `127.0.0.1` 访问
Docker host 上发布的 Compose 端口。CI 会暂时将 job container 加入 Compose 网络，
并通过 `postgres:5432` 运行集成测试；清理前先断开该网络，才能删除 Compose
network。本机执行仍使用默认的 `127.0.0.1:15432`。

本机运行 controller 时，先确认当前 kubeconfig 指向专用 Kind，而不是真实 homelab：

```sh
kubectl config current-context
make setup-test-e2e
kubectl config current-context
make install
make run
```

此时 controller 运行在开发容器内，可以直接访问上面的回环端口。若要验证 Tenant
Ready，专用 Kind 还必须安装 ESO、创建测试 ClusterSecretStore，并让 Kind workload
能够访问测试 OpenBao。不要把包含
`127.0.0.1` 端点的样例部署到 Kind 内；Pod 中的回环地址只指向 Pod 自身。

### 4. 清理

```sh
make dev-down
make cleanup-test-e2e
```

`dev-down` 会删除 Compose volume；所有数据库和 OpenBao dev 数据都应视为一次性。

## E2E 顺序

CI 的 E2E 与本机 `make run` 不同：controller 会作为 Pod 运行在 Kind 中。因此完整
E2E fixture 必须把测试 PostgreSQL、OpenBao 和 ESO 部署进 Kind，并等待依赖 Ready 后
再创建 `PostgreSQLInstance` 和 `PostgreSQLTenant`：

```text
创建 Kind
  -> 安装 CRD
  -> 部署 PostgreSQL/OpenBao fixture，签发含 DNS/IP SAN 的测试证书
  -> 安装 ESO，配置 OpenBao auth/policy 和 ClusterSecretStore
  -> 等待依赖 Ready 并写入测试管理凭据
  -> 构建并加载 controller image
  -> 部署 controller
  -> 创建 Instance
  -> 等待 Instance Ready
  -> 创建 Tenant
  -> 等待 Tenant Ready
  -> 验证 registry、PostgreSQL catalog、OpenBao KV、ExternalSecret 和 Secret
  -> 分别使用 DNS host 与 IP hostaddr 登录
  -> 删除 Tenant 并分别验证 Retain 与 Delete（含故障点重试）
  -> 删除 Kind
```

当前 controller 已实现第一条 Instance Ready 纵向链路：E2E fixture 在 Kind 内启动
PostgreSQL/OpenBao，配置 Kubernetes auth，验证管理凭据读取、PostgreSQL 登录、registry
migration 和 Instance Ready。Tenant provisioning、ESO、TLS DNS/IP SAN 与删除路径仍需
后续纵向切片覆盖，不能从 Instance Ready 推断这些合同已经通过。

## 测试数据与泄漏检查

- 只使用显眼的固定 canary 测试密码，测试后扫描日志、Event、Condition、metrics 和
  CR dump，出现 canary 即失败。
- 每个最终一致性阶段都注入一次中断，重启后验证密码不变且阶段只向前推进。
- 清空、落后或伪造超前的 `status.phase` 后验证它能从外部事实保守恢复/纠正，且不会
  跳过任何回读。
- 为未知同名 database、role、Bao record 和伪造 COMMENT 分别构造 Conflict。
- Delete 在每个外部删除步骤失败后重试，确认未误删非当前 UID 资源。
- 迁移测试按 [`migration.md`](migration.md) 完整执行，不以单纯 `pg_restore` 成功代替
  应用读写和回滚验证。

## 故障排查

查看依赖状态与日志：

```sh
docker compose -f hack/dev/compose.yaml ps
docker compose -f hack/dev/compose.yaml logs postgres openbao
```

如果 envtest 报端口监听失败，通常是当前执行环境禁止监听回环端口，而非 controller
失败；在 devcontainer 或允许本机监听的 runner 中执行。若 Kind 无法创建，先运行
`docker info`，确认当前用户可以访问 devcontainer 内的 Docker daemon。
