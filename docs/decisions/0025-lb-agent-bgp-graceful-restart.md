# ADR-0025：LB agent 开启 BGP graceful restart

- 状态：Proposed（lb-dev 实验已完成；实现与测试见「待验证」）
- 日期：2026-11-11
- 依据：维护者讨论（[ADR-0024](0024-unified-version-with-skew.md) 的讨论中确认：agent 重启的代价
  落在数据面，是不强制同版本部署的主要原因）。
- 补充：[ADR-0017](0017-lb-host-local-announcement.md)。speaker 仍内嵌在 agent 中，本 ADR 只改变
  会话断开时路由器如何处理本机的路径。

## 问题

agent 内嵌 BGP speaker。进程一退出，会话就断开，路由器立即撤回这台主机的 VIP。HAProxy 和 `lo` 上
的 VIP 其实都还在，但 ECMP 已经把流量切到另一台主机，而那台主机上没有这些连接的状态。

lb-dev 上的对照实验（见下文）显示：`systemctl restart` agent 时，经过这台主机、且正在传输数据的
连接全部被重置。所以 agent 的每次升级都要逐台滚动，CI 的并行 apply 不能直接合并。

## 不选的方案

- **把 speaker 拆成独立进程**：独立的 speaker 自身也要升级，照样会断开会话；agent 与它之间要
  恢复一个本地 API。这正是 ADR-0017 内嵌 speaker 时去掉的东西，而且会失去「agent 不在，路径就
  不在」的性质。
- **开启 RFC 8538（`notification-enabled`）**：开启后，hold timer 超时也会进入保留，主机失联时
  路由器要多等一个 restart time 才撤回。不开启时，hold timer 超时的处理与现在相同（实验 E）。

## 决策

agent 开启 BGP graceful restart（RFC 4724），只作为 restarting speaker。路由器（FRR）默认就是
helper，不需要改配置。

1. **配置。** agent 新增配置项 `gracefulRestartTime`，为零时关闭（保持现有行为）。它的值同时是
   agent 不再回来时，路由器继续保留路径的最长时间。部署值放在环境 policy 中，初值 30s：要大于
   systemd 的 `RestartSec`（5s）加上启动时间，又不宜太长。
2. **启动。** 先按 HAProxy 现状填好本地 RIB，再连接邻居。每次启动都以 restarting 身份连接
   （OPEN 中置 R 位）：路由器保留着的旧路径要等本机发出 End-of-RIB 才会被替换。若先连接后填表，
   可能先发出空的 End-of-RIB，路由器随之删掉保留的路径。
3. **退出。** 系统正常运行时（`systemctl is-system-running` 为 `running` 或 `degraded`）直接断开，
   不发 NOTIFICATION，路由器进入保留。其他情况，包括系统关机与查询失败，都发 NOTIFICATION，路由器
   立即撤回：关机时 HAProxy 随后也会停止，保留路径只会让流量在 restart time 内被拒绝（实验 F）。
4. **运维约定。** 要让一台主机不再承接流量时，先停 HAProxy（agent 在下一轮收敛时撤回），而不是
   先停 agent。只停 agent 时路径会保留到 restart time 到期；这期间 HAProxy 仍在服务，所以不影响流量
   （实验 D）。

## 实验（lb-dev，2026-11-11）

目标是 Dev 上一个临时 LoadBalancer，后端是笔记本上的 echo 服务。探测程序每 100ms 新建一条连接，
同时在 20 条长连接上每 100ms 收发一次数据。VyOS 按三层地址做 ECMP 哈希，笔记本的流量全部落在
lb-dev-2 上，所以动作都在 lb-dev-2 上执行。restart time 为 30s。

| 组 | agent | 动作 | 路由 | 新建连接失败 | 长连接存活 |
|---|---|---|---|---:|---:|
| A | 0.3.0 | `systemctl restart` | 撤回约 3s 后恢复 | 0 | 0/20 |
| B | GR | `systemctl restart` | 不撤回 | 0 | 20/20 |
| C | GR | SIGKILL，systemd 5s 后拉起 | 不撤回 | 0 | 20/20 |
| D | GR | 停止 agent，不再拉起 | 30s 后撤回 | 0 | 0/20（30s 时） |
| E | GR | 丢弃 BGP 报文（模拟主机失联） | 约 63s 后撤回（hold timer） | 0 | 0/20（撤回时） |
| F | GR（无关机判断） | 依次停止 agent、HAProxy | 30s 后撤回 | 30s 内全部失败 | 0/20 |
| G | GR | 重启主机 | agent 退出时即撤回 | 关机期间 0 | 0/20 |
| H | GR | `systemctl restart`（含关机判断的版本） | 不撤回 | 0 | 20/20 |

说明：

- 在 E 组中，FRR 记录的断开原因是 `Hold Timer Expired`，并发送了 NOTIFICATION，因此没有进入
  保留。主机失联时的黑洞时长仍由 hold timer 决定（协商值 90s），与是否开启 GR 无关。
- 在 D 组中，长连接在撤回时被重置，原因是路由切换，不是 HAProxy 停止服务。
- F 组暴露了退出时需要判断系统是否在关机，第 3 条决策由此而来。G 组和 H 组是加入关机判断后的
  验证。

## 后果

- agent 的重启和升级不再影响经过这台主机的连接。两台主机都在 restart time 内回来时，并行 apply
  也不会撤回 VIP。逐台滚动不再是可用性的要求，只用于尽早发现坏版本：坏版本起不来时，restart time
  到期后照样撤回。
- 若两台主机上的 agent 同时起不来，撤回会比现在晚一个 restart time。在此期间 HAProxy 仍在服务，
  所以不会造成拒绝。
- [ADR-0024](0024-unified-version-with-skew.md) 不强制同版本部署的主要理由会因此变弱，届时可以
  重新评估。

## 其他发现

G 组的主机重启后，HAProxy 启动约 4 秒后 VIP 就被宣告，此时后端的健康检查还没有通过，新建连接在
约 6 秒内被接受后又被立即关闭。这与 GR 无关，现有 agent 在主机启动时同样如此。ADR-0017 让后端
健康不影响宣告，但它针对的是稳态；HAProxy 刚启动、检查尚未完成时是否应该推迟首次宣告，另行讨论。

## 待验证

- [ ] 集成测试：GoBGP 作为 helper 路由器，覆盖重启时保留、restart time 到期撤回、关机时立即撤回，
      以及先填 RIB 再连接（不发空的 End-of-RIB）。
- [ ] cookbook 写入 `gracefulRestartTime`，在 Dev 与 Prod 的 policy 中开启。
- [ ] Prod 上逐台验证一次 B 组。
