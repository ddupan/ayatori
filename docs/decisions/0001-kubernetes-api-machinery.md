# ADR-0001：采用 Kubernetes API machinery 作为状态协调平面

- 状态：Accepted
- 日期：2026-09-17

## 背景

homelab 的基础设施状态分散在多套工具和后端中。仅集中 IaC 文件不能提供持续观察、依赖
关系、异步状态、漂移纠正和人工任务协调。

## 决策

Ayatori 使用 kube-apiserver、etcd、Kubernetes API machinery 与 CRD 构成 API 和状态协调
平面。主要复用的是以下难以可靠重建的能力：

- 版本化对象 API、schema、defaulting、validation 与 admission；
- 带 `resourceVersion` 的乐观并发、list/watch 与断线恢复；
- informer/cache/workqueue 生态；
- authentication、RBAC、namespace、审计与 API discovery；
- spec/status、conditions、finalizer 等控制面约定。

这项选择不把 Ayatori 限定为容器编排系统，也不意味着原生 Kubernetes workload API 是领域
模型。kube-apiserver 保存期望、引用和观察状态；Ayatori controller-manager 实现平台领域的
调度、生命周期、故障恢复、垃圾回收和后端收敛。Controller 可以运行于专用 management
environment，并管理集群外的 VM、LB、数据库、对象存储、DNS、凭据和托管 Kubernetes 控制面。

Kubernetes workload 集群与 OpenSandbox、Proxmox 等一样，是通过 adapter 接入的 backend 或
executor。它可以是远端集群，也可以完全不存在。除 Flux 和 Ayatori controllers 等管理组件的
部署外，领域 API 不得隐含依赖 controller 所在集群的 Pod、Job、Service、NetworkPolicy、
namespace 共置或 owner reference 语义；确有需要的能力必须由领域 API 和 adapter 契约显式表达。

GitOps 是长期期望状态的主要提交入口；API 是当前意图、关系和状态的在线控制面；真实后端
仍是运行事实来源。Controller 负责三者之间持续收敛。

`generic-apiserver` 或 Kubernetes API aggregation 只会让 Ayatori 接管资源的服务端实现，并不会
替代上述领域 controller。除非 CRD/kube-apiserver 的存储模型、API 语义或扩展边界形成经过验证的
阻碍，Ayatori 不自行承担 watch、RBAC、API 兼容、存储版本迁移和高可用 API Server 的实现与运维。

## 结果

- 获得统一声明式 API、watch、RBAC、admission、conditions 和 controller 生态。
- Ayatori controller-manager 实际承担类似 kube-controller-manager 的领域控制循环职责，必须把
  reconcile、状态迁移、恢复与后端契约作为产品核心，而不是把它们误交给 kube-apiserver。
- 原生 Kubernetes workload 对象不能成为所有 adapter 的最低公共语义；Kubernetes 只是其中一种
  执行后端。
- 可以把机器与人工执行统一建模为异步控制循环。
- 必须维护 CRD 版本、conversion、认证、备份和控制面升级。
- 不在 API 中保存日志、指标、大对象或业务数据，只保存控制所需状态及引用。
