# 部署

Dev 与 Prod 各是一台 Incus LXC 上的 API-only 控制面：k0s controller-only（apiserver、etcd、只开
namespace 与客户端证书签发相关控制器的 kube-controller-manager）为 systemd 服务；ESO、Flux
source/kustomize-controller 与 ayatori-manager 为 podman quadlet 容器，使用 host 网络，各自以
CSR 签发并自行续签的客户端证书访问本机 API（ADR-0021）。LXC 由 homelab-infra 的 Incus Terraform 与
cloud-init 创建，需开启 `security.nesting` 才能运行 podman。

节点由 Chef 部署：cookbook 是 homelab-cookbooks 的 `homelab_ayatori`（k0s、第三方 CRD/RBAC、Flux
入口、quadlet、客户端证书、备份与证书指标），节点基线（node_exporter、CA 与 SSH CA 信任、SPIRE
agent）由 `homelab_base` 负责；环境声明是 homelab-infra `infrastructure/chef/policies/ayatori-<env>.rb`。
cookbook 与 release 是两条版本线：cookbook 按 homelab-cookbooks 的 tag，release 由 policy 属性指定。

```text
deploy/
└── flux/      # Flux 从 release tag 同步的仓库内清单（CRD、RBAC、ESO 对象、Instance）
```

## Release

一个 git tag `vX.Y.Z` 就是一个 release：Flux 清单与 manager 镜像是同一组制品。

1. 攒够一批变更再发版：功能 PR 不修改版本号。未发版的变更在 Dev 以本地构建的镜像（policy 中按
   digest 引用）验证。
2. 在 main 上打 **annotated** tag 并推送，tag 说明写本版变更（用户可见的行为、部署注意事项），
   它就是 Gitea Release 的发布说明。Release 工作流用 buildx 构建 manager 镜像，以同名 tag 推送
   `zot.lab.ddupan.top/panxiao81/ayatori-manager`，再以 tag 说明加镜像 digest 创建（重发时更新）
   Gitea Release。
3. 部署方把 policy 的 `homelab_ayatori.release` 改为该 tag、`image` 改为该镜像的 digest。Promotion
   即把 Prod 的 policy 改为 Dev 已验收的 release。

完全不可用的 release 不升版本：修复合并后删除并在修复提交上重打同名 tag，Release 覆盖同名
镜像；镜像按 digest 引用，部署方随之更新 digest。

环境不跟随 main：Flux GitRepository 固定到 policy 中的 release tag。

## 部署方接口

policy 属性（`homelab_ayatori`）：`environment`（dev/prod）、`release`、`image`（按 digest）、
`postgresql_instance`（`{name, host, port}`，可选 `hostaddr`），可选的 `gitops`（`{url, path, branch}`）
与 `loadbalancer`（`address_pool`，可选 `dns: {url, zone}`）。OpenBao mount/role、凭据前缀与管理
凭据路径由 `environment` 推导。

`gitops` 让本机 Flux 再同步部署方仓库中的一个目录，以 GitOps 声明 namespace、PostgreSQLTenant、
PostgreSQLDatabase、LoadBalancer、Backend 与 LoadBalancerHost。该 Kustomization 以
`flux-system/gitops-tenants` 模拟应用，只能管理这些资源，其他对象被 apiserver 拒绝；
`prune: true`，从目录移除 Tenant 即释放（默认 Retain，不删数据）。

声明 `loadbalancer` 时 Flux 另同步 `deploy/flux/loadbalancer`：ESO 把 PowerDNS API key（默认 Bao
`kv/k8s/powerdns` 的 `api_key`）写入 `ayatori-system/loadbalancer-credentials`；不声明 `dns` 时同样
会读取，所以 ESO 角色始终需要它的读权限。首次部署时，Secret 同步完成之前 manager 会启动失败并不断重试。
同一目录还有 LB agent 的 RBAC 与准入策略（`agent-*.yaml`）：LoadBalancerHost 由各主机的 lb-agent
加入控制面后自行注册，并在本机收敛代理配置；manager 不连接 LB 主机（ADR-0022）。

