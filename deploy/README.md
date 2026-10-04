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

1. 在 main 上提交，把 `deploy/ansible/galaxy.yml` 的 `version` 与 role defaults 的
   `ayatori_version` 改为新版本。
2. 在该提交上打 tag 并推送。Release 工作流校验两处版本与 tag 一致，用 ko 构建 manager
   镜像，以同名 tag 推送 `zot-push.ad.ddupan.top/panxiao81/ayatori-manager`。

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
    ayatori_postgresql_instance: {name: homelab-dev, host: pg-dev.ad.ddupan.top, hostaddr: 192.168.10.127, port: 5433}
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

首次部署后，OpenBao 的 Kubernetes auth mount 需要本环境 API 的公开 CA
（`/var/lib/k0s/pki/ca.crt`），由 homelab-infra 的 OpenBao Terraform 管理；合并前 ESO 与
manager 会持续报告 OpenBao 登录失败并重试。

## 已知限制

- k0s 客户端证书一年到期：删除 `/etc/ayatori-control-plane/<identity>/kubeconfig` 后重跑
  playbook 即轮换。
- 修改 `k0s_disabled_components` 需要先 `k0s reset` 再重装；k0s 版本与配置变更会重启 API。
- Flux 不选主，每台 LXC 单实例；kustomize-controller 使用 cluster-admin。
- 第三方镜像只固定 digest，未做 cosign 验签。
- etcd 与 LXC 尚无备份；控制面开始承载不可重建的状态前必须先建立备份与恢复。
