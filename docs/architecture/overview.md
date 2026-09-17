# 总体架构

```text
Git / CLI / Backstage
          │
          ▼
 Kubernetes API + CRD
          │
   Ayatori controllers
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

## 控制面

Dev 与 Prod 使用独立的 Kubernetes API、数据库、身份和 controller 实例。两者可以
共享物理宿主，但不能只依赖 namespace 隔离 cluster-scoped API 与高权限凭据。

Proxmox 作为稀缺物理基础设施可以共享，通过 pool、tag、token 和明确的资源范围区分
环境。其他后端尽量使用独立数据库、角色、地址池、DNS 空间与凭据。

## 数据面

Ayatori 不承载或重新实现数据面。控制面故障只应阻止创建与变更，不应停止已有 VM、
负载均衡、数据库、对象存储或租户 Kubernetes 集群。

## 资源分层

平台提供正交产品能力，例如：

- `Job`、`Sandbox`、`ManualTask`
- `VirtualMachine`
- `LoadBalancer`
- `Database`
- `Bucket`
- `DNSRecord`
- `Credential`
- `KubernetesCluster`

只有具备独立领域生命周期的能力才应成为高阶资源。应用本身通过 GitOps 组合上述资源，
重复组合可通过模板或 Composition 表达，而不是扩展中央 Application API。

## 后端策略

优先级如下：

1. 直接调用成熟且可观察的后端 API。
2. 通过固定版本的 Terraform module 或 Ansible playbook 执行。
3. 仅在必要时使用 GitOps bridge。

Controller 无论采用哪种执行方式，都必须提供一致的 ownership、conditions、删除语义、
错误分类和恢复行为。
