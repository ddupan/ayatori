# 总体架构

```text
Git / CLI / Backstage
          │
          ▼
 kube-apiserver + etcd + CRD
   API / state coordination plane
          │
   Ayatori controller-manager
 scheduling / lifecycle / recovery / GC
          │
   ┌──────┼──────────────┐
   │      │              │
Machine  Human       Direct adapters
runner   runner           │
   │      │               │
Pod      Runbook      Proxmox / Envoy / BGP
OpenSandbox          PostgreSQL / SeaweedFS
Terraform            OpenBao / DNS / KaaS
Ansible
```

Ayatori 复用 Kubernetes 的 API machinery，而不是 Kubernetes 的容器编排产品边界。
kube-apiserver 提供版本化对象、并发控制、list/watch、RBAC、admission 和审计；Ayatori
controller-manager 承担所有领域控制循环。Kubernetes workload 集群只是与 OpenSandbox、
Proxmox 等并列的 executor/backend，不默认等于运行 controller 的 management environment。

因此，领域 API 不得依赖“资源最终一定变成同集群原生对象”的假设。原生 Pod、Job、Service、
NetworkPolicy、namespace 共置与 owner reference 只有在 Kubernetes adapter 内才具有原生含义；
跨后端所需能力必须由领域模型显式定义。

## 控制面

Dev 与 Prod 使用独立的 Kubernetes API、数据库、身份和 controller 实例。两者可以
共享物理宿主，但不能只依赖 namespace 隔离 cluster-scoped API 与高权限凭据。

Proxmox 作为稀缺物理基础设施可以共享，通过 pool、tag、token 和明确的资源范围区分
环境。其他后端尽量使用独立数据库、角色、地址池、DNS 空间与凭据。

## 数据面

Ayatori 不承载或重新实现数据面。控制面故障只应阻止创建与变更，不应停止已有 VM、
负载均衡、数据库、对象存储或租户 Kubernetes 集群。

## 资源分层

平台只为已经验证的管理缺口提供正交产品能力。当前优先资源为：

- `LoadBalancer`
- `Database`
- `Bucket`
- `VirtualMachine`

`Run`/当前实验性的 `Job`、`ManualTask` 等可以作为控制面执行原语，但不是因为底层能运行 OCI
image 就自动成为面向使用者的计算产品。`DNSRecord`、`Credential`、`KubernetesCluster` 等只在
出现独立生命周期和真实消费者后加入；尤其 KaaS 不是预定终点。

只有具备独立领域生命周期的能力才应成为高阶资源。应用本身通过 GitOps 组合上述资源，
重复组合可通过模板或 Composition 表达，而不是扩展中央 Application API。

## 后端策略

优先级如下：

1. 直接调用成熟且可观察的后端 API。
2. 通过固定版本的 Terraform module 或 Ansible playbook 执行。
3. 仅在必要时使用 GitOps bridge。

Proxmox 是已知例外：其远程 API 不能覆盖所需的完整 VM 生命周期。VirtualMachine adapter 可以
按操作能力选择 Proxmox API、部署在节点上的受限强类型 Agent/CLI，或生成 `ManualTask`。Agent
必须提供版本化操作、幂等查询、operation ID 与审计，不能暴露任意 shell，也不能把 CLI 输出
直接当作长期稳定协议。

Controller 无论采用哪种执行方式，都必须提供一致的 ownership、conditions、删除语义、
错误分类和恢复行为。

## API Server 边界

首选 kube-apiserver + CRD，持续复用其成熟的 watch、RBAC、版本化存储和 API 生态。
generic-apiserver 或聚合 API Server 不会减少领域 controller 的数量，只会把资源服务端、
兼容性和存储迁移责任转移给 Ayatori。只有 CRD 的限制已经形成可复现、不可通过合理领域建模
解决的阻碍时，才重新评估自建 API Server。
