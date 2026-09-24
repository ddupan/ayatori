# 现有数据库导入与迁移 Runbook

| 项目 | 内容 |
| --- | --- |
| 状态 | Review；尚未在临时 PostgreSQL 演练 |
| 适用范围 | 管理员显式导入，或通过 dump/restore 迁移到新资源 |
| 最后更新 | 2026-09-24 |

当前设计支持管理员显式登记已有 Database；未知同名资源仍不得自动认领。
导入不要求移动数据，不隐含改密码、owner、授权或删除权限。API 尚未实现，以下导入步骤
是验收要求而非可直接执行的命令。

## 显式导入与保留资源复用

1. 核对 Instance、数据库、owner、角色权限、扩展、使用者与备份，确定允许管理的范围。
2. 由管理员声明 Database，指定已有目标，回收策略默认 Retain；导入验证初始只读。
3. 安全关联现有应用凭据；具体 API 待定，不把密码写入 CR，不因验证失败重置密码。
4. 管理员授权目标 Tenant；controller 验证资源与申请符合要求并建立排他绑定。
5. 验证实际登录与 ESO 交付；不满足时停止，不以修改原数据库作为默认修复。

Released 资源复用前另需核实旧使用者的访问权限、数据交接与投射处置。保留旧绑定身份直到
人工处理完成，不仅靠清空 claimRef 或修改 UID 授予新使用权。

导入失败时原数据库应保持不变；撤回登记不得触发 Delete。绑定后的回退按 Retain 释放，
检查新投射与访问授权的影响，不能承诺撤回 CR 自动恢复此前所有外部访问状态。

## 可选的 dump/restore 路径

不适合直接导入、需要改变 owner/权限模型或移动数据时，可使用下述逻辑迁移流程。
它不是纳管现有数据库的唯一路径；保留旧资源作为限时回滚点。

以下命令是顺序模板，不可原样复制到真实环境。先把尖括号变量解析成明确值，确认当前
连接目标，再逐条执行。dump 可能包含敏感业务数据，必须放在加密临时存储且不得提交 Git。

## 前置条件

- 已验证 PostgreSQL/OpenBao 备份和恢复；记录恢复点。
- Instance 已 Ready，目标 namespace 存在，ESO ClusterSecretStore Ready。
- 最终 database/login role 当前由旧应用占用，但改名后的保留名称、新推导的 Bao path
  均不存在。
- 已记录旧 database owner、grants、extensions、locale/encoding、连接配置和验证清单。
- 已确认应用可停止写入，并确定回滚窗口和负责人。
- 已确认旧 login role 不被其他 database/应用共享，且角色改名不会破坏未纳入本次维护
  的依赖。

## 迁移顺序

### 1. 盘点与预演

```sh
pg_dump --schema-only --no-owner --no-privileges \
  --dbname='<old-admin-connection>' > schema-preview.sql
```

检查不受 v1alpha1 管理的对象：额外 roles、跨库依赖、FDW、large objects、订阅、显式
tablespace、owner/grant 和目标实例不支持的 extension。无法映射为单 database + 单 login
owner 的环境必须先人工简化，不能让 controller 猜测。

### 2. 创建一致性 dump

停止应用写入并确认活跃写事务结束，然后创建最终 custom-format dump：

```sh
pg_dump --format=custom --no-owner --no-privileges \
  --file='<secure-temp>/tenant.dump' \
  --dbname='<old-admin-connection>'
pg_restore --list '<secure-temp>/tenant.dump'
```

不要删除旧 database/role。记录停写时间、dump checksum 和 PostgreSQL 版本。

### 3. 释放最终名称

保持应用停写，终止旧 database 的应用连接。连接其他管理 database，以管理员身份把旧
database 和旧 login role 改为明确的保留名称：

```sql
ALTER DATABASE <old_database> RENAME TO <old_database>_retained_<timestamp>;
ALTER ROLE <old_login_role> RENAME TO <old_login_role>_retained_<timestamp>;
```

identifier 必须由管理员工具安全引用，不能把未经校验的值直接拼入 SQL。PostgreSQL 在
角色改名时会清除以旧角色名加盐的 MD5 密码；使用 MD5 的旧环境必须在维护前准备安全的
密码重设/回滚方法。SCRAM verifier 不受角色名改动影响，但仍须实际验证回滚登录。

### 4. 创建受管空目标

应用 `PostgreSQLTenant`，使用未被占用的 database/loginRole，等待 Ready。确认：

- Database/Instance 身份及 Tenant 排他绑定正确；
- OpenBao 凭据位置与 Database 管理范围及当前交付授权一致；
- ExternalSecret Ready 且目标 Secret 已投射；
- 新凭据可以通过 DNS host 和 IP hostaddr 分别登录空 database。

### 5. Restore

从 OpenBao 或目标 Secret 安全取得新应用凭据，不要把密码放进 shell history。以新 login
owner 连接目标 database：

```sh
pg_restore --exit-on-error --no-owner --no-privileges \
  --dbname='<new-application-connection>' \
  '<secure-temp>/tenant.dump'
```

extension 应由 Tenant spec 创建。若 dump 仍包含 extension 定义，预演必须确认 restore
行为幂等；目标实例不支持的 extension 必须在迁移前解决。

### 6. 验证并切换

- 对比关键 schema、表数、行数/校验和、sequence、function 和 migration version。
- 用新 login 验证读写、migration 和应用健康检查。
- 将应用配置切换到新 Secret 或 OpenBao URL，保持旧数据库只读/停写。
- 观察一个约定窗口，确认错误率、连接数和关键业务功能。

### 7. 收尾

回滚窗口结束后，按独立变更删除旧 database/role/旧凭据；它们不属于 controller，禁止
通过新 Database 的 `Delete` 清理。安全删除 dump 和临时凭据材料，并记录验证结果。

## 回滚

在新目标出现问题且旧资源仍保留时：

1. 立即停止新目标写入。
2. 评估切换后是否产生新数据；若有，先决定反向迁移或接受丢弃，不能盲目切回。
3. 将应用连接切回 retained database/role；若必须恢复原名称，先确保新受管目标已用
   资源侧 `Delete` 完整清理或改用不同名称，再安全地反向执行 rename。
4. 恢复旧凭据（MD5 环境可能需要重设），验证旧服务。
5. 保留失败 Tenant 供排障；修改 Database 的 Retain/Delete 策略前明确其外部资源后果。

若已经删除旧资源，则只能使用已验证备份恢复，不再属于本 runbook 的快速回滚。

## 演练验收

发布首个可用版本前，必须在临时 PostgreSQL/OpenBao/Kind 环境执行本文并记录：

- 显式导入与 Released 重新绑定的授权、旧访问处置和失败不修改原资源；
- 使用的 PostgreSQL major version 和命令版本；
- dump/restore 返回码和对象差异；
- DNS/IP TLS 登录结果；
- ESO 投射与应用启动结果；
- 回滚演练结果；
- 哪些命令或前置检查需要修订。

完成演练前，本文不得标记为 `Verified`。
