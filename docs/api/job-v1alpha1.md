# Job API v1alpha1 草案

- 状态：Draft；CRD 已生成，Kubernetes executor 的 controller 未合入 main，暂停（见[路线图](../roadmap.md)）
- 日期：2026-09-17
- API group：`execution.ayatori.ddupan.top`
- Kind：`Job`
- Scope：Namespaced

## 目标

`Job` 表达一次有限时长、有明确退出结果的机器执行。调用者描述任务载荷和资源需求，平台
选择 execution backend 并持续观察，直到任务成功、失败或取消。

首个 adapter 使用 Kubernetes `batch/v1 Job`，第二个 adapter 使用 OpenSandbox lifecycle
与 execd API。API 不暴露 PodSpec、sandbox ID 创建参数或具体 adapter 配置，但允许表达两个
真实后端共有的 OCI image、进程、环境变量和资源语义。

`Job` 是短生命周期控制对象。完成后依据 `ttlSecondsAfterFinished` 回收，长期业务状态由调用
者保存，日志由 observability 平台保存。详细保留策略见 ADR-0005。

## 非目标

v1alpha1 不提供：

- DAG、workflow 或多步骤 task；
- 定时执行与可复用 Job template；
- 并行 completions、indexed job 或 gang scheduling；
- 自动业务重试；
- 暂停后恢复；
- 交互式 shell、endpoint、snapshot 或长生命周期 sandbox；
- workspace、cache、artifact 上传协议或结构化 task outputs；
- 永久 Job history。

上述能力应由后续独立资源或经过真实需求验证的兼容字段提供，不能通过透传 PodSpec 或
OpenSandbox extensions 提前进入 API。

## 示例

```yaml
apiVersion: execution.ayatori.ddupan.top/v1alpha1
kind: Job
metadata:
  generateName: hello-
  namespace: ci
spec:
  jobClassName: default
  task:
    image: docker.io/library/alpine:3.22
    imagePullSecrets: []
    command: ["/bin/sh", "-c"]
    args:
      - echo "hello ${TARGET}"
    workingDir: /workspace
    env:
      - name: TARGET
        value: world
      - name: TOKEN
        valueFrom:
          secretKeyRef:
            name: example-token
            key: token
      - name: CONFIG_VALUE
        valueFrom:
          configMapKeyRef:
            name: example-config
            key: value
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: "1"
      memory: 512Mi
  activeDeadlineSeconds: 600
  ttlSecondsAfterFinished: 3600
  desiredState: Running
```

## Spec

```yaml
spec:
  jobClassName: string
  task: TaskSpec
  resources: ResourceRequirements
  activeDeadlineSeconds: int64
  ttlSecondsAfterFinished: int32
  desiredState: Running | Cancelled
```

### `jobClassName`

可选。引用平台管理员维护的 cluster-scoped `JobClass`。调用者选择服务等级或执行
能力，而不是直接选择 adapter driver。省略时由 namespace policy 解析默认 class；不存在
默认值时 Job 保持未接受状态，不得静默选择任意后端。

一旦 Job 被接受，该字段不可变。解析出的实际 class 写入 status，以便默认策略后来变化时仍
能恢复原执行。

`JobClass` 及其强类型 parameters API 定义后端配置和调度策略。Job API 不暴露
Kubernetes namespace、OpenSandbox endpoint、API key 或 backend raw configuration。

### `task`

必填，创建后不可变。

```yaml
task:
  image: string
  imagePullSecrets: []LocalObjectReference
  command: []string
  args: []string
  workingDir: string
  env: []EnvVar
```

- `image`：必填 OCI image reference。首版允许 tag；生产策略可以通过 admission 要求 digest。
- `imagePullSecrets`：可选，同 namespace 的私有 registry 凭据引用。
- `command`：可选，覆盖 image entrypoint；空值表示使用 image 默认值。
- `args`：可选，传给 entrypoint/command。
- `workingDir`：可选；为空时使用 image/backend 默认值。
- `env`：可选，名称必须唯一。

