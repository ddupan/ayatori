# JobClass API v1alpha1 草案

- 状态：Draft
- 日期：2026-09-17
- API group：`execution.ayatori.ddupan.top`
- Kind：`JobClass`
- Scope：Cluster

## 目标

`JobClass` 是平台管理员提供给 Job 调用者的执行服务等级。名称表达稳定的用户语义，
例如 `default`、`rootless`、`microvm` 或 `trusted-infra`；调用者不需要知道它当前由 Kubernetes
还是 OpenSandbox 实现。

JobClass 负责：

- 选择拥有该 class 的 adapter/controller；
- 引用 adapter 自己的强类型参数对象；
- 限制允许使用该 class 的 namespace；
- 提供跨后端一致的资源默认值与范围；
- 向 Job controller 报告配置是否被接受、后端是否可用。

它不负责保存队列状态、并发配额、Job history 或任意后端 raw config。

## 设计依据

- Kubernetes `RuntimeClass` 使用 cluster-scoped class 将调用者与具体 runtime handler、调度约束
  和 overhead 隔离。
- `StorageClass` 允许管理员用稳定名称提供不同服务等级，并由调用者显式或默认选择。
- Gateway API `GatewayClass` 使用 `controllerName + parametersRef` 将稳定 class API 与实现专用
  参数分离，并通过 `Accepted` Condition 报告配置有效性。
- Kueue `ResourceFlavor` 将资源规格与具体节点标签、taint 等 placement 细节分开。

Ayatori 采用 GatewayClass 风格的参数引用，不在 JobClass 中建立随 adapter 数量膨胀的
union，也不使用 `map[string]any`。

## 示例

### Kubernetes execution

```yaml
apiVersion: execution.ayatori.ddupan.top/v1alpha1
kind: JobClass
metadata:
  name: rootless
  annotations:
    execution.ayatori.ddupan.top/is-default-job-class: "true"
spec:
  controllerName: execution.ayatori.ddupan.top/kubernetes
  parametersRef:
    group: execution.ayatori.ddupan.top
    kind: KubernetesExecutionParameters
    name: rootless
  allowedNamespaces:
    matchLabels:
      ayatori.ddupan.top/execution: enabled
  resources:
    defaults:
      requests:
        cpu: 250m
        memory: 256Mi
      limits:
        cpu: "2"
        memory: 2Gi
    maximum:
      limits:
        cpu: "8"
        memory: 16Gi
---
apiVersion: execution.ayatori.ddupan.top/v1alpha1
kind: KubernetesExecutionParameters
metadata:
  name: rootless
spec:
  serviceAccountName: ayatori-job
  runtimeClassName: runc
  scheduling:
    nodeSelector:
      ayatori.ddupan.top/node-role: execution
    tolerations:
      - key: ayatori.ddupan.top/execution
        operator: Equal
        value: "true"
        effect: NoSchedule
  podSecurityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
```

### OpenSandbox execution

```yaml
apiVersion: execution.ayatori.ddupan.top/v1alpha1
kind: JobClass
metadata:
  name: microvm
spec:
  controllerName: execution.ayatori.ddupan.top/opensandbox
  parametersRef:
    group: execution.ayatori.ddupan.top
    kind: OpenSandboxExecutionParameters
    name: microvm
  allowedNamespaces:
    matchLabels:
      ayatori.ddupan.top/microvm-access: "true"
  resources:
    defaults:
      requests:
        cpu: "1"
        memory: 1Gi
      limits:
        cpu: "2"
        memory: 2Gi
    maximum:
      limits:
        cpu: "8"
        memory: 16Gi
---
apiVersion: execution.ayatori.ddupan.top/v1alpha1
kind: OpenSandboxExecutionParameters
metadata:
  name: microvm
spec:
  endpoint: https://opensandbox-api.example.internal
  apiKeySecretRef:
    namespace: ayatori-system
    name: opensandbox-api
    key: api-key
  poolRef: microvm
  requestMapping: AdmissionOnly
  allowSecretEnv: true
  allowImageAuth: true
  allowInsecureHTTP: false
```

