# 服务边界与部署

当前有五个应用服务，PostgreSQL 为外部依赖；独立采集、测活、出口和属性服务均不依赖 Redis 或共享 manager。共享 manager、共享 Mihomo 和 Redis 的代码及部署声明已移除。前端保持不变。

| 服务 | 职责 | 依赖 |
| --- | --- | --- |
| node-explorer | PHP API、前端、只读节点与服务查询 | PostgreSQL |
| source-ingest | 来源采集、链接准入、来源与注册表写入 | PostgreSQL、上游来源 |
| node-liveness | 唯一节点测活和可用状态维护 | PostgreSQL、私有 Xray / Mihomo |
| exit-provision (`node-exit`) | 仅为可用节点获取出口 IP | PostgreSQL、私有 Mihomo |
| exit-attributes (`node-attributes`) | 可用出口 IP 缺失分类、风险补全 | PostgreSQL、供应商 HTTP API |

测活服务不投递其他业务任务，不依赖其他服务的运行状态，也不自动恢复、退役或删除节点。来源记录是输入数据契约；新节点默认不可用，重入不改变测活状态。历史 retirement / tombstone 不再决定当前可用性。旧 recovery、dead-verify、manager sweeper 和来源 TCP 准入已删除。

独立测活的调度、错误语义、配置与验收见 [节点测活服务](docs/service-refactoring/10-node-liveness.md)。服务实现文档见 [服务 TODO](docs/service-refactoring/README.md)。

采集服务只有 PostgreSQL 与上游来源依赖，不创建下游任务，不维护测活或属性事实。历史补链扫描、快照压缩、跨服务任务清理、全局监控采样和飞书通知已删除；旧监控采样表与查询已删除，API 保留空历史响应结构。详见 [采集清理记录](docs/service-refactoring/02-source-ingest-cleanup.md)。

## 升级

备份数据库、Compose、`.env`、`release.env` 与旧镜像。先停旧 recovery、dead-verify 和 manager，更新所有应用服务，避免旧版本继续写测活状态。通用 worker 已删除；共享 schema 须在部署前准备，不能再依赖旧 worker 启动迁移。独立测活只维护自有尝试表；独立采集使用 `node-source`，不运行共享迁移，均需要部署时预先建立共享表。独立出口也不运行共享迁移，仅维护 `node_explorer_exit_attempts`；升级前必须停止旧出口 worker，新旧入口共享数据库互斥锁。

```sh
psql -v ON_ERROR_STOP=1 -f deploy/sql/016-retire-legacy.sql
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d
bash deploy/compose.sh ps
```

包装脚本只按当前 Compose project 和已退役 service 标签删除旧容器，包括 manager、shared-mihomo 和 task-redis；不删除数据卷或数据库历史记录。回退先停止新测活服务，再恢复完整旧版本；不要混用两套写入者。迁移 v16 删除旧任务、恢复、退役、投影与采样表；回退旧版本必须同时恢复停机数据库备份。

## 本地验证

```sh
npm run build
npm test
npm run test:backend
python3 -m unittest discover -s deploy -p 'test_*.py'
```

PostgreSQL 集成测试仅在显式指定隔离测试库时启用，每个测试使用独立 schema：

```sh
NODE_EXPLORER_TEST_PG=postgresql://user:password@127.0.0.1/test_db go -C backend test -race ./...
```

可设置 `LIVENESS_TEST_XRAY` / `LIVENESS_TEST_MIHOMO` 为真实二进制路径，执行私有内核回环代理测试；无生产节点凭证，无外部订阅依赖。

独立属性服务自行扫描有效可用出口并按 IP 去重，只补缺失组件，已成功的数据不周期刷新。它只写 IP 属性、自有尝试/供应商冷却和自身心跳，不写节点、出口、测活、生命周期和任务表；不租代理、不触发其他服务。实现与切换步骤见 [属性服务](docs/service-refactoring/05-exit-attributes.md)。

采集服务已完成第三方转换和严格去重，实现见 [采集验收记录](docs/service-refactoring/02-source-ingest-implementation.md)。本地启动前执行 `npm --prefix services/node-source-converter ci --ignore-scripts`，运行时需要 Node.js >=22.14.0；Docker 已包含固定 22.14.0。采集只维护自有来源表及 registry 连接输入，不写其他服务事实；部分批次提交有效节点并报告健康异常。生产部署尚未进行。
