# API 设计原则

## 合理抽象

API 应当比后端简单，但不能剥夺自用场景真正需要的控制力。

- 用户主动做出的资源决策进入产品 API。
- 平台稳定的运维策略进入 Profile、Class 或 Policy。
- 能够稳定推导的机械参数由 adapter 生成。
- 罕见但合理的特殊需求使用受控 override。
- 解析结果、外部 ID 和后端摘要通过只读 status 展示。

例如 VM 用户可以指定 CPU、内存、磁盘、存储、镜像和逻辑网络；QEMU machine type、
cloud-init 设备、bridge/VLAN 映射和默认 placement 由平台维护。

## 不做虚假可移植性

允许 API 表达当前真实后端的有用能力，但不接受任意 `rawConfig` 透传。未来出现第二个
真实实现时，根据已经观察到的共同语义抽象，而不是预先猜测最低公分母。

## 引用与依赖

- 使用 typed reference 表达资源依赖，不复制动态地址和外部 ID。
- 被引用资源暂时不存在或未 Ready 时，controller 应等待而不是要求 apply 顺序。
- 长期依赖通过 API 关系推导；Flux `dependsOn` 只用于确实需要的提交顺序。

## 生命周期基线

新增资源、绑定、回收或恢复语义前，先引用 Kubernetes 官方对应资源设计与成熟 controller
模式，说明采用部分及有意偏离的原因。不要仅沿用字段名称而忽略生命周期与权限边界。
例如 Database 借鉴 PV/PVC 的资源与申请分离、排他绑定和 Retain，但不引入 CSI 协议、
存储调度或额外 registry；见 [ADR-0009](../decisions/0009-database-resource-and-claim.md)。

所有受管资源必须定义：

- `observedGeneration`
- 结构化 `status.conditions`
- ownership 与外部资源标识
- finalizer 与删除策略
- import/adopt/observe 行为
- controller 重启后的恢复行为
- 可重试错误与需要人工介入错误的区别

对数据库、bucket、持久磁盘等资源，默认删除行为必须保守并显式表达。
