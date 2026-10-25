# 当前服务与执行边界

| 服务 | 职责 | 执行资源 |
| --- | --- | --- |
| node-explorer | PHP/webman API、前端静态文件、只读监控 | PostgreSQL |
| source-ingest | 订阅、Qilan 和公共源导入，注册表同步、监控采样与历史保留 | 文件监听、PostgreSQL |
| mihomo-manager | 共享内核配置、探测租约和常规连通性扫描 | shared-mihomo |
| exit-provision | 实际出口 IP 核验 | manager 的出口通道 |
| exit-attributes | IP 分类、风险查询 | 外部属性服务、manager 风险通道 |
| recovery | 住宅及普通退役节点恢复评估、生命周期治理 | manager 的快速和严格通道 |
| dead-verify | 长期失活节点的受控握手、受保护删除、批量快照压缩 | 容器内私有 Mihomo |
| task-redis | 仅保存任务引用与限流状态 | Redis 持久卷 |

常规探测共享一个 Mihomo；`dead-verify` 使用较新实现中的私有内核，不占共享严格探测通道。旧 `node-cleanup` 调度、TCP 判死、独立 lifecycle 角色、多内核 Compose 与旧 Node API 已移除。SQLite 适配器仅用于测试；人工审计工具仍可使用 Native/Mihomo 传输。

## 重叠实现的保留依据

按实际职责的最近修改判断，保留当前有效行为，同时保留其他服务中新增加的公共保护规则：

| 重叠部分 | 保留 | 删除或收敛 |
| --- | --- | --- |
| API、来源与属性采集 | 当前 PHP API、Go 来源和工作进程 | 旧 Node 服务、未引用的 collector scheduler、patrol |
| 常驻删除核验 | dead-verify（`6ca562c` 引入，`50480bd` 更新配置拒绝处理） | node-cleanup 调度及其 TCP 判死路径 |
| 删除保护 | 最小存活池 500（`caf1c73`） | 删除入口统一调用公共门禁 |
| 来源重入 | tombstone 冷却与年龄继承（`38481be`） | 同一文件来源的重复轮询触发 |
| 生命周期 | recovery 聚合角色 | 独立 lifecycle 进程入口，保留 lifecycle 任务 |
| 共享内核 | mihomo-manager 的配置和租约管理 | 无 manager 的旧多内核降级路径与 Compose |

## 并发和事务

每个常驻 Go 服务持有独立 PostgreSQL 会话锁，重复实例在启动实际工作前退出；所有权连接失效会取消并等待工作结束。manager 仍独占共享内核控制器。不同服务按职责并行运行。

`node_explorer_work` 是持久化投递账本；Redis 仅含任务 ID 与代数。领取和提交检查代数、租约与优先级，过期消息不能重复提交。recovery 继续消费 lifecycle、fast-delay、connectivity 依赖，退出后等待消费者结束。

删除只由 `dead-verify` 发起，复用原事务保护：新近出现/最后成功时间、配置摘要、节点实例、证据时效、执行中探测、更新的连通性观察和最小存活池。删除写入 tombstone，来源冷却及继承年龄逻辑保持。删除事务不逐节点重写快照，由批量压缩回收残余。被门禁拒绝的有效核验记录会提交，避免每批重复核验同一队首。

`LIFECYCLE_DRY_RUN=true` 会阻止生命周期变更、物理删除和快照压缩。核验候选与删除保护共用 `NODE_CLEANUP_MIN_AGE_SECONDS`，默认七天；`NODE_CLEANUP_MIN_POOL` 默认 500。

## 升级

备份数据库、Compose、`.env`、`release.env` 与旧镜像。更新所有应用服务及部署脚本，执行：

```sh
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d
bash deploy/compose.sh ps
```

`deploy/compose.sh up` 会先按当前 Compose project 与 service 标签精确删除旧 `node-cleanup` 容器，避免残留实例继续写入。不要再使用旧多内核 overlay；自定义 `COMPOSE_FILE` 必须使用当前配置。迁移版本 13 将旧清理任务及其依赖置为完成、递增代数并清除租约，只清除对应旧证明和服务心跳；恢复任务及历史业务表保留。回滚必须恢复备份的部署文件及镜像，不能同时运行两套清理服务。

监控 `/api/services` 与前端面板展示 `dead-verify` 心跳；该服务每 20 秒独立刷新心跳，不受单批核验耗时影响。其任务不走 Redis，因此不显示队列计数。`node-worker -role dead-verify -health` 只检查心跳；启动使用 `dead-verify` 命令。

## 本地检查

```sh
npm run build
npm test
npm run test:backend
python3 -m unittest discover -s deploy -p 'test_*.py'
```

集成测试只连接显式指定的测试库，每项 PostgreSQL 测试使用独立 schema：

```sh
NODE_EXPLORER_TEST_PG=postgresql://user:password@127.0.0.1/test_db \
NODE_EXPLORER_TEST_REDIS=redis://127.0.0.1:6379/15 \
go -C backend test -race ./...
```

历史身份契约固定在 `backend/internal/domain/testdata/identity-contract.json`；较新的 Go 分享链接规范化规则由 sharelink 测试覆盖。API 参数、凭证剥离、过滤排序分页契约由 `webman/tests` 验证。
