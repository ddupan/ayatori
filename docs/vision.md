# 产品愿景

## 问题

当前 homelab 的期望状态散落在 Kubernetes manifests、Terraform state、Ansible、
Proxmox、libvirt、OpenBao、DNS、对象存储以及人工文档中。即使每一部分分别实现
IaC，跨系统变更仍需要人工安排顺序、复制状态并确认结果，容易产生遗漏、错误和漂移。

文档不能成为运行状态的权威来源：它只能解释设计和操作背景，无法持续观察现实并纠偏。

## 目标

Ayatori 提供统一、声明式、可组合的基础设施 API：

1. Git 保存经过 review 的长期期望状态。
2. Kubernetes API 保存当前意图、资源关系与状态摘要。
3. Controller 将请求翻译给成熟后端，并持续 observe/reconcile。
4. 机器和人通过统一任务协议执行动作。
5. 后端保存实际运行状态，数据面在控制面不可用时继续工作。

平台成功的直接标准是：新增 workload 时，使用者只需考虑它需要哪些资源，而不再回忆
需要依次修改哪些系统、文件和文档。

## 产品定位

Ayatori 是具有产品质量的内部平台，而非初期即面向公众的通用私有云。源码公开不等于
承担未知部署环境、任意后端和第三方兼容性的成本。

平台允许对当前环境形成明确意见：Proxmox、OpenSandbox、Envoy、GoBGP、OpenBao、
PostgreSQL、SeaweedFS、Samba AD DNS、Cloudflare 和 Flux 都可以是已知实现。

## 非目标

- 不替代 hypervisor、microVM runtime、数据库、对象存储或网络协议栈。
- 不从第一天构建通用 provider/plugin ABI。
- 不以隐藏全部后端信息或制造虚假多云可移植性为目标。
- 不创建理解所有应用需求的中央 Application controller。
- 不要求所有人工步骤立即自动化。