`endpoint` 默认必须使用 HTTPS。隔离的本地开发环境可以显式设置
`allowInsecureHTTP: true` 使用 HTTP；生产配置不得启用该开关。

## JobClass spec

```yaml
spec:
  controllerName: string
  parametersRef: ParametersReference
  allowedNamespaces: LabelSelector
  resources: ExecutionResourcePolicy
```

### `controllerName`

必填、创建后不可变。使用 domain-prefixed path 标识负责处理该 class 和 Job 的 controller，例如：

```text
execution.ayatori.ddupan.top/kubernetes
execution.ayatori.ddupan.top/opensandbox
```

它是 controller 所有权标识，不是任意可执行插件名称。一个 controller 只能处理自己明确支持
的名称。未来 adapter 拆成独立 Deployment 时，class 和 Job API 无需改变。

### `parametersRef`

必填、创建后不可变：

```yaml
parametersRef:
  group: execution.ayatori.ddupan.top
  kind: KubernetesExecutionParameters
  name: rootless
```

v1alpha1 只允许引用 cluster-scoped 参数对象，并限制 `group`、`kind`、`name` 的长度与格式。
每个 controller 明确列出支持的 kind；引用 ConfigMap、Secret 或未知 CRD 不被接受。

参数引用只有一层，参数 CRD 可以进一步引用 Secret 等运行配置。禁止嵌套通用参数链和 raw
JSON，避免 class 成为无法校验的配置转发器。

### `allowedNamespaces`

可选 Kubernetes `LabelSelector`。Job 所在 namespace 必须匹配才可使用该 class。省略表示允许
所有 namespace；这是显式的管理员选择，而不是用户能力。

该检查由 Job controller 执行，并应尽可能增加 CEL/admission policy 作为快速反馈。用户即使
知道 privileged class 名称，也不能仅靠设置 `jobClassName` 绕过授权。

Namespace label 在 Job 被接受后发生变化，不中断已经运行的 Job，但影响新的 Job。紧急终止
使用独立管理员操作，不通过修改 selector 隐式杀死任务。

### `resources`

可选，定义后端无关的 CPU、内存策略：

```yaml
resources:
  defaults:
    requests: {cpu, memory}
    limits: {cpu, memory}
  minimum:
    requests: {cpu, memory}
    limits: {cpu, memory}
  maximum:
    requests: {cpu, memory}
    limits: {cpu, memory}
```

规则为：

1. Job 未设置的值由 defaults 补齐；
2. 解析结果必须满足 minimum/maximum；
3. CPU 与内存 request 不得大于对应 limit；
4. 解析后的 effective resources 写入 Job status；
5. 后续修改 class 不改变已经接受的 Job；
6. adapter 不能静默降低 effective resources。

只支持 CPU 和内存。GPU、临时磁盘等资源在出现真实后端需求后增加，不先复制完整 Kubernetes
ResourceList。

Runtime/VM overhead 是 adapter 参数或后端调度实现，不计入用户请求的 task resources。
Kubernetes adapter 应优先利用 RuntimeClass Pod overhead；OpenSandbox adapter 在其 capacity
profile 中计算 microVM overhead。

## 默认 class 选择

Job 显式设置 `spec.jobClassName` 时始终优先使用该值。省略时按以下顺序解析：

1. Job namespace annotation
   `execution.ayatori.ddupan.top/default-job-class`；
2. 唯一带有
   `execution.ayatori.ddupan.top/is-default-job-class: "true"` annotation 的 JobClass。

若不存在默认 class，Job 保持 `Accepted=False/NoDefaultJobClass`。若存在多个全局默认值，
Job 保持 `Accepted=False/AmbiguousDefaultJobClass`，同时产生平台告警；不得模仿
StorageClass 选择最新创建对象，因为执行隔离与权限不应随创建时间变化。

