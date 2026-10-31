# PoC 07：实例内 spire-agent 的节点证明，以及 PVE 上的 ayatori-imds

- 日期：2026-10-31
- 对应：[ADR-0016](../decisions/0016-instance-node-services.md) 待验证第 6、7 项
- 环境：
  - 沿用 PoC 节点上的 SPIRE（trust domain `poc.ayatori`）和 dev 模式的 Bao；
  - 生产 PVE 集群的 pve3 上临时建了测试 VM 990（Ubuntu 24.04 cloud image）和 LXC 991（Debian 13），
    加上 pve3 宿主机上的 SPIRE agent、imds 替身和 nftables 规则；
  - 测试结束后，pve3 上的所有改动和这两个实例都已删除。
- 结论：**两项都通过**。实例里的标准 spire-agent 通过宿主机签发的实例身份文档完成节点证明，伪造和
  重放的文档都被拒绝；同一套 imds 在 PVE 上按 PVE 自己的端口名识别实例，包括 VM 开了防火墙的情况。

## 实例身份文档与 `ayatori_iid` 节点证明插件

### 协议

1. 实例内的 spire-agent 发起证明，`ayatori_iid` agent 插件发送一个不含信息的首包。
2. SPIRE server 回 32 字节随机挑战。
3. agent 插件把挑战交给 `http://169.254.169.254/latest/meta-data/attestation?challenge=…`（走 IMDSv2
   token）。
4. 宿主机上的 imds 签发文档 `{instance, serviceAccount, challenge, iat}`，用**宿主机自己的
   X.509-SVID** 做 ECDSA 签名，并附上证书链。
5. server 插件依次校验：
   - 证书链由本 trust domain 的根签发，根证书通过 SPIRE 的 IdentityProvider host service 获取；
   - 宿主机的 SPIFFE ID 在 `host_ids` 名单中；
   - 签名正确，挑战一致，`iat` 在 ±1 分钟内；
   - 实例名和 service account 名的格式合法。
6. 通过后，agent ID 为 `spiffe://<td>/spire/agent/ayatori_iid/<ns>/<name>`，选择器为
   `instance:<ns>/<name>`、`sa:<ns>/<sa>`、`host:<宿主机 ID>`，`CanReattest = true`（每次都有新
   挑战，实例换节点后可以重新证明）。

工作负载条目通过 node alias 按 service account 挂接：
- alias `…/ayatori/sa-hosts/<ns>/<sa>`，选择器 `ayatori_iid:sa:<ns>/<sa>`，覆盖所有挂了这个
  service account 的实例；
- 工作负载条目以这个 alias 为 parent，SPIFFE ID 为 `…/ayatori/ns/<ns>/sa/<sa>`。

插件约 250 行 Go（server 和 agent 两个 binary，依赖 `spire-plugin-sdk` v1.15.3）。server 侧的校验
函数有单元测试，覆盖合法、重放、伪造 CA、宿主机不在名单中、签名密钥不匹配、过期 6 种情况。

### 结果（poc 节点，Alpine vm1）

| 测试 | 结果 |
|---|---|
| vm1 内 spire-agent 启动，trust bundle 从元数据服务获取 | 节点证明成功，agent ID `…/spire/agent/ayatori_iid/default/vm1` |
| server 记录的选择器 | `instance:default/vm1`、`sa:default/web`、`host:spiffe://poc.ayatori/ayatori-agent` |
| 实例内用标准 Workload API 取 SVID | X.509-SVID `…/ayatori/ns/default/sa/web`；JWT-SVID 登录 Bao role `web`，读到 secret |
| 假 imds 返回一份截获的合法文档（重放） | 拒绝：`challenge mismatch (replayed document?)` |
| 假 imds 用自签证书签名，SPIFFE ID 与宿主机相同（伪造） | 拒绝：`host certificate not trusted: x509: certificate signed by unknown authority` |
| vm1 evict 到 poc-2，清空 agent 数据后重新证明 | 成功，同一个 agent ID，这次由 poc-2 的 imds 签发 |

