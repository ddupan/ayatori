# ADR-0003：直接连接 Dev API 的开发循环

- 状态：Accepted；Flux 与 in-cluster controller 部署的部分已被 [ADR-0013](0013-api-only-control-plane-on-lxc.md) 取代
- 日期：2026-09-17

## 背景

如果每次 controller 代码变更都必须构建镜像、推送 registry、等待集群拉取并 rollout，
开发反馈会被制品发布流程主导。Ayatori 的开发机与 Dev 位于可互通的内部网络，不需要为
获得快速反馈而复制一套本地集群。

Dev 本身就是可破坏的集成与实验环境，允许随时部署、停止和删除 workload。完整 GitOps
是 Prod 的运行要求，也是 Dev 的最终集成验证目标，但不是每一个开发中组件在每一个时刻
都必须采用的交付方式。

## 决策

日常 controller 开发默认直接连接 Dev API：

```text
Dev k0s API
    ↑ kubeconfig / internal network
developer workstation
    └── locally built controller process
```

修改代码后只需重新编译或由 file watcher 重启本地进程。只有进入真实部署集成验证时，
才构建不可变镜像并交给 Dev GitOps 部署。

API-only profile 仍然保留，但主要用于 CI 中按 job 创建的临时集成环境，也可以服务于
隔离 API 实验、bootstrap 和恢复开发；它不是为了每位开发者复制一个长期 local
environment。

CI 根据测试范围选择环境：

- controller/API 集成测试使用临时 API-only k0s；
- 只涉及纯逻辑的测试不启动 Kubernetes；
- 需要 Pod、Service、CNI、Flux 或 executor 的端到端测试部署到 managed runtime Dev，
  或在具备相应能力的临时执行环境中运行。

## 单写入者约束

本地 controller 接管某类资源前，必须暂停对应的 in-cluster controller。若该 Deployment
由 Flux 管理，应暂停对应 Flux reconciliation 或使用明确的 Dev deployment mode，避免
Flux 立即恢复副本数。

本地与 in-cluster controller 使用相同的 leader-election lease 和 identity domain，作为
防止意外双写的最后一道保护；正常流程仍应先停止集群内实例，而不是依赖抢占 leader。

Dev controller 的凭据只允许操作 Dev API 与 Dev 后端资源，不得拥有 Prod 权限。

## GitOps 范围

Dev 可以只用 GitOps 管理稳定底座：

- k0s 之上的基础 namespace、RBAC 与 policy；
- Flux 自身；
- 已稳定的 Ayatori controller/operator；
- 共享的观测和开发依赖。

活跃开发中的 controller 可以暂不纳入 Flux，或以可暂停的独立 Kustomization 管理。进入
集成验证时必须恢复以下真实部署路径：

```text
source commit
→ immutable image
→ Dev GitOps deployment
→ RBAC/network/service/admission integration tests
```

Prod 只接收经过该路径验证的制品，不运行来自开发机的进程。

## Admission 策略

Ayatori 尽量避免 webhook，降低控制面可用性依赖和本地开发复杂度。校验按以下顺序实现：

1. CRD OpenAPI schema、枚举、范围与结构化默认值；
2. CEL validation 与 `ValidatingAdmissionPolicy`；
3. controller reconcile 中依赖后端状态的异步校验，并通过 conditions 报告；
4. 只有无法由上述方式正确表达的同步校验才使用 validating webhook。

不使用 mutating webhook 隐藏重要默认行为。API version conversion webhook 只在真实版本
演进需要时引入。

确实需要调试 webhook 时，Dev apiserver 可以通过内部网络直接访问开发机上的 HTTPS
endpoint；这不要求 controller 镜像先部署进集群。最终证书、Service 和 failure policy
仍必须在 Dev GitOps 集成阶段验证。

## 结果

- 大多数 reconcile 开发不再经历镜像 push/pull 循环。
- 所有开发者共享真实 Dev API 和后端边界，减少本地环境漂移。
- Dev 不保证持续保持完整 GitOps 状态，但必须能够恢复到完整集成形态。
- 需要明确的暂停、接管和恢复流程，避免本地与集群 controller 双写。
- webhook 被限制为必要机制，而不是默认扩展方式。
