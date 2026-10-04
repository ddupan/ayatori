# ADR-0011：Bucket 由 CI Terraform 管理，不进入 Ayatori 北向 API

- 状态：Accepted
- 日期：2026-10-04
- 依据：维护者讨论与 SeaweedFS 兼容性验证；修订 [ADR-0006](0006-demand-driven-resource-scope.md)
  的优先方向列表，不改变其“按实际管理缺口扩展资源”的原则。

## 问题

ADR-0006 将 `Bucket` / Object Storage 列为首批方向。实际缺口是：每接入一个应用都要手工
`s3.bucket.create`、新建 Bao KV、修改合成 `seaweedfs-s3-config` 的 ESO template，再向
SeaweedFS 发送 SIGHUP。zot 与 TechDocs 都按此流程接入，确属重复劳动。

但按 ADR-0006 的判据，还需证明现有 IaC 不能提供足够的生命周期、状态或权限体验。

## 评估

- 若由 Ayatori 维护静态 config 并发送 SIGHUP：需要写入 workload 集群的 Secret 并在其 Pod
  内 exec，违反 [ADR-0010](0010-credential-delivery-ends-at-openbao.md) 不访问 workload
  集群的边界，也无法可靠确认重载时间。
- 若 Ayatori 调用 SeaweedFS 内嵌 IAM API：为支持授权，北向至少要拆分 `Bucket` 与访问授权；
  与 Terraform 的 `aws_s3_bucket` / `aws_iam_*` 相比，Ayatori 独有的价值只剩“申请方无需
  持有 SeaweedFS 管理凭据的自助申请与跨主体授权”。
- homelab 约定应用 Terraform 统一由 CI 执行，CI 经 Bao role `ci-terraform` 获得凭据，变更
  经 homelab-infra PR review 落地；CI 已是唯一可信写入方。上述自助价值在当前环境不存在，
  持续收敛与漂移由 main apply 和计划中的漂移检测覆盖。
- 已验证 Terraform AWS provider 可管理 SeaweedFS 的 bucket 与 IAM 资源：SeaweedFS 升级到
  4.48 后完全可用。

## 决策

- Ayatori 不实现 `Bucket` 或对象存储访问授权资源。SeaweedFS bucket、身份、策略与凭据在
  homelab-infra 中以 Terraform AWS provider 声明，由 CI 执行；凭据写入 OpenBao。
- 移出后的首批方向为 `Database`、`LoadBalancer`；`VirtualMachine` 的定位不变。

## 重新评估条件

出现以下任一情况时重新评估，并以新 ADR 记录：

- 需要不持有 SeaweedFS 管理凭据、也不经 homelab-infra PR 的自助申请或跨主体授权；
- Ayatori 的其他资源需要以 bucket 作为受管依赖，且其生命周期无法由 Terraform 表达；
- SeaweedFS 升级后 Terraform AWS provider 兼容性失效且无法以 provider 配置规避。
