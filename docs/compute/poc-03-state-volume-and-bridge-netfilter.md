# PoC 03：实例状态卷与 kubelet 共存时的 bridge 流量

- 日期：2026-10-30
- 对应：[ADR-0020](../decisions/0020-linstor-drbd-storage-integration.md) 待验证第 4 项（实例状态卷，原 ADR-0015 第 11 项）和
  第 6 项（与 kubelet 共存，仅网络机制部分）
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的三台嵌套 VM。
- 结论：**实例状态卷通过**：NVRAM、vTPM 和电源意图都随硬断电后的冷切换迁移。**网络机制已确认**：
  启用 Kubernetes 要求的 bridge netfilter 后，实例的 bridge 流量受宿主机 iptables FORWARD 支配，
  必须在设计中规避。

## 实例状态卷

### 布置

- DRBD 资源 `uefi` 带两个 volume：volume 0 是 300 MiB 系统盘（Alpine UEFI 镜像），volume 1 是
  64 MiB 的 ext4 **状态卷**。
- promoter 的 `start` 依次为：
  1. 把状态卷挂载到 `/var/lib/ayatori/instances/uefi`；
  2. 把状态卷上的 `tpm/` bind mount 到 libvirt 固定的 `/var/lib/libvirt/swtpm/<uuid>`；
  3. 启动监管进程 `poc-vm@uefi`。
- domain：q35，OVMF（未启用 Secure Boot），`<nvram>` 指向状态卷上的 `VARS.fd`，`tpm-crb` + swtpm
  2.0。
- 监管进程：观察到非崩溃停止时在状态卷上写 `power-off` 标记，启动前检查该标记。由 systemd 停止时
  （evict、切换）通过 TERM trap 执行 `destroy` 并以 0 退出，不写标记（单元还需要
  `KillMode=mixed`，否则 trap 中的 `destroy` 可能被 systemd 杀掉，见
  [PoC 05](poc-05-spiffe-identity-and-console.md)）。单元不再使用
  `ExecStop=`，否则 evict 时监管进程可能先看到 `destroyed` 并误写标记。

### 结果

| 验证 | 过程 | 结果 |
|---|---|---|
| NVRAM 初始化 | 首次启动 | OVMF 从模板在状态卷上创建 `VARS.fd` |
| NVRAM 随实例迁移 | 客户机经 efivarfs 写入自定义变量（NV+BS+RT），然后对 poc-1 执行 `qm stop` | poc-2 约 11 秒后接管，客户机内仍能读到 `ayatori-poc-marker` |
| vTPM 随实例迁移 | 同上 | poc-2 的 swtpm 使用 `tpmstate dir=/var/lib/libvirt/swtpm/<uuid>/tpm2`，来源 `/dev/drbd104[/tpm]`；swtpm 日志为空，即**没有**运行 `swtpm_setup`（manufacturing），沿用了原有的 TPM 身份 |
| 电源意图随实例迁移 | 客户机内 `poweroff`，然后对 poc-2 执行 `qm stop` | 状态卷上写入 `power-off`；poc-1 接管 Primary 并挂载状态卷，监管进程输出"powered off on purpose (flag on state volume), not starting"，**没有开机** |
| 运行时开机 | 移除标记，`systemctl start poc-vm@uefi` | 在 poc-1 正常启动，NVRAM 变量仍在 |

### 发现

- Alpine `virt` 内核不包含任何 TPM 驱动，因此 vTPM 是否沿用由宿主机侧判断（是否执行了
  manufacturing、swtpm 的状态目录来源），没有在客户机内读取 EK。
- bind mount 单元名包含 `\x2d` 转义。在 TOML 的双引号字符串里 `\x` 是非法转义，drbd-reactor 会
  拒绝整个配置。Agent 生成 promoter 配置时，单元名必须使用单引号（literal）字符串。
- 状态卷首次使用前需要初始化目录结构（`tpm/`）。由 Agent 在创建实例时完成。

## 与 kubelet 共存：bridge netfilter

Kubernetes 要求加载 `br_netfilter` 并设置 `net.bridge.bridge-nf-call-iptables=1`。这是全局开关，
会让宿主机上**所有** Linux bridge 的流量经过 iptables，包括 VM/LXC 使用的 bridge。

用 `curl | sh` 安装 k0s 的操作被 Claude Code 的自动模式拦截，因此本次没有运行真实的
kubelet，而是直接施加 Kubernetes 所要求的内核设置，验证机制本身。测试 bridge 上挂两个 netns
端点（veth，与 LXC veth、VM tap 在 bridge 上的行为相同）：

| 宿主机状态 | 跨 bridge ping |
|---|---|
| 未加载 `br_netfilter` | 通 |
| `br_netfilter` + `bridge-nf-call-iptables=1`，FORWARD 策略 ACCEPT | 通 |
| 再把 FORWARD 策略设为 DROP（Docker 和部分严格 CNI 会这样做） | **100% 丢包**，FORWARD 计数到 3 个被丢弃的 bridge 包 |
| 再在 FORWARD 中插入放行该 bridge 流量的规则 | 通 |

结论：

- 节点一旦运行 kubelet，实例网络的可用性就取决于谁在管理 iptables FORWARD。
- 放行规则必须位于 FORWARD 链**内部**。在另一个更高优先级的 nftables base chain 中 accept 无效：
  accept 只结束当前 base chain，后面的 iptables filter 仍可以丢弃这个包。
- 可选方案：
  1. Chef 在 FORWARD 首位维护放行实例 bridge 的规则。简单，但会受其他管理者（Docker、CNI）
     插入或重排规则的影响。
  2. **实例网络使用 OVS**：OVS bridge 不是 Linux bridge，`br_netfilter` 不作用于它。还能在
     `LibvirtNetwork` 的 network XML 里直接写 `<vlan><tag>`（ADR-0015 已列为将来的选项）。
  3. macvtap/macvlan：绕过 bridge，但宿主机与实例之间的通信需要额外处理。

## 未覆盖

- 真实的 kubelet/k0s：cgroup 共存、`systemReserved`/`reservedSystemCPUs` 静态切分、CNI 的实际规则。
- OVS 方案下的 VM/LXC 网络与 drbd-reactor 冷切换。
