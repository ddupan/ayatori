# PoC 15：串口与 SPICE 控制台、软驱换盘

- 日期：2026-10-31
- 对应：[ADR-0016](../decisions/0016-instance-node-services.md) 待验证第 3 项（串口、SPICE）与第 4 项（软驱）
- 环境：沿用 [PoC 05](poc-05-spiffe-identity-and-console.md) 的 SPIRE、Dev 上的 RBAC 与控制台程序。PoC 节点重启过
  几次，SPIRE、中继与代理已改为持久的 systemd 单元，SPIRE agent 用新的 join token 重新做了节点证明。
- 结论：**通过**。串口与 SPICE 复用 VNC 的中继、mTLS 与 SubjectAccessReview；SPICE 的一次性 ticket 不能重放；
  软驱的 `change-media --live` 与光驱语义相同。

## 做法

- **节点中继**：首行改为 `<实例> <类型>`。
  - `vnc`、`spice`：`virsh domdisplay --type <类型>` 给出的 unix socket。domain 中两者都是
    `<listen type='socket'/>`，可以同时存在。
  - `serial`：libvirt 默认的 `<serial type='pty'>`，路径由 `virsh ttyconsole` 给出，中继直接读写该 pty。
- **中央代理**：
  - 串口走同一个 websocket 端点（`?kind=serial`），浏览器端对应 xterm.js。
  - SPICE：
    - `/vv/<ns>/<name>` 经 SubjectAccessReview 后签发 60 秒有效的一次性 ticket，返回 `.vv` 文件：
      `host=<ticket>`、`port=5900`、`proxy=http://<代理>`；
    - remote-viewer 经 `proxy=` 向代理发 `CONNECT <ticket>:5900`，代理按 ticket 找到实例，经中继连到节点上的
      SPICE socket；
    - ticket 放在主机名里，不依赖客户端是否支持代理认证。

## 结果

| 测试 | 结果 |
|---|---|
| VNC（回归） | `QEMU (vm1)`，正常 |
| 串口（alice） | 收到 `Kernel 6.12.81-0-virt on x86_64 (/dev/ttyS0)` 与 `vm1 login:` |
| 串口（bob，无授权） | 403 |
| SPICE：取 `.vv` 后 CONNECT，完成 SPICE link 握手 | 代理回 200；服务端回 `REDQ`，协议 2.2，link error 0 |
| SPICE：同一 ticket 再 CONNECT | 403（一次性） |
| SPICE `.vv`（bob） | 403 |
| 审计 | Dev 中分别出现 `opened vnc console`、`opened serial console`、`opened SPICE console` |

SPICE 只用自写的探针完成了 link 握手，没有用真正的 remote-viewer 打开画面。

## 软驱换盘

vm1（i440fx）增加只读软驱 `fda`，配置的盘为 iso pool 中的 `NT4SCSI.vfd`。客户机读第一个扇区并计算 md5：

| 时刻 | 客户机读到 | 持久配置 |
|---|---|---|
| 启动后 | 与 `NT4SCSI.vfd` 一致 | `NT4SCSI.vfd` |
| `change-media fda Windows_98_Second_Edition_Boot.img --live` 后 | 第 1 次读为空，之后与 Win98 启动盘一致 | 不变 |
| 客户机内重启（QEMU 进程不变） | Win98 启动盘 | 不变 |
| `kill -9` 后冷启动 | 回到 `NT4SCSI.vfd` | 不变 |

换盘后第一次读取为空，是软驱驱动通过换盘信号检测介质变化的正常行为（DOS/Windows 同样如此）。

## 未覆盖

- 浏览器端的 xterm.js 页面与真正的 remote-viewer；
- 串口同时只能有一个读者：中继直接打开 pty，与 `virsh console` 会互相抢输入输出。正式实现应改用 libvirt 的
  console stream（`virDomainOpenConsole`），并在已有会话时拒绝或接管；
- 经 iam-login 的 OIDC 登录。
