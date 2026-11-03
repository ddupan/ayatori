# PoC 13：启动失败时 `evict --keep-masked`

- 日期：2026-10-31
- 对应：[ADR-0018](../decisions/0018-linstor-drbd-storage-integration.md) 待验证第 6 项（原 ADR-0015 第 13 项）
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的嵌套 VM，drbd-reactor 1.12。
- 结论：**通过**。节点本地的启动失败会自动换到下一个节点；所有候选节点都失败时，各节点都被屏蔽，
  实例停机，不会来回弹跳；问题修好后解除屏蔽即可恢复。

## 真实案例

验证之前，这个问题已经真实发生了一次：

- 18:25 前后 poc-2 卡死。日志停在 18:25，没有 OOM 或 hung task 记录；sysstat 18:20 的采样显示可用内存仍有
  1.5 GiB，原因没有查明。
- 与 [PoC 11](poc-11-kubelet-coexistence.md) 中"宿主机活着但不响应"不同，这次 DRBD 连接也断了，poc-1 在 18:30
  按设计自动接管了三个资源。
- 但只有 uefi 起来了。vm1 和 ct1 依赖 network `lan`，而 poc-1 之前重启后 PoC 临时建的 bridge 不在了，两者启动
  失败。按旧规则（启动失败不 evict），它们停在 poc-1 上持有 Primary，直到人工介入。

## 做法

- 监管进程（VM 与 LXC）启动失败时写 `/run/poc-vm/<实例>.startfail` 后以非零值退出；重启次数耗尽时仍写
  `.exhausted`。
- `OnFailure=` 触发的处理单元按标记区分（`ConditionPathExists=|` 任一满足即执行）：
  - `.exhausted`：`drbd-reactorctl evict <实例>`，与以前相同；
  - `.startfail`：`drbd-reactorctl evict --keep-masked <实例>`。本节点上该实例的
    `drbd-services@<实例>.target` 保持屏蔽（`masked-runtime`，重启后失效），不再参与 promote。
- 解除屏蔽：`drbd-reactorctl evict --unmask <实例>`。正式实现由 Agent 在确认本节点的问题已解决（例如所需
  network、pool 已就绪）后执行。

## 结果

| 场景 | 操作 | 结果 |
|---|---|---|
| 节点本地失败 | vm1 在 poc-2，停止 poc-2 的 `lan`，`kill -9` QEMU | 本地重启时启动失败 → `evict --keep-masked` → poc-1 接管并运行 vm1；poc-2 的 target 为 `masked-runtime` |
| 全局失败 | 接着停止 poc-1 的 `lan`，`kill -9` QEMU | poc-1 启动失败后同样屏蔽自己；evict 报 `no other node took over`；两个节点都是 Secondary，观察 90 秒内 poc-2 没有任何 promote 尝试，**没有弹跳** |
| 恢复 | 两个节点恢复 `lan`，各执行 `--unmask` | vm1 在 poc-1 上被 promote 并启动 |

## 对设计的影响

- 启动失败不再"留在原地"，也不像崩溃那样普通 evict，而是屏蔽本节点后交给下一个节点。屏蔽本身就是对
  "这个节点不能运行这个实例"的记录，避免了 PoC 02 担心的来回弹跳。
- 全部候选节点都失败时实例停机，依赖告警（`InstanceDown`）提醒人工处理；屏蔽状态应同时回写 status 并作为
  指标导出，否则修好问题后没人知道要解除屏蔽。
- `masked-runtime` 在节点重启后失效。重启后若问题依旧，该节点会再失败一次、再屏蔽自己，结果仍然正确。
- 实例依赖的 network、pool 没有就绪时，Agent 本来就不应把该节点算作就绪的候选节点（ADR-0015 中候选节点的
  计算）；本机制兜住的是"看起来就绪、实际启动失败"的情况。
