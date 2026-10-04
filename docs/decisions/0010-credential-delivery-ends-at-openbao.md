# ADR-0010：Tenant 凭据交付止于 OpenBao 位置公布

- 状态：Accepted
- 日期：2026-10-04
- 依据：维护者讨论；修订 [ADR-0009](0009-database-resource-and-claim.md) 中“OpenBao 应用凭据与
  ESO 投射……继续适用”的沿用合同，不改变其资源模型。

## 问题

原 PostgreSQL Tenant Operator 与 workload 运行在同一集群，因此规定 controller 在 Tenant
namespace 创建 ExternalSecret，由 ESO 投射为应用 Secret，Tenant Ready 以投射完成为准。

并入 Ayatori 后这一前提不成立。按 [ADR-0001](0001-kubernetes-api-machinery.md)，领域 API 不得
隐含依赖 controller 所在集群的 namespace 共置或 ownerReference 语义，workload 集群只是经
adapter 接入的远端 backend；按 [ADR-0002](0002-k0s-optional-workload-runtime.md)，控制面可以是
不运行 workload 的 API-only 形态。在控制面创建 ExternalSecret 会把 Secret 投射到没有消费者的
集群；以 systemd 运行 ESO 也只会写回同一个 API server。

## 决策

- Ayatori 的凭据交付终点是：应用凭据已在 OpenBao 中创建并回读确认，Tenant status 公布其
  `credentialRef`（mount/path）、`credentialVersion` 与不含认证信息的 `credentialURL`。
- Tenant `Ready=True` 要求绑定完成、凭据已确认且 Database `Ready=True`（资源、扩展与应用
  登录已验证），不依赖任何集群内投射。
- 投射由消费方所在集群自行完成，例如其 GitOps 中的 ExternalSecret 按公布的位置与版本读取。
  Ayatori 不在控制面创建 ExternalSecret 或 Secret，也不访问 workload 集群。
- Tenant namespace 是租户与 RBAC 边界，不代表 workload namespace；移除 `spec.secretName`
  与 `status.secretName`。v1alpha1 尚无部署，不保留兼容字段。

## 结果与后续

- controller 不需要 ESO、ExternalSecret 权限或租户 namespace 的 Secret 权限。
- 消费方集群需要能读取对应 Bao 路径的身份。原规格设想的“租户 ESO 身份只读整个 tenant
  base path”会让任一消费集群读到所有租户凭据；按 Tenant 签发更窄的读取 policy 留作后续切片。
- 确需由 Ayatori 主动投射到远端集群时，须新增显式的投射目标领域 API 与 adapter，另行决策。
- 管理凭据仍由 controller 从自身 namespace 的 Secret 读取；API-only 形态下该 Secret 由谁同步
  是部署问题，不在本决策范围。
