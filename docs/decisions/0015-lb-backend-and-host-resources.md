# ADR-0015：后端与 LB 主机成为 API 资源

- 状态：Proposed
- 日期：2026-10-16
- 依据：维护者讨论（ADR-0014 实现后的建模复盘）。
- 部分取代：[ADR-0012](0012-l4-loadbalancer.md)「北向资源」中 `LoadBalancer` 内嵌 endpoint 与
  健康检查的形状；[领域模型](../loadbalancer/domain-model.md)「待评审」第 3 条中 Host 来自 controller
  配置的部分；[ADR-0014](0014-lb-host-local-announcement.md) 中「不开放任何管理接口」与「status
  的宣告事实由 controller 按规则计算」两点。

## 问题

当前 `LoadBalancer.spec.ports[]` 内嵌 endpoint、健康检查与 PROXY protocol，LB 主机清单来自
controller 的 `--loadbalancer-host` 参数。两者都把生命周期不同的东西绑在了一起：

- **后端成员与 VIP 的写入方不同。** VIP 与端口几乎不变；成员随节点、迁移、主备切换变化。
  等待 LB 的 `PostgreSQLInstance` 这类组件应只维护成员，内嵌时无法用 RBAC 把「改成员」与
  「改 VIP、端口」分开。ADR-0012 承诺的「各 endpoint 健康」在 LoadBalancer status 中也一直
  没有合适的位置。
- **主机没有生命周期。** 增减主机要改参数并重启 controller；摘掉主机后其上的配置无人清理
  （领域模型只能写「由管理员清理」）；主机自身是否可达只能在每个 LoadBalancer 的
  `status.hosts[]` 里各报一遍。
- **agent 的状态 controller 看不到。** ADR-0014 让 controller 按同一规则从 Data Plane API 的
  事实推算「应宣告」，与 agent 实际宣告可能短暂不一致；agent 进程挂掉、BGP 会话断开时，
  controller 侧没有任何体现。

## 决策

### Backend（namespaced）

后端集合成为独立资源，取代 port 内嵌的 endpoint：

```yaml
apiVersion: network.ayatori.ddupan.top/v1alpha1
kind: Backend
metadata: {name: pg, namespace: platform}
spec:
  endpoints:
    - {address: 192.168.10.127, port: 5432}
  healthCheck: {type: HTTP, port: 8008, path: /primary}
  proxyProtocol: None
  drainTimeout: 30s
status:
  endpoints: []        # 各 LB 主机观察到的 endpoint 健康，只做呈现
---
kind: LoadBalancer
spec:
  ports:
    - port: 5432
      backendRef: {name: pg}
      idleTimeout: 1h
```

- 字段按归属划分：endpoint、健康检查、PROXY protocol（与后端怎么说话）、`drainTimeout`
  （成员移出时的排空）属于 Backend；`port`、`idleTimeout`（连接级）留在 LoadBalancer。
  字段语义与校验沿用现有 CRD，不重新设计。
- 只能引用同 namespace 的 Backend；跨 namespace 引用需要 ReferenceGrant 一类机制，首版不做。
- 一个 Backend 可以被多个 LoadBalancer 或多个端口引用。HAProxy 侧仍按「LoadBalancer × 端口」
  各生成一个 backend section，共享的后端会被重复健康检查；这样 section 仍只有一个所有者，
  不需要引用计数。
- 引用的 Backend 不存在时，该端口拒绝连接（fail-closed，与空 endpoint 相同），LoadBalancer
  报告 `ResolvedRefs=False`（命名沿用 Gateway API）。controller watch Backend，变化时收敛引用它的
  LoadBalancer。
- 删除 Backend 不被阻止：引用方随之进入 `ResolvedRefs=False`。

### LoadBalancerHost（cluster-scoped）

LB 主机成为平台资源，取代 `--loadbalancer-host`：

```yaml
kind: LoadBalancerHost
metadata: {name: lb-prod-1}
spec:
  dataPlaneURL: https://lb-prod-1.lab.ddupan.top:5555
  agentURL: http://lb-prod-1.lab.ddupan.top:9641
status:
  conditions:
    - type: DataPlaneReachable   # Data Plane API 应答
    - type: AgentReady           # agent 应答、HAProxy 应答、至少一个 BGP 会话建立、收敛未停滞
```

