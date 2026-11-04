# 产品愿景

## 问题

当前 homelab 的期望状态散落在 Kubernetes manifests、Terraform state、Ansible、
Proxmox、libvirt、OpenBao、DNS、对象存储以及人工文档中。即使每一部分分别实现
IaC，跨系统变更仍需要人工安排顺序、复制状态并确认结果，容易产生遗漏、错误和漂移。

文档不能成为运行状态的权威来源：它只能解释设计和操作背景，无法持续观察现实并纠偏。

## 目标

Ayatori 提供统一、声明式、可组合的基础设施 API：

1. Git 保存经过 review 的长期期望状态。
2. Kubernetes API 保存当前意图、资源关系与状态摘要。
3. Controller 将请求翻译给成熟后端，并持续 observe/reconcile。
4. 机器和人通过统一任务协议执行动作。
5. 后端保存实际运行状态，数据面在控制面不可用时继续工作。

平台成功的直接标准是：新增 workload 时，使用者只需考虑它需要哪些资源，而不再回忆
需要依次修改哪些系统、文件和文档。

## 产品定位

Ayatori 是具有产品质量的内部平台，而非初期即面向公众的通用私有云。源码公开不等于
承担未知部署环境、任意后端和第三方兼容性的成本。

平台允许对当前环境形成明确意见：HAProxy、GoBGP、PowerDNS、OpenBao、PostgreSQL、libvirt、
LXC、LINSTOR、Proxmox、SeaweedFS、Cloudflare 和 Flux 都可以是已知实现。

Ayatori 不以补齐传统私有云或公有云的产品目录为目标。一个资源只有同时满足以下条件，才进入
北向 API：

1. homelab 存在现实消费者和重复需求；
2. 现有后端 API 或 IaC 无法提供足够的管理体验；
3. 持续 observe/reconcile 明显优于一次性自动化；
4. 统一生命周期、状态、组合或权限能产生可验证的收益；
5. 收益足以承担长期 API 兼容、controller 和恢复测试成本。

当前最明确的管理缺口是 Database 与 LoadBalancer。Bucket/Object Storage 已由 CI Terraform
覆盖，不进入北向 API（[ADR-0011](decisions/0011-bucket-managed-by-ci-terraform.md)）。
Database 已在 Dev/Prod 上线，LoadBalancer 处于 v0.3.0 预发布并已承载部分入口。

计算实例同样具有明确价值：Proxmox HA 依赖全集群 quorum 与自我 fencing，不符合"宿主机故障后
自动冷启动"的需求，Windows/retro VM 又需要完整的 libvirt 设备模型。因此计算层规划为自建的
libvirt/LXC backend：北向透传原生 domain/lxc 配置，由每台宿主机上的 Compute Agent 在本机收敛
（[ADR-0015](decisions/0015-libvirt-lxc-compute-backend.md)，Proposed）。KaaS 以 CAPI 管理集群
生命周期、Ayatori 供应跨后端节点（[ADR-0014](decisions/0014-kaas-cross-backend-node-supply.md)，
Proposed），只在出现实际需求时实现，不是产品路线的必达终点。

## 非目标

- 不替代 hypervisor、microVM runtime、数据库、对象存储或网络协议栈。
- 不从第一天构建通用 provider/plugin ABI。
- 不以隐藏全部后端信息或制造虚假多云可移植性为目标。
- 不创建理解所有应用需求的中央 Application controller。
- 不要求所有人工步骤立即自动化。
- 不因为已有 Run、OpenSandbox 或 microVM backend，就构建 FaaS、Cloud Run 或应用托管产品。
- 不预先承诺 KaaS；它是需求驱动的候选能力。
