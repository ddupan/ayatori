# 总体架构

```text
Git (homelab-infra GitOps) / kubectl
          │
          ▼
 kube-apiserver + etcd + CRD（k0s controller-only，每环境一台 LXC）
   API / state coordination plane
          │
   ┌──────┴───────────────────┬──────────────────────┐
   │                          │                      │
ayatori-manager           lb-agent              Compute Agent
（中央 controllers）       （每台 LB 主机）       （每台宿主机，开发中）
   │                          │                      │
PostgreSQL / OpenBao      GoBGP 宣告 VIP          libvirt / liblxc
HAProxy Data Plane API    读本机 HAProxy 状态     LINSTOR / drbd-reactor
PowerDNS
```

已实现的是 ayatori-manager（Database、LoadBalancer 的 controller）与 lb-agent。Compute Agent 见
[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md)；Machine/Human executor（Pod、OpenSandbox、
Terraform、ManualTask）仍是[执行模型](../concepts/execution-model.md)中的设计，尚未实现。

Ayatori 复用 Kubernetes 的 API machinery，而不是 Kubernetes 的容器编排产品边界。
kube-apiserver 提供版本化对象、并发控制、list/watch、RBAC、admission 和审计；Ayatori
controller-manager 承担所有领域控制循环。Kubernetes workload 集群只是与 OpenSandbox、
Proxmox 等并列的 executor/backend，不默认等于运行 controller 的 management environment。

因此，领域 API 不得依赖“资源最终一定变成同集群原生对象”的假设。原生 Pod、Job、Service、
NetworkPolicy、namespace 共置与 owner reference 只有在 Kubernetes adapter 内才具有原生含义；
跨后端所需能力必须由领域模型显式定义。

内置 API 类型也按相同原则选择性复用。采用 `core/v1 Node` 作为计算节点 API 时，可以由
Ayatori Compute Agent 写入状态、由 Ayatori 自有调度 controller 消费；这不会引入 kubelet、
Pod 或 kube-scheduler。API contract、负责实现它的 controller/agent 和数据面是三个独立决策，
不得从其中一个自动推导另外两个。

## 控制面

Dev 与 Prod 使用独立的 Kubernetes API、数据库、身份和 controller 实例。两者可以
共享物理宿主，但不能只依赖 namespace 隔离 cluster-scoped API 与高权限凭据。每个环境是一台
Incus LXC 上的 API-only 控制面：k0s controller-only，ESO、Flux 与 ayatori-manager 以 podman
quadlet 运行，不运行 kubelet 与 Pod（[ADR-0013](../decisions/0013-api-only-control-plane-on-lxc.md)，
部署细节见 [deploy/README.md](../../deploy/README.md)）。

稀缺的物理基础设施可以共享，通过明确的资源范围区分环境。其他后端尽量使用独立数据库、
角色、地址池、DNS 空间与凭据：例如 LoadBalancer 的 VIP 池 Dev 为 `10.62.1.0/24`、Prod 为
`10.62.0.0/24`，OpenBao mount 与凭据前缀按环境推导。

节点侧 agent（lb-agent，以及规划中的 Compute Agent）不持有中央凭据，只在本机收敛。lb-agent
目前读取本地配置与 HAProxy 状态，不连接 API；加入控制面并 watch 资源见
[ADR-0022](../decisions/0022-lb-agent-joins-control-plane.md)（Proposed）。

## 进程内依赖边界

`cmd/main.go` 是 ayatori-manager 的入口，只调用 `internal/bootstrap.Run` 并处理退出状态。
`cmd/lb-agent` 是运行在 LB 主机上的独立二进制，与 manager 同一 release 发布；
`cmd/client-credentials` 在控制面主机上替 manager、Flux、ESO 与 kube-state-metrics 管理客户端证书：以 bootstrap token 首次签发并续签（ADR-0021）。
命令行参数、manager 创建、infra 与领域装配集中在 `internal/bootstrap`，
不在 `cmd` 平铺组件装配文件，也不为每个领域生成独立二进制。
Makefile 与 Dockerfile 均继续构建 `cmd/main.go`。

`Run` 只编排解析配置、创建 manager、显式装配组件、启动与退出清理。
`options.go` 组织配置和通用 flags；`manager.go` 处理 scheme、metrics、webhook、TLS 与探针；
`database.go`、`loadbalancer.go`、`openbao.go` 各自维护组件参数及装配细节。新增组件不向 `Run` 堆叠参数和内部
条件分支，也不为此引入插件注册框架。组件启动失败时释放已装配资源，正常退出则先停止
manager worker，再释放连接。