`command` 和 `args` 使用 argv 语义，不隐式经过 shell。需要 shell 展开时，调用者必须显式
指定 `/bin/sh -c` 等命令。

#### 环境变量

```yaml
- name: EXAMPLE
  value: literal

- name: TOKEN
  valueFrom:
    secretKeyRef:
      name: example
      key: token
      optional: false
```

`value` 与 `valueFrom` 必须且只能设置一个。v1alpha1 支持同 namespace 的 `SecretKeyRef` 和
`ConfigMapKeyRef`，两者具有相同的引用、optional 和等待语义。Adapter 负责以适合后端且不
写入 Job status 的方式传递值。引用对象或 key 缺失时 Job 保持未开始并通过 Condition 报告。
Secret 内容不得复制到 Event、日志或 backend reference；ConfigMap 值虽然不视为机密，也不
写入 status，避免状态膨胀和不同后端行为不一致。

Kubernetes adapter 保留原生 `SecretKeyRef`/`ConfigMapKeyRef`，由 kubelet 在执行节点解析，
controller 不读取内容。OpenSandbox create API 只接收已经解析的环境变量值，因此该 adapter
必须读取引用并把值放入 sandbox create request。JobClass 必须明确允许 Secret 的这种
传递路径，且 controller 的日志、Event 和 status 不得记录请求正文。未来需要避免把真实凭据
暴露给 sandbox 进程时，使用 OpenSandbox Credential Vault 或 Ayatori 独立 Credential 能力，
而不是改变 `SecretKeyRef` 的既有语义。

私有镜像凭据采用 Kubernetes `kubernetes.io/dockerconfigjson` Secret。Kubernetes adapter
直接传递引用；OpenSandbox adapter 选择与目标 image registry 匹配的条目，并映射到其
`image.auth` create 参数。无法解析、没有匹配 registry 或所选 OpenSandbox runtime 不支持
per-request image auth 时，Job 以明确 reason 失败，不得退回匿名拉取后隐藏真实原因。

### `resources`

可选，使用 Kubernetes `resource.Quantity` 表示数值，但不复用完整 Pod
`ResourceRequirements` 行为。

v1alpha1 支持 `cpu` 和 `memory` 的 requests/limits。Requests 表达准入与调度需求，limits
表达执行上限。JobClass 可以提供默认值和允许范围；解析后的实际资源写入 status。

Adapter 必须显式验证能否满足请求，不能无提示地忽略 limit。后端无法区分 request 与 limit
时，其映射规则属于 JobClass，并在 Job 接受前确定。对 OpenSandbox，CPU 和内存
limits 直接映射为 sandbox VM/container 的 `resourceLimits`；requests 用于 Ayatori 的准入与
调度，并可由 JobClass 映射到后端 resource request 或 capacity profile。映射失败必须
显式拒绝或失败，不能静默降低资源保证。

### `activeDeadlineSeconds`

可选，必须大于零。表示从实际执行开始到任务必须终止的最长时间，不包含排队、class 解析或
后端 provisioning 时间。到期后 controller 请求终止后端，最终以 `Succeeded=False`、
`reason=DeadlineExceeded` 结束。

后端自身的 timeout 可以作为执行机制，但 Ayatori controller 仍以 `status.startTime` 和观察
结果维护领域语义。调度等待超时是不同概念，v1alpha1 不提供。

### `ttlSecondsAfterFinished`

可选，必须大于或等于零。语义与 Kubernetes Job 一致：从终态 transition time 起计算，零
表示立即具备删除资格。该字段在任务完成前后均可修改，但不能保证在既有 TTL 已过期后通过
延长 TTL 阻止并发删除。

平台应通过 schema、CEL 或 policy 设置最大值和推荐默认值。controller 本身不偷偷填充一个
无法从 spec 观察到的永久策略。

### `desiredState`

可选，默认 `Running`。允许的状态迁移只有：

```text
Running → Cancelled
```

