# PoC 09：用 ValidatingAdmissionPolicy 按节点身份限制 Agent 写 status

- 日期：2026-10-31
- 对应：[ADR-0015](../decisions/0015-libvirt-lxc-compute-backend.md) 待验证第 7 项
- 环境：本地 envtest（kube-apiserver 1.37.0），最小的 `LibvirtDomain` CRD（带 status 子资源）。
  用 impersonation 模拟各节点的 Agent：用户名 `system:ayatori:node:<node>`，组 `system:ayatori:nodes`。
- 结论：**可行**。RBAC 加上一条 VAP 就能把 Agent 的写权限收窄到"自己是候选节点的实例的 status"，
  并且 Agent 只能把自己报告为运行节点。

## 约定

- compute controller 给每个候选节点打 label `compute.ayatori.ddupan.top/candidate-<node>`，Agent 也按
  它 watch（ADR-0015 已有）。
- structured authentication 把 Agent 的 SPIFFE ID 映射为用户名 `system:ayatori:node:<node>`，并加入组
  `system:ayatori:nodes`。映射本身在 ADR-0014 的待验证项中，这里用 impersonation 代替。

## 策略

```yaml
# Agent 只能写候选节点包含自己的实例的 status，且 status.nodeName 只能写成自己或保持不变。
# 候选节点由 compute controller 以 label compute.ayatori.ddupan.top/candidate-<node> 标出；
# Agent 的用户名由 structured authentication 从 SPIFFE ID 映射为 system:ayatori:node:<node>。
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: ayatori-compute-agent}
rules:
- apiGroups: [compute.ayatori.ddupan.top]
  resources: [libvirtdomains]
  verbs: [get, list, watch]
- apiGroups: [compute.ayatori.ddupan.top]
  resources: [libvirtdomains/status]
  verbs: [update, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: ayatori-compute-agent}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: ayatori-compute-agent}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: Group, name: "system:ayatori:nodes"}]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: ayatori-compute-agent-status}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: [compute.ayatori.ddupan.top]
      apiVersions: ["*"]
      operations: [UPDATE]
      resources: [libvirtdomains/status]
  matchConditions:
  - name: is-agent
    expression: "'system:ayatori:nodes' in request.userInfo.groups"
  variables:
  - name: node
    expression: "request.userInfo.username.startsWith('system:ayatori:node:') ? request.userInfo.username.substring(20) : ''"
  validations:
  - expression: "variables.node != ''"
    message: "agent username must be system:ayatori:node:<node>"
  - expression: "has(oldObject.metadata.labels) && ('compute.ayatori.ddupan.top/candidate-' + variables.node) in oldObject.metadata.labels"
    messageExpression: "'node ' + variables.node + ' is not a candidate for this instance'"
  - expression: >-
      !has(object.status) || !has(object.status.nodeName) || object.status.nodeName == variables.node ||
      (has(oldObject.status) && has(oldObject.status.nodeName) && object.status.nodeName == oldObject.status.nodeName)
    messageExpression: "'node ' + variables.node + ' may only report itself as the running node'"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: ayatori-compute-agent-status}
spec:
  policyName: ayatori-compute-agent-status
  validationActions: [Deny]
```

## 结果

| 用例 | 结果 |
|---|---|
| 候选节点 poc-1 把 vm1 的 `status.nodeName` 报告为自己 | 允许 |
| 非候选节点 poc-3 写 vm1 的 status | 拒绝：`node poc-3 is not a candidate for this instance` |
| poc-1 写 vm2（候选只有 poc-2）的 status | 拒绝：同上 |
| poc-1 把 vm1 的运行节点报告为 poc-2 | 拒绝：`node poc-1 may only report itself as the running node` |
| 另一候选节点 poc-2 把运行节点改为自己（冷切换后） | 允许 |
| poc-1 写 vm1 的 status 但不改 `nodeName`（例如回写副本健康） | 允许 |
| Agent 修改 spec | RBAC 拒绝（只授予了 `libvirtdomains/status`） |
| poc-3 经 status 子资源给自己加候选 label | 拒绝，label 也没有被加上 |

策略创建后约十几秒才生效，测试中以"非候选节点被拒绝"为生效标志等待。

## 限制与后续

- **候选节点之间互相信任。** 任一候选节点都可以随时把自己报告为运行节点，VAP 无法知道它是否真的是
  DRBD Primary。实际运行位置的权威仍然是 DRBD（ADR-0015），status 只是观察；一台被攻破的候选节点
  能让 status 报错，但不能让实例在别处运行。
- **多个 Agent 同时写 status。** 副本健康、quorum 等每个候选节点都有自己的观察，应当放在按节点分开的
  字段里（如 `status.nodes[<node>]`），VAP 再约束 Agent 只能修改自己那一项，避免互相覆盖。本次只测了
  `nodeName` 一个字段。
- 同样的策略要覆盖 `LXCContainer` 以及 Agent 写的 `LibvirtStoragePool`、`LibvirtNetwork`、
  `LibvirtStorageVolume` 的 status。
