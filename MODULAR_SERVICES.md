# 服务边界与部署

当前有六个应用服务，PostgreSQL 为外部依赖；Redis、共享 Mihomo 为出口和属性任务提供基础设施。

| 服务 | 职责 | 依赖 |
| --- | --- | --- |
| node-explorer | PHP API、前端、只读节点与服务查询 | PostgreSQL |
| source-ingest | 来源导入、注册表同步、历史保留、监控采样 | PostgreSQL、上游来源 |
| node-liveness | 唯一节点测活和可用状态维护 | PostgreSQL、私有 Xray / Mihomo |
| mihomo-manager | 共享内核配置与出口 / 风险租约 | PostgreSQL、shared-mihomo |
| exit-provision | 出口 IP 核验、属性 / 风险任务生成 | PostgreSQL、Redis、manager |
| exit-attributes | IP 属性分类、风险查询 | PostgreSQL、Redis、manager、供应商 |

测活服务不投递其他业务任务，不依赖其他服务的运行状态，也不自动恢复、退役或删除节点。来源记录是输入数据契约；新节点默认不可用，重入不改变测活状态。历史 retirement / tombstone 不再决定当前可用性。旧 recovery、dead-verify、manager sweeper 和来源 TCP 准入已删除。

独立测活的调度、错误语义、配置与验收见 [节点测活服务](docs/service-refactoring/10-node-liveness.md)。其他服务后续重构规划见 [服务 TODO](docs/service-refactoring/README.md)。

## 升级

备份数据库、Compose、`.env`、`release.env` 与旧镜像。先停旧 recovery、dead-verify 和 manager，更新所有应用服务，避免旧版本继续写测活状态。常规 worker 启动迁移 schema v15；独立测活只维护自有尝试表，需要共享表已存在。

```sh
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d
bash deploy/compose.sh ps
```

包装脚本只按当前 Compose project 和已退役 service 标签删除旧容器。回退先停止新测活服务，再恢复完整旧版本；不要混用两套写入者。历史业务数据保留，迁移会终止旧测活、恢复和生命周期任务及会话依赖，过期任务租约无法提交。

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
