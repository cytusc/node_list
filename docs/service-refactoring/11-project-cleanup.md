# 项目与生产数据库清理

执行日期：2026-10-26。按用户要求，先停止线上服务，再备份和清理。四个独立业务服务与 PHP API 保留，前端 `src/` 无改动。新版代码尚未部署，线上保持停止，不能启动旧镜像连接清理后的数据库。

## 代码范围

- 保留 `cmd/node-source`、`cmd/node-liveness`、`cmd/node-exit`、`cmd/node-attributes`，及其来源、供应商、私有代理进程、锁和配置依赖。
- 删除旧 store 接口、SQLite / PostgreSQL adapter、内存结果投影、变更通知、旧写入/退役/归档路径；删除 residential-audit、linkdiag 与旧 transport。
- 删除旧 Go API 排序、国家名、facets、身份哈希、链接拼接与合并、准入检查、旧来源匹配等无生产调用代码。新采集继续使用第三方 Sub-Store；历史 rawLink 解码仍是私有代理进程必要依赖，保留。
- 删除 Import 接口无用 Sources 参数和旧模型辅助方法。保留共享 JSON 形状兼容，避免改变上游节点和属性数据。
- PostgreSQL 连接工具和共享 DDL 收敛到 `backend/internal/database`；服务启动不执行共享迁移。Go 依赖已 tidy，移除 SQLite 等旧依赖。
- PHP 状态聚合改读 exit-provision / exit-attributes 心跳；移除旧 work / monitor_samples 查询。原前端响应键保留，历史监控为空桶，不伪造采样。
- 删除退役设计/调优文档与无关 opencode 部署资料。保留内核评估、四服务实现、生产采集分析和脱敏证据。统一测试夹具、CI、发布打包、容量统计引用。

## 数据库

共享定义：`backend/internal/database/schema.sql`。旧库迁移：`deploy/sql/016-retire-legacy.sql`，通过 `psql -v ON_ERROR_STOP=1 -f deploy/sql/016-retire-legacy.sql` 执行。SQL 使用事务、锁超时、明确依赖顺序和不带 CASCADE 的 DROP；意外依赖导致整批回滚。两张 live 视图在事务内重建，以解除生产旧视图对 retired_nodes 的依赖。

删除 20 张表：

| 类别 | 表名（省略 node_explorer_ 前缀，zz_ 除外） |
| --- | --- |
| 通用队列、探测会话、供应商限流 | work、probe_results、provider_limits |
| 生命周期、删除、快速探测、恢复 | lifecycle_events、deleted_nodes、fast_results、recovery_events、recovery_totals |
| 旧存储投影和观察结果 | ippure_results、retired_nodes、current_nodes、results_changes、results_state |
| 旧来源游标、旧链接档案、采样 | registry_batches、node_links、monitor_samples |
| 一次性历史备份 | firstseen_backup_20261025、ip_type_clean_backup_archive、zz_cleanup_attrs_20261023、zz_cleanup_work_20261023 |

删除 registry 的 3 个字段：

- `historical_residential`：只服务于已退役的恢复/删除逻辑。
- `connection_key`：旧弱归一化连接键；新采集严格指纹由 source_connections 独立维护，无有效读取者，移除重复写入与索引。
- `source_at`：旧来源覆盖水位，无有效读取者；新版批次已维护 fetched_at，不再重复写 registry。

移除旧结果通知触发器/函数、清理索引及退役服务心跳。现有节点 JSON 不批量裁剪，原始链接和配置不改写。

保留 8 张共享表：snapshots、snapshot_members、node_registry、connectivity、exits、ip_attributes、service_status、service_migrations。四个服务自有的来源、尝试和冷却表由各自启动准备，生产尚未运行新版，因此目前未创建。

保留字段理由：identity 关联观察事实；snapshot_id/node_index 与成员表确定来源位置；first_seen_at 用于展示加入时间；last_available_at 由测活维护；incarnation 防止旧探测结果误写；exits.version 区分出口变化；snapshots.payload 保留采集原文，与 registry 当前配置承担不同职责。保留 migration 账本和 ICU 排序规则。

旧 node_links 的完整档案在停机备份中；当前 registry / snapshots 中的原始及生成链接均保留。新采集自有 source_links 记录后续来源链路，不做旧弱身份档案的错误归并。

## 线上执行结果

- 停止项目全部 10 个容器；禁用 node-list.service、node-list-update.timer，停止更新及临时采集任务；容器 restart policy 改为 no。
- 删除 recovery、dead-verify、mihomo-manager、shared-mihomo、task-redis 共 5 个退役容器。
- 归档并删除仅属于退役 Redis / 共享 Mihomo 的 2 个卷，未执行全局 prune。
- 剩余 API、采集、出口、属性、subs-check 共 5 个旧容器保持停止。其他项目和外部 PostgreSQL 继续运行。
- 数据库由 28 张表收敛为 8 张共享表。停机后保留事实的数量与摘要校验一致：

| 数据 | 保留行数 |
| --- | ---: |
| 节点 registry | 5,706 |
| 快照 | 1,337 |
| 来源成员 | 1,039 |
| 测活记录 | 1,574 |
| 出口记录 | 2,900 |
| IP 属性 | 9,473 |

备份位于服务器 `/root/node_list/backups/service-cleanup-20261026/`，仅 root 可读。包含完整数据库 dump、Compose / 环境 / 部署脚本归档、容器定义、两个退役卷归档、执行日志与前后事实摘要。凭据未进入仓库。

数据库 dump SHA-256：`4ccd1b6799a06f5b849d921b1fe3e77da216896a7f2069137740c02993e0278d`。

已实际恢复完整 dump 到隔离数据库，验证迁移重放和保留事实摘要；验证库随后删除。最终两项冗余列删除也已完成生产前后摘要比较。没有重启旧服务或部署新镜像。

## 验证与恢复

- Go 全包 race、隔离 PostgreSQL 和私有真实内核测试通过；最终 schema / ingestion / JSON 模型变更再次通过 race。
- 迁移回归覆盖保留事实、视图重建、重复执行，以及意外依赖时事务回滚。
- PHP 离线 144 项、认证/链接 34 项、真实 PostgreSQL 契约 96 项通过。
- 部署脚本 10 项通过，Go vet 通过。前端源码无改动。
- 本机没有 Docker，未执行新镜像构建和容器冒烟；线上停机/备份/迁移使用服务器 Docker 实际执行。

下一步需发布新镜像、同步新版部署文件，再启动五个应用服务；在新版验证通过前保持自动更新关闭。恢复旧版必须先停所有新写入者，恢复完整停机数据库备份、旧部署配置、必要卷和旧镜像，不能仅回滚镜像。备份文件保留用于回退，不作为在线业务表。
