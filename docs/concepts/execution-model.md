# 执行模型

Ayatori 将执行者统一视为具有不同能力与延迟特性的 executor。

```text
Action
├── Machine executor
│   ├── Kubernetes Pod
│   ├── OpenSandbox
│   ├── Terraform
│   └── Ansible
└── Human executor
    └── Versioned Runbook
```

## Job 与 Sandbox

`Job` 表达一次有限时长、有退出结果的批处理。第一执行后端是 Kubernetes Pod，第二
执行后端通过 OpenSandbox API 获得强隔离环境。调用者表达资源、隔离与 capability
需求，由调度策略选择后端。

`Sandbox` 表达带 TTL 的交互环境，可提供 shell、文件、快照或临时 endpoint。它与
`Job` 共享镜像、资源、网络、凭据和回收能力，但具有不同生命周期，不合并为带大量
互斥字段的上帝资源。

## 人工执行

无法自动化的步骤使用 `ManualTask` 建模：

```text
Pending → Claimed → InProgress → Reported → Verifying → Succeeded
```

通知 controller watch 待处理任务，并发送到 Telegram、Email 或 UI。人依据绑定版本的
Runbook 操作后提交 `TaskReport`；人只报告执行结果，不直接宣告系统状态成功。

验证 controller 必须 observe 后端并验证证据，验证成功后更新 status，上游 reconcile
自然继续。人工步骤因此是高延迟异步处理，而不是控制面之外的黑洞。

## 自动化演进

人工任务未来可以替换为机器 Job，但上游工作流不应改变：

```text
executor: Human  →  executor: Job
```

平台优先做到任务可建模、可通知、可追踪、可验证和可恢复，再逐步降低人工参与。
