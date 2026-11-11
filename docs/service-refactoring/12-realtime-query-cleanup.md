# 实时查询清理与索引分析

更新：2026-10-27。代码与本地验证已完成；017 迁移尚未在生产执行。本轮没有改动线上数据，也没有提交或部署。

## 查询路径

节点页面请求 `/api/results`，接口查询 PostgreSQL 的当前节点、可用状态、测活结果、出口及 IP 属性。`available=false` 的节点不进入查询结果。`node_explorer_live_facts` 和 `node_explorer_live_nodes` 是普通视图，每次执行查询时访问底层表，没有物化结果或定时刷新任务。

`/api/services` 每次请求读取当前服务心跳，只返回当前服务与测活队列状态。页面每 15 秒重新请求，节点列表每 5 秒重新请求；手动刷新也直接请求接口。查询失败清空旧结果并提示重试，不回退到历史文件或缓存。响应使用 `Cache-Control: no-store`。

请求内的只读、可重复读事务用于保证多条 SQL 的一致性。更新提交后，后续请求即可读到；它不保存跨请求的业务快照。

## 已删除内容

| 范围 | 清理内容 |
| --- | --- |
| 数据表 | `node_explorer_snapshots`、`node_explorer_snapshot_members` |
| 字段 | registry 的 `snapshot_id`、`node_index`；source_batches 的 `snapshot_id` |
| 采集 | 整批节点 JSON 快照写入、快照成员写入、成员定位修复 |
| 测活 | 候选查询及结果保存对快照成员的依赖；保留 incarnation 和配置摘要校验，防止旧任务覆盖新节点 |
| 数据契约 | 删除旧 Snapshot 容器的无用字段，改为采集期间使用的 SourceBatch；删除 Node.SnapshotID |
| 监控 | 15 秒接口缓存、24 小时/7 天空历史桶、已退役内核占位数据、无数据历史图表 |
| 冗余实现 | ReadCache、缓存配置和容器注入、webman/cache 及无用传递依赖、recharts 和图表组件、废弃分页解析与 SQL 排序表达式方法 |
| 数据库对象 | 无使用方的 `node_explorer_numeric` 排序规则；简化普通视图，删除重复 registry 关联 |

首页结果文件读取与历史回退已在前一轮删除，本轮清理剩余采集、测活及监控依赖。

## 保留数据的用途

- registry、connectivity、exits、ip_attributes：当前节点及各服务拥有的最新事实。只有测活服务维护 available。
- service_status：每个服务的最新心跳，供监控实时读取。
- source_connections：规范配置、指纹与节点身份，完成严格去重。
- source_batches、source_occurrences：来源内容摘要、处理状态和来源成员关系，支持幂等重放、完整/部分采集保护及当前来源激活。
- source_links、source_rejections、source_identity_aliases：原始/生成链接、冲突及拒绝原因、旧身份采用证据，服务于链接与去重要求。
- 各服务 attempts/limits：任务调度、失败重试、冷却与限流状态。

这些记录不作为网页历史结果或失败回退来源。第三方 IP 属性请求仍保留按 IP/ASN 合并请求和短期缓存，用于供应商限额与重复请求控制；数据查询 API 不通过这些缓存返回结果。

## 索引与性能验证

本地 PostgreSQL 隔离测试 schema 使用 100,000 条合成节点，其中 1,000 条可用，执行 ANALYZE 后检查实际查询的 `EXPLAIN (ANALYZE, BUFFERS)`。

| 查询 | 计划与结果 |
| --- | --- |
| 接口全部可用节点 facts 查询 | 使用 `node_explorer_registry_available` 部分索引，读取 1,000 行；数据库执行约 10.50 ms |
| 当前页 50 个 identity 的展示数据 | 使用可用节点部分索引及 connectivity/exits/ip_attributes 主键索引；执行约 1.44 ms |
| 调用实际 PHP LiveResults 查询，第一页 50 条 | 三次约 43.94、39.83、39.17 ms，包含查询、筛选、排序和响应数据组装，不包含 HTTP 网络传输 |

现有 `registry(identity) WHERE available` 部分索引和关联表主键能够支持本次规模，没有新增索引的测量依据。批量关联时，小表的顺序扫描与哈希连接是优化器选择，不等同于缺少索引。

**当前性能边界：**接口仍将全部可用节点的 facts 读入 PHP，再筛选、排序、分页。可用节点大幅增加时，这部分 CPU、内存和数据传输可能成为瓶颈；只给筛选字段加索引无法优化 PHP 内的筛选。出现实际慢查询时，先记录接口耗时及查询执行计划，将筛选/排序/分页下推 SQL，再根据常用条件与执行计划增加必要索引。不要用结果快照或旧结果回退掩盖慢查询。

以上为本地合成数据测量，不代表生产延迟保证。本轮未对生产执行压力测试。

## 数据库升级

迁移文件：[017-remove-snapshots.sql](../../deploy/sql/017-remove-snapshots.sql)。

1. 停止应用容器，确保旧版本采集与测活进程退出。
2. 对目标数据库执行完整 pg_dump，并校验备份可用。
3. 使用 `psql -v ON_ERROR_STOP=1 -f deploy/sql/017-remove-snapshots.sql` 执行迁移。需要在完整仓库中执行，脚本会相对引用当前 schema.sql。
4. 部署本次匹配版本，再启动服务；检查采集、测活写入及实时查询。

脚本使用事务、schema advisory lock、锁等待与语句超时；不使用 CASCADE。未知依赖会阻止删除并回滚，应先查明依赖。删除快照后的旧代码无法继续使用，不应单独回滚镜像；需要回退时停服务、恢复已验证备份并恢复匹配旧版本。

迁移保留节点身份、可用状态、当前测活/出口/IP 属性与来源摘要。既有 016 迁移补充表存在性判断，避免在删除快照后重复执行时访问已删除的表。

## 验证

- Go 全包 race 测试和 vet 通过；最终 schema 调整后再次通过数据库迁移 race 测试。
- 017 回归覆盖重复执行、未知依赖时事务回滚、保留当前事实，以及修改 available 后视图立即反映变化。
- PHP 单元 133 项、认证/链接 34 项、真实 PostgreSQL 集成 93 项断言通过；集成包含连续请求读取修改后的服务心跳。
- 前端 TypeScript 与生产构建通过；浏览器回归 29 项通过、1 项因本地真实登录前置条件跳过。构建仍提示既有主 JS 超过 500 kB，未阻止构建。
