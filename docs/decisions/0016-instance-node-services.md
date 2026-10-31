# ADR-0016：实例的节点侧服务：元数据、身份、控制台与运行时操作

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-10-30
- 依据：维护者讨论；建立在 [ADR-0015](0015-libvirt-lxc-compute-backend.md) 的计算后端之上，与
  [ADR-0010](0010-credential-delivery-ends-at-openbao.md)、[ADR-0013](0013-api-only-control-plane-on-lxc.md)、
  [ADR-0014](0014-kaas-cross-backend-node-supply.md) 保持一致。

## 问题

自建计算后端以后，实例相关的几件事需要我们自己提供：

- **启动配置**：实例首次启动需要 cloud-init 数据。
- **凭据**：ADR-0015 中每个 Agent 都能读到所有实例的配置，配置里不能放密钥，但实例仍然需要访问
  Bao 等服务。
- **控制台**：retro 系统和 Windows 安装需要图形控制台，headless 的 Linux/VyOS 需要串口。
- **运行时操作**：换光盘和软盘、开关机、重置。PVE 没有换软盘的界面，为此 homelab-infra 专门写过
  一个约 250 行的 `pve_floppy` 页面，说明这是真实、反复出现的需求。

这些操作都不应该通过修改 spec 完成：spec 由 GitOps 管理，运行时操作写进 spec 会和 git 脱节，用
起来也不方便。

## 原则：配置与运行时分离

参照 vSphere 对"开机时连接"（配置）和"已连接"（运行时）的区分：

| | 位置 | 修改方 | 冷启动或换节点后 |
|---|---|---|---|
| 声明的配置（设备、开机时插入的介质、`runState`） | `spec` | git | 按配置生效 |
| 运行时操作（换盘、开关机、重置、发送按键） | 控制台通道 → 节点 Agent | 控制台页面 | 回到配置；电源意图例外，见下 |
| 观察到的状态（当前介质、电源状态、所在节点） | `status` | Agent | 反映实际情况 |

- 运行时操作不修改 spec，因此不会与 Flux 冲突，也不需要"不 reconcile 某些字段"的特例。
- 唯一需要跨越冷切换保存的运行时状态是**电源意图**：有意关机的实例在宿主机故障后不应被其他节点
  开机。它写在实例的状态卷上（ADR-0015），跟随实例迁移，不依赖控制面。

## 决策

### cloud-init：NoCloud seed

- **VM**：Agent 根据 `spec.userData` 和 Ayatori 生成的 meta-data（`instance-id` 取对象 UID）
  生成 `cidata` ISO，作为只读光驱挂载。PoC 02 中 Alpine 的 SSH key 就是这样注入的。
- **LXC**：启动前把 seed 写入 rootfs 的 `/var/lib/cloud/seed/nocloud/`，与 Incus 的做法相同。
- seed 由 Agent 从本地状态生成，冷启动不依赖网络和控制面。
- `userData` 不得包含密钥，密钥一律通过下文的 SPIFFE 身份向 Bao 取用。

### 元数据服务：169.254.169.254

- **由每个节点上的 Agent 提供**，数据来自 Agent 的本地状态，控制面不可用时照常响应。
- **拦截**：在实例接入的 tap/veth 端口上，用 nftables 把发往 `169.254.169.254:80` 的流量重定向
  到本机 Agent。VM 通过 bridge 直连物理 VLAN，需要先在 bridge 层改写目的 MAC 再做 DNAT。
- **按入口端口识别实例，不按源 IP。** 客户机可以伪造 IP，但无法伪造自己接在哪个 tap/veth 上。
  OpenStack 的 metadata proxy 需要额外签名的 header 才能防伪，宿主机上则直接掌握这个事实。
  PoC 04 的做法：bridge 层按入口端口打 mark，三层按 mark DNAT 到该实例专属的端口；不需要
  `br_netfilter`。
- **实例端口名由 Agent 固定**：domain 中写 `<target dev='ay-<实例>'/>`，LXC 写
  `lxc.net.N.veth.pair`。默认的 `vnetN` 在冷启动后会变，按端口写的规则随之失效（PoC 04）。
