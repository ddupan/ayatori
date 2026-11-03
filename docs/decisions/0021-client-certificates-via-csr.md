# ADR-0021：控制面客户端凭据以 CSR 签发与自行续签

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-11-03
- 依据：维护者讨论；修订 [ADR-0013](0013-api-only-control-plane-on-lxc.md) 中客户端证书的签发方式。
  编号 0019、0020 已被 dev/compute 上的计算后端 ADR 占用。

## 问题

- 控制面上的 ayatori-manager、Flux 的两个控制器、ESO 与 kube-state-metrics 用 `k0s kubeconfig create`
  签发的证书访问 apiserver，证书嵌在 kubeconfig 中，一年到期，只能人工删除后重跑 playbook 轮换。
- 计算节点的 Compute Agent（ADR-0015）、将来托管集群上的 CAPI shim（ADR-0014）同样需要访问 Ayatori
  apiserver，而且运行在控制面以外的机器上，私钥不应经过部署机或 Bao 传递。
- 这些客户端的身份是静态的：一个组件、一个节点就是一个固定身份，不需要短期 token。

## 评估

- **以 SPIFFE JWT-SVID 登录（structured authentication）**：apiserver 要能访问 SPIRE 的 OIDC discovery，
  SPIRE 运行在 Kubernetes 集群中，而控制面将来可能运行在计算后端管理的 LXC 上，形成循环依赖；静态身份
  也用不上短期 token 的好处。SPIFFE 留给实例身份（ADR-0016）。
- **部署机签发后经 Bao 交付**：私钥离开了使用它的机器；Bao 成为控制面凭据的前置依赖。
- **继续 `k0s kubeconfig create`，到期人工重签**：每年一次的人工操作，过期即控制面中断。

## 决策

照搬 kubelet 的 TLS bootstrap（kubeadm join）：

| 环节 | 做法 |
|---|---|
| 签发 | kube-controller-manager 的 `csrsigning` 以 k0s 的 CA 签发 `kubernetes.io/kube-apiserver-client`。同时打开 `csrcleaner`、`tokencleaner` 清理过期的 CSR 与 bootstrap token。k0s 已为 kube-controller-manager 配好 CA 并启用 bootstrap token 认证 |
| 私钥 | 只在使用证书的机器上生成 |
| 首次签发 | 部署脚本创建 15 分钟有效的 bootstrap token（额外组 `system:bootstrappers:ayatori`），写入身份目录的 `bootstrap-kubeconfig`；持有者以它提交 CSR；部署脚本核对 CSR 申请的正是这个身份后，以管理员身份批准（`client-credentials approve-bootstrap`），不符合的拒绝 |
| 续签 | 持有者在有效期过去 70–90% 时以现有证书提交 CSR（client-go 的 certificate manager）；ayatori-manager 的批准者只批准"为自己续签"：申请者带 `system:ayatori:client-certificates` 组，证书的 CN 与 O 恰好是申请者自己的用户名与组，只用于客户端认证、没有 SAN |
| 过期后 | 重跑部署脚本，按首次签发处理 |

- **bootstrap token 本身换不到任何证书**：批准者不处理 bootstrap 申请，首次签发总要经过部署脚本的核对。
  token 泄露只会留下待批准的 CSR。kubeadm 的自动批准没有这层核对。
- **续签不会扩大权限**：签出的证书与申请者现有的身份完全相同。所有以本方式签发的证书都带
  `system:ayatori:client-certificates` 组，批准者只对带此组的申请者续签，避免 ServiceAccount 或将来的
  OIDC 用户借续签换得一年期证书。
- **证书文件**：身份目录中的 `client.pem` 同时包含证书与私钥，整体原子替换；`kubeconfig` 按相对路径
  引用它。client-go 每 5 分钟重新加载按路径引用的证书文件，第三方组件不必重启。分成两个文件时，可能
  在两次替换之间读到不配对的证书与私钥。
- **控制面组件由 `client-credentials` 容器代管**：Flux、ESO、kube-state-metrics 不会自己提交 CSR，
  该容器以 root 运行，为每个身份生成私钥、申请与续签，文件属于该身份的运行用户；`bootstrap-kubeconfig`
  只属于 root，组件读不到 token。它与 ayatori-manager 同一镜像。
- **集群外的客户端自行管理**：Compute Agent 等在进程内使用同一个包（`internal/infra/clientcert`）。
- **批准者自己的续签**：ayatori-manager 的证书同样由它自己的批准者续签。它停摆超过证书剩余有效期
  （正常为数月）时，所有证书都会过期；重跑部署脚本即可恢复，部署脚本以 k0s 的管理员凭据批准。
- **不使用 `system:node:`/`system:nodes`**：那是 kubelet 与 Node authorizer 专用的名字。

## 待验证（Accepted 之前必须完成）

1. envtest 中：bootstrap token 只能提交 CSR；部署方拒绝申请其他身份的 CSR、批准正确的 CSR；持有者
   自行续签；续签前建立的客户端在旧证书过期后仍可访问。
   **已完成（2026-11-03）**：`internal/infra/clientcert` 的集成测试，测试中以测试 CA 代替 csrsigning。
2. Dev 控制面上：k0s 的 csrsigning 以其 CA 签发；从 `k0s kubeconfig create` 迁移到本方式时各组件
   不中断；Flux、ESO、kube-state-metrics 在续签后继续工作。

## 结果

- 控制面不再有需要人工轮换的客户端证书，证书到期告警只在续签失败时出现。
- 集群外的新客户端（Compute Agent、CAPI shim）按同一流程加入，只需在部署脚本中增加首次签发。
- apiserver 与 SPIRE、Bao 之间没有新增依赖。
- `k0s kubeconfig create` 只作为恢复手段。
