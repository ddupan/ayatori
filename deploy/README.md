# 部署

Dev 与 Prod 各是一台 Incus LXC 上的 API-only 控制面：k0s controller-only（apiserver、
etcd、只开 namespace 控制器的 kube-controller-manager）为 systemd 服务；ESO、Flux
source/kustomize-controller 与 ayatori-manager 为 podman quadlet 容器，使用 host 网络和
k0s 签发的各自证书用户访问本机 API。LXC 由 homelab-infra 的 Incus Terraform 与 cloud-init
创建，需开启 `security.nesting` 才能运行 podman。

```text
deploy/
├── ansible/   # collection panxiao81.ayatori，role control_plane：k0s、podman、证书、
│              # 第三方 CRD/RBAC、Flux 入口、quadlet
└── flux/      # Flux 从 release tag 同步的仓库内清单（Database CRD、RBAC、ESO 对象、Instance）
```

## Release

一个 git tag `vX.Y.Z` 就是一个 release，其中的部署 role、Flux 清单与 manager 镜像是同一组
制品：

1. 攒够一批变更再发版：功能 PR 不修改版本号；发版时单独提交，把 `deploy/ansible/galaxy.yml` 的
   `version` 与 role defaults 的 `ayatori_version` 改为新版本。未发版的变更在 Dev 以本地 role
   直接部署验证。
2. 在该提交上打 **annotated** tag 并推送，tag 说明写本版变更（用户可见的行为、部署注意事项），
   它就是 Gitea Release 的发布说明。Release 工作流校验两处版本与 tag 一致，用 buildx 构建 manager
   镜像，以同名 tag 推送 `zot-push.lab.ddupan.top/panxiao81/ayatori-manager`（部署从 `zot.lab.ddupan.top` 拉取），再以 tag 说明加镜像 digest
   创建（重发时更新）Gitea Release。

完全不可用的 release 不升版本：修复合并后删除并在修复提交上重打同名 tag，Release 覆盖同名
镜像；部署时 role 重新拉取镜像，镜像变化才重启容器。

环境不跟随 main：Flux GitRepository 固定到 role 所属 release 的 tag，quadlet 拉取同名镜像。
第三方组件（k0s、ESO、Flux）的版本、镜像 digest 与清单 sha256 固定在 role defaults。

## 部署方接口

部署方（homelab-infra）以 git 子目录按 tag 安装 collection，并只声明环境与 PostgreSQL 实例：

```yaml
# requirements.yml
collections:
  - name: git+https://git.ddupan.top/panxiao81/ayatori.git#/deploy/ansible
    type: git
    version: v0.1.0
```

```yaml
- hosts: ayatori_dev
  become: true
  vars:
    ayatori_environment: dev
    ayatori_postgresql_instance: {name: homelab-dev, host: pg-dev.lab.ddupan.top, hostaddr: 192.168.10.127, port: 5433}
  roles: [panxiao81.ayatori.control_plane]
```

OpenBao mount/role、凭据前缀与管理凭据路径由 `ayatori_environment` 推导，见 role defaults。

可选的 `ayatori_gitops`（`{url, path, branch}`，branch 默认 `main`）让本机 Flux 再同步部署方仓库
中的一个目录，用于以 GitOps 声明 namespace、PostgreSQLTenant 与 PostgreSQLDatabase：

```yaml
ayatori_gitops:
  url: https://git.ddupan.top/panxiao81/homelab-infra.git
  path: ./clusters/ayatori-dev
```

该 Kustomization 以 `flux-system/gitops-tenants` 模拟应用，只能管理上述三类资源，其他对象被
apiserver 拒绝；`prune: true`，从目录移除 Tenant 即释放（默认 Retain，不删数据）。
Promotion 即把 Prod 安装的 release 改为 Dev 已验收的 tag。

可选的 `ayatori_loadbalancer` 启用 LoadBalancer controller：

```yaml
ayatori_loadbalancer:
  address_pool: 10.62.1.0/24
  hosts:  # 每项渲染为一个 LoadBalancerHost
    - name: lb-dev-1
      data_plane_url: https://lb-dev-1.lab.ddupan.top:5555
      agent_url: http://lb-dev-1.lab.ddupan.top:9641   # agent 的 metricsAddress，manager 须能访问
```

role 在控制面容器启动、Flux 同步 CRD 之后应用这些 LoadBalancerHost，并按标签
`app.kubernetes.io/managed-by=ayatori-deploy` 删除 inventory 中已移除的主机。删除是异步的：
controller 先清理该主机上的全部 LoadBalancer 配置再放行，主机不可达时对象停在删除中。

