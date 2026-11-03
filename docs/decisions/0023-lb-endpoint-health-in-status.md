# ADR-0023：LoadBalancer 与 Backend 的 status 呈现后端健康

- 状态：Proposed
- 日期：2026-11-03
- 依据：维护者讨论（homelab 把 VyOS 上的四层入口迁到 Ayatori LoadBalancer 时，发现无从知道
  流量正送往哪个成员）。
- 补全：[ADR-0018](0018-lb-backend-and-host-resources.md) 中 Backend 的 `status.endpoints`（当时只
  写了形状，未实现）。与 [ADR-0022](0022-lb-agent-joins-control-plane.md) 的关系见「数据来源」。

## 问题

LoadBalancer 的 status 只有 VIP、Hostname、各主机的落地阶段，以及 `Programmed`、`Degraded` 等
条件；Backend 没有 status。它们回答了「配置下发了没有」，回答不了消费方真正关心的问题：

- **哪些成员在接流量。** 以 Patroni 为后端时，健康检查只放行主库；`kubectl get` 看不出现在是
  哪个成员，切主后也看不出是否已经跟过去。
- **一个端口是否已经没有可用成员。** 这时连接被拒绝（fail-closed），但 status 仍是
  `Programmed=True`、`Degraded=False`。
- **主机之间看法是否一致。** 每台主机独立做健康检查，网络分区时可能一台认为 A 健康、另一台
  认为 B 健康；对主库这类后端，这就是两边各写一个库。

homelab 现在靠 VyOS 上 HAProxy 的 Prometheus 指标回答这三个问题（「没有主库」「同时两个主库」
两条告警）。迁到 Ayatori 后这些信号要有对应物，而且应当是 API 的一部分，不依赖部署方另配监控。

## 决策

### 呈现什么

**Backend.status.endpoints[]**：每个 endpoint 一项，汇总所有引用它的端口在所有主机上的检查结果。

```yaml
status:
  observedGeneration: 3
  endpoints:
    - address: 10.60.0.11
      port: 5432
      ready: true          # 至少一台主机上处于 UP
      readyHosts: 2        # 处于 UP 的主机数
      hosts: 2             # 应承载它的主机数（Programmed 的主机）
      lastTransitionTime: "2026-11-03T16:41:00Z"   # ready 最近一次变化
    - address: 10.60.0.12
      port: 5432
      ready: false
      readyHosts: 0
      hosts: 2
      lastTransitionTime: "2026-11-03T09:12:00Z"
  conditions:
    - type: Ready          # 至少一个 endpoint ready
      status: "True"
```

- 一个 Backend 被多个端口引用时，每个端口各有一份 HAProxy backend section、各自检查（ADR-0018）。
  它们检查的是同一个 endpoint、同一套参数，所以按 endpoint 合并；端口之间结果不一致只会是短暂的
  检查相位差，不单独呈现。
- 状态只取健康检查的结论：UP 计为 ready；DOWN、MAINT、DRAIN 以及检查尚未完成都不计。具体是
  哪一种看主机上的 HAProxy，status 不复制。
- `readyHosts < hosts` 说明主机之间看法不一致，或有主机还没检查完；这是分区、单侧故障的信号。

**LoadBalancer.status.ports[]**：每个端口一项，给出它引用的 Backend 此刻的可用情况，不重复
endpoint 列表。

```yaml
status:
  ports:
    - name: postgresql
      port: 5432
      readyEndpoints: 1
      endpoints: 2
```

**新条件 `EndpointsReady`**（LoadBalancer）：所有端口都至少有一个 ready endpoint 时为 True；
否则 False，reason `NoReadyEndpoints`，message 列出没有可用成员的端口。`Degraded` 的含义不变
（主机层面），两者分开，因为「主机没收敛」和「后端全挂」的处理人不同。

不呈现的：会话数、流量、检查耗时等计数类数据。它们变化太快，写进 status 只会制造写入；需要时
从主机上的 HAProxy 指标取。

### 数据来源

健康检查的结论只存在于每台主机的 HAProxy 里，status 由 controller 汇总写入：

- **ADR-0022 落地前**：controller 已经通过 Data Plane API 读取 server 的 stats（排空 endpoint 时
  用到），在同一轮收敛里读取各 server 的 `status`，汇总后写入。不需要新的接口。
- **ADR-0022 落地后**：controller 不再连接主机。agent 从本机 stats socket 读出各 server 状态，
  写进自己那台 LoadBalancerHost 的 `status.loadBalancers[].ports[].endpoints[]`；controller 只读
  LoadBalancerHost，汇总逻辑不变。

两种来源汇总到同一组字段，消费方看不到区别。

### 写入时机

- 只在汇总结果变化时写入，SSA 的内容不变时不产生新版本。
- 健康状态的翻动频率由 HAProxy 的 `rise`/`fall` 限制（默认需连续 2 次成功、3 次失败），不再
  另加去抖。
- 主机不可达（ADR-0022 后为 `AgentReady` 不是 True）时，该主机不计入 `hosts`，它此前的观察也不再
  计入 `readyHosts`。看不到的主机不能替任何 endpoint 作证。

### 告警

status 是消费方与人的接口；告警由部署方用 kube-state-metrics 的 custom resource state 把上面的
字段导出为指标，例如 Backend 的 ready endpoint 数为 0、或对主库类后端大于 1。Ayatori 的部署
配置随本 ADR 一起提供这两类指标的导出定义。

## 已接受的代价

- **呈现有延迟。** ADR-0022 前取决于 controller 的收敛周期；之后取决于 agent 的写入周期。都在秒级
  到十秒级，比直接抓 HAProxy 指标慢。需要秒级精度的场景仍可抓主机上的 HAProxy。
- **`ready` 是「至少一台主机认为健康」。** 对普通后端这是对的（流量会被送到那里）。对「只能有一个
  的」后端，要看 `readyHosts` 与 ready endpoint 的个数，而不是单个 `ready`。

## 不选的方案

- **只靠主机上的 HAProxy Prometheus 指标**：HAProxy backend section 以 LoadBalancer 的 uid 命名，
  告警要把指标与对象名关联，部署方还得为每台主机配置抓取。这回答了监控的问题，回答不了
  `kubectl get` 的问题。
- **把逐主机、逐端口的明细都写进 Backend status**：列表大小是 endpoint × 端口 × 主机，变化时整组
  重写。明细留在主机上（ADR-0022 后在 LoadBalancerHost 上），Backend 只放汇总。
- **endpoint 不健康时把 LoadBalancer 标为 Degraded**：混淆了两类责任。主机层面的问题归 LB 运维，
  后端全挂归消费方。

## 测试合同

- 领域单测：多主机、多端口的 server 状态汇总为 `endpoints[]`、`ports[]` 与 `EndpointsReady`；
  主机不可达时不计入；端口引用的 Backend 不存在时 `readyEndpoints` 为 0。
- envtest：健康状态变化后 Backend 与 LoadBalancer 的 status 随之更新；内容不变时不产生新版本。
- 集成测试（现有 HAProxy 容器环境）：停掉一个后端，status 在 `fall × interval` 之后反映出来；
  Patroni 式的「只有一个 200」后端只呈现一个 ready endpoint。
