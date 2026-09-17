# ADR-0001：采用 Kubernetes API 作为资源模型

- 状态：Accepted
- 日期：2026-09-17

## 背景

homelab 的基础设施状态分散在多套工具和后端中。仅集中 IaC 文件不能提供持续观察、依赖
关系、异步状态、漂移纠正和人工任务协调。

## 决策

Ayatori 使用 Kubernetes API machinery 与 CRD 表达平台资源、引用和状态，但不将平台
限定为容器编排系统。Controller 可以运行于专用 management environment，并管理集群外
的 VM、LB、数据库、对象存储、DNS、凭据和托管 Kubernetes 控制面。

GitOps 是长期期望状态的主要提交入口；API 是当前意图、关系和状态的在线控制面；真实后端
仍是运行事实来源。Controller 负责三者之间持续收敛。

## 结果

- 获得统一声明式 API、watch、RBAC、admission、conditions 和 controller 生态。
- 可以把机器与人工执行统一建模为异步控制循环。
- 必须维护 CRD 版本、conversion、认证、备份和控制面升级。
- 不在 API 中保存日志、指标、大对象或业务数据，只保存控制所需状态及引用。
