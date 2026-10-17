# Node Explorer

React／TypeScript 节点浏览器与 Go 后端，实时展示 subs-check 和 Qilan 节点的当前连通性及出口 IP 属性。支持筛选、分页、详情、列设置和深浅主题。

现在由六个独立服务运行：API 查询、来源导入、测活、IP 属性、住宅恢复、节点生命周期。**PostgreSQL 保存业务数据，Redis 分发任务；在用住宅节点默认每 5 分钟检查，自动退役节点仅做一轮恢复核验，合格立即恢复。** 人工禁用不会自动恢复。

完整的模块职责、任务契约、故障恢复、部署与升级步骤见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)，住宅判定规则见 [RESIDENTIAL_RECOVERY.md](RESIDENTIAL_RECOVERY.md)。

## 开发与运行

需要 Node.js 22.13+、Go 1.26+、PostgreSQL 和 Redis。复制 `.env.example` 为 `.env`，填写 PostgreSQL、Redis 及四组专用 Mihomo 地址。

```bash
npm ci
npm run dev
```

开发命令启动 Vite 和 API，打开 http://localhost:5173 。另外分别运行来源和工作进程：

```bash
go -C backend run ./cmd/node-worker -role source
go -C backend run ./cmd/node-worker -role connectivity
go -C backend run ./cmd/node-worker -role ip
go -C backend run ./cmd/node-worker -role residential
go -C backend run ./cmd/node-worker -role lifecycle
```

生产构建使用 `npm run build && npm run build:backend`；`npm start` 启动 API，工作进程用 `backend/node-worker -role <角色>` 独立启动。运行时必须接入 PostgreSQL，不再使用 SQLite。测试夹具仍保留 SQLite，用于验证旧行为。

## 容器部署

复制 `.env.example`，填写已有 PostgreSQL 的连接信息、所在外部 Docker 网络 `POSTGRES_NETWORK` 和宿主订阅目录 `SUBS_CHECK_HOST_DIR`。数据库用户需要建表权限。设置 `release.env` 中 `IMAGE_TAG` 为包含本次模块化实现的版本。

```bash
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d --remove-orphans
bash deploy/compose.sh ps
curl --fail http://127.0.0.1:3001/api/health
curl --fail http://127.0.0.1:3001/api/services
```

默认 `COMPOSE_FILE=compose.yaml:compose.subs-check.yaml`，包含六个应用服务、Redis、四个专用 Mihomo 和上游 subs-check。已有 subs-check 时可只使用 `compose.yaml`。数据库是已有外部服务，不自动新建。只有来源服务挂载订阅目录；所有代理和 Redis 都不发布宿主端口。Web 默认绑定 `127.0.0.1:3001`，可用 `BIND_ADDRESS`／`WEB_PORT` 调整。

旧的 PG／IPPure／巡检 overlay 已删除（它们曾是为旧部署保留的空兼容文件，生产自 v1.4.x 起已不在 `COMPOSE_FILE` 中引用）。升级应停止旧巡检与采集进程，更新 Compose 文件，保留数据库与旧 SQLite 卷；SQLite 历史不会自动迁移。详见模块化升级说明。

`deploy/node-list.service`、`deploy/node-list-update.service` 和 timer 可安装到 systemd。`deploy/update.sh` 会同步更新六个应用服务，健康检查失败时回滚对应版本。`NODE_EXPLORER_AUTO_UPDATE=false` 可关闭应用自动更新。

## 来源与当前状态

来源服务监听 `SUBS_CHECK_DIR` 下 `config/cache/config/results.json`、`output/all.yaml` 和 `output/base64.txt`，防抖导入，并周期兜底。`RESULTS_FILE` 可覆盖结果文件路径。来源批次按检测内容去重；同步游标持久化，仅在批次新增或修改时更新连接注册表，不在空闲时遍历全部批次。

Qilan 来源在 `COMPOSE_FILE` 追加 `:compose.qilan.yaml`，设置 `QILAN_SESSION_HOST_FILE` 为宿主会话文件；挂载到来源服务，文件只含 session Cookie 值。可设置 `QILAN_REFRESH_INTERVAL_SECONDS`（默认 900，至少 60）、`QILAN_USER_AGENT`。非容器运行支持 `QILAN_SESSION` 或 `QILAN_SESSION_FILE`。`FEISHU_WEBHOOK_URL` 是可选的会话过期／恢复通知配置。Cookie 和节点凭据不进入公开 API。

历史快照保存完整 `rawConfig` 或分享链接，作为后续检测的连接依据。同一连接跨批次按配置身份合并，当前连通性、出口和 IP 属性按身份保存；页面每次请求直接关联这些当前记录，在 SQL 中筛选、排序和分页，不读取来源批次的完整内容。数据库备份包含连接凭据，按私有订阅管理。

首页展示保留期内所有未退役连接。PostgreSQL 默认保留 7 天快照，`SNAPSHOT_RETENTION_DAYS` 设为负值禁用自动清理；没有有效源快照的连接不会被恢复。人工退役由维护操作写入 `node_explorer_retired_nodes`；自动恢复只匹配已知的自动退役原因和原退役时间。

## 调度与展示

