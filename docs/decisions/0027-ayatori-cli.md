# ADR-0027：Ayatori CLI 是面向人的主入口

- 状态：Proposed
- 日期：2026-11-13
- 依据：维护者讨论（控制台中继落地后讨论中央控制台代理与 OIDC 登录时提出："这个 cli 该对标 openshift 的
  oc""永远记住 ayatori 不是 kubernetes""在 cli 设计上还能参考各大云"）。
- 修订：[ADR-0010](0010-credential-delivery-ends-at-openbao.md)——人按需读取凭据改为经 Ayatori API
  返回，见「凭据经 API 返回」。交付给 workload 仍止于 OpenBao。
- 关联：[ADR-0001](0001-kubernetes-api-machinery.md)（只复用 API machinery）、[ADR-0016](0016-instance-node-services.md)
  （控制台与运行时操作）、[ADR-0019](0019-compute-day2-operations.md)、
  [ADR-0024](0024-unified-version-with-skew.md)。

## 问题

Ayatori 复用 kube-apiserver 的 API machinery（ADR-0001），但它不是 Kubernetes。现在人要用它，只能拿
kubeconfig、kubectl 和 kubelogin，看到的是 CRD、namespace、field manager、`kubectl describe` 里的
Event——这是在把 Kubernetes 当成 Ayatori 的产品界面。

更具体的后果是：不少需求因为没有一个"第一方客户端"而一直没有好去处，只能推给 Ansible、手工步骤或
未来的 Web 前端：

| 需求 | 现在 |
|---|---|
| 拿到数据库凭据（ADR-0010 只交付到 OpenBao） | 自己找 status 里的 Bao 路径，再另外登录 Bao |
| 串口、VNC、SPICE、换盘、电源（ADR-0016） | 只设计了 Web 前端，尚未实现 |
| `ManualTask`、证明不了安全时 Blocked 的实例（ADR-0006/0007） | 手工改对象 |
| 节点维护、疏散、迁移（ADR-0019） | `kubectl cordon`，手写 `InstanceMigration` |
| libvirt XML 与结构化 spec 互转（ADR-0026） | 手工对照 |
| KaaS 集群的 kubeconfig（ADR-0014） | 手工交付 |
| 节点 join（ADR-0021/0022） | homelab-infra 的 Ansible playbook |

## 原则

1. **CLI 是人使用 Ayatori 的主入口。** kubectl 只是运维排障的 break-glass，文档与日常流程不以它为前提。
   Web 控制台（另行决策）与 CLI 是同一套 API 的两个客户端，不是两套能力。
2. **用 Ayatori 的概念说话。** 命令、输出、错误提示面向 Database、LoadBalancer、实例、节点这些资源；
   kubeconfig、namespace 之外的 Kubernetes 概念（field manager、owner reference、Pod 等）不出现在常规
   输出里。
3. **只用公开 API，不开后门。** CLI 只调用 apiserver 的 API（含聚合子资源），以用户自己的身份经 RBAC
   授权；其他后端（OpenBao、节点上的中继）对 CLI 透明。CLI 能做的，任何持有同样权限的客户端都能做；
   CLI 不持有任何服务端没有授予用户的能力。
4. **不与 GitOps 抢所有权。** spec 由 git 管理的对象（Flux 应用的），CLI 不修改，提示去改 git；
   运行时操作（ADR-0016 的"配置与运行时分离"）不改 spec，不受此限制。
5. **不是任意 shell，也不是通用 Kubernetes 客户端。** 不提供 `exec`/任意对象的通用 CRUD；每个命令都是
   有类型、可审计的操作（与 AGENTS.md 对节点 Agent 的要求一致）。

## 参照

- **OpenShift `oc`**：`oc login <server>` 自带 OAuth 登录并维护自己的上下文；`project` 切换；`oc adm`
  收拢运维命令；领域命令（`new-app`、`rsh`、`debug`）之外才是 kubectl 兼容的通用命令。
- **各大云的 CLI**（gcloud、aws、az）：
  - 登录：`gcloud auth login`、`az login --use-device-code`——浏览器与 device code 两种方式；
  - 配置档：`gcloud config configurations`、aws `--profile`——多环境切换；
  - 命令树：资源名词在前、动词在后（`gcloud compute instances list`、`az vm start`）；
  - 输出：统一的 `--format`/`--output` 与查询（`--filter`、`--query`）；
  - 长操作：默认等待并显示进度，`--async` 立即返回，另有 waiter（`aws ec2 wait instance-running`）；
  - 运行时接入：`gcloud compute connect-to-serial-port`、`az serial-console connect`；
  - 凭据获取：`gcloud container clusters get-credentials`、`az aks get-credentials`。

