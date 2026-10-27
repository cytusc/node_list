# 独立服务与文档索引

更新：2026-10-27。四个业务服务已重写；节点列表与监控查询均已移除结果快照、业务查询缓存和历史回退。本次 017 迁移尚未部署。

| 服务 | 职责 | 文档 |
| --- | --- | --- |
| node-source | 第三方解析与补链、严格去重、独立入库 | [采集实现](02-source-ingest-implementation.md) |
| node-liveness | 唯一测活及 available 写入者，Xray 优先、Mihomo 补测 | [测活](10-node-liveness.md) |
| node-exit | 为可用节点获取出口 IP | [出口](04-exit-provision.md) |
| node-attributes | 为可用出口 IP 补缺失属性 | [属性](05-exit-attributes.md) |
| node-explorer | PHP API 与前端 | [API](01-node-explorer.md) |

recovery、dead-verify、manager、共享 Mihomo、Redis 任务队列及旧存储已删除。部署现状、删表清单、备份与回退见 [项目清理](11-project-cleanup.md)。

- [实时查询清理与索引分析](12-realtime-query-cleanup.md)
- [公共数据契约](09-common-contracts.md)
- [基础设施](08-infrastructure-operations.md)
- [生产采集数据评估](02-source-ingest-evaluation.md)
- [无链接数据清理](02-source-ingest-cleanup.md)
- [内核评估](../kernel-evaluation/README.md)

待办：发布本次改动，停机备份后执行 017 迁移，部署匹配镜像并观察服务。
