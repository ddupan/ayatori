# ADR-0028：workload 集群的 cloud-provider 按 loadBalancerClass 接入 LoadBalancer

- 状态：Proposed（Dev 端到端验证后改为 Accepted，见「待验证」）
- 日期：2026-11-25
- 依据：维护者讨论（"给平台做一个 k8s 用 cloud provider，现在马上可以实现的是 LB 接口，compute 上线之后
  可以补全其他接口；需要考虑在外部集群访问控制集群"；选定按 `loadBalancerClass` 逐个 opt-in）。
- 细化：[ADR-0012](0012-l4-loadbalancer.md)、[ADR-0014](0014-kaas-cross-backend-node-supply.md) 中由消费方部署的
  CCM 类组件。修订 ADR-0014「子集群内的 Service LoadBalancer」：不使用 `k8s.io/cloud-provider` 的 service
  controller，原因见下文。
- 关联：[ADR-0024](0024-unified-version-with-skew.md)（版本）、[ADR-0027](0027-ayatori-cli.md)（SPIFFE 认证）。

## 问题

homelab 的 k3s 集群（homelab、sandbox）靠 klipper/servicelb 共享节点 IP 暴露 Service，ADR-0012 的
`LoadBalancer` 已经上线，但 workload 集群里还没有组件把 `Service type=LoadBalancer` 翻译过去。现在每个需要
VIP 的入口都要在 homelab-infra 里手写 `LoadBalancer` + `Backend`，节点增减时还要手改 endpoint。

## 决策

### 按 `loadBalancerClass` opt-in

- 只处理 `spec.loadBalancerClass: ayatori.ddupan.top/loadbalancer` 的 Service。未设置 class 的 Service 继续
  由集群原有实现（k3s servicelb）处理，可以一个 Service 一个 Service 地迁移，不需要整集群切换。
- 上游 `k8s.io/cloud-provider` 的 service controller 只处理 class 为空的 Service（`wantsLoadBalancer`），
  所以这里是一个 controller-runtime 的 Service controller，与 AWS Load Balancer Controller、MetalLB 处理
  自有 class 的方式相同。它不是 CCM，kubelet 不需要 `--cloud-provider=external`。
- compute 上线后需要 Node 初始化（providerID、地址、zone、删除）时，再按 `k8s.io/cloud-provider` 实现
  `InstancesV2`，与本组件放在同一个二进制，届时另行决策。

### 映射

| workload 集群 | Ayatori（代表该集群的 namespace 内） |
|---|---|
| Service | `LoadBalancer`，名字 `<svc-ns>-<svc-name>`，超过 63 字符截断加 hash（ADR-0014 确定性推导） |
| Service 的每个端口 | `Backend` `<lb>-<port>`：各端口 nodePort 不同，而 LB 的一个端口只引用一个 Backend |
| 节点 InternalIP × nodePort | Backend endpoints；带 `node.kubernetes.io/exclude-from-external-load-balancers` 的节点除外 |
| `externalTrafficPolicy: Local` | HTTP 健康检查 `healthCheckNodePort` 的 `/healthz`（kube-proxy 只在有本地 endpoint 时返回 200） |
| `spec.loadBalancerIP` | `spec.address`，只在创建时生效（address 不可变）。池外或已被占用的地址由 LoadBalancer controller 拒绝（`AddressOutOfPool`、`AddressConflict`），唯一性由 IPAddress 对象名保证 |
| `status.loadBalancer.ingress` | LB 首次 `Programmed` 后写入 `status.address` 与 `status.hostname`，之后不撤回 |
| `status.conditions` | `ayatori.ddupan.top/LoadBalancerProgrammed`，照抄 LB 的 `Programmed`（含拒绝原因） |

- 首版只支持 TCP，并要求有 nodePort；不支持的 Service 记日志后不处理。
- 派生对象带 `app.kubernetes.io/managed-by: ayatori-cloud-provider` 与来源 Service 的 namespace/name
  label。同名对象没有这些 label 时（声明的对象，或截断后撞名）拒绝接管并报错。
- 以 server-side apply 写入，只拥有自己设置的字段；端口减少时先更新 LB 再删多余的 Backend。
- 节点上 Ready 的变化不改写 Backend：与上游一致，能否接流量由健康检查决定。

### 删除

Service 带 finalizer `ayatori.ddupan.top/loadbalancer-cleanup`。Service 删除或改离本 class 时，先删
LoadBalancer 并**等它消失**（LB 自己的 finalizer 会删 DNS 记录、释放 VIP），再删 Backend、撤回 ingress、
移除 finalizer。整个集群下线时删除 Ayatori 中代表它的 namespace。Ayatori 控制面的 KCM 不运行 garbage
collector（ADR-0013），因此不依赖 ownerReference 级联。

