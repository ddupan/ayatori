# PoC 08：go-lxc 的构建与容器停止原因

- 日期：2026-10-31
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 5 项；接续
  [PoC 04](poc-04-metadata-media-lxc-supervisor.md) 中"`go-lxc` 能否取得 init 的 wait status"的问题
- 环境：poc-2（Ubuntu 24.04，liblxc 5.0.3，Go 1.22.2），一次性测试容器 t1（Alpine 3.24），测试后
  已删除。
- 结论：**go-lxc 能构建，但通过 liblxc 的任何 API 都拿不到容器 init 的退出原因。** 唯一能区分崩溃
  和关机的是 liblxc INFO 级日志里的 `Child <pid> ended on signal …`，Agent 只能解析这行日志。

## 构建

- `github.com/lxc/go-lxc`（`v0.0.0-20260316180011-3af4ce000ed7`）在装有 `liblxc-dev`、`pkg-config`、
  `gcc` 的节点上直接 `go build` 即可，cgo 动态链接 `liblxc.so.1`。
- 对发布形态的影响：Agent 不能像其他组件那样做成纯静态的单一二进制，要么按发行版构建（在与节点相同
  发行版的容器里构建，依赖系统的 liblxc），要么连同 liblxc 一起打包。节点统一使用 Ubuntu 24.04 时，
  前者最简单。

## 拿停止原因的几条路

| 途径 | 结果 |
|---|---|
| go-lxc 前台启动（`WantDaemonize(false)`），读 `ErrorNum()` | **不可行**：liblxc 拒绝在多线程进程中前台启动容器（`Cannot start non-daemonized container when threaded`），Go 进程天生是多线程的 |
| `lxc-start -F` 的退出码（PoC 04） | 恒为 0 |
| liblxc monitor 通道（`lxc-monitor`）的 exit code 消息 | 崩溃（`kill -9` init）、容器内 `poweroff`、`lxc-stop -k` 都是 `exited with status [0]` |
| monitor 的状态序列 | 都是 `STOPPING` → `STOPPED`，没有区别 |
| `lxc.hook.stop` / `lxc.hook.post-stop` 的环境变量 | 只有 `LXC_TARGET=stop`，没有退出状态 |
| liblxc 日志（INFO 级即可） | 崩溃：`Child <pid> ended on signal Killed(9)`；关机：`Child <pid> ended on signal Interrupt(2)` |

## 对设计的影响

- LXC 监管进程按日志判断：容器配置中写 `lxc.log.file` 与 `lxc.log.level = INFO`（或由 Agent 以
  daemonize 方式启动时指定），容器停止后读取最后一条 `ended on signal`：
  - `Interrupt(2)`：容器内调用 `reboot(2)` 关机（`lxc-stop` 同样走这条路），即有意关机；
  - `Hangup(1)`：容器内重启，liblxc 会自行重新启动容器，监管进程不介入；
  - 其他信号或 `init exited`：崩溃。
- 这依赖 liblxc 日志的文本格式。升级 liblxc 时要有回归测试覆盖，测试放在节点的端到端测试中。
- 强制断电（`lxc-stop -k`）在日志中是 `Killed(9)`，与崩溃相同。因此按 ADR-0016，运行时的强制断电由
  Agent **先写电源意图标记再执行**，监管进程看到标记就不重启。
- 不采用自写的单线程 C 辅助程序来拿 wait status：多一个要维护的组件，换来的只是去掉日志解析。日志
  格式真的变动时再考虑。
