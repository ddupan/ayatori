# ADR-0004：采用可拆分的模块化 Controller 架构

- 状态：Accepted
- 日期：2026-09-17

## 背景

Ayatori 将逐步提供任务执行、虚拟机、数据库、负载均衡、对象存储、托管 Kubernetes 和
人工操作等领域能力。这些能力拥有不同的生命周期、权限、网络位置和后端实现，若直接在
一个 controller 中相互调用并共享内部状态，后续接入新后端时容易形成代码耦合，也难以
独立扩缩容、发布和隔离故障。

另一方面，在首个领域能力完成前就拆分为多个独立服务，会立即引入镜像与部署管理、服务
间认证、版本兼容、分布式观测和故障处理成本，而这些成本尚未由真实运行需求证明。

Run 是首个领域对象。它既是最初的单次任务调度 API，也用于验证领域状态机与 Kubernetes
Job、OpenSandbox 和未来执行后端之间的适配边界。

## 决策

Ayatori 初期采用模块化单体：多个领域 controller 可以编译进同一个 controller manager，
但代码、API 所有权和依赖方向按照未来可独立部署的服务边界组织。

### 领域所有权

每个领域模块拥有自己的：

- CRD 与 API 版本；
- reconciler 和状态机；
- finalizer、conditions、删除及恢复语义；
- backend adapter contract；
- 领域测试。

初始领域包括但不限于 execution、compute、database、networking 和 human operations。领域
模块不得导入其他领域的内部实现，也不得直接修改其他领域所拥有对象的 spec 或 status。

### 跨领域协作

领域间的持久协作通过 Kubernetes API 对象、typed reference、owner reference 和
conditions 完成，而不是通过进程内 service 方法调用。

例如虚拟机完成创建后需要执行 provision，应创建或引用 Run 对象并观察其状态，而不是
直接调用 execution 模块的内部 Go API。更高层的资源组合由专门的领域对象或 GitOps 声明
完成，不引入统一包装所有底层能力的 Application controller。

该约束使 controller 即使暂时位于同一进程，其通信、失败和恢复行为仍与未来分进程部署
一致。

### Backend adapter

领域状态机只依赖本领域定义的最小 adapter contract，不依赖 Kubernetes Job、Crossplane、
OpenTofu、OpenSandbox 或具体厂商 SDK 类型。Adapter 负责：

- 幂等地确保期望外部资源存在；
- 观察并翻译外部状态；
- 执行取消、删除或 orphan 策略；
- 返回稳定的外部引用、能力和分类错误。

不同领域分别定义 adapter contract，不建立能包装所有资源类型的万能 Provider 接口。
后端不具备的能力必须显式报告，不通过虚假的统一语义隐藏差异。

Crossplane、OpenTofu 和 Ansible 等系统是可替换的 backend 实现或执行机制，不构成 Ayatori
面向用户的稳定 API。它们的 ProviderConfig、Workspace、playbook 等实现细节不得直接成为
领域 API 的必填契约。

### 共享代码

默认不建立跨领域的万能 service、repository 或 util 层。共享并非禁止，但必须来自已经
存在的真实重复，并同时满足：

1. 至少有两个真实调用者；
2. 重复的行为和语义一致，而不只是代码形状相似；
3. 调用方对其生命周期和预期演化方向一致；
4. 共享包不依赖任何具体领域的内部包。

适合共享的通常是机制，例如 conditions 操作、typed reference、重试退避、Secret 引用
读取、观测初始化和测试环境。领域状态机、资源策略、错误含义及后端选择不得为了消除少量
重复而抽取到共享层。

共享包使用表达具体职责的窄名称，初期保留在 `internal/shared/`。不建立内容持续膨胀的
通用 `util` 包，也不在实现稳定前承诺公共 Go API。

### 部署与拆分

controller manager 应支持按领域或 controller 集合选择性启用。初期可以使用一个二进制和
一个 Deployment；需要隔离时，优先使用同一制品部署为多个 Deployment。只有独立版本和
依赖关系成为真实需求后，才进一步拆分二进制或仓库。

出现下列任一情况时，应评估拆分部署：

- 需要独立扩缩容或显著不同的 reconcile 并发；
- 权限边界要求独立 ServiceAccount 与 RBAC；
- 后端只能从特定网络或节点访问；
- 沉重、不可信或冲突的 SDK 需要隔离；
- 一个领域的故障不应影响其他控制循环；
- 发布节奏或维护责任已经明确分离。

拆分不得改变领域 API，也不应将原本通过 API 对象完成的协作改为同步 RPC 链路。

## 结果

- 首个 Run 实现需要同时建立 execution 状态机和 adapter 边界，不能把 Kubernetes Job
  细节写入领域模型。
- 初期避免承担不必要的微服务运维成本，同时保留按权限、网络位置和故障域拆分的路径。
- Kubernetes API 成为领域间异步协作和恢复边界，领域 controller 必须正确处理最终一致性。
- 部分机械代码会有意保留重复，直到共享语义被至少两个真实实现证明。
- 代码评审需要检查跨领域 import、对象写入所有权和后端类型泄漏。
- 当 Run 同时拥有 Kubernetes Job 与 OpenSandbox adapter 后，应复审 adapter contract，确认
  它来自真实后端差异而非单一实现假设。
