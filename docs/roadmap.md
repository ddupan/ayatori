# 路线图

路线图按能够立即产生价值的纵向切片推进，而不是先构建完整通用框架。

## 0. 平台基础

- 建立 Dev 与 Prod API/control plane。
- 建立认证、RBAC、OpenBao 凭据和备份恢复。
- 定义 API、conditions、ownership 和 executor 公共约定。
- 建立不可变制品与 Dev 到 Prod promotion。

## 1. Job Service

- 实现最小 `Job` API。
- Kubernetes Pod executor。
- 统一日志、退出状态、超时、workspace、cache 与 artifact。
- 接入 Gitea Actions 和平台内部 IaC 执行。

## 2. OpenSandbox Executor

- 通过 OpenSandbox lifecycle 与 execd API 创建、执行和清理 sandbox。
- 支持强隔离任务、未知代码、嵌套容器和 AI agent。
- 增加交互式 `Sandbox` API、TTL、endpoint 与 snapshot。

## 3. Human Executor

- `ManualTask`、`TaskReport` 和版本化 Runbook。
- Telegram/Email 通知、领取、提醒和升级。
- 后端验证与上游 reconcile 恢复。

## 4. LBaaS

- Envoy 配置/xDS adapter。
- 健康检查与 GoBGP 路由宣告。
- 固定 VIP、listener/backend 引用和故障恢复。

## 5. Compute 与节点生命周期

- Proxmox VM adapter 与现有资源 adopt。
- ComputeNode 加入、drain 和 `SafeToRemove`。
- StorageClass、StoragePool、Volume 与迁移计划。
- 先支持人工磁盘迁移，再通过 Job executor 自动化。

## 6. 数据服务

- PostgreSQL database/role/credential。
- SeaweedFS bucket/policy/credential。
- DNS 与证书资源。

## 7. KaaS

- 采用成熟 hosted-control-plane 后端。
- 组合控制面、worker、LB、DNS、网络和凭据。
- 用户集群只暴露 worker node，控制面完全由平台托管。

## 首个业务里程碑

完成 Laptop Rebuild Readiness：

1. 临时节点加入。
2. laptop 上的 workload 被重建、迁移或形成可执行人工任务。
3. laptop 达到 `SafeToRemove=True`。
4. laptop 重装并重新加入。
5. 临时节点排空并安全退出。
