# PoC 06：HA 告警链路

- 日期：2026-10-31
- 对应：[ADR-0018](../decisions/0018-linstor-drbd-storage-integration.md) 待验证第 5 项（原 ADR-0015 第 12 项）与"可观测性"一节
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的三台嵌套 VM。监控放在 tiebreaker poc-3 上，
  不跑实例，断掉 poc-1/2 时监控本身不受影响。用 Ubuntu 包的 Prometheus 2.45 和 Alertmanager
  0.26 代替中央 VictoriaMetrics；接收端是一个把告警写入日志的 webhook，代替 Telegram。
- 结论：**通过**。可以触发的 7 条告警都按预期送达，恢复后也都发出了 resolved。发现两点：drbd-reactor
  1.12 的 `drbd_resource_forceiofailures` 指标恒为 0；Agent 指标的实例标签不能叫 `instance`。

## 布置

| 组件 | 位置 | 内容 |
|---|---|---|
| drbd-reactor Prometheus 插件 | poc-1、poc-2 `:9942` | `/etc/drbd-reactor.d/prometheus.toml`：`[[prometheus]]`、`enums = true` |
| Agent 指标替身 | poc-1、poc-2 `:9943` | 抓取时现算：期望运行、是否在跑、本节点是否 Primary、在非 Primary 上运行、电源意图、重启与 evict 次数（数监管进程的 journal） |
| Prometheus + Alertmanager | poc-3 | 抓取间隔 10 秒；规则即 ADR-0015 的第一批告警 |
| 告警接收端 | poc-3 | 把 FIRING/RESOLVED 写入 `/var/log/ayatori-alerts.log` |

规则中的 `for` 时长为了测试而缩短，生产值写在规则注释里（如冗余降级 5 分钟）。

## 结果

| 场景 | 操作 | 收到的告警 |
|---|---|---|
| A. tiebreaker 掉线 | poc-3 `drbdadm down ct1` | 约 70 秒后两个节点各一条 `DRBDPeerDisconnected`（`drbd-poc-3` 为 `Connecting`）；`up` 后 30 秒内 resolved。期间 quorum 指标一直为 1 |
| B. 原地重启 | `kill -9` vm1 的 QEMU | `InstanceRestarted` |
| C. 残留 QEMU | 对运行中的 vm1 执行 `drbdadm secondary --force`，另一节点接管 | 约 35 秒后 `InstanceRunningNotPrimary`；`destroy` 残留后 resolved。`DRBDForceIOFailures` **没有触发**，见下文 |
| D. exporter 消失 | 停止 poc-2 的 Agent exporter | `ExporterDown`；恢复后 resolved |
| E. 应运行但未运行 | vm1（没有状态卷）客户机内关机 | 约 80 秒后 `InstanceDown`；开机后 resolved |
| E'. 有意关机 | uefi 客户机内关机，状态卷上写入电源意图标记 | **没有**告警 |
| F. 重启耗尽换节点 | 120 秒内让 ct1 的 init 崩溃到监管进程耗尽 | `InstanceEvicted`，ct1 已由 poc-2 接管 |

没有触发：`DRBDNoQuorum`、`DRBDSplitBrain`、`DRBDOutOfSyncStuck`。前两者的状态在 PoC 01 中出现过，
指标名与 `drbdsetup` 的字段一一对应；后者需要人为制造长时间同步停滞。

## 发现

### drbd-reactor 1.12 的 `drbd_resource_forceiofailures` 恒为 0

场景 C 中 `drbdsetup status` 和 `events2` 都显示 `force-io-failures:yes`，内核日志也有
`Rejected WRITE request, not in Primary role`，但 reactor 导出的指标一直是 0，reload 后仍然如此。

- ADR-0015 中这条告警不能依赖 reactor 的指标。残留 QEMU 由 Agent 的 `InstanceRunningNotPrimary`
  覆盖，本次就是靠它发现的。
- 如果需要单独告警 force-io-failures，由 Agent 从 `drbdsetup events2` 读取并导出。
- 值得向上游报告，本次没有报告。

### Agent 指标的实例标签不能叫 `instance`

`instance` 是 Prometheus 的抓取目标标签。exporter 自带的 `instance` 会被改写成
`exported_instance`，告警里显示的变成了 `10.60.0.72:9943`，按实例聚合的 `InstanceDown` 等规则全部
算错。改用 `name`，与 DRBD 指标的资源名标签一致。

### 其他

- 重启计数器的告警依赖 `increase()`：exporter 重启后从非零值出现的新时间序列不会被算作增加。正式
  Agent 的计数器应当是进程内单调递增的，并接受 exporter 重启时丢失一次增量。
- 场景 E 说明了"应运行"的判定需要电源意图：没有状态卷的实例被有意关机后，告警无法与故障区分。
  这与 ADR-0015 中"受保护实例都带状态卷"的要求一致。
- 场景 F 的滑动窗口统计的是 120 秒内的**启动次数**（包括首次启动）。首次启动已滑出窗口时，要
  第 4 次崩溃才会耗尽。

## 未覆盖

- 接入中央 VictoriaMetrics、按现有约定的 `VMStaticScrape` 与 Telegram 通知；
- `DRBDNoQuorum`、`DRBDSplitBrain`、`DRBDOutOfSyncStuck` 的实际触发；
- 节点整体宕机时的告警（由现有的 node exporter `up` 告警覆盖）。