首次部署后，OpenBao 的 Kubernetes auth mount 需要本环境 API 的公开 CA
（`/var/lib/k0s/pki/ca.crt`），由 homelab-infra 的 OpenBao Terraform 管理；合并前 ESO 与
manager 会持续报告 OpenBao 登录失败并重试。

## 监控

各 LXC 上的 metrics 监听在 LXC 地址（LAN 明文，由 homelab 中央 vmagent 采集，告警规则在
homelab-infra `platform/observability`）：

| 端口 | 组件 | 内容 |
|---|---|---|
| 9100 | node_exporter（homelab_base，发行版包） | 容器内关闭宿主硬件类 collector；policy 追加 systemd collector（只含控制面服务）；textfile：`ayatori_backup_last_success_timestamp_seconds`、`ayatori_certificate_not_after_timestamp_seconds`（`ayatori-cert-metrics.timer` 每小时导出 k0s PKI 与各身份客户端证书到期时间）、Cinc 运行报告 |
| 8085 | kube-state-metrics（quadlet） | 只导出 CR 状态：`gotk_resource_info`（Flux）、`ayatori_eso_resource_info`（ExternalSecret/SecretStore）、`ayatori_resource_info`（Instance/Database/Tenant 的 Ready、原因与阶段） |
| 8080 | ayatori-manager | controller-runtime 指标 |
| 8180 / 8280 / 8281 | ESO / Flux source / Flux kustomize | 各自进程指标 |

ESO v2 不导出状态条件指标，ExternalSecret/SecretStore 的状态改由 KSM 导出。

## 已知限制

- 客户端证书在有效期过去 70–90% 时自行续签。续签失败（例如 ayatori-manager 长期停摆）导致证书
  过期或不足 7 天到期时，重新 converge 即以 bootstrap token 重新签发。
- 修改 cookbook 中 k0s 的 disabled components 需要先 `k0s reset` 再重装；k0s 版本与配置变更会
  重启一次 API。
- Flux 不选主，每台 LXC 单实例；kustomize-controller 使用 cluster-admin。
- 第三方镜像只固定 digest，未做 cosign 验签。
- 备份：`ayatori-backup.timer` 每日以 `k0s backup` 把 etcd 快照、PKI 与 k0s.yaml 写入
  `/var/backups/ayatori`（部署方挂载的独立卷，未挂载时部署失败），保留最新 7 份。恢复见下节。
  这不是离机备份。

## 恢复

按原地址重建 LXC（homelab-infra Incus Terraform；备份卷独立于实例，重建后仍在），安装相同版本的
k0s 后恢复并启动，再 converge 恢复容器与身份：

```sh
k0s restore --config-out /etc/k0s/k0s.yaml /var/backups/ayatori/k0s_backup_<时间>.tar.gz
k0s install controller --config /etc/k0s/k0s.yaml --disable-components <cookbook 属性中的列表>
k0s start
```

恢复保留原 PKI（CA 指纹不变），OpenBao 的 kubernetes auth 无需更新 CA，已签发的客户端证书
仍有效。恢复到不同地址的主机时需先把 `/etc/k0s/k0s.yaml` 的 `spec.api.address` 改为新地址，
且 OpenBao mount 的 `kubernetes_host` 也要随之修改。2026-10-04 已在临时 LXC 演练：恢复后
apiserver Ready，演练前写入的标记、Instance、Flux 对象与同步的 Secret 均在，CA 指纹与源一致；
演练环境不运行 controller，避免与原环境双写。

恢复较旧的快照不会破坏外部资源：快照之后新建的 Database 在 PostgreSQL/OpenBao 中成为未登记
对象，同名再供应只报冲突，可用 `source: Import` 重新登记；快照之后已删除的 Database 记录会因
已登记 OID 不存在而报告不可用或冲突，由管理员按 Retain 删除记录。均需人工对账。