- **防 SSRF，照搬 AWS IMDSv2**：先 PUT 换取会话 token；响应包的 IP TTL 设为 1，嵌套容器或被转发的
  请求拿不到；请求必须带指定 header（GCE 用 `Metadata-Flavor`）。TTL 要设在监听 socket 上，否则
  SYN-ACK 仍是默认 TTL，嵌套的请求能完成握手。
- **TTL 可按实例调整**：默认 1。实例内还要运行容器（Docker 默认网络多一跳）时，用
  `spec.metadata.hopLimit` 调大，对应 EC2 的 `HttpPutResponseHopLimit`。每个实例本来就有自己的
  监听 socket，按实例设置不需要额外机制。
- **不依赖客户机路由**：拦截发生在 bridge 层，客户机把请求发给哪个网关都能拦到，retro 和
  Windows 也不需要任何配置。下发 `169.254.169.254/32` 主机路由（cloud-init network-config 或
  DHCP option 121）只解决**没有默认网关的隔离网络**里请求发不出去的问题，OpenStack 的 DHCP
  agent 推送这条路由也是出于同样原因。路由 onlink 时宿主机需要应答 ARP，而且 ARP 不能泄漏到
  上联口，否则同一 VLAN 上的每台宿主机都会应答。出现隔离网络的需求时再做。
- **提供的内容**：实例元数据、动态更新的 user-data，以及下文的 SPIFFE JWT-SVID。

### 身份：Agent 通过 SPIRE 代领 SVID

- 宿主机的 spire-agent 开启 **Delegated Identity API**，Ayatori Agent 作为授权的 delegate，按
  实例的选择器代领 SVID，例如 `spiffe://<trust-domain>/ayatori/instance/<ns>/<name>`。
- **JWT-SVID 通过元数据服务提供**，路径带 `audience` 参数，与 GCE 的
  `.../service-accounts/default/identity?audience=` 是同一种模式。实例拿到后可以直接用
  `auth/jwt-spire/login` 登录 Bao，符合 ADR-0010"凭据交付止于 Bao"。
- **X.509-SVID 和证书轮换**等出现需求再做：VM 可用 virtio-vsock，LXC 可绑定挂载一个只服务于该
  容器的 socket。
- **信任边界是宿主机。** 宿主机本来就拥有实例的全部权限，代领身份不增加新的风险。不在每个实例里
  运行 spire-agent，也不使用 vTPM 做节点证明。
- 注册条目由 compute controller 在实例创建时通过 SPIRE Server API 写入，删除时清理：
  - 选择器 `ayatori:instance:<ns>/<name>`，自定义类型，由 Agent 按请求进入的实例端口填写；
  - parent 是覆盖所有候选节点的"计算节点" node alias。节点证明方式必须能给出共同的选择器（如
    `x509pop` 由同一 CA 签发节点证书）；join token 做不到，PoC 中只能每个节点各建一份条目。

### 控制台

- **协议**：
  - VNC 为默认，retro 的 cirrus/VGA 和 UEFI 安装界面都能使用，浏览器端用 noVNC；
  - 串口用于 headless 系统，浏览器端用 xterm.js；
  - SPICE 适合 Windows 桌面（剪贴板、USB 重定向）。浏览器客户端 spice-html5 已基本无人维护，
    因此通过代理下发 `.vv` 文件，由 `remote-viewer` 连接。
- **节点不对外暴露控制台端口。** QEMU 只监听本机 unix socket（`<listen type='socket'/>`），由 Agent
  中继。中继只接受控制台代理的 SPIFFE ID，并且只在本节点是实例 DRBD Primary 时服务，避免连到
  失去 Primary 后残留的 QEMU（PoC 05）。
- **中央控制台代理**，参照 OpenStack nova-novncproxy：
  - 用户通过 iam-login（OIDC）登录；
  - 代理向 Ayatori apiserver 发起 **SubjectAccessReview**，检查用户对 `libvirtdomains/console`
    等子资源的权限。RBAC 规则可以写实际并不提供服务的子资源名，与 kube-rbac-proxy 的做法相同，
    权限因此直接复用 RBAC；
  - 代理按 `status` 查到实例当前所在节点，用 SPIFFE mTLS 连接该节点 Agent 的中继。实例换节点后
    代理自动连到新节点，用户的 URL 不变。