此时 Flux 另同步 `deploy/flux/loadbalancer`：ESO 把 Data Plane API 客户端证书（`pki-loadbalancer-<env>`
每天签发）、服务端 CA（`pki/cert/ca`）与 Basic 凭据（`kv/infra/loadbalancer/<env>`）写入
`ayatori-system/loadbalancer-credentials`。首次部署时 Secret 同步前 manager 启动失败并重试。

首次部署后，OpenBao 的 Kubernetes auth mount 需要本环境 API 的公开 CA
（`/var/lib/k0s/pki/ca.crt`），由 homelab-infra 的 OpenBao Terraform 管理；合并前 ESO 与
manager 会持续报告 OpenBao 登录失败并重试。

## 监控

各 LXC 上的 metrics 监听在 LXC 地址（LAN 明文，由 homelab 中央 vmagent 采集，告警规则在
homelab-infra `platform/observability`）：

| 端口 | 组件 | 内容 |
|---|---|---|
| 9100 | node_exporter（systemd 二进制） | 只开 cpu、meminfo、pressure、filesystem（`/` 与备份卷）、netdev（eth0）、systemd（控制面服务）、textfile（`ayatori_backup_last_success_timestamp_seconds`；`ayatori_certificate_not_after_timestamp_seconds`，由 `ayatori-cert-metrics.timer` 每小时导出 k0s PKI 与各 kubeconfig 客户端证书到期时间）、time |
| 8085 | kube-state-metrics（quadlet） | 只导出 CR 状态：`gotk_resource_info`（Flux）、`ayatori_eso_resource_info`（ExternalSecret/SecretStore）、`ayatori_resource_info`（Instance/Database/Tenant 的 Ready、原因与阶段） |
| 8080 | ayatori-manager | controller-runtime 指标 |
| 8180 / 8280 / 8281 | ESO / Flux source / Flux kustomize | 各自进程指标 |

node_exporter 不以容器运行：容器内重新挂载的 procfs 没有 LXCFS 覆盖，会读到宿主数据。非特权
LXC 中 loadavg、vmstat、hwmon 等本就是宿主数据，因此只开启上表的 collector，宿主指标由宿主自己的
node-exporter 负责。ESO v2 不导出状态条件指标，ExternalSecret/SecretStore 的状态改由 KSM 导出。

## 已知限制

- k0s 客户端证书一年到期：删除 `/etc/ayatori-control-plane/<identity>/kubeconfig` 后重跑
  playbook 即轮换。
- 修改 `k0s_disabled_components` 需要先 `k0s reset` 再重装；k0s 版本与配置变更会重启 API。
- Flux 不选主，每台 LXC 单实例；kustomize-controller 使用 cluster-admin。
- 第三方镜像只固定 digest，未做 cosign 验签。
- 备份：`ayatori-backup.timer` 每日以 `k0s backup` 把 etcd 快照、PKI 与 k0s.yaml 写入
  `/var/backups/ayatori`（部署方挂载的独立卷，未挂载时部署失败），保留最新 7 份。恢复见下节。
  这不是离机备份。

## 恢复

按原地址重建 LXC（homelab-infra Incus Terraform；备份卷独立于实例，重建后仍在），安装相同版本的
k0s 后恢复并启动，再执行部署 role 恢复容器与身份：

```sh
k0s restore --config-out /etc/k0s/k0s.yaml /var/backups/ayatori/k0s_backup_<时间>.tar.gz
k0s install controller --config /etc/k0s/k0s.yaml --disable-components <role defaults 中的列表>
k0s start
```

恢复保留原 PKI（CA 指纹不变），OpenBao 的 kubernetes auth 无需更新 CA，已签发的证书 kubeconfig
仍有效。恢复到不同地址的主机时需先把 `/etc/k0s/k0s.yaml` 的 `spec.api.address` 改为新地址，
且 OpenBao mount 的 `kubernetes_host` 也要随之修改。2026-10-04 已在临时 LXC 演练：恢复后
apiserver Ready，演练前写入的标记、Instance、Flux 对象与同步的 Secret 均在，CA 指纹与源一致；
演练环境不运行 controller，避免与原环境双写。

恢复较旧的快照不会破坏外部资源：快照之后新建的 Database 在 PostgreSQL/OpenBao 中成为未登记
对象，同名再供应只报冲突，可用 `source: Import` 重新登记；快照之后已删除的 Database 记录会因
已登记 OID 不存在而报告不可用或冲突，由管理员按 Retain 删除记录。均需人工对账。