Database 使用 Wire 式的显式构造函数注入，目前手写装配，不引入 Wire 生成器、Dig/Fx
容器或运行时服务查找。`database_wiring.go` 集中展示后端 adapter → Reconciler 的对象构造；
`registerDatabaseControllers` 负责注册，返回的 `closeDatabaseConnections` 只在 worker 停止后
关闭 Instance 连接。controller 在启动时接收完整依赖，不在 Reconcile 或 SetupWithManager 中
补建 adapter；缺失依赖在注册时失败。

Reconciler 本身就是 Kubernetes adapter 并承担用例：直接用 `client.Client` 读写 CR，负责 CR
与领域对象的映射、外部操作顺序和 Conditions 呈现。domain 聚合根据持久状态与本轮事实决定
下一步，不依赖 Kubernetes 或后端。只为可能出现新实现的后端（PostgreSQL、凭据后端）在
controller 中按使用方需要声明接口；后端 adapter 只返回事实，并把错误归为领域结果分类。
Kubernetes 侧不再包一层 Repository/Presenter 接口。判断放置与命名规则见
[Database 领域模型](../database/domain-model.md#分层)。
单测检查 domain/controller 的依赖边界，并验证共享 writer、直连 reader 和后端的注入。

基础设施能力属于整个 controller-manager，不因首个消费者是 Database 就归入该领域。
`internal/infra/openbao` 管理官方 SDK client 的 TLS 配置、Kubernetes 认证及 token 生命周期，
`internal/infra/clientcert` 负责控制面客户端证书的 CSR 签发与续签（[ADR-0021](../decisions/0021-client-certificates-via-csr.md)），
二者都不依赖 Database 或其他产品领域。Bao client 默认禁用自动重试，写入结果不确定时由用例处理；
领域适配器不修改共享 client 的全局配置。Kubernetes 客户端、cache 和直连 reader 由 manager 管理；
启动入口负责装配与注入，不在领域适配器内重复创建客户端。

读写能力优先直接使用官方 `client.Reader`、`client.Client`、OpenBao KV API 等接口，
不为统一命名再包一层通用 reader/writer，也不引入全局注册中心。共享连接不表示扩大授权；
不同身份或权限边界仍由启动装配显式隔离。

业务结果语义归产品领域：Database 的七键凭据格式、UID 路径、禁止覆盖和不确定结果处理由 Database 维护，
它们不是公共 KV 存储的业务规则。Secret 管理凭据读取注入 `manager.GetAPIReader()`，
保持直连 API server、不缓存 Secret 内容的安全边界；资源写入复用 `manager.GetClient()`。

## 数据面

Ayatori 不承载或重新实现数据面。控制面故障只应阻止创建与变更，不应停止已有 VM、
负载均衡、数据库、对象存储或租户 Kubernetes 集群。

## 资源分层

平台只为已经验证的管理缺口提供正交产品能力。当前资源为：

- Database：`PostgreSQLInstance`、`PostgreSQLTenant`、`PostgreSQLDatabase`（已上线）
- LoadBalancer：`LoadBalancer`、`Backend`、`LoadBalancerHost`（v0.3.0 预发布）
- Compute：`LibvirtDomain`、`LXCContainer` 及其 pool/network/volume（Proposed，开发中）

`Run`/当前实验性的 `Job`、`ManualTask` 等可以作为控制面执行原语，但不是因为底层能运行 OCI
image 就自动成为面向使用者的计算产品。`DNSRecord`、`Credential`、`KubernetesCluster` 等只在
出现独立生命周期和真实消费者后加入；尤其 KaaS 不是预定终点。

只有具备独立领域生命周期的能力才应成为高阶资源。应用本身通过 GitOps 组合上述资源，
重复组合可通过模板或 Composition 表达，而不是扩展中央 Application API。

## 后端策略

优先级如下：

1. 直接调用成熟且可观察的后端 API。
2. 通过固定版本的 Terraform module 或 Ansible playbook 执行。
3. 仅在必要时使用 GitOps bridge。

需要在节点本机完成的工作由节点侧 agent 承担，而不是让中央 controller 持有所有宿主机的 root
凭据：lb-agent 在 LB 主机本地决定 VIP 宣告（[ADR-0017](../decisions/0017-lb-host-local-announcement.md)），
Compute Agent 在宿主机本地下发 libvirt/LXC 对象并负责冷启动（[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md)）。
agent 只按 API 资源与本机事实收敛，不能暴露任意 shell，也不能把 CLI 输出直接当作长期稳定协议。

Controller 无论采用哪种执行方式，都必须提供一致的 ownership、conditions、删除语义、
错误分类和恢复行为。

## API Server 边界

首选 kube-apiserver + CRD，持续复用其成熟的 watch、RBAC、版本化存储和 API 生态。
generic-apiserver 或聚合 API Server 不会减少领域 controller 的数量，只会把资源服务端、
兼容性和存储迁移责任转移给 Ayatori。只有 CRD 的限制已经形成可复现、不可通过合理领域建模
解决的阻碍时，才重新评估自建 API Server。
