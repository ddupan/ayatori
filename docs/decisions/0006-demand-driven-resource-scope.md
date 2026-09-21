# ADR-0006：按实际管理缺口扩展资源 API

- 状态：Accepted
- 日期：2026-09-20

## 背景

Ayatori 可以在技术上逐步加入 VM、任务、数据库、负载均衡、对象存储、KaaS、FaaS 与应用
托管等能力。如果按传统私有云产品目录推进，项目会把后端“能够实现”的能力误当成 homelab
实际需要的产品，并承担没有消费者的 API、controller、升级和恢复成本。

当前真正反复出现的问题，是 Database、LoadBalancer 和 Bucket/Object Storage 缺少符合本环境
需求的稳定管理 API。Proxmox VM 也存在明确缺口：远程 API 能力有限，一部分操作只能登录节点
使用 CLI 完成，因此单靠 Terraform provider 或 Proxmox API 无法覆盖期望生命周期。

当前 `Job` controller 是验证 Kubernetes API machinery、状态机、finalizer、回收和 adapter 边界
的首个纵向切片。OpenSandbox 和 microVM 可以成为内部执行后端，但这不等于平台需要 Lambda、
Cloud Run 或其他 FaaS/PaaS 产品。

## 决策

Ayatori 不设置必须完成的云产品清单。新增北向资源必须由现实消费者、重复管理缺口和持续
reconcile 的明确收益驱动。

当前优先方向是：

1. `Database`；
2. `LoadBalancer`；
3. `Bucket` / Object Storage；
4. `VirtualMachine`，其价值已确认，但实现成本更高。

`Run`/当前实验性的 `Job` 定位为控制面执行原语和架构验证切片，不自动扩展为面向用户的计算
产品。KaaS 是可能有真实需求的候选能力，但不是必达终点。FaaS、Cloud Run 和应用托管默认不做，
除非未来以新的需求和 ADR 改变决定。

VirtualMachine controller 对外提供稳定北向 API；南向允许根据操作选择 Proxmox API、节点上的
受限强类型 Agent/CLI 或 `ManualTask`。节点 Agent 必须提供版本化、幂等、可观察和可审计的操作，
不能退化为任意远程 shell。

## 结果

- 路线图可以根据当前收益调整，不把技术可行性误作产品承诺。
- 第一个 Job controller 的实现仍有测试和架构验证价值，但其 API 不约束长期产品形态。
- VM 被保留为核心高价值方向，同时承认其南向集成不是单一 provider 能解决的问题。
- 每个新增资源都要独立证明生命周期和管理价值；已有 backend 不自动产生新的产品层。
