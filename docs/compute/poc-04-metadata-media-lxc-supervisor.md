# PoC 04：元数据重定向、运行时换盘与 LXC 监管进程

- 日期：2026-10-31
- 对应：[ADR-0016](../decisions/0016-instance-node-services.md) 待验证第 1、4、5 项；
  [ADR-0018](../decisions/0018-linstor-drbd-storage-integration.md) 第 3 项（原 ADR-0015 第 9 项）的 LXC 部分
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的三台嵌套 VM。
- 结论：**三项都通过**。另外发现几条对设计有约束的事实：实例端口名必须固定；不能用
  `systemctl restart` 重启实例；`lxc-start -F` 的退出码区分不了崩溃和关机；tiebreaker 必须在
  故障发生前就在线。

## 元数据服务：169.254.169.254

### 布置

- 每个节点上运行一个 Python 写的元数据服务替身，**每个实例一个监听端口**（vm1 → 18001，
  ct1 → 18002）。端口即身份，服务端不看源 IP。
- 仿照 IMDSv2：先 `PUT /latest/api/token`，并带 `X-aws-ec2-metadata-token-ttl-seconds` 头换取
  token；`GET` 时必须带 token，token 绑定实例。
- 监听 socket 设置 `IP_TTL=1`。
- nftables 分两层：

```nft
# 二层：按入口端口打 mark；目的 MAC 不是宿主机（实例的网关在外部）时改写为 bridge MAC 交给本机
table bridge imds {
  chain pre {
    type filter hook prerouting priority -300;
    iifname "ay-vm1" ip daddr 169.254.169.254 tcp dport 80 meta mark set 1 ether daddr set <virbr0 MAC> meta pkttype set host
    iifname "ay-ct1" ip daddr 169.254.169.254 tcp dport 80 meta mark set 2 ether daddr set <lxcbr0 MAC> meta pkttype set host
  }
}
# 三层：按 mark DNAT 到该实例专属的端口；没有 mark 的不会被 DNAT
table ip imds {
  chain pre {
    type nat hook prerouting priority -110;
    ip daddr 169.254.169.254 tcp dport 80 meta mark 1 dnat to 192.168.122.1:18001
    ip daddr 169.254.169.254 tcp dport 80 meta mark 2 dnat to 10.0.3.1:18002
  }
}
```

这套规则不需要 `br_netfilter`：bridge 层规则把帧交给本机，三层由普通的 IP 路径处理。

### 结果

| 测试 | 结果 |
|---|---|
| vm1 不带 token `GET` | `401 token required` |
| vm1 不带 ttl 头 `PUT` | `400` |
| vm1 正常流程 | `vm1 (src 192.168.122.253)` |
| vm1 加一个新源 IP 后请求 | `vm1 (src 192.168.122.250)`：身份不随源 IP 变化 |
| vm1 把 169.254.169.254 路由到一个外部网关 MAC | `vm1`：bridge 层改写目的 MAC 生效，模拟实例直连物理 VLAN 的情况 |
| 对照：去掉 MAC 改写后经外部网关请求 | 无响应 |
| ct1（LXC veth，另一节点）正常流程 | `ct1 (src 10.0.3.31)` |
| ct1 内再嵌套一个 netns（经 ct1 转发和 masquerade）请求 | 无响应；ct1 的 `IpInHdrErrors` 增加并发出 ICMP time exceeded |
| 同时 ct1 自己请求 | 正常拿到 token |

### 发现

- **实例端口名必须固定。** vm1 冷启动后 tap 名从 `vnet2` 变成了别的名字，按端口名写的规则立刻
  失效。domain 中写 `<target dev='ay-<实例>'/>`，LXC 写 `lxc.net.N.veth.pair = ay-<实例>`，由
  Agent 生成。libvirt 保留 `vnet`、`vif` 前缀。Linux 接口名最长 15 字符，名字需要截断或改用
  短 ID。
- **`IP_TTL=1` 要设在监听 socket 上。** 起初在 accept 之后才设置，SYN-ACK 仍是 TTL 64，嵌套
  的请求能完成握手，只是收不到数据。设在监听 socket 上后，所有回包（包括 SYN-ACK）都是 TTL 1。
- **`modprobe -r br_netfilter` 会连带卸载 `bridge` 模块**：bridge 接口不占模块引用计数，宿主机上
  所有 Linux bridge 随之消失，实例断网。这是 PoC 03 清理时造成的，本次开始时才发现。运维手册中
  不得这样卸载。

## 运行时换盘：`change-media --live`

对运行中的 vm1 执行 `virsh change-media vm1 hdc <iso pool 中的 VirtDrivers.iso> --live`：

| 时刻 | 客户机看到的 `/dev/sr0` | `domblklist` | 持久配置 |
|---|---|---|---|
| 换盘后 | `LABEL="CDROM"`（新盘） | 新盘 | 仍是 seed ISO |
| 客户机内 `reboot`（QEMU 进程不变） | 新盘 | 新盘 | 仍是 seed ISO |
| `kill -9` QEMU，由监管进程冷启动 | `LABEL="cidata"`（配置中的盘） | 配置中的盘 | 仍是 seed ISO |

