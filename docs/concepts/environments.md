# 环境与发布

Ayatori 从第一天维护 Dev 与 Prod 两套控制面，因为首个稳定产品能力会立即承载真实服务，
同时后续能力仍需要破坏性集成测试。

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

Dev 应连接真实后端，但使用独立身份、地址空间和资源范围。Dev 凭据在后端权限层面不应
具备修改 Prod 资源的能力。

当前手工管理的基础设施视为 legacy 数据面，由 Prod 逐项 import/adopt 或替换；它不是
第三套 Ayatori 控制面。