设置 `Cancelled` 表示请求终止当前执行并保留 Job 至 TTL 到期。取消是尽力而为的异步操作；
只有 adapter 确认执行不会继续后，Job 才进入终态。字段不得从 `Cancelled` 改回 `Running`。
重新执行必须创建新的 Job。

v1alpha1 不提供 suspend/resume。对任意后端可靠实现 checkpoint/resume 并非共同能力，且暂停
不应被伪装为取消。

## 不可变性

创建后仅允许修改：

- `spec.desiredState`，且只能单向变为 `Cancelled`；
- `spec.ttlSecondsAfterFinished`。

`task`、`resources`、`activeDeadlineSeconds` 和 `jobClassName` 均不可变。首选 CRD CEL
validation 表达这些约束；只有 schema/CEL 无法正确表达时才引入 admission webhook。

Controller reconcile 的技术重试不表示任务重跑。v1alpha1 每个 Job 最多启动一个逻辑执行；
adapter 必须使用 Job UID 作为幂等键。若请求结果未知，controller 必须先 Observe，不能因为
网络超时重新创建可能已经开始的执行。

首个 Kubernetes adapter 创建 `backoffLimit: 0`、`restartPolicy: Never` 的原生 Job，避免继承
Kubernetes 默认的多次业务执行语义。需要重新执行时创建新的 Ayatori Job。

## Status

```yaml
status:
  observedGeneration: 1
  conditions:
    - type: Accepted
      status: "True"
      reason: Valid
      observedGeneration: 1
      lastTransitionTime: ...
    - type: Scheduled
      status: "True"
      reason: BackendCreated
      observedGeneration: 1
      lastTransitionTime: ...
    - type: Succeeded
      status: "Unknown"
      reason: Running
      observedGeneration: 1
      lastTransitionTime: ...
  resolvedJobClass:
    name: default
    uid: 8aa4...
    controllerName: execution.ayatori.ddupan.top/kubernetes
    parametersRef:
      group: execution.ayatori.ddupan.top
      kind: KubernetesExecutionParameters
      name: default
      uid: c413...
  effectiveResources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: "1"
      memory: 512Mi
  execution:
    adapter: kubernetes
    references:
      - type: Job
        id: 5cb0...
  startTime: ...
  completionTime: ...
  result:
    exitCode: 0
    reason: Completed
```

### Conditions

使用标准 `metav1.Condition`。v1alpha1 定义三个核心 Condition：

- `Accepted`：spec、引用、policy 和 JobClass 已解析，可进入调度；
- `Scheduled`：后端已确定并存在可观察的逻辑执行；
- `Succeeded`：任务结果。`Unknown` 表示尚未结束，`True` 表示成功，`False` 表示已经失败或
  取消。

失败不使用单独的 `Failed` Condition。`Succeeded=True` 与 `Failed=True` 会形成需要额外维护的
互斥状态，而标准三态 Condition 已能完整表达一次执行：运行中为 `Unknown`、成功为 `True`、
失败为 `False`。失败类型由稳定 reason 区分；这与 Tekton `TaskRun` 的状态约定一致。

不增加与 Conditions 重复的 `phase` 字段。面向 CLI 的阶段摘要由 printer columns 或客户端从
Conditions 推导，避免两个状态源发生漂移。

常用 `Succeeded` reason 初始包括：

- `Pending`、`Scheduling`、`Running`；
- `Completed`；
- `ProcessFailed`；
- `DeadlineExceeded`；
- `Cancelled`；
- `BackendLost`；
- `ResultUnknown`。

Reason 是稳定、机器可读的 PascalCase 标识；message 面向人类且不得承载程序逻辑。

## 状态机

状态机名称用于设计、测试和 metrics，不增加持久化 `status.phase`。当前状态必须能够从 spec、
deletionTimestamp、Conditions、时间和 execution references 唯一推导。

### 状态定义

