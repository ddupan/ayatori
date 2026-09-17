# ADR-0005：Job 使用短生命周期控制对象与 TTL 回收

- 状态：Accepted
- 日期：2026-09-17

## 背景

Ayatori 的 `Job` 表达一次有限时长、有退出结果的执行。CI、基础设施 controller 和用户可能
持续创建大量 Job。若所有 Job CR、后端 Kubernetes Job、Pod、Event 和日志都长期保存在
Kubernetes 中，etcd、apiserver list/watch、controller cache、备份及恢复会持续承担历史
数据成本。

Kubernetes API 适合作为活动执行的在线控制与协调面，但不应在没有明确产品需求时兼任无限
增长的执行历史数据库。当前也尚未证明 Ayatori 必须独立于调用者提供长期历史或审计检索。
Gitea Actions 等调用方已经拥有自己的执行历史；基础设施 controller 也应把 Job 结果转化为
所属领域资源的 status。执行日志属于观测数据，应进入统一 observability 平台，而不是由
Job CR 或专用执行历史存储重复保存。

Kubernetes 原生 `batch/v1 Job` 使用独立的 TTL-after-finished controller 回收完成对象。
该 controller 只支持原生 Job，不能直接处理 Ayatori CRD，但其基于 informer 与延迟工作队列
的实现模式可以复用。

## 决策

Ayatori 提供 `execution.ayatori.ddupan.top` API group 下的 `Job` Kind。与 Kubernetes
`batch/v1 Job` 同名不构成冲突，完整 GVK 明确资源身份。

Ayatori Job 是短生命周期控制对象，不是永久执行记录。首版不要求将结果归档到 PostgreSQL，
也不引入 `Archived` condition 或 `JobRecord` CRD。

### 终态与 TTL

Job 到达成功、失败或取消终态后保留有限时间，随后由 Ayatori 自己的 TTL controller 删除。
API 提供与原生 Job 语义一致的 `spec.ttlSecondsAfterFinished`；未设置时是否允许无限保留由平台
准入策略决定，而不是隐式默认永久保存。

TTL 从 controller 写入的可信终态时间开始计算。TTL controller：

1. watch Job 的新增和更新；
2. 只处理已终止、设置 TTL 且尚未删除的对象；
3. 未到期时使用延迟工作队列在到期时间重新入队；
4. 到期时重新读取最新对象并复核 UID、终态和 TTL；
5. 使用 UID precondition 发起删除，防止删除同名重建对象；
6. 由 Job finalizer 完成实际执行后端与临时凭据清理。

Controller 重启后，informer 的初始 LIST 会重新触发现存对象的 reconcile 并重建内存中的延迟
任务，因此不以周期性全量扫描作为正确性基础。

### 结果消费

调用者必须在 TTL 窗口内观察 Job 终态，并把需要长期存在的业务事实写入自身状态。例如 VM
provision Job 成功后，VM controller 更新 `Provisioned` condition；之后删除 Job 不影响 VM
状态。CI 系统负责保存自身 workflow 历史。

Job status 只保存控制和短期诊断需要的结构化结果，不保存完整日志、大型输出或 artifact。

### 日志与 artifact

各 execution adapter 必须为执行实例注入稳定的关联信息，使 stdout/stderr 能由共享
observability 管道采集，并能够按 Ayatori Job 的 namespace、name 和 UID 查询。Job status
可以保存查询观测数据所需的关联标识或受控链接，但日志内容及其索引、保留和查询能力属于
observability 平台。

Job 与后端执行对象的 TTL 应为日志采集提供合理窗口，但 GC 不以日志归档成功作为前置条件，
避免观测平台故障阻塞控制面资源回收。日志采集延迟、丢失和后端不可用通过 observability
自身的监控和告警处理。

Artifact 与日志语义不同。调用者需要消费的构建产物、状态文件或结构化输出必须显式写入
对象存储等持久后端，并通过引用交付；不能依赖日志系统作为 artifact 存储。

### GitOps 边界

一次性 Job 不由 Flux 持续管理。否则 TTL 删除会被视为漂移并重新创建，从而重复执行。GitOps
可以管理 Job template、schedule、execution class 和策略；CI、CLI、UI 或其他 controller
通过 Kubernetes API 命令式创建 Job。

### 未来归档

只有出现长期历史查询、统一审计、调用者无法及时消费结果等真实需求时，才引入
外部 `JobRecord`/History API。届时可以为需要持久化的 retention policy 增加归档流程，并将
归档成功作为删除前置条件；不要求所有 Job 无条件承担该成本。

## 结果

- etcd 中只保留活动 Job、短期已完成 Job 和需要人工处理的异常 Job。
- 首版不依赖 PostgreSQL 和对象存储即可完成 Job 纵向切片。
- 调用者必须正确 watch 或轮询结果；TTL 配置必须为其提供足够消费窗口。
- 历史日志由共享 observability 平台查询，Job CR 只提供执行关联信息。
- Job 删除后的历史默认不可从 Kubernetes API 恢复，这是有意接受的语义。
- TTL controller 是 execution 领域的一部分，可以和其他 controller 编译、部署在同一个
  controller manager 中。
- 若未来增加归档，应作为独立产品能力和 retention policy 演进，不改变 Job 作为短生命周期
  控制对象的基本定位。

## 参考

- [Kubernetes Automatic Cleanup for Finished Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/)
- [Kubernetes TTL-after-finished controller](https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/ttlafterfinished/ttlafterfinished_controller.go)
