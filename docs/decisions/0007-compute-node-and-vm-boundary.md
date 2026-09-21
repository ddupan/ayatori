# ADR-0007：复用 Node API 建立按需实现的 Compute 能力

- 状态：Accepted
- 日期：2026-09-20
- 实施优先级：Deferred；当前优先 Database、LoadBalancer 与 Bucket

## 背景

Ayatori 长期可能需要管理现有 Proxmox VM、当前 libvirt VM，以及允许普通计算节点临时加入、
排空和退出。Proxmox 的远程 API 不能覆盖全部所需操作；若 Ayatori 进一步实现节点 inventory、
简单 placement、fencing 和安全 reschedule，Proxmox 的控制面价值会逐步被替代。

同一物理节点未来也可能运行 OpenSandbox/Kata 等执行后端。Kata 虽然以 microVM 隔离 Pod 或
container，但其公开生命周期是 Sandbox/Run，不是具有磁盘、NIC、console、placement、迁移和
长期身份的 VirtualMachine 产品。

## 决策

### 节点 API

Ayatori 选择性复用 `core/v1 Node` 与 `coordination.k8s.io/v1 Lease` 表达计算节点身份、能力、
容量、健康、维护状态与心跳。它们只是 API contract：由 Ayatori Compute Agent 写入，并由
Ayatori 自有 controller 消费。

这项选择不引入 kubelet、Pod、CRI、kube-scheduler 或 kube-controller-manager。Compute Agent
不是对 kubelet 的模拟或兼容实现，而是 Node API 在 Ayatori Compute 领域中的正式 producer。
每个 Node 必须带 Ayatori ownership label；Agent 只能更新自己的 Node/status 与 Lease。

初版 VirtualMachine 显式指定 Node。出现实际需求后，再由 Ayatori controller 基于 Node 的
Ready、unschedulable、taints、labels、capacity 和已有 allocation 实现小规模 filter/score。
具体资源分配不能依靠多个 controller 反复改写 `Node.status.allocatable`；需要并发预留时增加
独立 Allocation 资源或等价的原子分配记录。

### VM 数据面

长期主路径可以是普通 Linux Compute Node 上的 libvirt/QEMU，由受限的 Compute Agent 执行
版本化、强类型、幂等且可观察的 VM 操作。Agent 不提供任意远程 shell。

Proxmox 是 brownfield 迁移后端：初期用于 adopt 现有 VM，并继续提供当前已有的集群、存储、
备份与 HA 能力。若 Ayatori Compute 已经可靠覆盖所需 placement、fencing、存储可移植性和恢复
语义，可以逐步把 PVE 节点迁移为普通 Compute Node；不为维持虚假 backend 对等性承诺永久支持
所有 Proxmox 特性。

### HA 边界

自动 reschedule 必须满足：旧节点已经可靠 fenced，且 Volume 明确报告可在目标节点使用。
任一条件无法证明时，VM 进入 Blocked/ManualTask，不得冒险在第二个节点启动。首版允许完全
人工 placement 与恢复；不以通用 Placement、透明 live migration、多租户 SDN 或 Nova 兼容为目标。

### Sandbox 边界

OpenSandbox/Kata microVM 归属于 Run/Sandbox backend 的隔离实现，不创建 VirtualMachine 资源。
若未来 VM 与 Sandbox 共享物理节点，容量协调必须另行形成经过验证的设计；不能仅因两者底层
都使用 KVM 就合并其北向生命周期。

## 结果

- 复用成熟 Node/Lease API，而不继承 Kubernetes workload plane。
- Compute 能力可以按 homelab 所需规模实现，不必复制完整 Nova。
- PVE 帮助现有资源平滑迁移，但不是长期架构必须保留的一层。
- Compute 方向已记录，但不改变当前 Database、LoadBalancer、Bucket 的产品优先级。
