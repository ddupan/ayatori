# 对照 vSphere 功能集的设计缺口

- 日期：2026-11-03
- 范围：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md)、
  [ADR-0016](../decisions/0016-instance-node-services.md) 的计算后端
- 方法：按 vSphere 8 文档（Availability、Resource Management、VM Administration、Storage、
  Networking、Security、Monitoring、Host Profiles、Lifecycle Manager）逐项对照，只保留 homelab
  中**有现状证据**的需求（AGENTS.md：其他平台提供不构成需求）。证据来自 homelab-infra 与
  homelab-wiki。
- 结论：缺口的设计见 [ADR-0017](../decisions/0017-compute-day2-operations.md)。

## 一、有需求、尚无设计

| vSphere | 证据 | 设计 |
|---|---|---|
| VMCP（APD：存储暂时不可用时冻结，恢复后继续） | 2026-09-25 事故：新卷初始同步期间多个 DRBD 资源 quorum 抖动 43 次，`on-no-quorum io-error` 让 sandbox1、etcd CT150 的 rootfs 变为只读，控制面中断 33 分钟（wiki `incidents/2026-09-25-drbd-quorum-sandbox.md`）。ADR-0015 沿用了同样的 `io-error` | ADR-0017 §1 |
| SIOC / 存储 I/O 争用 | 同上：触发因素是两块新卷的初始同步 | ADR-0017 §1 |
| 备份（VADP/CBT）与恢复 | PVE 上 9 个客户机全部没有备份，未设告警；dc1 无任何备份（`platform/observability/COVERAGE.md:120-121`）。LINSTOR 用的是 thick LVM（`pve_linstor/tasks/linstor.yml:82`），没有快照能力 | ADR-0017 §2 |
| 维护模式与 evacuation | 2026-07-25 三台节点全部经 PXE 重装；升级按 `serial: 1` 逐台进行，需要重启时只报告不执行（`pve_post_install`、`pve_kernel_params`）；roadmap 的 Laptop Rebuild 验收场景 | ADR-0017 §3 |
| 冷迁移（指定目标节点） | VM 101 先建在 pve1 再手工挪走（`pve_vm/defaults/main.yml:11,52`）；`qm migrate` 离线迁移 | ADR-0017 §3 |
| EVC（CPU 兼容基线） | pve1 是 Intel，pve2/pve3 是 AMD；VM 100 用 `host` CPU 在线迁移失败，改为 `x86-64-v2-AES`（`pve_ha/defaults/main.yml:27-30`） | ADR-0017 §4 |
| VM-VM 反亲和 | LB 每个环境两台分在不同节点、etcd 每节点一个、sandbox1/2 分开，全部靠 inventory 手工保证 | ADR-0017 §5 |
| HA admission control（故障切换容量） | 宿主机为 16G/8G/8G，单台故障后剩余容量能否承接没有任何检查 | ADR-0017 §6 |
| 告警与性能统计 | 现有 PVE 告警 `StorageAlmostFull`、`OnbootGuestDown`、`GuestLockedLong`、`NodeOffline`（`rules/proxmox.yaml`）在换掉 PVE 后没有对应项；ADR-0015 的告警只覆盖 HA | ADR-0017 §7 |
| 磁盘扩容（hot-extend） | sandbox LXC 经 API 扩 rootfs（`sandbox_lxc/tasks/main.yml:119-146`） | ADR-0017 §8 |
| 导入（Converter） | 现有 PVE 客户机都要迁过来；VM/CT 磁盘已经在 LINSTOR 上 | ADR-0017 §9 |
| Content Library（ISO/镜像分发） | 换盘需要 ISO pool，但谁来填充没有设计；`qm importdisk` 曾写坏磁盘（`pve_vm/tasks/image.yml:4-7`）；LXC 的 `download` 模板依赖外网，而家宽上联不稳定（`group_vars/pve.yml:35-38`） | ADR-0017 §10 |

## 二、已有设计

| vSphere | 对应 |
|---|---|
| HA 主机故障检测与重启、FDM 本地缓存 | drbd-reactor promoter + Agent 本地状态（ADR-0015） |
| VM Monitoring | `<watchdog>`、`<on_crash>` 与监管进程（ADR-0015） |
| reservation / limit / shares、内存超售 | 资源模型（ADR-0015） |
| 虚拟硬件、Secure Boot、vTPM、直通、串口 | 原生 domain XML 透传；NVRAM/vTPM 在状态卷（ADR-0015） |
| Guest customization | cloud-init NoCloud（ADR-0016） |
| VMware Tools 客户机信息 | qemu-guest-agent 回写 IP（ADR-0015） |
| 控制台、换盘、电源操作 | 计算控制台（ADR-0016） |
| 端口组 / VLAN | `LibvirtNetwork` + Chef bridge（ADR-0015） |
| 权限、审计、事件、标签 | Kubernetes RBAC、审计日志、Event、label |
| Host Profiles / Lifecycle Manager 的配置部分 | Chef Policyfile |

## 三、暂无需求

没有现状证据，或已有更合适的层承担。出现证据时再评估。

| vSphere | 理由 |
|---|---|
| 快照（作为用户功能） | 没有任何 `qm/pct snapshot` 使用；快照只作为备份的实现手段 |
| 模板、链接克隆、即时克隆 | 没有模板和克隆的使用；实例从镜像加 cloud-init 创建 |
| 重启优先级与编排 | 只有 sandbox 用过 `startup order`；依赖顺序由应用重试承担。全站断电恢复成为问题时再评估 |
| DRS 负载均衡、DPM、资源池层级 | 3 台节点，没有自动平衡的需求 |
| vMotion | ADR-0007 不作为目标；是否为 VyOS 例外见 ADR-0017 §3 的待决问题 |
| Fault Tolerance、vSphere Replication/SRM | 单站点；冗余由应用层或 DRBD 承担 |
| 防火墙、安全组、PVLAN、端口镜像、NIOC、LACP | PVE 防火墙未使用，VyOS 也未配置防火墙（`README-monitoring.md:76`） |
| VM 加密与 KMS | 没有使用；bao1 的 TPM 直通在笔记本的 libvirt 上，由原生 XML 覆盖 |
| 计划任务（定时开关机） | 没有使用 |
| 静态 IPAM、DNS 自动注册 | ADR-0015 已决定暂不做；DNS 型资源待新 DNS 基建 |