解析后 Job status 保存：

```yaml
resolvedJobClass:
  name: rootless
  uid: 8aa4...
  controllerName: execution.ayatori.ddupan.top/kubernetes
  parametersRef:
    group: execution.ayatori.ddupan.top
    kind: KubernetesExecutionParameters
    name: rootless
    uid: c413...
```

Job 后续 reconcile 使用已解析引用，不能因 namespace 默认值或全局默认 class 改变而切换
adapter。若同名 class 被删除并重建，UID 不匹配，现存 Job 不得自动采用新对象。

## Status

```yaml
status:
  observedGeneration: 1
  conditions:
    - type: Accepted
      status: "True"
      reason: Accepted
      observedGeneration: 1
      lastTransitionTime: ...
    - type: Ready
      status: "True"
      reason: BackendReachable
      observedGeneration: 1
      lastTransitionTime: ...
```

### `Accepted`

表示 controller 已识别 controllerName，parametersRef 指向受支持且 schema 有效的对象，通用
resource policy 自洽。无效 class 使用 `Accepted=False` 和稳定 reason，例如：

- `UnsupportedController`；
- `InvalidParametersReference`；
- `ParametersNotFound`；
- `InvalidResourcePolicy`。

### `Ready`

表示该 class 当前具备接受新执行的基本条件。Kubernetes adapter 检查 RuntimeClass 等集群级
依赖；具体 namespace 中的 ServiceAccount 在 Job 调度时检查。OpenSandbox adapter 检查参数
引用、认证材料和后端健康端点。

`Ready=False` 阻止创建新的后端执行，但不改变已经开始 Job 的终态。Controller 仍必须尝试
Observe、Cancel 和 Delete 已存在执行，不能因 class 不 Ready 而停止清理。

Ready 是观测值，不是容量预留。容量不足、排队和并发配额属于调度系统，不通过 Ready 频繁
抖动。

## 生命周期与修改

- `controllerName` 和 `parametersRef` 不可变；切换后端必须创建新 class 名称。
- `allowedNamespaces` 与 resource policy 可以修改，只影响尚未接受的新 Job。
- adapter 参数对象允许更新 endpoint、Secret 引用和其他运维配置，以支持凭据轮换与故障切换。
- 参数更新不得使 adapter 为现存 Job 创建新的逻辑执行；Job 中已持久化的 execution reference
  始终优先。

JobClass controller 添加保护 finalizer。删除 class 前必须确认不存在引用其 UID 的非终态
Job。终态 Job 已完成后端清理，不阻塞 class 删除；其 TTL 回收不再需要 class 后端配置。

参数对象删除保护由各 adapter controller 负责。在仍有 class 引用时，参数对象不得被无提示
删除。强制移除 finalizer 是管理员恢复操作，必须有 runbook 和审计记录。

## KubernetesExecutionParameters

这是 Kubernetes adapter 自己拥有的 cluster-scoped 管理员 API，不是 Job 用户 API。首版字段：

```yaml
spec:
  serviceAccountName: string
  runtimeClassName: string
  scheduling:
    nodeSelector: map[string]string
    tolerations: []Toleration
  podSecurityContext: PodSecurityContext
  imagePullPolicy: Always | IfNotPresent | Never
```

首版原生 `batch/v1 Job` 与 Ayatori Job 位于同一 namespace，因此 Secret/ConfigMap、ResourceQuota、
NetworkPolicy、日志和 owner reference 都保持原生语义。普通调用者只拥有 Ayatori Job 权限，
不应拥有修改生成的 batch Job/Pod 的权限。

`serviceAccountName` 是每个允许 namespace 中预先提供的同名 ServiceAccount。缺失时 Job 保持
未调度并报告原因，不回退到 `default` ServiceAccount。

参数允许使用 Kubernetes 强类型的 Toleration 和 PodSecurityContext，因为这是明确属于
Kubernetes adapter 的管理员 API；这不构成向 Job API 透传 PodSpec。