- 每个 LoadBalancer 仍落地到全部 LoadBalancerHost；按 label 选主机等出现多组主机时再加。
- 凭据仍按环境统一：Data Plane API 的 Basic 凭据、客户端证书与服务端 CA 留在 controller 的
  凭据 Secret，资源中只有 URL。
- LoadBalancerHost 由部署方创建（homelab-infra 中各环境的清单），环境相关的值不进入 Ayatori
  发布物。
- 删除带 finalizer：先删除该主机上全部 `lb_` 前缀 section 并确认 reload 完成（agent 随后撤回），
  再放行。主机不可达时删除等待，由管理员移除 finalizer 强制完成，与 LoadBalancer 删除语义一致。
- LoadBalancer 的 `status.hosts[]` 保留：它是该 LoadBalancer 在各主机上的落地进度，与主机自身
  健康是两件事。主机不可达时各 LoadBalancer 的 Attachment 仍为 `Unavailable`。

### agent 状态接口

agent 在指标端口上增加只读的 `GET /status`，返回本机事实：HAProxy 是否应答、各邻居会话是否
建立、本地 RIB 中已宣告的 VIP、最近一次成功收敛的时间。

- 只读，无任何写操作，不含凭据；明文 HTTP，与 `/metrics` 同一监听。伪造的应答只会让 status
  失真，不会改变路由，因此不为它签发证书。
- controller 周期读取（与主机观察同一周期），写入 LoadBalancerHost 的 `AgentReady`。
- LoadBalancer 的 Attachment `Ready` 改为「配置一致且 agent 报告已宣告该 VIP」。controller 不再
  自行按宣告规则推算，ADR-0014 中「两处求值可能短暂不一致」的代价随之消失；宣告规则只在
  agent 中求值，仍是领域纯函数。agent 不应答时 Attachment 为 `Unavailable`，路由不受影响。
- 告警仍以 Prometheus 指标为准；status 面向 `kubectl` 与消费方，不承担告警。

## 已接受的代价

- LoadBalancer 的就绪依赖 agent 状态接口可达：接口不可达而 agent 实际在宣告时，status 报告
  `Unavailable`，与实际路由不符。路由以路由器为准，status 只是观察。
- 共享 Backend 时健康检查按引用重复执行；规模小，可接受。
- API 不兼容变更：`spec.ports[].endpoints/healthCheck/proxyProtocol/drainTimeout` 移出。CRD
  尚未部署（不在 Flux 中），v1alpha1 内直接修改，不做转换。

## 不选的方案

- **Pool 同时成为资源**：VIP 段几乎不变，且多池会立即引出「从哪个池分配、谁能用哪个池」的
  授权问题；当前不需要按消费方划分地址段，保持 controller 参数。
- **一个 Backend 对应一个共享的 HAProxy backend section**：省掉重复检查，但 section 有多个
  引用方，删除与变更需要引用计数，且一个 LoadBalancer 的变更会 reload 影响其他 LoadBalancer。
- **agent 状态接口加 mTLS**：接口只读、只影响呈现；加证书会把 ADR-0014 刚删掉的签发与分发
  重新引回来。
- **controller 读取 Prometheus 判断 agent 健康**：引入对监控栈的运行时依赖，Dev 与 Prod 的
  监控并不对等。

## 后续（不在本次）

- LoadBalancerHost 的维护腾空（如 `spec.drain`）：controller 删除该主机上的配置，agent 随之撤回。
  第一次需要维护时再加。
- 按 label 选择主机、Pool 资源：出现第二组主机或第二个地址段时再加。

## 测试合同

- 领域单测：端口引用缺失的 Backend 时的期望配置与 `ResolvedRefs`；Attachment 由 agent 报告的
  宣告集合决定（已宣告、未宣告、agent 不应答）。
- envtest：Backend 变化触发引用方收敛；删除 Backend 后引用方 fail-closed；LoadBalancerHost 增删
  无需重启；删除 LoadBalancerHost 时先清理配置，不可达时等待。
- agent 集成测试：`/status` 与本地 RIB、HAProxy 状态一致（沿用现有容器环境）。
