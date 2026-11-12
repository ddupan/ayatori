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
连接全部被重置。agent 被 OOM kill 或崩溃时也是如此。所以 agent 的每次升级都要逐台滚动，CI 的
并行 apply 不能直接合并。

## 原则

可用性相关的行为只用协议的原生语义，不在 agent 里加判断。升级是低频操作，可以为它保留一条
专门的执行路径。

## 不选的方案

- **把 speaker 拆成独立进程**：独立的 speaker 自身也要升级，照样会断开会话；agent 与它之间要
  恢复一个本地 API。这正是 ADR-0017 内嵌 speaker 时去掉的东西，而且会失去「agent 不在，路径就
  不在」的性质。
- **由 agent 判断退出原因**（例如查询 systemd 是否在关机，原型的第一版）：stop、restart 与关机时，
  systemd 发出的都是同一个 `KillSignal`，agent 只能再去查询系统状态。这把可用性建立在一次外部
  查询和对它的解读上，查询失败时的处理又是一条要验证的分支。
- **开启 RFC 8538（`notification-enabled`）**：开启后，hold timer 超时也会进入保留，主机失联时
  路由器要多等一个 restart time 才撤回。不开启时，hold timer 超时的处理与现在相同（实验 E）。

## 决策

agent 开启 BGP graceful restart（RFC 4724），只作为 restarting speaker。路由器（FRR）默认就是
helper，不需要改配置。路由器是否保留路径，完全按协议由会话如何断开决定：

| 会话如何断开 | 何时发生 | 路由器 |
|---|---|---|
| 发出 NOTIFICATION | SIGTERM：`systemctl stop`、`restart`、关机 | 立即撤回（与现在相同） |
| TCP 直接断开 | SIGKILL、OOM kill、崩溃 | 保留 restart time，本机重连并发出 End-of-RIB 后替换 |
| 没有报文（hold timer 超时） | 主机或网络失联 | hold timer 超时后撤回（与现在相同） |

1. **配置。** agent 新增配置项 `gracefulRestartTime`，为零时关闭（保持现有行为）。部署值放在
   环境 policy 中，初值 30s：要大于 systemd 的 `RestartSec`（5s）加上启动时间。它只是 agent
   起不来时的上限；agent 一旦拉起，就会按 HAProxy 的现状重新宣告并发出 End-of-RIB，路由器立即
   结束保留（实验 K2）。
2. **启动。** 先按 HAProxy 现状填好本地 RIB，再连接邻居。每次启动都以 restarting 身份连接
   （OPEN 中置 R 位）：路由器保留着的旧路径要等本机发出 End-of-RIB 才会被替换。若先连接后填表，
   可能先发出空的 End-of-RIB，路由器随之删掉保留的路径。
3. **退出。** SIGTERM 时与现在相同，发出 NOTIFICATION。agent 不判断退出原因。
4. **升级。** cookbook 以 `systemctl kill -s SIGKILL ayatori-lb-agent` 重启 agent，由
   `Restart=on-failure` 在 `RestartSec` 后拉起。这是唯一有意走保留路径的操作。agent 唯一会写的
   文件是客户端证书，写法是临时文件加 rename，被杀掉不会留下半个文件。人手动执行
   `systemctl restart` 时照常撤回。

## 实验（lb-dev，2026-11-11）

目标是 Dev 上一个临时 LoadBalancer，后端是笔记本上的 echo 服务。探测程序每 100ms 新建一条连接，
同时在 20 条长连接上每 100ms 收发一次数据。VyOS 按三层地址做 ECMP 哈希，笔记本的流量全部落在
lb-dev-2 上，所以动作都在 lb-dev-2 上执行。restart time 为 30s。

| 组 | agent | 动作 | 路由 | 新建连接失败 | 长连接存活 |
|---|---|---|---|---:|---:|
| A | 0.3.0 | `systemctl restart` | 撤回约 3s 后恢复 | 0 | 0/20 |
| I | GR | `systemctl restart` | 立即撤回（与 A 相同） | 0 | 0/20 |
| J | GR | `systemctl kill -s SIGKILL`，5s 后拉起 | 不撤回 | 0 | 20/20 |
| D | GR | 停止 agent，不再拉起 | 30s 后撤回 | 0 | 0/20（撤回时） |
| E | GR | 丢弃 BGP 报文（模拟主机失联） | 约 63s 后撤回（hold timer） | 0 | 0/20（撤回时） |
| K2 | GR | SIGKILL agent，同时停止 HAProxy | agent 拉起后撤回（约 7s） | 7.4s 内失败 | 0/20 |
| K | GR | SIGKILL HAProxy 与 agent，均由 systemd 拉起 | 不撤回 | 8.8s 内失败 | 0/20 |

说明：

- 在 E 组中，FRR 记录的断开原因是 `Hold Timer Expired`，并发送了 NOTIFICATION，因此没有进入
  保留。主机失联时的黑洞时长仍由 hold timer 决定（协商值 90s），与是否开启 GR 无关。
- K2 是 GR 的代价：agent 与 HAProxy 同时故障时，撤回要等 agent 被拉起（约 7s）；不开 GR 时会在
  agent 退出的那一刻撤回。单独 agent 故障（OOM、崩溃）时则相反，不开 GR 会重置全部连接，开了则
  不受影响（J）。
- K 组的失败来自 HAProxy 重启与后端健康检查尚未通过（见「其他发现」），与 GR 无关。
- D 组用的是原型的第一版（SIGTERM 时也保留），最终的决策下 `systemctl stop` 立即撤回（同 I）；
  D 组仍说明了 agent 不再回来时保留的上限。原型第一版还做过 B、C、F、G、H 组，因方案改变不再
  列出。

## 后果

- 升级与单独的 agent 故障不再影响经过这台主机的连接。两台主机在 restart time 内都回来时，
  并行 apply 也不会撤回 VIP。逐台滚动不再是可用性的要求，只用于尽早发现坏版本：坏版本起不来时，
  restart time 到期后照样撤回。
- agent 与 HAProxy 同时故障时，撤回推迟到 agent 被拉起（实验 K2，约 7s）；agent 起不来时最长为
  restart time。
- [ADR-0024](0024-unified-version-with-skew.md) 不强制同版本部署的主要理由会因此变弱，届时可以
  重新评估。

## 其他发现

主机重启或 HAProxy 重启后，VIP 在后端健康检查通过之前就被宣告，新建连接在约 6s 内被接受后又被
立即关闭（实验 K，以及原型第一版的主机重启实验）。这与 GR 无关，现有 agent 同样如此。ADR-0017
让后端健康不影响宣告，但它针对的是稳态；HAProxy 刚启动、检查尚未完成时是否应该推迟首次宣告，
另行讨论。

## 待验证

- [x] 集成测试（`TestAgentGracefulRestart`）：GoBGP 作为 helper 路由器，覆盖 SIGKILL 式断开时保留、
      restart time 到期撤回、`Stop` 时立即撤回，以及重启期间路由器不撤回任何路径。「先填 RIB 再
      连接」本身测不出来：GoBGP 首次主动连接前有延迟，去掉连接前的那次收敛测试照样通过；这个顺序
      只是不依赖该延迟的保证。
- [ ] cookbook 写入 `gracefulRestartTime`，升级时以 SIGKILL 重启 agent；在 Dev 与 Prod 的 policy
      中开启。
- [ ] Prod 上逐台验证一次 J 组。