Ayatori 取 oc 的"自带登录与上下文、`adm` 分区"，取云 CLI 的"名词—动词命令树、统一输出、长操作与
运行时接入"。

## 决策

### 登录与配置档

- **环境由 CLI 自己的配置文件管理**（`~/.ayatori/config.yaml`），不用 kubeconfig：
  `ayatori config add <名字> <API 地址>`、`list`、`use`、`remove`，`--profile` 单次指定。
- **`config add` 从服务端发现登录信息**：未登录时读 `kube-public/ayatori-login`（issuer、clientID、集群 CA），
  apiserver 只对这一个路径允许匿名请求（AuthenticationConfiguration 的 `anonymous.conditions` 加 RBAC，
  由 homelab-cookbooks `homelab_ayatori` 发布）。第一次读不校验证书，只用来取得 CA；CLI 显示 CA 的指纹
  （`sha256:` + SubjectPublicKeyInfo 摘要，与 kubeadm 相同），用户确认或以 `--ca-fingerprint` 比对后固定该
  CA，再以它校验证书重新读取。
- **`ayatori login`** 登录当前环境：经 iam-login OIDC，默认浏览器（public client、PKCE、本机回环端口），无浏览器
  时 `--device`。客户端按环境区分（`ayatori-dev`、`ayatori-prod`），apiserver 只认本环境的 aud。
- ID token 与 refresh token 放在 `~/.ayatori/tokens/`，仅本人可读；ID token 快过期时 CLI 自动刷新。
  iam-login 对公共客户端轮换 refresh token，刷新在文件锁内进行，同时运行的多个 CLI 只有一个去换。
- `ayatori logout`、`ayatori whoami`（显示 apiserver 认定的用户与组）。
- 不导出 kubeconfig 作为常规用法；需要 break-glass 时 `ayatori config kubeconfig` 生成一份以
  `ayatori` 自身为 exec credential plugin 的 kubeconfig。

### 命令树

名词在前、动词在后，`adm` 收拢运维命令：

```
ayatori login | logout | whoami | config ...
ayatori db       list | describe | credentials ...
ayatori lb       list | describe ...
ayatori vm       list | describe | console | media | start | stop | reset | migrate ...
ayatori ct       list | describe | console | start | stop ...
ayatori task     list | describe | complete ...        # ManualTask
ayatori adm node list | drain | uncordon | evacuate | join ...
```

具体命令随各切片的实现确定，本 ADR 只定结构。名词是 Ayatori 的资源，不是 CRD 的名字：`vm` 对应
LibvirtDomain、`ct` 对应 LXCContainer，后端换成别的实现时命令不变。

### 输出与长操作

- 默认人读的表格；`-o json|yaml` 输出 API 对象原样；`-o jsonpath=...`/`--query` 用于脚本（具体语法实现
  时选定，只选一种）。
- 有明确完成条件的操作（开机、迁移、疏散）默认等到完成并显示进度，`--async` 立即返回；等待用 watch，
  不轮询。

### 非 CRUD 操作是聚合子资源

有些面向人的操作不是对象的 CRUD：控制台、换盘、电源（ADR-0016 的运行时操作），读取凭据。从产品看
API 应当是一个整体，这些操作不另开一套端点，而是参照 KubeVirt（`virt-api` 以聚合 API
`subresources.kubevirt.io` 提供 `virtualmachineinstances/console`、`virtualmachines/start` 等）做成
apiserver 上的子资源。

- **`ayatori-console-api`** 是一个聚合 API server，只提供子资源，不存储任何对象：主资源仍是 CRD，存储、
  watch、版本迁移都不变。这正是 AGENTS.md 所说的例外——CRD 只支持 `status`/`scale` 子资源，这是已验证的
  扩展能力阻碍；它不为减少 controller 而引入，也不接管存储。
- **认证、授权、审计都在 apiserver：** apiserver 认证用户（OIDC），以 front-proxy 头把身份转给
  `ayatori-console-api`；后者用 `k8s.io/apiserver` 的委托授权按 RBAC 判断（例如
  `libvirtdomains/console`、`postgresqldatabases/credentials`），请求进 apiserver 审计日志，必要时
  在对象上记 Event。然后以**自己的身份**调用后端：节点中继（SPIFFE mTLS）、OpenBao。
