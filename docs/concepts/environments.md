# 环境与发布

## CI 验证入口

Verify 工作流在 PR 上执行全量测试与 lint；Database 与 LoadBalancer 的集成测试是按路径触发的
独立工作流（`database-integration.yml`、`loadbalancer-integration.yml`），只改另一个模块时不运行。
合并到 main 后不通过 push 事件重复运行。只改文档（`docs/**`、`*.md`）、Flux 清单（`deploy/**`）或
Release 工作流的 PR 不触发 Verify；Flux 清单在 Dev 同步时验证。
需要排障或验证直接推送的紧急修复时，可通过 workflow_dispatch 手动运行。此约定不减少代码变更
的检查项目，也不修改分支保护设置；常规变更必须经过 PR，直接推送 main 不会自动验证。

GitHub 的 `pull_request` 工作流验证的是合并预览提交（`refs/pull/<n>/merge`，即 head 与当时的
main 合并后的结果），但只在 PR 推送新提交时重新运行，main 之后的变化不会触发。合并前必须确认最新
head 检查通过，且与当前 main 合并不会引入未经验证的内容组合；基线有实质变化时先更新分支并重验。
只改变基线引用且目标文件树不变时，不需要为了合并提交的 SHA 不同重复全量验证。

## 环境与制品晋级

Dev 与 Prod 均已建立（2026-10-04），各是一台 Incus LXC 上的 API-only 控制面：k0s
controller-only 加 podman quadlet 运行的 ESO、Flux 与 ayatori-manager，由 homelab-cookbooks 的
Chef cookbook `homelab_ayatori` 部署，见
[ADR-0013](../decisions/0013-api-only-control-plane-on-lxc.md) 与 [部署说明](../../deploy/README.md)。

Dev 是允许随时部署、停止和试验的环境：可以本地构建镜像直接加载，也可以停止 Dev 上的
manager 服务、由开发机进程直接连接 Dev API（同一选主 Lease 防止双写）。准备集成验证时，
再以 release 制品重新部署。

Prod 只接收 Dev 验收过的同一制品：

```text
source commit
   ↓
immutable artifact
   ↓
Dev deployment + integration tests
   ↓
promotion review
   ↓
Prod deployment of the same artifact
```

不维护长期漂移的环境分支。一个 git tag `vX.Y.Z` 就是一个 release（Flux 清单、同名 manager 镜像
与附在 GitHub Release 上的 lb-agent 二进制）；homelab-infra 的 Chef policy 按环境钉 release 与镜像
digest，promotion 即把 Prod 改为 Dev 已验收的 tag。完全不可用的 release 不升版本，修复后删除并
重打同名 tag。tag 由 release-please 的 release PR 合并时创建（见 [deploy/README.md](../../deploy/README.md)）。
部署逻辑在 cookbook 中，homelab-cookbooks 单独发版，policy 按精确版本引用。

Prod 建立后，Dev 与 Prod 使用独立 API、数据库、身份和凭据。Dev 应连接真实后端，但
使用独立地址空间和资源范围；Dev 凭据在后端权限层面不应具备修改 Prod 资源的能力。

当前手工管理的基础设施视为 legacy 数据面，由 Prod 逐项 import/adopt 或替换；它不是
第三套 Ayatori 控制面。