与 ADR-0016 的设计一致：运行时状态只活在 QEMU 进程里，冷启动和换节点后回到配置。本次只测了
光驱，没有测软驱。

## LXC 监管进程

### `lxc-start -F` 区分不了停止原因

| 停止方式 | `lxc-start -F` 退出码 | 日志（DEBUG） |
|---|---|---|
| `kill -9` init（崩溃） | 0 | `init exited`，`ended on signal Killed(9)` |
| 容器内 `poweroff` | 0 | `is halting`，`ended on signal Interrupt(2)` |
| `lxc-stop` | 0 | 同 `poweroff`（向 init 发 SIGPWR，init 自行关机） |
| 向 `lxc-start` 发 SIGTERM | 不退出 | — |

- 退出码全部是 0，PoC 01 中"`lxc@`（`lxc-start -F`）的单元能变成 `failed`"并不可靠。
- 能区分的是 **init 的终止信号**：pid namespace 内调用 `reboot(2)` 关机时，内核以 SIGINT 结束
  init，重启时用 SIGHUP（由 liblxc 自行重启容器）。其他死法都算崩溃。PoC 通过 grep 日志得到；
  正式实现需要从 liblxc 拿到 init 的 wait status，`go-lxc` 是否提供待确认（ADR-0015 第 5 项）。
- `lxc-start` 不响应 SIGTERM，systemd 停止单元时必须由监管进程调用 `lxc-stop`。

### 监管进程

`poc-ct-run` 与 VM 版规则相同：只在 DRBD Primary 上启动；init 根本没跑起来（例如容器已在运行）
算启动失败，不 evict；崩溃在本机重启，120 秒内 3 次后写标记并以非零值退出，由 `OnFailure=`
evict；有意关机写电源意图标记；被 systemd 停止时（evict、切换）`lxc-stop` 后以 0 退出，不写标记。

PoC 中标记写在 rootfs 文件系统里（`/.ayatori-power-off`）。正式布局应在 DRBD 文件系统上把
`rootfs/` 和状态目录并列存放，标记不在容器可见范围内。

### 结果

| 测试 | 结果 |
|---|---|
| `drbd-reactorctl evict ct1`（poc-1 → poc-2） | 换到 poc-2，没有写入标记 |
| 连续三次 `kill -9` init | 前两次各 1 秒内在 poc-2 原地重启；第三次耗尽，**不到 1 秒**后 poc-1 启动 |
| 容器内 `poweroff` | 写入标记，没有重启，poc-1 仍持有 Primary |
| 随后硬断电 poc-2（此时持有 Primary）| poc-1 在 5 秒内接管 Primary，读到标记，**没有启动** |
| 运行时开机：移除标记，启动监管单元 | 正常启动，心跳日志继续 |

### 发现

- **不能用 `systemctl restart` 重启实例单元。** 重启 `lxc@ct1` 时，systemd 连带停止了挂载和
  `drbd-promote@`，资源被 demote，另一节点趁机 promote，实例换了节点。"重启"这一运行时操作必须
  由监管进程在内部完成（停止实例后再启动，进程本身不退出），Agent 不得重启 promoter 管理的单元。
- **切换监管单元时会误 evict。** 把 promoter 的 `start` 从 `lxc@ct1` 改成 `poc-ct@ct1` 并
  reload 后，新单元在旧容器仍运行时就启动了；第一版监管进程把"已在运行"当作崩溃，耗尽次数后
  evict。加入"init 没跑起来就是启动失败"的判断后不再发生。Agent 升级监管逻辑时也要注意这个
  顺序。

## tiebreaker 必须在故障前就在线

硬断电测试第一次失败：poc-2 一直是 Secondary，`quorum:no`。原因是 poc-3 上的 `ct1`、`vm1`
在 PoC 01 场景 C 之后就没有再 up，这两个资源**静默地以两节点运行了 11 小时**，期间
`drbdadm status` 只显示 `Connecting`，没有任何告警。

- 两节点之一断电后，剩下的节点没有多数，拒绝 promote：quorum 保护在起作用。
- 此时再把 poc-3 up 起来，连接建立，**仍然 `quorum:no`**。poc-3 没有见证 poc-1 的最后状态，
  无法证明 poc-1 在失联后没有继续写入，DRBD 因此拒绝恢复 quorum。tiebreaker 必须在故障发生前就
  在线。
- 把 poc-1 开机回来后才恢复：poc-2 取得 Primary，读到标记，没有启动 ct1。
- 对设计的影响：Agent 必须把每个资源的 peer 连接和 quorum 状态回写到 status，并对"副本或
  tiebreaker 缺失"告警；否则 HA 会在没人知道的情况下降级成"任一节点故障即停机"。

## 未覆盖

- 软驱换盘；
- 元数据服务的真实实现（Agent 内）、端口名截断规则、IPv6 的 `fd00:ec2::254`；
- `go-lxc` 能否取得 init 的 wait status；
- SPIFFE 代领身份与控制台代理（ADR-0016 第 2、3 项）。
