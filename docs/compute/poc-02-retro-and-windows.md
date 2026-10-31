# PoC 02：retro VM 与现代 Windows 通过 domain XML 透传运行

- 日期：2026-10-30
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 4 项（部分覆盖第 3 项）
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的三台嵌套 VM；drbd-poc-1 内存调到 4.5 GiB。
  libvirt 10.0.0（Ubuntu 24.04）。
- 结论：**设备模型方面通过**。retro VM 能在两个节点之间冷切换。现代 Windows 能启动到安装程序，
  但 NVRAM 和 vTPM 状态默认在节点本地，HA 方案还需要补上（见下文）。

## 安装介质：共享的 `iso` pool

- 两个节点都定义了同名的 `netfs` pool `iso`，只读挂载 laptop 上的
  `192.168.10.127:/mnt/pool/proxmox/template/iso`（PVE 的 ISO 也存在这里），能看到 19 个卷。
- **AppArmor 不认 `type='volume'`**：domain 用 `<source pool='iso' volume='…'/>` 引用卷时，
  QEMU 打开文件得到 `Permission denied`。改成解析后的 `type='file'` 路径就能启动，且该路径会出现在
  每个 VM 的 AppArmor 配置文件里。也就是说，`virt-aa-helper` 不会把 pool 卷加入 profile。关闭
  `security_driver` 能绕过，但这样就失去了 VM 之间的隔离，不采用。
- 结论：用户仍然可以在 XML 里写原生的 pool 引用，但 **Agent 在 `DefineXML` 之前必须用
  `virsh vol-path` 把它们解析成具体路径**。

## retro：Windows 98 SE

domain 要点：

- `machine='pc'`（i440fx），`pentium3` CPU，128 MiB 内存，启用 ACPI；
- IDE 硬盘放在新的 DRBD 资源 `retro`（400 MiB）上，IDE CD-ROM 和软驱从 `iso` pool 读取；
- `ne2k_pci` 网卡，`sb16` 声卡（`<audio type='none'/>`），`cirrus` 显卡，VNC 只监听 127.0.0.1。

结果：

- 从启动软盘启动，经 IDE 光驱进入 **Windows 98 Setup 欢迎界面**。
- QEMU 的 `info qtree` 确认所有设备都已创建：`cirrus-vga`、`sb16`、`ne2k_pci`、`piix3-ide`
  （`ide-hd` + `ide-cd`）、`isa-fdc`（`floppy`）。
- 冷切换：在 poc-2 上执行 `drbd-reactorctl evict retro`，**4 秒**后 poc-1 开始运行，并同样进入
  Win98 Setup。DRBD 上的硬盘和共享 pool 里的介质在两个节点上都可用。

## 现代 Windows：Windows Server 2025

domain 要点：`machine='q35'`，`<os firmware='efi'>` 并启用 `secure-boot` 和 `enrolled-keys`
（`OVMF_CODE_4M.ms.fd`），SMM，Hyper-V enlightenments，`tpm-crb` + swtpm 2.0，SATA 光驱
从 `iso` pool 读取 7.5 GB 的 ISO（NFS）。

结果：

- 在 Secure Boot + vTPM 下启动，约 4 分钟后进入 **Windows Server 安装程序**。启动时需要发送按键
  来跳过"Press any key to boot from CD"，本次用 `virsh send-key`。
- 本次没有完成安装，也没有测试 virtio 驱动。

**HA 缺口：固件状态默认在节点本地。**

| 状态 | 默认位置 | 冷切换后 |
|---|---|---|
| UEFI 变量（启动项、Secure Boot 数据库） | `/var/lib/libvirt/qemu/nvram/<name>_VARS.fd` | 新节点从模板重建，启动项丢失 |
| vTPM（BitLocker、Windows Hello 等依赖它） | `/var/lib/libvirt/swtpm/<uuid>/` | 新节点生成新的 TPM，BitLocker 会要求输入恢复密钥 |

- libvirt 10.0 接受 `<nvram type='block'><source dev='…'/></nvram>`，因此 NVRAM 可以放在实例的
  DRBD 资源上（作为同一 resource 的另一个 volume）。
- libvirt 10.0 会**静默丢弃** TPM backend 的 `<source type='dir'>`，状态目录固定在
  `/var/lib/libvirt/swtpm/<uuid>`。可以由 promoter 在启动前把实例 DRBD 资源上的小文件系统挂载到
  该路径，做法与 LXC rootfs 相同。
- 上述两个方案都还**没有完成冷切换验证**，另外还需确认 block 类型的 NVRAM 首次启动时是否会从
  模板初始化。

## 测试中暴露的监管进程缺陷（已修复）

retro 因为上面的 AppArmor 问题，在两个节点上都启动失败，暴露了 PoC 01 监管进程的两个缺陷：

1. **evict 来回弹跳**：启动失败也被当成"重启次数耗尽"，于是 evict 到另一节点，那边同样失败，
   再 evict 回来，循环不止。与节点无关的失败（配置错误）不应该升级为换节点。vSphere HA 同样
   限制开机重试次数。
2. **在 Secondary 节点上启动了实例**：弹跳期间，`drbd-promote@` 因为 ExecCondition 被跳过，
   systemd 却仍然启动了 `poc-vm@retro`，QEMU 卡在打开 Secondary 设备上（`paused (starting up)`）。
   DRBD 拒绝了打开，因此没有发生写入。

修复：

- 监管进程启动实例前先检查本节点是否为 DRBD Primary，不是就以 0 退出（不算失败，不会 evict）。
- `virsh start` 失败时以非零值退出，但不留下标记；只有运行中反复崩溃导致次数耗尽时才写入
  `/run/poc-vm/<实例>.exhausted`。evict 单元用 `ConditionPathExists=` 只在有标记时执行。
- 修复后，retro 的 evict 冷切换正常（见上文）。

## 未覆盖

- 完整安装 Windows 和 Win98、virtio-win 驱动、SB16 实际发声；
- NVRAM（block）和 vTPM（挂载的状态目录）跟随实例冷切换；
- 本地 `logical` pool 与 bridge 型 `LibvirtNetwork`（第 3 项其余部分）。
