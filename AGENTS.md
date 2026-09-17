# Agent Notes

- 本仓库是 ddupan.top homelab 的内部基础设施控制平面，不以通用发行版为初期目标。
- 提交、文档和代码注释优先使用中文；公共 API 标识符和代码遵循对应语言惯例。
- 不要重新实现已有成熟后端的核心能力；新增实现前先确认能否通过稳定 API 进行薄适配。
- 在自行设计通用控制循环、资源生命周期、调度、回收或故障恢复机制前，先调查 Kubernetes
  核心及成熟开源 controller/operator 的实现；优先复用经过验证的模式，并记录有意偏离的
  理由。
- 不要引入统一包装所有能力的 Application CRD；应用应直接组合正交的平台资源。
- 所有 controller 必须考虑幂等、observe、finalizer、conditions、删除策略和恢复行为。
- Secret、token、kubeconfig 及具体生产凭据不得提交到仓库。
- `deploy/dev/` 与 `deploy/prod/` 使用相同制品；生产版本只通过 promotion 更新。
- 内部专用不构成降低测试、版本、恢复、安全和可审计要求的理由。