| 状态 | 判定摘要 | 含义 |
|---|---|---|
| `Resolving` | `Accepted!=True`，非终态 | 等待 JobClass、Secret、ConfigMap 或 policy 解析 |
| `Scheduling` | `Accepted=True`、`Scheduled!=True` | 选择 adapter 并幂等创建后端执行 |
| `Starting` | `Scheduled=True`、无 `startTime` | 后端已存在，任务主体尚未确认开始 |
| `Running` | 有 `startTime`、`Succeeded=Unknown` | 任务主体正在执行 |
| `Cancelling` | `desiredState=Cancelled`、非终态 | 正在确认后端已经停止 |
| `ResultUnknown` | `Succeeded=Unknown/ResultUnknown` | 无法证明任务仍在运行或已经停止 |
| `Succeeded` | `Succeeded=True` | 成功终态 |
| `Failed` | `Succeeded=False`，reason 非 `Cancelled` | 失败终态 |
| `Cancelled` | `Succeeded=False/Cancelled` | 取消终态 |
| `Deleting` | 存在 `deletionTimestamp` | finalizer 正在停止并清理后端，覆盖其他状态 |

存在多个判定条件时按 `Deleting → terminal → Cancelling → ResultUnknown → Running → Starting →
Scheduling → Resolving` 的优先级推导，保证状态唯一。

`ResultUnknown` 不是终态，不设置 `completionTime`，也不启动 TTL。只有确认任务已经停止，才能
转为成功、失败或取消。暂时无法联系后端不等于后端执行失败。

### 正常转移

```text
Resolving
    │ 引用与策略解析完成
    ▼
Scheduling
    │ 后端逻辑执行已建立并持久化引用
    ▼
Starting
    │ adapter 确认任务主体开始
    ▼
Running ───────────────→ Succeeded
    └──────────────────→ Failed
```

后端在任务主体开始前就确定失败，例如 image pull、runtime 不兼容或 provisioning 失败，可以从
`Scheduling` 或 `Starting` 直接进入 `Failed`，此时 `startTime` 允许为空。

### 取消转移

```text
Resolving  ─┐
Scheduling ─┤
Starting   ─┼→ Cancelling → Cancelled
Running    ─┤
ResultUnknown ─┘
```

尚未创建后端执行时，取消可以立即确认。已经存在或可能存在后端执行时，必须反复执行
Cancel/Observe，确认不会继续运行后才能进入 `Cancelled`。取消请求与成功完成并发时，以先从
后端确认到的不可逆事实为准：已经成功完成的任务保持 `Succeeded`，不能改写成 `Cancelled`。

### 不确定结果与恢复

```text
Ensure/Observe 返回歧义
          ↓
    ResultUnknown
      ├── 找回执行 → Starting / Running
      ├── 找到终态 → Succeeded / Failed / Cancelled
      └── 管理员确认无法继续 → Failed(BackendLost)
```

在 `Ensure` 请求超时且尚未成功写入 external reference 时，adapter 必须使用 Job UID 查询后端，
不能直接再次创建。Controller 重启后遵循相同规则。

### 删除与 TTL 转移

任意状态收到 deletionTimestamp 后进入 `Deleting`。若执行可能存在，先 Cancel/Observe，再
Delete 后端资源并移除 finalizer。TTL controller 只对具有 `completionTime` 的三个终态发起
删除；`Resolving`、`Scheduling`、`Starting`、`Running`、`Cancelling` 和 `ResultUnknown` 均不
具备 TTL 删除资格。

### 状态不变量

- `Succeeded=True/False` 是不可逆终态；终态 reason、`completionTime` 和 result 不再改变。
- `startTime` 和 `completionTime` 一旦设置不可改变；两者都存在时 completionTime 不早于
  startTime。
