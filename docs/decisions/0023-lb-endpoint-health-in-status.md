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

指标分两类，按变化频率放在不同的地方，名字都用 Ayatori 的对象名（namespace/name、端口、endpoint），
不用 HAProxy section 名里的 uid。参照的是 HAProxy 自带 Prometheus exporter 的指标集。

| 类别 | 对应的 exporter 指标 | 放在哪里 |
|---|---|---|
| 状态 | `server_status`、`server_check_status`、`server_check_code`、`server_check_last_change_seconds`、`backend_active_servers` | status，由 KSM 导出 |
| 计数 | 会话、字节、错误、重试、检查失败与翻转次数、downtime、排队与耗时，以及 frontend 的同类指标 | agent 的 `/metrics` |

状态类只在健康翻转时变化，写进 status 的代价可以忽略，`kubectl get` 也能直接看到。计数类每秒
都在变，写进 status 等于持续写 API，KSM 拿到的也只是收敛周期上的快照。

### 状态：Backend 与 LoadBalancer 的 status

**Backend.status.endpoints[]**：每个 endpoint 一项，`observations[]` 是每台主机、每个引用它的端口
上的一次检查结论。

```yaml
status:
  observedGeneration: 3
  endpoints:
    - address: 10.60.0.11
      port: 5432
      observations:
        - host: lb-prod-1
          loadBalancer: sandbox/postgresql
          port: 5432
          state: UP                 # UP、DOWN、MAINT、DRAIN、NOLB 等，同 server_status
          checkStatus: L7OK         # 同 server_check_status：L4OK、L4TOUT、L4CON、L7OK、L7STS 等
          checkCode: 200            # 同 server_check_code；L4 检查没有
          lastTransitionTime: "2026-11-03T16:41:00Z"   # state 最近一次变化
        - host: lb-prod-2
          ...
  conditions:
    - type: Ready                   # 至少一个 endpoint 在某台主机上 UP
      status: "True"
```

- 一个 Backend 被多个端口引用时，每个端口在每台主机上各有一份 HAProxy backend section、各自检查
  （ADR-0018），所以按（主机、LoadBalancer、端口）各记一条。列表大小是
  endpoint × 引用数 × 主机数，以现有上限（64 个 endpoint、16 台主机）为界。
- 主机不可达（ADR-0022 后为 `AgentReady` 不是 True）时，删去该主机的观察。看不到的主机不能替
  任何 endpoint 作证。
- `lastTransitionTime` 只在观察到 `state` 变化时更新。HAProxy 报的是「距上次变化的秒数」，每次读取
  换算出的时间都会有秒级抖动，直接写入会让每一轮都产生新版本。

**LoadBalancer.status.ports[]**：每个端口给出它引用的 Backend 此刻的可用情况，供 `kubectl get` 一眼
看出端口是否可用；明细在 Backend 上。

```yaml
status:
  ports:
    - name: postgresql
      port: 5432
      readyEndpoints: 1   # 至少一台主机上 UP 的 endpoint 数，同 backend_active_servers 的跨主机合并
      endpoints: 2
```

**新条件 `EndpointsReady`**（LoadBalancer）：所有端口都至少有一个 ready endpoint 时为 True；
否则 False，reason `NoReadyEndpoints`，message 列出没有可用成员的端口。`Degraded` 的含义不变
（主机层面）。「主机没收敛」与「后端全挂」的处理人不同，两者分开。

### 计数：agent 的 `/metrics`

agent 从本机 stats socket 读取计数，以 exporter 的指标名加 `ayatori_lb_` 前缀导出，例如
`ayatori_lb_server_sessions_total{namespace,name,port,endpoint}`、
`ayatori_lb_frontend_bytes_in_total{namespace,name,port}`。只导出 TCP 模式有意义的指标，HTTP 的
响应码等不导出。

这一部分要等 ADR-0022：agent 要 watch LoadBalancer 才知道 section 对应哪个对象。在那之前不提供
计数类指标。

### 数据来源

- **状态，ADR-0022 落地前**：controller 已经通过 Data Plane API 读取 server 的 stats（排空 endpoint
  时用到），在同一轮收敛里读取各 server 的状态并汇总写入。不需要新的接口。
- **状态，ADR-0022 落地后**：controller 不再连接主机。agent 把本机的观察写进自己那台
  LoadBalancerHost 的 `status.loadBalancers[]`，controller 只读 LoadBalancerHost，汇总到 Backend
  与 LoadBalancer。消费方看不到区别。
- **计数**：只在 ADR-0022 之后，由 agent 导出。

### 写入时机

只在汇总结果变化时写入，SSA 的内容不变时不产生新版本。健康状态的翻动频率由 HAProxy 的
`rise`/`fall` 限制，不另加去抖。

### 告警

Ayatori 的部署配置随本 ADR 提供 KSM custom resource state 定义，把 `observations[]` 导出为
`ayatori_backend_endpoint_status{namespace,name,endpoint,host,load_balancer,port,state}` 等指标。
「端口没有可用成员」「只能有一个的后端同时有两个 UP」之类的告警由部署方基于它们编写。

## 已接受的代价

- **状态的呈现有延迟。** ADR-0022 前取决于 controller 的收敛周期；之后取决于 agent 的写入周期。都在
  秒级到十秒级，比直接抓 HAProxy 指标慢。
- **ADR-0022 之前没有计数类指标。** 需要时只能在主机上看 HAProxy。

## 不选的方案

- **只靠主机上的 HAProxy Prometheus 指标**：HAProxy backend section 以 LoadBalancer 的 uid 命名，
  告警要把指标与对象名关联，部署方还得为每台主机配置抓取。这回答了监控的问题，回答不了
  `kubectl get` 的问题。
- **计数类也写进 status**：每秒都在变，等于持续写 API；KSM 也只能拿到收敛周期上的快照。
- **Backend 只放汇总（每个 endpoint 一个 ready）**：看不出主机之间是否一致，也看不出检查失败在
  哪一层（连不上、超时、返回码不对），监控需要这些区分。
- **endpoint 不健康时把 LoadBalancer 标为 Degraded**：混淆了两类责任。主机层面的问题归 LB 运维，
  后端全挂归消费方。

## 测试合同

- 领域单测：多主机、多端口的 server 状态汇总为 `observations[]`、`ports[]` 与 `EndpointsReady`；
  主机不可达时删去其观察；`lastTransitionTime` 只在 state 变化时更新；端口引用的 Backend 不存在时
  `readyEndpoints` 为 0。
- envtest：健康状态变化后 Backend 与 LoadBalancer 的 status 随之更新；内容不变时不产生新版本。
- agent 单测（ADR-0022 后）：stats 行到指标的映射，section 名到对象名的对应。
- 集成测试（现有 HAProxy 容器环境）：停掉一个后端，status 在 `fall × interval` 之后反映出来；
  Patroni 式的「只有一个 200」后端只呈现一个 ready endpoint。