- **不使用聚合 API。** KubeVirt 通过聚合 API 提供 console/vnc 子资源；AGENTS.md 要求只有在
  CRD 形成已验证的阻碍时才引入聚合 API，上述方案不需要。
- **部署**：代理是无状态服务，不运行在 API-only 的控制面 LXC 上（ADR-0013）。可以作为计算节点上
  的普通服务运行，通过 Ayatori `LoadBalancer` 的 VIP 和 Hostname 对外提供。

### 运行时操作

- **范围**：换光盘和软盘、开机、关机（ACPI）、强制断电、重置、发送 Ctrl+Alt+Del 等按键。
- **入口**：控制台页面的工具栏，换盘的下拉框列出 `iso` 等 pool 中的卷。
- **路径**：与控制台相同，代理检查 SubjectAccessReview（`libvirtdomains/media`、
  `libvirtdomains/power` 等子资源），再交给节点 Agent 执行。换盘使用 libvirt 原生的
  `change-media --live`，不加 `--config`，不改持久配置。
- **审计**：代理在实例对象上记录 Kubernetes Event，`kubectl describe` 可以看到谁在什么时候做了
  什么操作。
- **状态**：Agent 把当前介质和电源状态回写到 `status.media`、`status.powerState`。
- **电源意图**：关机和强制断电写入实例状态卷上的"off"标记，开机时清除。监管进程只对崩溃自动重启
  （ADR-0015），因此客户机内主动关机和控制台关机都会保持关闭。强制断电（`virsh destroy`、
  `lxc-stop -k`）在监管进程看来可能与崩溃相同，所以由 Agent **先写标记再执行**。
- **重启**由监管进程在内部完成，Agent 不得 `systemctl restart` promoter 管理的单元：那会连带
  demote 资源，实例被另一节点接管（PoC 04）。

## 待验证（Accepted 之前必须完成）

1. 用 nftables 在 VM bridge 和 LXC veth 上把 `169.254.169.254` 重定向到 Agent，并按入口端口
   识别实例；IMDSv2 式 token 与 TTL 1 生效。
   **已完成（2026-10-31）**：通过，见 [PoC 04](../compute/poc-04-metadata-media-lxc-supervisor.md)。
2. 通过 Delegated Identity API 为实例选择器签发 JWT-SVID，实例用它登录 Bao；确定注册条目的
   选择器和父 ID。
   **已完成（2026-10-31）**：通过，见 [PoC 05](../compute/poc-05-spiffe-identity-and-console.md)。
3. 控制台代理：OIDC 登录、SubjectAccessReview、noVNC 经 Agent 中继访问 VNC unix socket；SPICE
   `.vv` 经代理连接。
   **部分完成（2026-10-31）**：SubjectAccessReview、Event 审计、SPIFFE mTLS 中继和换节点后
   自动跟随通过，见 PoC 05；OIDC 登录、SPICE、串口未测。
4. `change-media --live` 在客户机重启后保留、在冷启动后回到配置。
   **已完成（2026-10-31）**：光驱通过，见 PoC 04；软驱未测。
5. 电源意图标记：有意关机的实例在原节点故障后不会被其他节点开机。
   **已完成（2026-10-31）**：VM 见 [PoC 03](../compute/poc-03-state-volume-and-bridge-netfilter.md)，
   LXC 见 PoC 04。

## 结果

- 实例获得启动配置、元数据和身份，配置中不再需要任何密钥。
- 控制台和运行时操作复用 RBAC 和审计，不改变 GitOps 对 spec 的所有权。
- 新增两个组件：中央控制台代理（无状态），以及 Agent 中的元数据服务、SPIFFE 代领和控制台中继。
- 元数据服务和控制台中继都在节点上运行，不依赖控制面。只有控制台代理的 SubjectAccessReview
  依赖 apiserver：控制面不可用时，控制台需要走节点本地的 break-glass 路径（`virsh console`、
  本地 VNC）。
