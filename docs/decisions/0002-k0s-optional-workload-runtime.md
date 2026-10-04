# ADR-0002：采用 k0s 与可选工作负载运行时

- 状态：Accepted；Dev/Prod 默认 managed runtime 与集群内 Flux 的部分已被 [ADR-0013](0013-api-only-control-plane-on-lxc.md) 取代
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

定义两种受支持的运行 profile。

### API-only

```text
k0s controller-only
├── kube-apiserver
├── control-plane components
└── external controllers/processes
```

该 profile 不存在可调度 Node，不运行 Flux。它是可选的最小形态，用于 bootstrap、
恢复、API 开发和不需要 in-cluster controller 的特殊部署。

API-only 描述的是运行拓扑，而不是 API 功能子集。其他组件仍可以作为本地进程、systemd
服务、普通容器或远程 Job，通过 kubeconfig 连接该 API。开发者可以使用一个临时的
API-only 实例测试真实 CRD、admission、watch 和 reconcile 行为，而不必先准备 CNI、
worker 与完整 GitOps 环境。

API-only 的主要用途是 CI 中的临时集成环境：每个 job 启动独立的真实 API server，安装
CRD，运行待测试的 controller 二进制，完成后整体销毁。由于不启动 worker、CNI 和容器
runtime，它适合验证 API、watch、admission、reconcile、finalizer 和 status 行为，而不必
为每个测试任务建立完整 Kubernetes workload cluster。

它也可以用于隔离 API 实验、bootstrap 与恢复开发，但不构成独立的长期环境，也不是日常
本地开发的默认路径。日常开发进程直接连接 managed runtime Dev 的 API。Flux、in-cluster
service discovery、Pod 调度和 executor 集成仍然必须在 Dev 中验证。

### Managed runtime

```text
k0s controller --enable-worker
├── Kubernetes control-plane components
├── Flux
├── Ayatori controllers
├── admission webhooks
└── platform operators

optional execution worker pool
└── Job Pod executor workloads
```

该 profile 是默认部署形态。k0s controller 使用 `--enable-worker` 同时运行 kubelet 与
容器 runtime，并注册为带 control-plane label/taint 的 Node。Flux、Ayatori controllers、
admission webhook 和平台 operator 本身就是管理控制面的一部分，应当调度到这些节点。

平台组件必须显式容忍 control-plane taint，并使用 affinity 或 node selector 约束到控制
节点。控制节点不接受普通应用或 Job workload。Job Pod executor 使用独立 execution
worker pool；只有显式修改调度策略时才能在控制节点执行短期恢复或诊断任务。

启用 worker 只改变控制节点是否提供 Pod runtime，不改变 API、CRD、数据库与控制面身份。
API-only 与 managed runtime 因而是同一架构的两种运行配置，而不是两种平台实现。

## Bootstrap 边界

Flux 无法负责为承载自身的控制节点启用 worker runtime，因此以下部分位于 GitOps 闭环
之外：

- k0s controller 初始安装；
- 控制面数据库和 PKI 恢复；
- control node 的 `--enable-worker` 安装配置；
- Flux 首次 bootstrap。

这些步骤必须由可重复执行的 bootstrap 流程管理，例如固定版本的 Ansible、安装脚本或
人工 Runbook。Flux 成功运行后，in-cluster controller、策略和后续 worker 配置进入
GitOps 管理。

平台必须保留从 API-only 恢复到 managed runtime 的流程，避免 Flux 故障形成无法恢复的
自举循环。独立 execution worker 的加入与退出可以在平台正常运行后自动化。

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
- 默认不需要为 Flux 和 Ayatori controllers 建立独立 management worker VM。
- 控制节点同时承担平台控制 workload，需要设置资源预留、taint 和调度约束。
- Job 等非控制 workload 仍需要独立 execution worker 或外部执行后端。
- bootstrap 层不能完全由 Flux 自我管理，必须独立记录并验证恢复流程。

## 参考

- [k0s Architecture](https://docs.k0sproject.io/stable/architecture/)
- [k0s Configuration Options](https://docs.k0sproject.io/stable/configuration/)
- [Flux installation](https://fluxcd.io/flux/installation/)