### 从外部集群访问控制集群

| 环节 | 做法 |
|---|---|
| 地址与 CA | 部署方以参数给出 API 地址，以 ConfigMap 给出 `kube-public/ayatori-login` 公布的 CA；启动时以该 CA 读取发现信息（issuer、clientID），不在运行时 TOFU |
| 认证 | 与 CLI 共用 `internal/ayatoriclient` 的机器身份链（ADR-0027）：Pod 经 SPIFFE CSI driver 取 JWT-SVID，audience 为发现信息中的 clientID（`ayatori-<env>`），按 `exp` 缓存、过期前换新；compute 上线后，Ayatori 实例上的集群改由元数据服务代领。apiserver 已信任 SPIRE 的 OIDC discovery，用户名即 SPIFFE ID。两者都没有的集群以 `--ayatori-kubeconfig` 给出任意 kubeconfig（如客户端证书），凭据交付与轮换由部署方负责 |
| 授权 | Ayatori 中每个 workload 集群一个 namespace（如 `homelab`、`sandbox`）。ClusterRole `ayatori:cloud-provider` 随 release 发布（`deploy/flux/loadbalancer`），只含 `loadbalancers`、`backends` 的 get/list/watch/create/patch/delete；部署方在该 namespace 中用 RoleBinding 绑定到该集群 cloud-provider 的 SPIFFE ID |
| 隔离 | 一个集群的身份只能写自己的 namespace；LB 的 Hostname 是 `<svc-ns>-<svc-name>.<集群 namespace>.<zone>` |
| 不可用 | Ayatori 或 SPIRE 不可用时只影响 Service 的变更，已生效的数据面不受影响。运行中 controller 退避重试；启动时读不到发现信息或取不到 token 就退出，由 kubelet 退避重启 |

不选：

- cloud-provider 自己以 CSR 申请并续签客户端证书。ADR-0021 这样做是为了让控制面不依赖 SPIRE、避免循环，
  cloud-provider 不在控制面的依赖链上；出现既无 SPIRE 也不在 Ayatori 实例上的集群时再评估。
- 把各 workload 集群的 ServiceAccount issuer 加进 Ayatori 的 AuthenticationConfiguration：每加一个集群改一次
  控制面，且依赖各集群的 discovery 可达。两个集群都已运行 SPIRE agent 与 ClusterSPIFFEID，不需要新机制。

示例（homelab 集群，SPIFFE ID 以 homelab-infra 的 ClusterSPIFFEID 为准，sandbox 带 `/sandbox/` 前缀）：

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: cloud-provider, namespace: homelab}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: "ayatori:cloud-provider"}
subjects:
  - {apiGroup: rbac.authorization.k8s.io, kind: User, name: "spiffe://ddupan.top/ns/ayatori-cloud-provider/sa/ayatori-cloud-provider"}
```

### 部署与版本

二进制 `cloud-provider` 在 manager 镜像中，与控制面同一 release；按 ADR-0024 不新于 manager。基础清单在
`config/cloud-provider/`，环境参数、镜像 digest、namespace、RoleBinding 与 ClusterSPIFFEID 由 homelab-infra 声明。

## 已知限制

- 派生 LB 只保证名字稳定，需要稳定 IP 的入口仍用声明的 LB（ADR-0014）。
- 不支持 UDP：数据面 HAProxy 不转发 UDP（只在 log-forward 与 QUIC 中监听 UDP），这是 ADR-0012 首版只做
  TCP 的原因，不是 cloud-provider 的限制。支持 UDP 要给 `LoadBalancer` 端口加协议字段，并为 UDP 端口换一个
  包转发的数据面（如 LB 主机上的 IPVS/nftables），需另行决策。
- 不支持 `allocateLoadBalancerNodePorts: false`、PROXY protocol 注解；出现真实需求再加。

## 待验证（Accepted 之前）

已由 envtest 验证：创建、VIP 稍后分配、地址被拒绝时原因同步到 Service、节点增加、端口减少、controller 重启后认领、同名对象不接管、删除
等待 LB finalizer、无 class 的 Service 不处理。

在 Dev（sandbox 集群 → ayatori-dev）验证：

1. k3s servicelb 不处理带 class 的 Service（两边不会同时写 ingress）。
2. Pod 能经网络访问 `ayatori-dev` 的 apiserver，JWT-SVID 认证与 namespace RBAC 生效，越权写其他 namespace 被拒。
3. Service 拿到 VIP 与 Hostname，从集群外经 VIP 访问；`Local` 时只有有 endpoint 的节点 UP。
4. 删除 Service 后 VIP 与 DNS 记录被释放。
