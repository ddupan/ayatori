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

- [产品愿景](docs/vision.md)
- [总体架构](docs/architecture/overview.md)
- [API 设计原则](docs/concepts/api-design.md)
- [执行模型](docs/concepts/execution-model.md)
- [环境与发布](docs/concepts/environments.md)
- [路线图](docs/roadmap.md)
- [ADR-0001：采用 Kubernetes API machinery 作为状态协调平面](docs/decisions/0001-kubernetes-api-machinery.md)
- [ADR-0002：采用 k0s 与可选工作负载运行时](docs/decisions/0002-k0s-optional-workload-runtime.md)
- [ADR-0003：直接连接 Dev API 的开发循环](docs/decisions/0003-dev-api-development-loop.md)
- [ADR-0006：按实际管理缺口扩展资源 API](docs/decisions/0006-demand-driven-resource-scope.md)
- [ADR-0007：复用 Node API 建立按需实现的 Compute 能力](docs/decisions/0007-compute-node-and-vm-boundary.md)
- [ADR-0008：将 PostgreSQL Tenant Operator 合并为 Ayatori Database 模块](docs/decisions/0008-merge-postgresql-tenant-operator.md)
- [ADR-0009：分离 Database 资源与 Tenant 申请](docs/decisions/0009-database-resource-and-claim.md)
- [ADR-0013：Dev/Prod 以 LXC 上的 API-only 控制面运行](docs/decisions/0013-api-only-control-plane-on-lxc.md)
- [部署与发布](deploy/README.md)

## 当前状态

Ayatori 处于设计与早期实现阶段。当前使用 Job controller 验证第一个完整控制循环与 adapter
边界；它不是通用 Job Service 或 FaaS 产品承诺。首批实际产品方向是 Database 与 LoadBalancer；
Bucket/Object Storage 由 CI Terraform 管理，不进入 Ayatori（ADR-0011）。
Database 模块已在 Dev 与 Prod 上线（v0.1.x，API-only 控制面，ADR-0013），通过 homelab-infra 的
GitOps 目录申请数据库。