- `Succeeded=True` 必须具有 `completionTime`，可以没有 exit code，但 adapter 应说明原因。
- `Succeeded=False` 必须具有 `completionTime`；进程失败且能取得退出码时必须保存 exit code。
- 非终态的 `Succeeded` 必须为 `Unknown`，不得省略为具有歧义的空状态。
- `Scheduled=True` 前不得设置 `startTime`；一旦为 True 不再回退。
- execution references 只能由 controller 写入；已有引用不能静默替换成新的逻辑执行。
- Job UID 是执行幂等键；同名但不同 UID 的 Job 必须被视为不同执行。
- `desiredState=Cancelled` 后不得创建新的后端执行。
- reconcile 错误和退避不得修改任务的业务结果。

### 状态机测试矩阵

实现必须至少覆盖以下 table-driven unit tests，并为关键恢复路径提供 envtest：

| 类别 | 场景 | 必要断言 |
|---|---|---|
| 正常 | 创建、开始、退出 0 | 单次 Ensure，时间与成功终态正确 |
| 正常 | 主进程非零退出 | `Succeeded=False/ProcessFailed` 与 exit code |
| 解析 | JobClass 后创建 | 不提前 Ensure，引用出现后继续 |
| 解析 | Secret/ConfigMap 或 key 后创建 | 不泄露值，解析后只启动一次 |
| 后端 | image pull 或 provisioning 失败 | 未设置 startTime 的失败终态合法 |
| 幂等 | Ensure 成功但 status 写入前崩溃 | 通过 UID 找回，不能创建第二次执行 |
| 幂等 | 重复 reconcile 与重复事件 | 不产生额外执行，不改变终态时间 |
| 恢复 | controller 在各非终态重启 | 从持久 status/reference 恢复正确状态 |
| 未知 | Ensure/Observe 超时且结果不明 | 保持非终态，不设 completionTime，不触发 TTL |
| 未知 | 后端恢复后找回运行任务 | 从 ResultUnknown 返回 Running |
| 未知 | 管理员确认执行丢失 | 只在确认后进入 `Failed/BackendLost` |
| 取消 | 在解析、调度、启动、运行阶段取消 | 不再创建或确认停止后才进入 Cancelled |
| 竞态 | 取消与成功完成并发 | 已确认成功不被取消覆盖 |
| 超时 | active deadline 到期 | 请求取消，确认停止后 `DeadlineExceeded` |
| 删除 | 每个非终态阶段删除 | finalizer 清理完成前对象不消失 |
| 删除 | 后端暂时不可达 | finalizer 保留并重试，不误报已清理 |
| TTL | 三种终态到期 | 到期前不删，到期后带 UID precondition 删除 |
| TTL | controller 在等待 TTL 时重启 | informer 恢复计时，最终删除一次 |
| TTL | 到期附近延长 TTL | 最终 GET 重新核对最新 TTL |
| 隔离 | 同名 Job 删除并以新 UID 重建 | 旧队列项和旧后端不得影响新 Job |
| 校验 | 修改不可变字段或取消后恢复 Running | schema/CEL 拒绝请求 |
| 引用 | OpenSandbox 保存 Sandbox 与 Command 引用 | 顺序重试后引用稳定且无凭据 |

### 时间

- `startTime`：adapter 确认任务主体开始执行的时间，而不是 CR 创建或 backend provisioning
  时间；设置后不可改变。
- `completionTime`：进入最终成功、失败或取消状态的时间；设置后不可改变。

TTL 以 `completionTime` 为基准。若后端已经完成但结果暂时无法确认，不得猜测 completionTime。

### Execution reference

`status.execution` 是 execution 领域定义的正式 API 字段，保存 controller 重启后重新 Observe
所需的最小稳定引用：

- `adapter`：实际 adapter 类型；
- `references`：一个或多个由 adapter 定义的不透明外部引用。

```yaml
execution:
  adapter: opensandbox
  references:
    - type: Sandbox
      id: sandbox-123
    - type: Command
      id: command-456
```

单个 `externalID` 不足以表达 OpenSandbox 的 sandbox 与 command 两级资源。`type` 和 `id` 的
值由对应 adapter 定义，调用者只能用于诊断和关联，不能据此实现领域逻辑。execution 领域将
每个引用限制为 `type` 与 `id` 两个非空、有长度上限的字符串，不提供任意 metadata map 或
raw JSON。引用不包含 endpoint、凭据或 Secret 内容。Kubernetes adapter 可以另外通过 owner
reference 管理原生 Job，但仍需把恢复所需引用持久化，并保证同名重建安全。