## PVE 上的 `ayatori-imds`

### 布置（pve3）

- 宿主机 SPIRE agent（join token），身份为 `spiffe://poc.ayatori/ayatori-imds`，同时作为 Delegated
  Identity API 的 delegate，也是签发实例身份文档的宿主机身份。
- imds 替身：
  - 每个实例一个监听端口 `20000+vmid`，绑定在 vmbr0 的地址上；
  - 元数据读 `/etc/pve/qemu-server/<vmid>.conf` 或 `/etc/pve/lxc/<vmid>.conf`（只取当前配置段，
    不含快照段）；
  - service account 取 PVE tag `sa-<名字>`，namespace 暂用 `pve-<集群>`。
- nftables：bridge 层按 `tap<vmid>i*`/`veth<vmid>i*` 打 mark = vmid，目的 MAC 改为 vmbr0 的 MAC 并
  `pkttype set host`；三层按 mark DNAT。
- SPIRE 选择器 `pve:vmid:homelab/<vmid>`，代领的身份为 `…/ayatori/ns/pve-homelab/sa/web`。

### 结果

| 测试 | 结果 |
|---|---|
| LXC 991：元数据 | `pve:homelab/991 lxc imds-test-ct tags=poc;sa-web` |
| LXC 991：代领 JWT-SVID → Bao | sub 为 `…/ayatori/ns/pve-homelab/sa/web`，读到 secret |
| VM 990（未开防火墙）：同上 | 通过 |
| VM 990 开启 PVE 防火墙（`firewall=1`，tap 接到 `fwbr990i0`） | 通过，规则不用改 |
| VM 990 内 spire-agent 经 pve3 的 imds 做节点证明 | 成功，选择器 `host:spiffe://poc.ayatori/ayatori-imds`、`sa:pve-homelab/web`；取得 X.509-SVID |

只在 VM 上开了防火墙，数据中心级的 PVE 防火墙是关着的，所以 `fwbr` 上没有过滤规则。开启数据中心级
防火墙后它会经 `br_netfilter` 过滤 fwbr 上的流量，这种情况没有测：这次不改生产集群的防火墙设置。

## 发现

- **只改写目的 MAC 不够，必须同时 `meta pkttype set host`。** bridge 判断帧是否发给本机发生在
  bridge prerouting hook 之前，只改 MAC 的帧到了 vmbr0 仍是 `PACKET_OTHERHOST`，被 IP 栈丢弃。抓包
  可以看到这个标记。开启防火墙的路径中，帧经过 fwbr 和 veth 进入 vmbr0，pkttype 会按新 MAC 重新
  计算，加上这句也不受影响。
- **在 promoter 之外 demote 资源，systemd 状态会过期。** PoC 06 场景 C 手动执行
  `drbdadm secondary --force` 后，poc-2 上的 `drbd-promote@vm1` 和 `drbd-services@vm1.target` 一直
  显示 active，实际角色却是 Secondary。之后 evict 到 poc-2 时：
  - promote 单元不会再执行；
  - 监管进程检查 Primary 后拒绝启动（PoC 02 的修复起了作用）；
  - 资源弹回 poc-1，没有造成损害，但 evict 实际上没有生效。
  停掉这两个过期单元后 evict 正常。Agent 应当对"target active 但不是 Primary"告警或自动复位。
- 实例内 agent 在冷启动后需要作为服务运行。PoC 中用 nohup 启动，evict 后 VM 冷启动，agent 就没有
  了；重新启动 agent 后能正常重新证明。

## 未覆盖

- 签发宿主机与实例实际所在节点的核对：server 插件目前信任宿主机的声明。正式实现应核对 compute
  controller 记录的候选节点或 DRBD Primary，防止一台被攻破的宿主机为任意实例签发文档；
- 实例内 agent 用 `disk` KeyManager 持久化 SVID 后，冷启动和换节点时直接沿用 SVID、不重新证明的
  路径；
- 开启数据中心级 PVE 防火墙的情况；
- LXC 内运行 spire-agent（PVE 上 LXC 991 只测了代领路径）。
