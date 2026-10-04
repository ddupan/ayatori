# ADR-0013：Dev/Prod 以 LXC 上的 API-only 控制面运行

- 状态：Accepted
- 日期：2026-10-04
- 部分取代：[ADR-0002](0002-k0s-optional-workload-runtime.md) 中 Dev/Prod 默认 managed runtime、
  集群内 Flux 的部分；[ADR-0003](0003-dev-api-development-loop.md) 中 Flux 与 in-cluster
  controller 的部分

## 背景

ADR-0002 把 managed runtime（带 worker 的 k0s，集群内运行 Flux 与 controller）定为 Dev/Prod
默认形态，API-only 只用于 CI 与恢复。实际可用的 Dev/Prod 主机是 laptop Incus 上的两台非特权
LXC（2 CPU、2 GiB），在其中运行 kubelet、容器 runtime 与 CNI 既不稳定也没有必要：Ayatori
当前的领域（Database）只需要 API、controller 与外部后端，没有需要调度的 workload。

## 决策

- Dev 与 Prod 都以 API-only 常驻运行：k0s controller-only，以 `--disable-components` 裁剪到
  apiserver、etcd 与只开 `namespace` 控制器的 kube-controller-manager。评估过直接运行上游
  kube-apiserver，因需自行维护 PKI、etcd 与证书轮换而未采用。
- ESO、Flux source/kustomize-controller 与 ayatori-manager 以固定 digest/tag 的镜像经 podman
  quadlet 运行：host 网络、只读根、各自的 k0s 证书用户。LXC 为此开启 `security.nesting`。
- Flux 在各自 LXC 上运行，只同步本仓库 release tag 的清单与部署方仓库中的 Tenant 目录；
  不由其他集群（homelab k3s）的 Flux 远程推送，以免消费方集群持有 Dev/Prod 两套近 admin
  凭据，也避免控制面的 GitOps 依赖消费方集群。Flux 不选主，每台单实例。
- 部署 role 在本仓库 `deploy/ansible`，环境声明（主机、环境名、PostgreSQL 实例、GitOps 目录）
  在 homelab-infra；cloud-init 只负责创建 LXC。一个 git tag 即一个 release：部署 role、Flux
  清单与 manager 镜像同版本。Dev 可直接试验，Prod 只经 release 与 promotion。
- OpenBao kubernetes auth 不持有 reviewer JWT，以登录 JWT 自身调用 TokenReview，因此 manager
  与 ESO 的登录 JWT 使用 API audience；role 只限定 SA 名称与 namespace。

## 结果

- 控制面不依赖 worker、CNI 或任何消费方集群，资源占用约 1 GiB 以内。
- 新增需要 Pod 的能力（如 Job 的 Pod executor）时，必须使用外部执行后端或重新评估运行形态，
  不能假设控制面集群可以调度 workload。
- quadlet 容器与 k0s 的版本、PKI 轮换、备份恢复由部署 role 负责，见
  [deploy/README.md](../../deploy/README.md)。
- 本地开发连接 Dev API（ADR-0003 的开发循环）仍然适用，但没有可暂停的 in-cluster
  Deployment：需要接管时停止 Dev 上对应的 quadlet 服务，依靠同一选主 Lease 防止双写。
