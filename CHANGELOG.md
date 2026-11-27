# Changelog

## 0.6.1 (2026-11-27)

<!-- Release notes generated using configuration in .github/release.yml at main -->

## What's Changed
### 依赖更新
* fix(deps): update kubernetes to v0.37.3 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/51
### 新功能
* feat(cli): 默认跨所有 namespace，按名字查找 by @panxiao81 in https://github.com/ddupan/ayatori/pull/45


**Full Changelog**: https://github.com/ddupan/ayatori/compare/v0.6.0...v0.6.1

## 0.6.0 (2026-11-25)

<!-- Release notes generated using configuration in .github/release.yml at main -->

## What's Changed
### 依赖更新
* chore(deps): update golang:1.27.4 docker digest to 87f49b7 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/23
* chore(deps): update golang:1.27.4 docker digest to 5f1b98f by @renovate[bot] in https://github.com/ddupan/ayatori/pull/29
* chore(deps): update anthropics/claude-code-action action to v1.0.290 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/30
* fix(deps): update module github.com/go-jose/go-jose/v4 to v4.1.9 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/32
* fix(deps): update module golang.org/x/oauth2 to v0.38.0 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/33
* chore(deps): update dependency ubuntu to v26 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/15
* fix(deps): update module golang.org/x/term to v0.48.0 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/38
* fix(deps): update module google.golang.org/grpc to v1.85.0 by @renovate[bot] in https://github.com/ddupan/ayatori/pull/39
### 新功能
* feat(cli): ayatori 随 release 发布 by @panxiao81 in https://github.com/ddupan/ayatori/pull/26
* feat(cli): ayatori version，登录时检查版本偏差 by @panxiao81 in https://github.com/ddupan/ayatori/pull/27
* feat(manager): 启动时在 kube-public/ayatori-version 公布自身版本 by @panxiao81 in https://github.com/ddupan/ayatori/pull/28
* feat(cli): ayatori config kubeconfig by @panxiao81 in https://github.com/ddupan/ayatori/pull/31
* feat(cli): ayatori 命令行（ADR-0027） by @panxiao81 in https://github.com/ddupan/ayatori/pull/12
* feat(cli): 没有人参与时以 SPIFFE 身份认证 by @panxiao81 in https://github.com/ddupan/ayatori/pull/37
* feat(api): ayatori-api 与 db credentials by @panxiao81 in https://github.com/ddupan/ayatori/pull/41
* feat(api): ayatori-api 进 manager 镜像，加部署的 RBAC by @panxiao81 in https://github.com/ddupan/ayatori/pull/43
* feat(api): ayatori-api 聚合子资源与 db credentials（ADR-0027） by @panxiao81 in https://github.com/ddupan/ayatori/pull/42
### 修复
* fix: Dev 测试发现的版本公布与错误显示问题 by @panxiao81 in https://github.com/ddupan/ayatori/pull/44


**Full Changelog**: https://github.com/ddupan/ayatori/compare/v0.5.0...v0.6.0

## [0.5.0](https://github.com/ddupan/ayatori/compare/v0.4.0...v0.5.0) (2026-11-24)


### ⚠ BREAKING CHANGES

* **loadbalancer:** LoadBalancerHost 删除 dataPlaneURL（ADR-0022）
* **loadbalancer:** 移除 manager 的远程收敛（ADR-0022 迁移第 3 步）

### 新功能

* **loadbalancer:** LoadBalancerHost 删除 dataPlaneURL（ADR-0022） ([2676000](https://github.com/ddupan/ayatori/commit/2676000f61a718e81ae69f792a0ac6a586c59079))
* **loadbalancer:** 移除 manager 的远程收敛（ADR-0022 迁移第 3 步） ([9e97134](https://github.com/ddupan/ayatori/commit/9e9713456029639054a7948212346e92ef300499))


### 修复

* **bootstrap:** 集成测试的 Secret 字面量改回直接写嵌入字段 ([4647d06](https://github.com/ddupan/ayatori/commit/4647d06079c4ccba50f58b1ba2d4aad2b1891256))
* **ci:** Regenerate 的 App ID 取自 org variable ([4f20f16](https://github.com/ddupan/ayatori/commit/4f20f1631b1240543ee79d0c17c420887a62470c))
* **deps:** update module github.com/oapi-codegen/runtime to v1.9.1 ([f38efd7](https://github.com/ddupan/ayatori/commit/f38efd749cb22fa3659853eb61bc9b2bb681f27f))
* **deps:** update module sigs.k8s.io/controller-runtime to v0.25.3 ([ced3a39](https://github.com/ddupan/ayatori/commit/ced3a39e6af4e7091f3d2d149f7b9e7a7a944b63))

### 部署注意事项

* agent：无行为变化（只有 module 路径改名、Data Plane API 客户端代码重新生成与依赖更新），LB 主机不必升级；manager 0.5.0 与 agent 0.4.x 在 ADR-0024 的偏差范围内。
* 先升 manager。`--loadbalancer-agent-convergence` 已废弃：默认 true，设为 false 时启动失败，下一个 minor 删除；`--loadbalancer-connect-timeout`、`--loadbalancer-resolvers` 已删除（没有部署传入）。
* `loadbalancer-credentials` 只同步 `powerdns-api-key`；两个环境都升级后，可以删除 Bao 中的 `pki-loadbalancer-<env>` 与 `kv/infra/loadbalancer/<env>` 的 Basic 凭据。
* LoadBalancerHost 已存储的 `spec.dataPlaneURL` 在读取时被剪除，不需要迁移。