## OpenSandboxExecutionParameters

这是 OpenSandbox adapter 自己拥有的 cluster-scoped 管理员 API。首版字段：

```yaml
spec:
  endpoint: string
  apiKeySecretRef:
    namespace: string
    name: string
    key: string
  poolRef: string
  requestMapping: AdmissionOnly | Native
  allowSecretEnv: bool
  allowImageAuth: bool
```

- endpoint 必须为 HTTPS，Dev 显式允许的本地配置除外；不得包含认证信息。
- API key 只通过 namespaced Secret 引用，status/Event 不显示内容。
- poolRef 映射为 OpenSandbox 支持的 pool/profile 选择，不允许 Job 覆盖。
- `requestMapping=Native` 要求后端忠实接受 requests 与 limits；`AdmissionOnly` 表示 requests
  只参与 Ayatori 准入，limits 映射为 OpenSandbox resourceLimits。
- Secret env 与 per-request image auth 都会使 adapter 读取 Kubernetes Secret 并把解析值发送
  到 OpenSandbox API，必须由管理员分别显式启用。

OpenSandbox 参数不暴露任意 `extensions` map。未来确需使用某项 extension 时，将其提升为该
参数 CRD 中经过校验的命名字段。

## Condition 与 Job 状态机交互

Job 只有在以下条件同时满足时进入 `Accepted=True`：

- class 已按默认或显式名称解析；
- class UID 与已解析引用一致；
- class `Accepted=True`；
- namespace 符合 allowedNamespaces；
- Job resources 成功解析并处于允许范围；
- Job 使用的 Secret/ConfigMap 存在且可以按 optional 语义解析。

`JobClass Ready=False` 时，Job 保持 `Accepted=True`、`Scheduled=False`，等待后端恢复。
这样 class 配置合法性与当前可用性不会混成同一状态。

若 Job 已经 Scheduled，后续 class Ready 或 namespace label 变化不撤销执行。若 class 或参数
对象意外消失，controller 仍以 Job status 中的 controllerName、参数 UID 和 execution
references 尝试恢复；无法安全观察时进入非终态 `ResultUnknown`，不能切换 class 重跑。

## 测试矩阵

实现至少覆盖：

- 显式 class、namespace 默认和全局默认的优先级；
- 零个与多个全局默认 class；
- allowedNamespaces 允许、拒绝及接受后 label 变化；
- unsupported controllerName 和错误 parameters kind；
- parameters 不存在、稍后出现、UID 删除重建；
- resource defaults、min/max、request 大于 limit 和 quantity 边界；
- class policy 更新不改变已接受 Job 的 effective resources；
- class Ready=False 阻止新 Ensure，但不阻止现存执行 Observe/Cancel/Delete；
- Kubernetes ServiceAccount/runtime 配置缺失且不回退；
- OpenSandbox API key Secret 缺失、轮换及后端健康恢复；
- allowSecretEnv/allowImageAuth 拒绝不允许的 Job；
- class 删除被非终态 Job 阻止，终态清理后允许删除；
- 同名 class 或参数对象以新 UID 重建时不劫持现存 Job。

## 延后事项

- class 级并发和速率限制；
- Kueue LocalQueue/ClusterQueue 映射；
- capability-based 自动 class 选择；
- 多集群 Kubernetes executor；
- GPU、临时磁盘和其他扩展资源；
- workload identity 与 OpenSandbox Credential Vault；
- class 成本、优先级与抢占策略。

## 成熟实现参考

- [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
- [Kubernetes StorageClass](https://kubernetes.io/docs/concepts/storage/storage-classes/)
- [Gateway API GatewayClass](https://gateway-api.sigs.k8s.io/reference/api-types/gatewayclass/)
- [Kueue ResourceFlavor](https://kueue.sigs.k8s.io/docs/concepts/resource_flavor/)
