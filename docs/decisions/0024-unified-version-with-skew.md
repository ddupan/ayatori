# ADR-0024：组件共用版本号，部署允许有限的版本偏差

- 状态：Proposed
- 日期：2026-11-11
- 依据：维护者讨论（v0.3.1 只改了 manager 与 CRD，LB agent 制品也随 tag 重新产出；Prod 实际运行
  manager v0.3.1 与 agent 0.3.0，但没有任何规则说明这个组合是否被允许）。

## 问题

一个 tag 产出三样制品：manager 镜像、Flux 清单（含 CRD）、LB agent 压缩包。部署方分别固定它们：
homelab-infra 的 `ayatori-<env>.rb` 固定 release（Flux 清单与 manager 镜像），
`loadbalancer-<env>.rb` 固定 agent 版本。两者可以不同，但哪些组合可用只能靠人判断。

升级 agent 不是免费的。agent 内嵌 BGP speaker（[ADR-0017](0017-lb-host-local-announcement.md)），
进程重启即会话断开、本机撤回 VIP；目前没有 graceful restart，两台 LB 主机必须逐台滚动，每台约
1 秒抖动。所以「manager 一发版 agent 就跟着升」的代价落在数据面上。

可选的做法：

1. **各组件独立版本**（`manager/v0.3.1`、`lb-agent/v0.3.0`）。不采用：两者在同一仓库、共用 API
   类型，独立版本需要维护兼容矩阵、按 tag 前缀拆 CI 与 release notes；两个组件不值得这套成本。
2. **强制同版本部署**。暂不采用：每个 manager 补丁都要滚动 LB 主机。agent 有了 graceful restart
   之后这个代价会变小，届时可以重新评估。
3. **共用版本号，允许有限偏差**。采用。这是 Kubernetes 的做法：kubelet 可以落后
   kube-apiserver，但不能领先。

## 决策

**版本。** 一个 tag `vX.Y.Z` 发布全部制品，版本号相同。制品内容没变也照常产出；部署与否由
环境的 policy 决定。

**偏差规则。** 下文 manager 指环境固定的 release（Flux 清单、CRD 与 manager 镜像），agent 指
LB agent：

- agent 不新于 manager（按 semver 比较，预发布低于正式版）；
- major 相同，agent 的 minor 至多比 manager 低 1；
- patch 不限。

0.x 期间同样适用，不因版本号小于 1 而放宽。

**升级顺序。** 先 manager（`ayatori-<env>`），再 agent（`loadbalancer-<env>`，逐台）。回滚反过来。
只修 agent 的补丁版本也要先把 manager 升到同一版本：manager 重启不影响数据面，代价落在便宜的
一侧。

**发版方的义务。**

- manager 与 CRD 必须兼容落后一个 minor 的 agent。做不到时拆成两个 minor：先加入新行为并保留
  旧行为，下一个 minor 再去掉旧的。
- release notes 写明 agent 是否有变化（「agent：无变化」或变更列表），部署方据此决定是否升级 agent。
  发布说明由 release-please 生成，这一条在合并 release PR 前补进 PR 说明。

**执行。**

1. homelab-infra 的 chef CI 检查各环境的 manager 与 agent 版本，超出规则的 PR 失败。
2. agent 在 LoadBalancerHost status 上报自身版本，manager 在超出规则时给出 condition 与指标。
   随下一个 minor 实现，字段在实现时确定。
3. agent 不因版本超出规则而拒绝运行：拒绝运行等于撤回 VIP，比偏差本身伤害更大。

## 后果

- 现状（manager v0.3.1、agent 0.3.0）合规，无需为此滚动 LB 主机。
- agent 只在自身有变化或即将超出偏差时升级。
- 兼容一个 minor 的义务会让破坏性改动多走一个版本。
- agent 重启抖动仍在，降低它（BGP graceful restart）另行决策；它落地后可重新评估强制同版本。

## 待验证

- [ ] homelab-infra chef CI 的版本检查合入，并在违反规则的 PR 上失败。
- [ ] agent 上报版本，manager 在超出规则时给出 condition 与指标。
