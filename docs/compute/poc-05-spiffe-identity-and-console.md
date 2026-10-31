# PoC 05：SPIFFE 代领身份与控制台代理

- 日期：2026-10-31
- 对应：[ADR-0016](../decisions/0016-instance-node-services.md) 待验证第 2、3 项
- 环境：沿用 [PoC 01](poc-01-drbd-reactor-failover.md) 的三台嵌套 VM。SPIRE 和 Bao 都是在 PoC 节点上
  新起的，trust domain `poc.ayatori`，不接触生产的 SPIRE 与 Bao。RBAC 判定使用 Ayatori Dev
  apiserver，测试对象放在 namespace `ayatori-poc-console`。
- 结论：**两项的核心链路都通过**。OIDC 登录、SPICE `.vv` 和串口没有测。测试中还发现并修复了 VM
  监管单元的一个严重缺陷：evict 时可能留下仍在运行的 QEMU（见文末）。

## SPIFFE：Agent 通过 Delegated Identity API 代领 JWT-SVID

### 布置

- SPIRE 1.15.3：server 在 poc-1，agent 在 poc-1、poc-2，节点证明用 join token。
- agent 开启 Delegated Identity API：`admin_socket_path`（不能和 Workload API socket 在同一目录或其
  子目录），`authorized_delegates = ["spiffe://poc.ayatori/ayatori-agent"]`。
- 注册条目（每个节点各一份）：
  - `spiffe://poc.ayatori/ayatori-agent`，选择器 `unix:uid:0`，即 Agent 自己的身份；
  - `spiffe://poc.ayatori/ayatori/instance/default/<实例>`，选择器 `ayatori:instance:default/<实例>`。
    选择器类型是自定义的，SPIRE 不校验，由 delegate 在请求时提供。
- `delegate-jwt`：约 50 行 Go，调用 `FetchJWTSVIDs`。PoC 04 的元数据服务替身增加
  `/latest/meta-data/identity?audience=…`，**选择器由请求进入的实例端口决定**，客户机无法指定。
- Bao（dev 模式，poc-1）：`auth/jwt-spire` 用 SPIRE bundle 中的 JWT 公钥校验；role `vm1` 绑定
  `bound_subject=spiffe://poc.ayatori/ayatori/instance/default/vm1`、`bound_audiences=bao`，策略只
  允许读 `kv/instances/vm1`。

### 结果

| 测试 | 结果 |
|---|---|
| Agent（root）代领 vm1 的 JWT-SVID | `sub=spiffe://poc.ayatori/ayatori/instance/default/vm1`，`aud=["bao"]` |
| 非 delegate 进程（打开 admin socket 权限后以 ubuntu 用户调用） | `PermissionDenied: no identity issued` |
| 未注册的选择器 | `PermissionDenied: no identity issued` |
| vm1 内：元数据 token → JWT-SVID → Bao 登录 → 读 secret | 成功，读到 `hello=from-vm1` |
| ct1 内：用自己的 JWT-SVID 登录 role `vm1` | `invalid subject (sub) claim` |
| vm1 evict 到 poc-2 后重复整条链路 | 成功，身份由 poc-2 的 SPIRE agent 签发 |

### 发现

- **实例条目必须覆盖所有候选节点。** 条目只下发到其 parent 对应的 agent。PoC 为每个节点各建一份
  条目；join token 的节点没有共同的选择器，无法用一个 node alias 覆盖多个节点。正式环境应使用能给
  出共同选择器的节点证明方式（如 `x509pop` 由同一 CA 签发节点证书），建一个"计算节点" alias 作为
  实例条目的 parent，每个实例只需一份条目。
- Delegated Identity API 对调用者做 workload attestation，admin socket 的文件权限只是第一道防线。
- Bao 使用静态公钥只是 PoC 的简化，SPIRE 轮换 JWT 密钥后会失效。正式环境沿用现有 `jwt-spire`
  的 JWKS 配置方式。

## 控制台代理

### 布置

一个约 300 行的 Go 程序，分两种角色：

- **节点中继**（poc-1、poc-2，root，SVID `ayatori-agent`）：
  - SPIFFE mTLS 监听，只接受 `spiffe://poc.ayatori/console-proxy`；
  - 本节点必须是该实例 DRBD 资源的 Primary；
  - 用 `virsh domdisplay` 找到 QEMU 的 VNC unix socket（domain 中写
    `<graphics type='vnc'><listen type='socket'/></graphics>`，不监听任何 TCP 端口）并转发。
