# ADR-0017：VIP 宣告由 LB 主机本地决定

- 状态：Accepted
- 日期：2026-10-16
- 依据：维护者讨论（LB 主机部署后复盘）。
- 部分取代：[ADR-0012](0012-l4-loadbalancer.md) 的「南向与收敛」第 2、3、5 步、「故障时的
  路由撤回」，以及 gobgpd 作为独立进程与其 API 的部署边界。

## 问题

ADR-0012 把 speaker 放在 LB 主机上，但宣告与否由 controller 远程决定：reload 成功后经 gobgpd
mTLS gRPC `AddPath`。部署后看清了三个后果：

- **数据面恢复依赖控制面。** gobgpd 不持久化路径，重启后要等 controller 观察到缺失再补宣告；
  controller 不在线时该主机一直不承接流量。
- **controller 看不到主机本地的故障。** 它只知道配置已生效。某台主机的 frontend 未能绑定
  （如 bind 失败、被禁用）时，VIP 仍从它宣告，ECMP 继续把一部分流量送进去。
- **gobgpd 的写接口必须对网络开放。** 为此要给 gobgpd 签服务端证书、给 controller 签客户端证书
  并经 OpenBao 分发，只为了 controller 能远程改一台主机的路由。

LB 是否健康、后端是否可达，应由承接流量的那台主机在本地判断，不从 controller 探测。

## 决策

每台 LB 主机运行一个 **LB agent**，内嵌 GoBGP（`github.com/osrg/gobgp/v4/pkg/server`）作为
本机唯一的 BGP speaker，取代独立的 gobgpd。controller 不再参与路由。

### 宣告规则

宣告只反映 **LB 本身** 是否能承接流量，不反映后端。agent 只读本机 HAProxy 的 runtime API（stats
socket），按 VIP 独立判断。VIP 是 bind 地址落在本环境 VIP 段内的 frontend：

- 宣告该 VIP /32，当且仅当本机 HAProxy 应答，且承载该 VIP 的 frontend 处于 `OPEN`。
- 否则撤回。HAProxy 不应答时撤回本机全部 VIP。

后端健康不影响路由。某个 VIP 的后端全部不健康时，LB 本身仍是好的，流量照常打到 LB，由
HAProxy 拒绝连接（fail-closed，ADR-0012 不变）：撤回并不能让请求成功，只会把立即拒绝变成
落到路由器的兜底路由。后端健康仍以 HAProxy 自身的检查为准，agent 不另做探测。

规则以 Ayatori 领域包中的纯函数实现，agent 与 controller 共用：controller 据 Data Plane API
观察到的同一组事实计算每台主机「应宣告」，写入 status，不必读取路由。

### agent 的运行方式

- 无状态的收敛循环：按固定间隔计算应宣告集合，与内嵌 BGP server 的本地 RIB 比较，增删路径
  补齐差异。agent 重启后从零开始，首轮即按 HAProxy 现状宣告，无需 controller。
- BGP 会话与策略（本机 AS、router-id、邻居、只出不进的 VIP 段过滤）来自本地配置，与
  ADR-0012 中 gobgpd 的静态配置相同；不开放 gRPC 或任何管理接口。HAProxy 经本地 socket。
- **失效即撤回**：路径只存在于 agent 进程内。agent 退出即 BGP 会话断开，路由器撤回该主机全部
  路径，不会留下无人维护的旧路径。agent 以 systemd `BindsTo=haproxy.service` 随 HAProxy 停止。
- 宣告状态以 Prometheus 指标导出（每 VIP 是否宣告、最近一轮收敛结果），供告警与排障；
  controller 不读取。
- agent 是 Ayatori 仓库中的独立二进制，随 Ayatori 发布；GoBGP 版本由 Ayatori 的 `go.mod`
  固定，升级随 Ayatori 经 Dev 验证后 promotion。环境相关的值（AS、邻居、VIP 段、HAProxy
  socket 路径、间隔）由 LB 主机部署写入本地配置文件，不进入发布物。
- LB 主机部署不再安装 gobgpd，也不再为它签发证书；dataplaneapi 的 mTLS 不变。

### controller 的变化

- 收敛只剩 Data Plane API：确保配置、等待 reload、endpoint 变化走 runtime server 接口。
- 创建：配置生效后由 agent 自行宣告，controller 不再有「宣告」步骤。
- 删除：controller 直接删除配置并等待 reload；agent 在下一轮发现 frontend 消失后撤回。不再有
  「先撤回后删除」的跨组件顺序。
- status 中每台主机的进度改为「配置一致」与「满足宣告规则」两项事实。

### 路由器侧

被撤回的 VIP 落到路由器上覆盖 VIP 段的汇总路由。该汇总以 **reject** 兜底（客户端立即收到 ICMP
unreachable），不是 blackhole，否则所有 LB 主机都不可用时客户端只能等超时。由 homelab-infra 的 VyOS
配置负责。ADR-0012「部署与凭据边界」中的「向 OSPF 的再分发」随 OSPF 退役不再适用：VyOS
只向上游宣告覆盖 VIP 段的汇总，VIP /32 明细留在 VyOS 上。

## 已接受的代价

- 删除 LoadBalancer 时，从 reload 完成到 agent 下一轮撤回之间（至多一个收敛间隔），到该 VIP 的
  新连接被拒绝。VIP 正在删除，可接受。
- 某台 LB 主机单独到后端不通（其余主机正常）时不会被切走，该主机承接的那部分连接被拒绝。
  在本环境中 LB 与后端同处 labnet/LAN，此类局部故障罕见；出现需求时按「后续」中的降级处理。
- BGP speaker 的版本与 Ayatori 绑定，不能单独升级 GoBGP；agent 的缺陷会直接表现为会话中断
  （对路由器而言等同主机故障，fail-closed）。
- 宣告规则在 controller 与 agent 两处求值，若两边观察到的事实不同（例如 Data Plane API 与
  stats socket 的瞬时差异），status 可能短暂与实际路由不一致；实际路由以 agent 指标为准。

## 不选的方案

- **ExaBGP healthcheck 等现成工具**：每个 VIP 需要一份检查配置，又要由 controller 写到主机上。
  agent 直接读取 HAProxy 已有的判定，不需要逐 VIP 配置。
- **agent 加独立 gobgpd（经 localhost gRPC）**：多一个进程、一个本地 API 与一组 systemd 依赖，
  且要处理 gobgpd 单独重启时的补宣告；内嵌后进程边界即故障边界。
- **继续由 controller 宣告、只加本地探测**：探测结果仍要回到 controller 才能生效，恢复依旧依赖
  控制面。

## 后续（不在本次）

- **按后端健康在 BGP 中降级而不撤回**：本机某 VIP 无健康后端时以 AS-path prepend 宣告，路由器
  只在 ECMP 中保留未降级的主机；所有主机都降级时路径重新等长，流量仍到 LB 由 HAProxy 拒绝，
  不出现路由消失。用于处理单台 LB 到后端的局部故障，出现实际需求时再加。

## 测试合同

- 宣告规则（frontend 各状态、HAProxy 不应答；后端健康变化不改变结果）：单元测试。
- agent 对真实 HAProxy 与作为对端的 gobgp 的集成测试（沿用 LB 集成测试的容器环境）：后端全部
  失败后仍宣告；frontend 禁用后撤回、恢复后重新宣告；agent 重启后首轮即补回；agent 或 HAProxy
  停止后对端撤回全部路径。
- controller 不再依赖 gobgpd：envtest 与 adapter 测试中移除 gobgpd。
- 主机级故障切换与 reject 兜底：Dev 环境端到端测试。
