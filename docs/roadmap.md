# 路线图

路线图按能够立即产生价值的纵向切片推进，而不是先构建完整通用框架。各阶段标注截至 2026-11 的
实际进度。

## 0. 平台基础（已完成）

- Dev 与 Prod 各一台 Incus LXC 上的 API-only 控制面（2026-10-04，[ADR-0013](decisions/0013-api-only-control-plane-on-lxc.md)），
  由 Chef cookbook `homelab_ayatori` 部署。
- 控制面组件以 CSR 签发并自行续签的客户端证书访问 API（[ADR-0021](decisions/0021-client-certificates-via-csr.md)）；
  manager 与 ESO 以 Kubernetes auth 登录 OpenBao。
- 一个 git tag 即一个 release，Dev 验收后 promotion 到 Prod，见[环境与发布](concepts/environments.md)。
- 备份：每日 `k0s backup` 写入独立卷；尚无离机备份。

## 1. Controller 纵向验证切片（暂停）

原计划先用最小 `Job` API 验证 watch、状态机、finalizer、取消、TTL 与 backend adapter。实际上
Database 与 LoadBalancer 先成为完整控制循环，`Job`/`JobClass` 目前只有 API 与 CRD，Kubernetes
executor 的 controller 未合入 main。只有出现真实的执行需求时才恢复；届时仍不扩展为 FaaS、
Cloud Run 或通用 Job Service。

## 2. 首批资源产品

- `Database`（已上线）：PostgreSQLInstance、PostgreSQLTenant 与 PostgreSQLDatabase，管理 database、
  role、凭据与回收，凭据交付止于 OpenBao（[ADR-0009](decisions/0009-database-resource-and-claim.md)、
  [ADR-0010](decisions/0010-credential-delivery-ends-at-openbao.md)）。
- `LoadBalancer`（v0.3.0 预发布）：LoadBalancer、Backend 与 LoadBalancerHost；数据面是 HAProxy
  （Data Plane API 下发配置），每台 LB 主机上的 lb-agent 按本机 frontend 状态以进程内 GoBGP 宣告
  VIP（[ADR-0012](decisions/0012-l4-loadbalancer.md)、[ADR-0017](decisions/0017-lb-host-local-announcement.md)、
  [ADR-0018](decisions/0018-lb-backend-and-host-resources.md)）；可选为 VIP 发布 PowerDNS 名称。
  - 0.3.0 GA 之前：lb-agent 加入控制面并在本机收敛（[ADR-0022](decisions/0022-lb-agent-joins-control-plane.md)），
    status 呈现后端健康（[ADR-0023](decisions/0023-lb-endpoint-health-in-status.md)）。
  - 之后把 pg-prod 入口从 VyOS/homelab-pg-proxy 迁到 Ayatori。
- `Bucket` 不进入 Ayatori：SeaweedFS bucket、身份与凭据由 homelab-infra CI Terraform 管理，
  重新评估条件见 [ADR-0011](decisions/0011-bucket-managed-by-ci-terraform.md)。

## 3. Compute（设计与 PoC 完成，实现中）

计算层规划为自建的 libvirt/LXC backend，不再以 Proxmox API 为南向主体
（[ADR-0015](decisions/0015-libvirt-lxc-compute-backend.md)，Proposed）：

- API group `compute.ayatori.ddupan.top`：`LibvirtDomain`、`LXCContainer`、`LibvirtStoragePool`、
  `LibvirtNetwork`、`LibvirtStorageVolume`，北向透传 libvirt/lxc 原生配置。
- 中央 compute controller 计算候选节点、检查容量并编排删除；每台宿主机的 Compute Agent 写
  `core/v1 Node` 与 Lease（[ADR-0007](decisions/0007-compute-node-and-vm-boundary.md)），在本机下发
  pool/network/volume/domain。
- 宿主机故障后的冷启动由节点自治（drbd-reactor + LINSTOR），不依赖 Proxmox HA。
- 元数据、实例身份、控制台与运行时操作见 [ADR-0016](decisions/0016-instance-node-services.md)（Proposed）。
- 待验证事项由 [PoC 01–15](compute/) 记录；Compute Agent 第一版在 `dev/compute` 分支开发。

验收场景沿用 Laptop Rebuild Readiness：

1. 临时节点加入。
2. laptop 上的 workload 被重建、迁移或形成可执行人工任务。
3. laptop 达到 `SafeToRemove=True`。
4. laptop 重装并重新加入。
5. 临时节点排空并安全退出。

## 4. Human Executor 与延迟自动化（未开始）

- `ManualTask`、`TaskReport` 和版本化 Runbook。
- Telegram/Email 通知、领取、提醒和升级。
- 后端验证与上游 reconcile 恢复。

## 5. 条件性扩展

- KaaS：候选方案是 CAPI 管理集群生命周期、Ayatori 以 `Machine` 供应跨后端节点
  （[ADR-0014](decisions/0014-kaas-cross-backend-node-supply.md)，Proposed）；只有出现托管控制面、
  租户隔离或频繁集群生命周期的真实需求时才立项。
- OpenSandbox/microVM 可以作为内部 Run backend，但不由此产生 FaaS 产品承诺。
- DNS、证书和 Credential 只有在跨系统协调收益明确时形成独立资源。IPAM 在 Ayatori 自己创建主机之后
  才考虑并入（LB VIP 已以 `networking.k8s.io/v1 IPAddress` 记录分配）。
