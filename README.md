# Ayatori

> Infrastructure, woven by intent.
> 意図から、インフラを編む。

Ayatori 是 `ddupan.top` homelab 的内部基础设施控制平面。它以 Kubernetes API
作为统一资源模型，通过薄适配器组合成熟后端，并让机器执行器与人工操作共同推动
实际状态持续收敛到声明的期望状态。

本仓库公开源代码，但当前只面向一个确定的内部环境。它不是通用私有云发行版，暂不
承诺开箱即用、后端可替换性或面向第三方的兼容性。

## 核心原则

- 内部专用只缩小需求与兼容范围，不降低软件工程质量。
- 优先采用可以立即投入使用的成熟后端，不重新实现复杂的数据面。
- 平台统一入口、策略、组合、状态与生命周期；后端保留领域实现。
- 应用通过 GitOps 直接组合正交的平台原语，不引入 `ApplicationService` 上帝控制器。
- Pod、OpenSandbox、Terraform、Ansible 和人都可以成为 executor。
- 暂时不能自动化的操作必须被建模、追踪并验证，而不是遗落在文档和记忆中。
- 控制面中断不得破坏已运行的数据面。

## 文档

总体设计：

- [产品愿景](docs/vision.md)
- [总体架构](docs/architecture/overview.md)
- [API 设计原则](docs/concepts/api-design.md)
- [执行模型](docs/concepts/execution-model.md)
- [环境与发布](docs/concepts/environments.md)
- [路线图](docs/roadmap.md)
- [部署与发布](deploy/README.md)

领域文档：

- [Database](docs/database/README.md)
- [LoadBalancer 领域模型](docs/loadbalancer/domain-model.md)
- Compute：PoC 记录见 [docs/compute](docs/compute/)
- [Job API 草案](docs/api/job-v1alpha1.md)（未实现，见下文）

架构决策（ADR）：

| ADR | 标题 | 状态 |
|---|---|---|
| [0001](docs/decisions/0001-kubernetes-api-machinery.md) | 采用 Kubernetes API machinery 作为状态协调平面 | Accepted |
| [0002](docs/decisions/0002-k0s-optional-workload-runtime.md) | 采用 k0s 与可选工作负载运行时 | 部分被 0013 取代 |
| [0003](docs/decisions/0003-dev-api-development-loop.md) | 直接连接 Dev API 的开发循环 | 部分被 0013 取代 |
| [0004](docs/decisions/0004-modular-controller-boundaries.md) | 可拆分的模块化 Controller 架构 | Accepted |
| [0005](docs/decisions/0005-ephemeral-job-retention.md) | Job 使用短生命周期控制对象与 TTL 回收 | Accepted |
| [0006](docs/decisions/0006-demand-driven-resource-scope.md) | 按实际管理缺口扩展资源 API | Accepted |
| [0007](docs/decisions/0007-compute-node-and-vm-boundary.md) | 复用 Node API 建立按需实现的 Compute 能力 | Accepted，VM API/Agent/HA 部分由 0015 修订 |
| [0008](docs/decisions/0008-merge-postgresql-tenant-operator.md) | 将 PostgreSQL Tenant Operator 合并为 Database 模块 | Accepted |
| [0009](docs/decisions/0009-database-resource-and-claim.md) | 分离 Database 资源与 Tenant 申请 | Accepted |
| [0010](docs/decisions/0010-credential-delivery-ends-at-openbao.md) | Tenant 凭据交付止于 OpenBao 位置公布 | Accepted |
| [0011](docs/decisions/0011-bucket-managed-by-ci-terraform.md) | Bucket 由 CI Terraform 管理 | Accepted |
| [0012](docs/decisions/0012-l4-loadbalancer.md) | 以独立 L4 LoadBalancer 统一 VIP 与入口 | Accepted，宣告部分被 0017 取代 |
| [0013](docs/decisions/0013-api-only-control-plane-on-lxc.md) | Dev/Prod 以 LXC 上的 API-only 控制面运行 | Accepted |
| [0014](docs/decisions/0014-kaas-cross-backend-node-supply.md) | KaaS 以 CAPI 管理集群，Ayatori 供应跨后端节点 | Proposed |
| [0015](docs/decisions/0015-libvirt-lxc-compute-backend.md) | 自建 libvirt/LXC 计算后端，冷启动由节点自治 | Proposed |
| [0016](docs/decisions/0016-instance-node-services.md) | 实例的节点侧服务 | Proposed |
| [0017](docs/decisions/0017-lb-host-local-announcement.md) | VIP 宣告由 LB 主机本地决定 | Accepted |
| [0018](docs/decisions/0018-lb-backend-and-host-resources.md) | 后端与 LB 主机成为 API 资源 | Accepted |
| [0021](docs/decisions/0021-client-certificates-via-csr.md) | 控制面客户端凭据以 CSR 签发与自行续签 | Accepted（续签待到期观察） |
| [0022](docs/decisions/0022-lb-agent-joins-control-plane.md) | LB agent 加入控制面，watch 资源并在本机收敛 | Proposed |
| [0023](docs/decisions/0023-lb-endpoint-health-in-status.md) | LoadBalancer 与 Backend 的 status 呈现后端健康 | Proposed |
| [0025](docs/decisions/0025-lb-agent-bgp-graceful-restart.md) | LB agent 开启 BGP graceful restart | Proposed |

0019、0020 编号由 `dev/compute` 分支上的计算后端 ADR 占用，尚未合入 main。

## 当前状态

Dev 与 Prod 均运行 v0.3.0-rc.3（API-only 控制面，ADR-0013；由 Chef cookbook `homelab_ayatori`
部署）。各模块进度：

| 模块 | API group | 状态 |
|---|---|---|
| Database | `database.ayatori.ddupan.top` | 已上线（v0.1.0 起）。homelab-infra 的 GitOps 目录申请 PostgreSQLTenant/Database，凭据交付到 OpenBao |
| LoadBalancer | `network.ayatori.ddupan.top` | v0.3.0 预发布，Prod 已承载 sandbox 入口。HAProxy（Data Plane API）+ lb-agent 进程内 GoBGP 宣告 VIP，可选 PowerDNS 名称 |
| Compute | `compute.ayatori.ddupan.top`（规划） | ADR-0014/0015/0016 Proposed，PoC 01–15 已完成，Compute Agent 在 `dev/compute` 分支开发中 |
| Job | `execution.ayatori.ddupan.top` | 只有 API 与 CRD；controller 未合入，暂停 |

Bucket/Object Storage 由 CI Terraform 管理，不进入 Ayatori（ADR-0011）。
