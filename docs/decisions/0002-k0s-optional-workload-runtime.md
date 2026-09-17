# ADR-0002：采用 k0s 与可选工作负载运行时

- 状态：Accepted
- 日期：2026-09-17

## 背景

Ayatori 需要 Kubernetes API machinery 提供 CRD、watch、RBAC、admission 和 controller
运行模型，但它并不天然要求在同一环境中调度容器 workload。最小控制面应能够仅作为
基础设施 API 使用。

k0s 的 controller 角色默认不运行 kubelet 或容器运行时，因此控制节点不可被用户调度。
这符合将 Kubernetes API 作为独立控制面运行时的需求。

然而 Ayatori 的完整 GitOps 模式需要运行 Flux controllers；Ayatori controllers、
Crossplane 组件以及 Kubernetes Pod executor 也适合以 Pod 交付。因此正常运行形态仍然
需要工作负载调度能力。

## 决策

Ayatori 采用 k0s 作为 Kubernetes API/control-plane 发行版，并将工作负载运行时视为
可选部署组件，而不是 API 控制面的固有组成。

控制节点始终保持 controller-only：

- 不运行 kubelet；
- 不运行容器 runtime；
- 不注册为 Node；
- 不承载 Flux、Ayatori controller 或用户 Job。

通过向集群加入独立 worker 启用工作负载调度。加入或移除 worker 不改变 API、CRD、
数据库和控制节点身份。

定义两种受支持的运行 profile。

### API-only

```text
k0s controller-only
├── kube-apiserver
├── control-plane components
└── external controllers/processes
```

该 profile 不存在可调度 Node，不运行 Flux。它用于最小控制面、bootstrap、恢复、API
开发和不需要 in-cluster controller 的特殊部署。

### Managed runtime

```text
k0s controller-only
        │
        ├── management worker pool
        │   ├── Flux
        │   ├── Ayatori controllers
        │   └── platform operators
        │
        └── execution worker pool（可选）
            └── Job Pod executor workloads
```

该 profile 是 Dev 与 Prod 的正常运行形态。它在 API-only 基础上加入至少一个 worker，
从而启用完整 GitOps 和 in-cluster controllers。

management worker 使用专用 label、taint、toleration 和 resource reservation，默认不接受
普通应用 workload。Job Pod executor 优先使用独立 execution worker pool；资源受限的
Dev 环境可以暂时合并两类 worker，但必须保留调度约束。

## Bootstrap 边界

Flux 无法负责创建承载自身的第一个可调度 worker，因此以下部分位于 GitOps 闭环之外：

- k0s controller 初始安装；
- 控制面数据库和 PKI 恢复；
- 第一个 management worker 的加入；
- Flux 首次 bootstrap。

这些步骤必须由可重复执行的 bootstrap 流程管理，例如固定版本的 Ansible、安装脚本或
人工 Runbook。Flux 成功运行后，in-cluster controller、策略和后续 worker 配置进入
GitOps 管理。

平台必须保留从 API-only 恢复到 managed runtime 的流程，避免 Flux 故障形成无法恢复的
自举循环。

## 工作负载范围

引入 worker 不改变 Ayatori 的定位。该集群是专用 management environment，而不是通用
应用集群。允许的 workload 默认限于：

- Flux 与平台 controller/operator；
- admission webhook 和必要的控制面辅助组件；
- Job Service 选择 Pod executor 后创建的受管任务；
- 平台恢复和诊断所需的短期 workload。

普通长期应用仍运行在其所属 workload cluster、VM 或其他后端。

## 结果

- 保留纯 API 控制面的简洁性与恢复价值。
- 能够使用 Flux 完成平台组件的 GitOps 管理。
- 能够直接复用 Kubernetes 生态中的 operator，而无需改写为 systemd 服务。
- 控制节点不会因为加入 workload runtime 而成为可调度节点。
- Dev 与 Prod 需要额外规划少量 management worker 容量。
- bootstrap 层不能完全由 Flux 自我管理，必须独立记录并验证恢复流程。

## 参考

- [k0s Architecture](https://docs.k0sproject.io/stable/architecture/)
- [k0s Configuration Options](https://docs.k0sproject.io/stable/configuration/)
- [Flux installation](https://fluxcd.io/flux/installation/)
