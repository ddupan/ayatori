# Agent Notes

- 本仓库是 ddupan.top homelab 的内部基础设施控制平面，不以通用发行版为初期目标。
- 提交、文档和代码注释优先使用中文；公共 API 标识符和代码遵循对应语言惯例。
- 不要重新实现已有成熟后端的核心能力；新增实现前先确认能否通过稳定 API 进行薄适配。
- 不要按传统私有云或公有云产品清单推导 Ayatori 应实现的资源。新增北向 API 前必须证明 homelab
  存在真实、重复的管理缺口，现有成熟 API/IaC 不能提供足够的生命周期、状态或权限体验；“后端
  能做到”或“其他云平台提供”本身不是产品需求。
- 当前已确认的首要产品方向是 Database、LoadBalancer、Bucket/Object Storage；VirtualMachine
  也具有明确价值，但南向实现较重。Run/Job 是验证 controller 与 adapter 的内部执行切片，不应
  自动演化为 FaaS、Cloud Run 或应用托管产品。KaaS 仅在出现真实需求时评估，不是必达终点。
- Ayatori 复用 Kubernetes 的核心目标是 API machinery：对象存储与并发控制、list/watch、
  informer、RBAC、admission、版本化 API 和审计；不要据此推断 Ayatori 是 Kubernetes
  workload 平台，也不要默认复用 Kubernetes 的调度与数据面语义。
- 复用 Kubernetes 内置资源只表示采用其 API contract，不表示必须运行或模拟上游实现组件。
  例如 Ayatori Compute Agent 可以直接实现 `core/v1 Node` 与 Lease 的状态语义，Ayatori
  controller 可以自行消费 Node；不得仅因使用 Node 推导必须引入 kubelet、Pod、CRI、
  kube-scheduler 或 kube-controller-manager。对每个复用资源分别明确 producer、consumer、
  ownership 与实际采用的字段语义。
- kube-apiserver 是 Ayatori 的 API 与状态协调平面，不是领域调度器。资源的调度、生命周期、
  故障恢复、垃圾回收和后端收敛由 Ayatori controllers 实现；新增能力前应明确其属于 API
  machinery、Ayatori 领域控制循环还是外部 backend，避免把职责放错层。
- Kubernetes、OpenSandbox、Proxmox 等均是 Ayatori 的可替换 backend/executor。除管理组件自身
  的部署外，不得仅因 controller 运行在 Kubernetes 中，就把原生 Pod、Job、Service、
  NetworkPolicy、owner reference 或同 namespace 行为作为领域 API 的隐含语义；需要这些能力时
  必须由 adapter 契约显式表达，并考虑后端位于其他集群或完全不是 Kubernetes 的情况。
- 不要以减少自有 controller 数量为目的引入 generic-apiserver、聚合 API Server 或自行实现
  API Server。只有 CRD/kube-apiserver 在存储、API 语义或扩展能力上形成已验证的阻碍时，才评估
  接管 watch、RBAC、版本兼容和存储迁移等复杂度；controller 工作本身不会因此消失。
- 在自行设计通用控制循环、资源生命周期、调度、回收或故障恢复机制前，先调查 Kubernetes
  核心及成熟开源 controller/operator 的实现；优先复用经过验证的模式，并记录有意偏离的
  理由。
- 不要引入统一包装所有能力的 Application CRD；应用应直接组合正交的平台资源。
- Proxmox VM 的北向管理不能假定单一 API 覆盖完整生命周期。允许按能力组合 Proxmox API、节点
  上的受限强类型 Agent/CLI 操作和 ManualTask；节点 Agent 不得退化为无版本契约的任意远程 shell。
- 所有 controller 必须考虑幂等、observe、finalizer、conditions、删除策略和恢复行为。
- Ayatori 会联动 Kubernetes API、虚拟化、存储、网络及其他外部控制面；集成测试是功能完成
  标准的一部分，不得仅凭 fake client 或 mock 测试宣告 controller、adapter 或生命周期变更完成。
- 测试应按风险分层：纯领域规则使用快速单元测试；API schema、CEL、status subresource、
  watch/cache、owner reference 和 reconcile 事件链使用 envtest；需要 scheduler、kubelet、网络、
  存储或真实后端行为的路径在 Dev 集群或对应后端环境执行端到端测试。
- fake client 适合穷举状态机和错误分支，但它不会完整执行 API server defaulting、validation、
  resourceVersion、garbage collection 或新版 Kubernetes 约束；涉及这些语义时必须增加真实 API
  server 测试。跨 adapter 的共同契约应使用同一套 contract tests，避免各实现产生语义漂移。
- 集成测试必须覆盖正常路径以及幂等重试、controller 重启、依赖稍后出现、删除/finalizer、
  后端结果不确定和并发竞态等恢复路径；无法在当前层测试的部分要明确记录由哪一层验证。
- Secret、token、kubeconfig 及具体生产凭据不得提交到仓库。
- `deploy/dev/` 与 `deploy/prod/` 使用相同制品；生产版本只通过 promotion 更新。
- 内部专用不构成降低测试、版本、恢复、安全和可审计要求的理由。
