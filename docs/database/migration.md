# 现有数据库 dump/restore 迁移 Runbook

| 项目 | 内容 |
| --- | --- |
| 状态 | Review；尚未在临时 PostgreSQL 演练 |
| 适用范围 | 通过 dump/restore 把已有数据库迁移到新的动态供应资源 |
| 最后更新 | 2026-09-24 |

纳管已有数据库首选原地显式导入（`source: Import`，不移动数据），见
[显式导入与重新开放](README.md#显式导入与重新开放)。不适合直接导入、需要改变 owner/权限模型
或移动数据时，才使用下述逻辑迁移流程；保留旧资源作为限时回滚点。

以下命令是顺序模板，不可原样复制到真实环境。先把尖括号变量解析成明确值，确认当前
连接目标，再逐条执行。dump 可能包含敏感业务数据，必须放在加密临时存储且不得提交 Git。

## 前置条件

- 已验证 PostgreSQL/OpenBao 备份和恢复；记录恢复点。
- Instance 已 Ready，目标 namespace 存在，消费方集群已能读取对应 Bao 路径。
- 最终 database/login role 当前由旧应用占用，但改名后的保留名称不存在（新凭据位置按
  Database UID 生成，无需预留）。
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
- Tenant 已公布凭据位置，消费方集群的投射已就绪；
- 新凭据可以通过 DNS host 和 IP hostaddr 分别登录空 database。

### 5. Restore

从 OpenBao（Tenant status 公布的位置）安全取得新应用凭据，不要把密码放进 shell history。以新 login
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

必须在临时 PostgreSQL/OpenBao 环境执行本文并记录：

- 使用的 PostgreSQL major version 和命令版本；
- dump/restore 返回码和对象差异；
- DNS/IP TLS 登录结果；
- 消费方投射与应用启动结果；
- 回滚演练结果；
- 哪些命令或前置检查需要修订。

完成演练前，本文不得标记为 `Verified`。