- **不是第二个控制面：** 不存状态、不拥有资源、不做调和；能用对象 CRUD 表达的不做成子资源。
- 控制台类子资源是 Upgrade 流，经 apiserver 的聚合层透传（KubeVirt 的 console/VNC 同样如此）。
- API group、版本与路径实现时确定，不在本 ADR 固定；只要求挂在 apiserver 上、以子资源授权。
- ADR-0016 的"中央控制台代理"即控制台子资源，不再是单独组件；中继本身不变。
- 部署前在 Dev 验证：控制面没有 kube-proxy，APIService 需要无 selector 的 Service 加手写
  EndpointSlice 与 apiserver 的 `--enable-aggregator-routing`；k0s 的 front-proxy（requestheader）CA
  是否默认就绪。

### 凭据经 API 返回

`ayatori db credentials <name>` 读 `postgresqldatabases/credentials` 子资源（形如
`serviceaccounts/token`），OpenBao 对 CLI 透明：CLI 不知道 Bao 的地址、路径，也不需要另外登录 Bao。

- 授权在 Ayatori：RBAC 对该子资源的授权通过后，`ayatori-console-api` 以自己的身份读 Bao。权限只看 RBAC
  一处，不需要与 Bao policy 保持同步。
- 它的 Bao role 只能读 Ayatori 自己交付的凭据路径（ADR-0010 的写入位置），不能读其他 secret；每次读取
  同时留在 Bao 的审计日志里。
- apiserver 的审计策略对该子资源只记 `Metadata` 级别，响应体（密码）不得进审计日志。
- 不采用"把用户的 token 换成 Bao token、由 Bao policy 授权"：不持有特权，但权限要在 RBAC 与
  Bao policy 两处维护，容易漂移。
- 不采用"ESO 把凭据同步成控制面里的 Secret、CLI 直接读 Secret"：控制面的 etcd 与每日备份会成为全部
  数据库密码的第二个存放处，轮换要等刷新周期，`list secrets` 的授权也比按 Database 粗。这与 #22 被否
  的理由相同（凭据不落进没有 workload 的控制面）。
- 对 ADR-0010 的修订只限于人按需读取：workload 仍从 Bao 取凭据，Ayatori 不把凭据投射进消费方集群
  （ADR-0010、"控制面不与 workload 共置"的结论不变）。

### 版本与发布

- CLI 在本仓库（`cmd/ayatori`），与其他制品同一个 tag 发布（ADR-0024）：共用 API 类型，不需要兼容矩阵。
- 偏差：CLI 与 manager 的 minor 相差至多 1（与 kubectl 相同，可新可旧）。CLI 登录与每次调用时比较服务端
  版本，超出时警告，不拒绝。
- 发布 Linux/macOS 的压缩包与校验和，作为 release 附件；包管理器以后再说。

## 不做

- 不包装任意 Kubernetes 对象的通用 CRUD，不提供 `exec`、节点 shell。
- 不在 CLI 里实现任何服务端没有的能力或策略：权限、校验、审计都在服务端。
- 不修改 GitOps 管理的 spec。

## 待定

1. **租户与"project"。** oc 的 project 对应 namespace；Ayatori 目前只有 Database 的 Tenant，是否需要
   一个跨资源的归属概念、CLI 的默认上下文是什么，另行讨论。
2. **节点 join 是否收进 `ayatori adm node join`。** 现在由 homelab-infra 的 Ansible playbook 完成（建
   bootstrap token、写入节点、批准 CSR），当时的考虑是不为一次性操作新造 CLI。有了 CLI 之后这个前提
   变了，但 join 需要在节点上写文件，CLI 只能负责控制面一侧；是否迁移、怎么拆分需要单独决定。
3. **Web 控制台**的范围、技术栈、是否另开仓库，另立 ADR；它只能是 apiserver 的另一个客户端。
4. **隧道**（经中继访问实例内的端口，类似 IAP tunnel）。只有在出现真实、重复的需要时再评估
   （AGENTS.md：需求驱动）。

## 实施顺序

1. apiserver 接受 iam-login 的 token（homelab-cookbooks `homelab_ayatori` 0.2.0），在 Dev 验证。
2. `ayatori login`/`whoami`/`config` 与 `db`、`lb`、`vm`、`ct` 的 `list`/`describe`。
3. `ayatori-console-api`（先只有控制台子资源）与 `ayatori vm console`。
4. 换盘、电源、`db credentials` 与 `task`。
5. `adm node`。

每一步都要有对真实 apiserver 的集成测试（envtest 或 Dev）；登录流程用测试 OIDC 提供方覆盖，不依赖 iam-login。

## 结果

- 人的日常操作不再以 kubectl 为前提，Kubernetes 退回为实现细节。
- 一批"没有去处"的需求有了统一的落点，但每个都仍需证明是真实、重复的需要（AGENTS.md）。
- 多一个随 tag 发布、需要维护兼容的制品；CLI 不持有特权，服务端的授权与审计不变。
