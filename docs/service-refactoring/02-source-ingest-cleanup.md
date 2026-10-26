# 无链接数据删除与采集服务清理

执行日期：2026-10-26。依据用户要求：删除没有链接的数据，等待后续新服务采集补充；采集入库只保留独立服务需要的最小职责。

## 生产执行结果

事务完成时间：北京时间 **20:41:49**（UTC 12:41:49）。筛选条件为原始链接和生成链接都为空或纯空白：

```sql
btrim(coalesce(node->>'rawLink','')) = ''
AND btrim(coalesce(node->>'generatedLink','')) = ''
```

| 指标 | 事务前 | 删除 | 事务后 |
| --- | ---: | ---: | ---: |
| 注册节点 | 9,005 | 3,288 | 5,717 |
| 其中 available=true | 903 | 6 | 897 |
| 无链接节点 | 3,288 | 3,288 | 0 |

这是一次用户明确要求的数据删除，并非测活判定。没有按旧连接键批量合并节点，也没有建立永久拒绝再次采集的墓碑。

已先停止 `node_list-source-ingest-1` 并设置 `restart=no`，避免旧采集重新写入。其他服务未在本次操作中更新或重启；生产旧版本的 recovery / dead-verify 仍须在新版测活部署时退役，不能把仓库中的删除等同于线上已经停用。

使用单个事务、表锁、备份目标集合核对与数量不变量校验完成删除，同时清除仅属于目标节点的引用：

| 表 | 删除行数 |
| --- | ---: |
| node_explorer_node_registry | 3,288 |
| node_explorer_connectivity | 349 |
| node_explorer_exits | 941 |
| node_explorer_fast_results | 349 |
| node_explorer_probe_results | 143 |
| node_explorer_current_nodes | 2 |
| node_explorer_work | 7,220 |
| node_explorer_snapshot_members | 6 |
| node_explorer_ippure_results | 0 |

5 个受影响快照已移除目标成员并重排位置索引，同步成员关系、注册表位置和属性索引。保留其他节点及其属性事实。此处为一次性授权清理，不作为采集服务的运行时职责。

20:49:34（北京时间）只读复核：注册节点 5,717、无链接节点 0；采集容器状态 `exited`、重启策略 `no`。可用节点数会受其他线上服务影响，表格为事务完成时计数。

## 备份与证据

备份时间：北京时间 20:39:33；服务器私有文件：

```
/root/node-list-backups/20261026-no-links/backup.json
SHA256: 2f4b89a28c697c98096ddc73637f5c16d788bd15a9d5379db8d6888fffde1958
```

包含待删注册节点及关联事实、受影响完整快照及位置属性、修订状态。备份含节点凭据，仅保留服务端私有目录，不加入代码库；本地 SSH 密码辅助文件已清除。脱敏机器记录见 [cleanup-result.json](02-source-ingest-cleanup-result.json)。

前次评估的 8,942 条统计属于另一个只读采样时点，详见 [评估](02-source-ingest-evaluation.md)，不能用于校验本次事务前后数量。

本次按用户意愿等待重新采集，不默认恢复。若要回退，先停所有相关写者、核对备份摘要与当前状态，在事务中恢复节点、关联事实和受影响完整快照及索引；不能只恢复 registry，也不能无条件覆盖后续产生的新事实。备份不是自动恢复脚本。

## 代码清理结果

- 新增独立 `node-source` 可执行程序，采集包迁至 `internal/ingestion`；通用 `node-worker` 不再接受 source 角色。
- 采集入口只连接 PostgreSQL，持有本服务锁、运行来源适配器和写入；不启动共享业务迁移、API、Redis、manager、Xray 或 Mihomo。部署需预建现有共享表。
- 采集管线直接使用存储 adapter，删除历史投影加载、恢复准入与重试回调。无法获得原始或生成链接的条目被过滤，全批拒绝不覆盖上一批。
- 删除历史全库补链扫描、快照压缩与其他服务位置属性清理、跨服务已完成任务清理、全局监控样本采样、飞书通知及配置。
- 注册同步不再推导或覆盖住宅属性；新节点初始化不可用，已有 available、last_available_at、incarnation 和其他服务事实保持原值。不生成下游任务。
- 来源文件 watcher 与 Qilan 退出时取消并等待后台采集，再释放服务锁；本服务心跳支持独立健康检查和停止状态。
- Compose、镜像构建与本地命令改为 `node-source`，采集不继承 Redis 或 manager 环境配置。

全局历史监控样本的表和 API 读者仍保留，删除原采集写者后不再持续生成全局任务/覆盖率样本。本服务只维护自身健康。监控采样若仍需要，应由后续监控职责承担。

## 验证与剩余工作

隔离 PostgreSQL 回归验证：无需其他业务服务即可启动及停止采集；原始快照与 URI 保留；重复导入不修改可用性、最近可用时间、住宅属性和 incarnation，不清理位置属性、不创建任务。另验证缺链接批次拒绝、文件更新与并发导入、文件 watcher 停止等待；执行 backend vet 与带 race 的全部 Go 测试。

验证结果：`go vet ./...`、隔离 PostgreSQL 下 `go test -race ./...`、三个 Go 服务入口构建、10 项部署脚本测试均通过；Compose 经 YAML 解析与采集依赖断言验证。`go list -deps ./cmd/node-source` 确认没有引入通用 service、taskqueue、transport、collector 或 liveness 包。当前环境无 Docker，未执行完整镜像构建或容器启动验收。

本轮没有部署新镜像，也没有自动恢复采集。第三方解析器、严格配置指纹、新版来源模型仍按 [SRC-04b～09](02-source-ingest.md) 推进。现有兼容协议/身份实现还有其他读者，当前没有盲删共享实现，也没有继续增加手写协议处理。恢复生产采集应等待这些实现与协议回归验收完成后切换新写者。
