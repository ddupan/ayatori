# PoC 10：同名 pool/network 的原生引用与冷切换

- 日期：2026-10-31
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 3 项（`logical` pool 与
  bridge 型 network；`rbd` 需要 Ceph，未测）
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的嵌套 VM。
- 结论：**通过**。domain 中写原生的 `<source network='lan'/>`，LXC 由 Agent 从同一份 network 定义解析出
  bridge，冷切换后两者都接到新节点自己的 bridge 上，即使各节点的 bridge 名不同。

## 布置

- 两个节点各定义同名 network `lan`（`<forward mode='bridge'/>`）。为了检验按名字解析，故意让底层
  bridge 名不同：poc-1 为 `br-lan`，poc-2 为 `br-lan2`。生产中 bridge 由 Chef 创建，名字通常一致。
- vm1 增加一块网卡：`<interface type='network'><source network='lan'/>…<target dev='ay-vm1-1'/>`。
- ct1 增加 `lxc.net.1`，`link` 由"Agent"在各节点上按 `virsh net-info lan` 的 `Bridge:` 字段生成，
  `veth.pair = ay-ct1-1`。
- 两个节点各定义同名 `logical` pool `local`，指向 VG `poc`。

## 结果

| 操作 | 新节点上的端口 |
|---|---|
| vm1 poc-2 → poc-1 | `ay-vm1-1` → `br-lan` |
| ct1 poc-1 → poc-2 | `ay-ct1-1` → `br-lan2` |
| vm1 poc-1 → poc-2 | `ay-vm1-1` → `br-lan2` |
| ct1 poc-2 → poc-1 | `ay-ct1-1` → `br-lan` |

`logical` pool：两个节点上 `<source pool='local' volume='scratch'/>` 都解析为 `/dev/poc/scratch`，但
是两块不同的 LV（UUID 不同）。这是本地存储的正确语义：引用的含义随节点变化，所以本地存储的实例只能
有一个候选节点，不能冷切换（ADR-0015 已有此规则）。

## 发现

- **本地存储的 VG 必须和 DRBD 底层 VG 分开。** `local` pool 把 DRBD 的底层 LV（`vm1`、`ct1`、
  `uefi-*`）也列成了卷。任何人通过 libvirt 对它们执行 `vol-delete`，或者把底层 LV 直接挂给 domain，
  都会绕过 DRBD，造成数据损坏或双写。PoC 中两者共用 VG 只是因为方便。
- Agent 解析 bridge 名要用 `virsh net-info`（或 libvirt API）的结构化输出。第一次用正则从 XML 里
  截取时，poc-1 上截出了多余字符、poc-2 上截错了名字。正式实现直接解析 network XML。
- LXC 的 veth 由 liblxc 创建，`bridge link` 显示的对端名会带上 `@<宿主机网卡>` 之类的后缀，与 tap 不同；
  按端口名写的 nftables 规则匹配的是本端名字 `ay-ct1-1`，不受影响。
