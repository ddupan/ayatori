# 环境与发布

## CI 验证入口

Verify 工作流在 PR 上执行全量测试、lint 和 Database 集成测试；合并到 main 后不通过 push
事件重复运行。需要排障或验证直接推送的紧急修复时，可通过 workflow_dispatch 手动运行。
此约定不减少检查项目，也不修改分支保护设置；常规变更必须经过 PR，直接推送 main 不会自动验证。

Gitea 的 PR 工作流验证分支 head，而不是合并预览提交，见
[官方事件说明](https://docs.gitea.com/usage/actions/faq/)。合并前必须确认最新 head 检查通过，
且与当前 main 合并不会引入未经验证的内容组合；基线有实质变化时先更新分支并重验。
只改变基线引用且目标文件树不变时，不需要为了合并提交的 SHA 不同重复全量验证。

## 环境与制品晋级

Ayatori 首先建立 Dev。首个产品能力完成开发并达到可发布状态前，Prod 不实际存在；此时
没有生产制品需要承载，提前维护第二套环境没有收益。

Dev 使用 managed runtime profile，以单台启用了 worker 的 k0s control node 同时承载
API、Flux、Ayatori controllers 和平台 operators。Job 等非控制 workload 应使用外部
执行后端或后续加入的 execution worker，不与平台控制组件争抢资源。

Dev 是允许随时部署、停止和销毁 workload 的可破坏环境。它不要求所有开发中组件始终
由 GitOps 管理：Flux 可以只维护稳定基础组件；正在开发的 controller 可以暂停其
in-cluster deployment，由开发机上的进程直接连接 Dev API。准备集成验证时，再将同一
组件构建为不可变镜像并恢复完整 GitOps 部署。

首个功能通过 Dev 集成测试后，创建独立 Prod，并从该制品开始执行正式 promotion 流程：

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

不维护长期漂移的环境分支。Git 中分别声明 Dev 与 Prod 当前采用的不可变制品版本或 digest。

Prod 建立后，Dev 与 Prod 使用独立 API、数据库、身份和凭据。Dev 应连接真实后端，但
使用独立地址空间和资源范围；Dev 凭据在后端权限层面不应具备修改 Prod 资源的能力。

Prod 默认同样使用 managed runtime profile，并将 Flux 与 Ayatori controller 调度到启用
worker 的 control node。是否采用多个 control node、是否加入独立 execution worker，由
首个生产功能的可用性和容量需求决定，而不是 Dev 阶段预先建设。

当前手工管理的基础设施视为 legacy 数据面，由 Prod 逐项 import/adopt 或替换；它不是
第三套 Ayatori 控制面。
