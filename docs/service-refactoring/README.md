# 独立服务与文档索引

更新：2026-10-26。四个业务服务已重写，前端保持不变。线上已停机清理，尚未部署新版本。

| 服务 | 职责 | 文档 |
| --- | --- | --- |
| node-source | 第三方解析与补链、严格去重、独立入库 | [采集实现](02-source-ingest-implementation.md) |
| node-liveness | 唯一测活及 available 写入者，Xray 优先、Mihomo 补测 | [测活](10-node-liveness.md) |
| node-exit | 为可用节点获取出口 IP | [出口](04-exit-provision.md) |
| node-attributes | 为可用出口 IP 补缺失属性 | [属性](05-exit-attributes.md) |
| node-explorer | PHP API 与原前端 | [API](01-node-explorer.md) |

recovery、dead-verify、manager、共享 Mihomo、Redis 任务队列及旧存储已删除。部署现状、删表清单、备份与回退见 [项目清理](11-project-cleanup.md)。

- [公共数据契约](09-common-contracts.md)
- [基础设施](08-infrastructure-operations.md)
- [生产采集数据评估](02-source-ingest-evaluation.md)
- [无链接数据清理](02-source-ingest-cleanup.md)
- [内核评估](../kernel-evaluation/README.md)

待办：发布新镜像并部署五个应用服务；观察采集拒绝率、严格去重比例、测活吞吐与资源使用。生产保持停止，自动更新保持关闭，直到新版部署完成。