该结构不提升为跨领域共享的万能 ExternalReference。VM、数据库和 LB 等领域根据真实后端
需要定义自己的受限引用 schema，只有多个领域出现语义完全一致的实际重复后才考虑共享。

### Result

`result.exitCode` 只在后端能够确定主进程退出码时设置。调度失败、取消、后端丢失等情况可以
没有退出码。`result.reason` 提供简短分类；详细诊断写入 Condition message 和 observability，
不得把完整日志写入 status。

Job UID 是跨后端日志、metrics 和 traces 的主要 correlation identity。Adapter 必须将
namespace、name 和 UID 传入执行环境或后端 metadata；高基数字段如何索引由 observability
平台决定，API 不要求把 UID 配置为日志 label。

## 删除与 finalizer

Execution controller 在可能创建外部执行前添加
`execution.ayatori.ddupan.top/job-cleanup` finalizer。

删除一个活动 Job 表示取消并清理，而不是 orphan：

1. 请求 adapter 终止执行；
2. Observe，确认执行不会继续；
3. 删除后端临时资源与短期凭据；
4. 移除 finalizer。

首版不提供用户可选 orphan policy。让一次性任务脱离控制面继续运行既难以观察，也可能产生
副作用。后端长期不可达时由管理员根据 runbook 判断并强制移除 finalizer，该操作必须可审计。

TTL controller 只发起 Job 删除，所有手工删除和 TTL 删除都经过相同 finalizer 路径。

## Adapter contract 对 API 的保证

每个 execution adapter 必须提供以下语义，而非暴露自身 SDK 类型：

```text
Ensure   幂等地建立以 Job UID 标识的一个逻辑执行
Observe  返回尚未开始、运行、成功、失败、取消或结果未知
Cancel   请求停止且可被重复调用
Delete   清理后端临时资源且可被重复调用
```

`Ensure` 的网络超时不能直接触发第二次执行。Adapter 必须能够通过 UID/metadata 查找已创建的
后端对象，或返回 `ResultUnknown` 交由人工处理。

Kubernetes adapter 与 OpenSandbox adapter 实现后，应复审 contract 和 API。只有两个真实
实现都需要且语义相同的字段才提升为通用能力；后端特有功能优先进入 JobClass 或独立
资源，不增加 `rawConfig`。

OpenSandbox 支持从 OCI image 创建 sandbox，但不保证每个 image 都能在所选 runtime、架构或
安全 profile 下成功启动。Adapter 对已知不支持的组合应尽早报告；image pull、进程启动或
运行时不兼容等实际后端失败最终统一表现为 `Succeeded=False`，并以 reason/message 保留可
诊断原因。这不要求 Ayatori 在提交前证明任意 OCI image 一定可运行。

## 待后续设计

- capability-based class 自动选择；
- 私有 image registry 的凭据和统一 workload identity；
- artifact、workspace 与 cache 的独立 API；
- Job 创建速率、并发、quota、公平调度以及是否集成 Kueue；
- observability correlation 的具体 OpenTelemetry/Loki 字段约定；
- 调用者错过 TTL 时是否需要可选的最小审计记录。

这些问题不阻塞首个 Kubernetes adapter 的 API review；image pull 的最小凭据路径必须在实现
前通过 Kubernetes 与 OpenSandbox adapter 测试验证。

## 成熟实现参考

- [Kubernetes Job](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [Kubernetes Job API](https://kubernetes.io/docs/reference/kubernetes-api/batch/job-v1/)
- [Tekton Pipeline API](https://tekton.dev/docs/pipelines/pipeline-api/)
- [Kueue Workload](https://kueue.sigs.k8s.io/docs/concepts/workload/)
- [OpenSandbox API specifications](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/api/index.md)
