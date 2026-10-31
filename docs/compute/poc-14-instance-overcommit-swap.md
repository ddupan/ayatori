# PoC 14：实例超售与 swap/zswap

- 日期：2026-10-31
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 15 项
- 环境：poc-1（4.3 GiB 内存），2 GiB swapfile + zswap（lz4，池上限 20%）；k0s 的 kubelet 加
  `--fail-swap-on=false`，`k8s.slice` 设 `MemorySwapMax=0`，Pod 不使用 swap。vm1 配置量 1 GiB、保障量
  （`memory.low`）768M，开启 free page reporting。
- 结论：**有 swap 时超售不会导致 OOM**，实例超出保障量的部分被换出，客户机照常工作，节点保持响应。另外两点
  影响设计：保护必须沿祖先链逐级设置；`memory.low` 是软保护，保障量之和必须给内核与宿主机留出余量。

## 方法

起一个占位进程，用 `memory.min` 占住大部分物理内存，代表本节点其他实例的保障量，只留约 450M 空闲；再在 vm1
客户机内向 tmpfs 写入数据，让 vm1 超出自己的保障量。

## 结果

### 第一轮：占位进程放在 `system.slice` 下

- 占位进程的 `memory.min` 为 2.4 GiB，但它被换出了 713M：`system.slice` 的 min 为 0，子 cgroup 的保护拿不到。
  同时 `machine.slice` 设了 `MemoryLow=1G`，vm1 只被换出 30M。
- 客户机内写 900M 超过了它自身 1 GiB 内存的承受范围，**客户机内部 OOM**，SSH 断开，之后客户机一直无响应，
  需要冷启动。这与宿主机无关，是测试参数过大。

### 第二轮：占位进程放在 `machine.slice` 下，祖先链都设保护

- `machine.slice`：`MemoryMin` = 占位量，`MemoryLow` = 占位量 + 1024M；`system.slice`：`MemoryLow=512M`。
- vm1 冷启动后在客户机内写入 600M：

| | 常驻 | 换出 |
|---|---|---|
| 占位进程（`memory.min` 2476M） | 2475M | 10M（即超出 min 的部分） |
| vm1（`memory.low` 768M） | 由 656M 降到 477M | 353M |
| k0s（`system.slice` 下） | — | 约 215–250M |

- OOM kill 次数为 0；vm1、uefi、ct1 一直运行；dd 正常完成，客户机正常；节点全程有响应。
- vm1 的 `memory.events` 中 `low 2`：软保护被突破过两次。

## 发现

- **保护要沿祖先链逐级设置。** cgroup v2 中，子 cgroup 能获得的保护不超过祖先的有效保护。Agent 不能只在实例的
  scope 上写 `memory.low`，`machine.slice` 等每一级都要设置，且不小于其下各级之和。LXC 的 payload cgroup 直接在
  根下，没有这个问题。
- **`memory.low` 是软保护。** 第二轮中各级保护之和（2476 + 768 + 256 + 512 MiB）已经超过扣除内核开销后的物理内存，
  内核只能突破 vm1 的保护。因此准入时保障量之和必须给内核与宿主机留出余量（allocatable 本身已扣除宿主机预留，
  但预留要定得足够）。`memory.min` 是硬保护，用在实例上会在内存不足时把 OOM 推给别人，不采用。
- **宿主机预留要覆盖宿主机进程的实际占用。** `system.slice` 的 512M 小于 k0s 等进程的实际用量，超出部分被换出。
- **客户机内的内存上限仍是配置量。** 超售只影响宿主机侧，客户机自己用超配置量照样会在客户机内 OOM。
- zswap 这次几乎没起作用：测试数据来自 `/dev/urandom`，无法压缩。真实负载的压缩率待观察。

## 未覆盖

- KSM；
- 可压缩的真实负载下 zswap 的收益；
- swap 对实例性能的影响（这次只看了是否存活）。