测活只检查 GitHub 连通性。两次失败且宿主直连、另一个近期可用节点的代理对照均成功，才提交自动退役建议；对照不足、配置不支持、控制器故障不会直接退役。IP 服务独立读取出口，按真实出口 IP 合并属性查询，不使用节点服务器地址替代出口。

在用住宅节点测活／出口检查默认 300 秒，普通节点 1800 秒。自动退役节点每次退役只核验一轮，持久化结果后不再重复排期。住宅和普通队列各有专用工作槽位及代理实例；风险接口请求只使用普通 IP 代理。分类与风险成功结果 24 小时后刷新，失败有独立重试。分类按原始来源时间判断有效性，风险接口失败不阻塞其他来源分类与住宅恢复。

默认查询 IPPure、IPinfo、IP-API、IPregistry、AbuseIPDB、ipapi.is、IP2Location。每家最多一票，使用类型优先、公司类型后备，失败或无有效类型时弃权。最高票并列时，在并列项中依次优先住宅 IP、商业宽带，剩余平票判为机房 IP；全部弃权为类型未知。来源详情与独立时间保存在 `classification` 中。

页面默认“住宅 IP 优先”，要求近期可用、出口检查新鲜且分类有效；用户选择其他排序后沿用其选择。页面自动刷新，详情分别展示测活、出口检查和属性时间。`/api/services` 展示工作进程状态与队列积压。

## API 与后端

- `/api/results?page=1&limit=50`：实时查询当前节点并分页，默认住宅优先；支持 `sort=preferred` 或速度、名称等显式排序。
- `/api/results`：实时查询全部当前节点。历史接口已移除，历史路径返回 404，snapshot 参数返回 400。
- `/api/services`：工作进程心跳、队列等待、执行数量及保留期内恢复数。
- `/api/health`：HTTP API 存活。worker 使用 `node-worker -role <角色> -health`。

后端主要目录：`internal/service` 为工作进程和事务边界，`internal/taskqueue` 为 Redis 引用队列，`internal/store/postgres` 为业务存储和当前状态 SQL 查询，`internal/domain` 为身份、分类与排序模型，`internal/transport` 为 Native／Mihomo 隔离通道。`cmd/node-explorer` 只提供 API，`cmd/node-worker` 按角色启动独立服务（source／connectivity／ip／residential／lifecycle）。切换期的一次性分析命令（idcheck/idscan/linkdiff/sortchk）与旧 patrol 入口已完成使命并删除。

`legacy/node-server/` 已归档，仅作为兼容行为基准及旧数据维护参考，不参与运行时调度。`npm test` 运行归档行为测试；`npm run test:backend` 验证 Go。集成测试使用显式 `NODE_EXPLORER_TEST_PG`／`NODE_EXPLORER_TEST_REDIS`，不自动连接生产环境。

GitHub Actions 执行构建、Go race 测试、归档行为测试，并验证 API 和独立生命周期 worker 镜像。容器只包含 Go 二进制和前端资源。发布不会自动执行当前工作区部署。

### 全库出口 IP 属性清洗

`ip-type-clean` 覆盖 `node_explorer_ippure_results` 中所有有效出口 IP，包括历史批次和已退役节点；按 IP 去重，不使用节点服务器地址替代出口。它不探测节点当前出口，也不覆盖 IPPure 风险分数。默认实时请求 IPPure 和六家外部类型来源；通过 `--query-proxy` 指定独立检测代理。每次刷新仅使用本轮结果投票，失败、限流或缺少有效类型的来源弃权，备份只用于回滚。`--refresh=false` 仅用于显式重算现有证据，不属于实时清洗。

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C backend build -o ip-type-clean ./cmd/ip-type-clean
bash deploy/ip-type-clean.sh --query-proxy http://audit-proxy:7890 --ip 51.158.105.199        # 单 IP 预览
bash deploy/ip-type-clean.sh --query-proxy http://audit-proxy:7890 --apply # 全库逐 IP 实时查询全部七家并清洗
```

在部署主机执行包装脚本；它从应用容器安全读取数据库配置，通过临时容器运行，报告写入宿主机 `ip-type-clean-reports/`。可通过 `IP_TYPE_CLEAN_CONTAINER`、`IP_TYPE_CLEAN_BINARY`、`IP_TYPE_CLEAN_REPORT_DIR` 覆盖默认值。二进制也可直接使用 PostgreSQL 标准环境变量运行。

默认不写数据库；`--apply` 在每个 IP 的事务中锁定当前记录，先将原始 JSON 备份至 `node_explorer_ip_type_clean_backup`（按运行 ID、批次、节点索引唯一），再仅更新 `classification` 和 `attributes`。保留节点、检测时间、风险分数、真实出口及其他字段，避免覆盖并发采集结果。失败重跑会重新读取当前数据，数据库互斥锁阻止两个清洗任务同时运行。JSONL 报告记录每个 IP 的证据、来源缺失和更新数量；终端出现 `COMPLETE` 才表示该次全量处理结束。主标签采用独立来源票数最多的类型；最高票并列时，仅在并列项中按住宅 IP、商业宽带的顺序优先判定，其余平票判为机房 IP，全部弃权时显示“类型未知”，各来源原始类型保留在明细中。
