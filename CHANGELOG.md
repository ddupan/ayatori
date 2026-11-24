# Changelog

## [0.5.0](https://github.com/ddupan/ayatori/compare/v0.4.0...v0.5.0) (2026-11-24)


### ⚠ BREAKING CHANGES

* **loadbalancer:** LoadBalancerHost 删除 dataPlaneURL（ADR-0022）
* **loadbalancer:** 移除 manager 的远程收敛（ADR-0022 迁移第 3 步）

### 新功能

* **loadbalancer:** LoadBalancerHost 删除 dataPlaneURL（ADR-0022） ([2676000](https://github.com/ddupan/ayatori/commit/2676000f61a718e81ae69f792a0ac6a586c59079))
* **loadbalancer:** 移除 manager 的远程收敛（ADR-0022 迁移第 3 步） ([9e97134](https://github.com/ddupan/ayatori/commit/9e9713456029639054a7948212346e92ef300499))


### 修复

* **bootstrap:** 集成测试的 Secret 字面量改回直接写嵌入字段 ([4647d06](https://github.com/ddupan/ayatori/commit/4647d06079c4ccba50f58b1ba2d4aad2b1891256))
* **ci:** Regenerate 的 App ID 取自 org variable ([2ed6c3c](https://github.com/ddupan/ayatori/commit/2ed6c3ced26e5b6be6c3f7c656921aeb9e249354))
* **ci:** Regenerate 的 App ID 取自 org variable ([4f20f16](https://github.com/ddupan/ayatori/commit/4f20f1631b1240543ee79d0c17c420887a62470c))
* **deps:** update module github.com/oapi-codegen/runtime to v1.9.1 ([6ab3307](https://github.com/ddupan/ayatori/commit/6ab33078fe2991959cec1d04c5455315f456c02a))
* **deps:** update module github.com/oapi-codegen/runtime to v1.9.1 ([f38efd7](https://github.com/ddupan/ayatori/commit/f38efd749cb22fa3659853eb61bc9b2bb681f27f))
* **deps:** update module sigs.k8s.io/controller-runtime to v0.25.3 ([ced3a39](https://github.com/ddupan/ayatori/commit/ced3a39e6af4e7091f3d2d149f7b9e7a7a944b63))