- **中央代理**（poc-1，专用用户，SVID `console-proxy`，Dev 上的 SA 绑定 `system:auth-delegator`）：
  - 提供 noVNC 静态页面和 websocket 端点 `/ws/<namespace>/<name>`；
  - 用 TokenReview 认证调用者，再用 SubjectAccessReview 检查
    `compute.ayatori.ddupan.top` 的 `libvirtdomains/console`（verb `get`）；
  - 在实例所在 namespace 记录 Event `ConsoleOpened`；
  - 经 SPIFFE mTLS 连到节点中继。PoC 依次尝试各节点，正式实现按 status 中的所在节点直连。
- Dev 中只有 RBAC 对象，没有 `LibvirtDomain` CRD：RBAC 判定不要求资源存在，虚拟子资源可以直接写在
  Role 里。

### 结果

| 测试 | 结果 |
|---|---|
| alice（Role 授权 `libvirtdomains/console`，`resourceNames: [vm1]`）打开 vm1 | RFB 握手完成，桌面名 `QEMU (vm1)` |
| bob（无授权）打开 vm1 | 403 |
| 无效 token | 401 |
| alice 打开 uefi（未授权该名字） | 403 |
| 审计 | Dev 中出现 `ConsoleOpened` Event：`…:alice opened console`，来源 `ayatori-console-proxy` |
| 无客户端证书直连节点中继 | TLS 握手失败：`certificate required` |
| 用合法的 `ayatori-agent` SVID 直连节点中继 | TLS 握手失败：`bad certificate`（身份不是 `console-proxy`） |
| vm1 换节点后用同一个 URL | 先经 poc-2、换节点后经 poc-1 的中继连上，用户无感知 |
| 浏览器 | noVNC 页面可访问（`/novnc/vnc.html?path=ws/<ns>/<name>%3Ftoken%3D…`） |

PoC 通过 query 参数传 token，只是为了能用 ServiceAccount 代替用户测试 RBAC。正式实现是 OIDC
登录后的会话 cookie，token 不出现在 URL 中。

## 发现：VM 监管单元在 evict 时可能留下 QEMU

控制台换节点测试中，代理在 vm1 被 evict 后仍连到了原节点。检查发现原节点的 QEMU **仍在运行**，
两个节点上的 vm1 同时存在：

- `poc-vm@` 被 systemd 停止时，TERM trap 中的 `virsh destroy` 没有生效，单元在 3 毫秒内结束。
- DRBD 发现设备仍被 QEMU 打开，以 `force-io-failures` 强制 demote，之后旧 QEMU 的所有 IO 都失败。
  **数据没有被双写**，这是 DRBD 的最后一道防线；但客户机看到了 IO 错误，而且旧节点上留下了一个
  僵尸实例，持续占用资源。
- 只要旧 QEMU 还开着设备，旧节点就无法再次 promote 这个资源。

根因：单元使用默认的 `KillMode=control-group`。systemd 停止单元时向 cgroup 中的所有进程发送
SIGTERM，并且为了处理 fork 竞争会**反复扫描 cgroup**，于是 trap 刚启动的 `virsh destroy` 也可能
收到 SIGTERM 而被杀死。日志中的证据：6 次 evict 里有 5 次，trap 记录时间戳用的 `date` 子进程也被
杀掉，输出为空；是否连 `virsh destroy` 一起被杀，取决于时机。LXC 单元从一开始就使用
`KillMode=mixed`，因此没有这个问题。

修复：VM 单元改为 `KillMode=mixed`（SIGTERM 只发给主进程，其余进程在主进程退出后才被清理）。
修复后连续 6 次 evict，trap 的输出完整，没有残留的 QEMU。

对设计的影响：

- 监管单元必须使用 `KillMode=mixed`，或者由监管进程在收到停止信号前就不依赖新 fork 的进程。正式
  实现中监管进程通过 libvirt API 调用 destroy，不 fork `virsh`，但 `KillMode=mixed` 仍然要写。
- 节点中继（以及 Agent 回写 status）必须以 DRBD Primary 判定实例所在位置；PoC 01 已有同样的结论。
  中继加入 Primary 检查后，即使留下僵尸 QEMU，控制台也不会连过去。
- PoC 03 中 evict 路径没有在 trap 修改后复测，只测了硬断电。这次的问题因此没有更早暴露。

## 未覆盖

- 经 iam-login 的 OIDC 登录与会话；
- SPICE `.vv` 经代理连接（remote-viewer 的 `proxy=` 走 HTTP CONNECT，代理需要支持 CONNECT 和一次性
  凭据）；
- 串口控制台（xterm.js），机制与 VNC 中继相同；
- 运行时操作（换盘、电源）经代理下发，以及对应子资源的 SubjectAccessReview；
- 每个节点的 Agent 使用各自的 SPIFFE ID，代理据此确认连到的是 status 中所写的节点。
