# 路线图

路线图按能够立即产生价值的纵向切片推进，而不是先构建完整通用框架。

## 0. 平台基础

- 建立 Dev 与 Prod API/control plane。
- 建立认证、RBAC、OpenBao 凭据和备份恢复。
- 定义 API、conditions、ownership 和 executor 公共约定。
- 建立不可变制品与 Dev 到 Prod promotion。

## 1. Controller 纵向验证切片

- 使用当前最小 `Job` API 验证 watch、状态机、finalizer、取消、TTL、external reference 与
  backend adapter。
- Kubernetes executor 不能与 management API client 或同集群 namespace 语义绑定。
- 验证完成后，将可复用机制收敛为内部 `Run`/execution 能力；不把这一切片扩展为 FaaS、
  Cloud Run 或通用 Job Service。

## 2. 首批资源产品

- `Database`：PostgreSQL database、role、credential 与回收。
- `LoadBalancer`：Envoy 配置/xDS、健康检查、固定 VIP 与 GoBGP 路由宣告。
- `Bucket`：SeaweedFS bucket、policy、credential 与删除策略。
- 按纵向价值选择先后，不为三者预先建立统一 provider 框架。

## 3. Human Executor 与延迟自动化

- `ManualTask`、`TaskReport` 和版本化 Runbook。
- Telegram/Email 通知、领取、提醒和升级。
- 后端验证与上游 reconcile 恢复。

## 4. Compute 与节点生命周期

- 建立稳定的 `VirtualMachine` 北向 API，并支持现有资源 adopt。
- 南向按能力组合 Proxmox API、节点受限 Agent/CLI 与 `ManualTask`，不假设 Proxmox API 完整。
- ComputeNode 加入、drain 和 `SafeToRemove`。
- StorageClass、StoragePool、Volume 与迁移计划。
- 先支持人工磁盘迁移，再按实际收益自动化。

## 5. 条件性扩展

- OpenSandbox/microVM 可以作为内部 Run backend，但不由此产生 FaaS 产品承诺。
- DNS、证书和 Credential 只有在跨系统协调收益明确时形成独立资源。
- KaaS 只有出现托管控制面、租户隔离或频繁集群生命周期的真实需求时才立项。

### KaaS 候选方案

- 采用成熟 hosted-control-plane 后端。
- 组合控制面、worker、LB、DNS、网络和凭据。
- 用户集群只暴露 worker node，控制面完全由平台托管。
- 本节记录候选实现边界，不构成路线图承诺。

## 首个业务里程碑

完成 Laptop Rebuild Readiness：

1. 临时节点加入。
2. laptop 上的 workload 被重建、迁移或形成可执行人工任务。
3. laptop 达到 `SafeToRemove=True`。
4. laptop 重装并重新加入。
5. 临时节点排空并安全退出。
