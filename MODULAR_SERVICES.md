# 模块化服务

同一个 Go 仓库和镜像提供六个独立进程。Redis 分发任务引用，PostgreSQL 保存快照、业务状态、任务投递记录和恢复审计。工作进程通过数据库事实与异步任务协作，不直接调用其他进程。

| 服务      | 命令                             | 唯一职责                                            |
| --------- | -------------------------------- | --------------------------------------------------- |
| API 查询 | `node-explorer`                  | 实时查询当前状态、SQL 筛选排序和分页；不执行网络检测        |
| 来源导入  | `node-worker -role source`       | subs-check 文件监听、Qilan、快照入库、连接注册      |
| 测活      | `node-worker -role connectivity` | 连接 GitHub，记录连通性；确认失败后提交状态变更建议 |
| IP 属性   | `node-worker -role ip`           | 获取节点真实出口；按出口 IP 共享分类、ASN、风险属性 |
| 住宅恢复  | `node-worker -role residential`  | 优先复检自动退役节点；一次完整验证合格就请求恢复    |
| 生命周期  | `node-worker -role lifecycle`    | 校验最新事实，统一执行自动退役／恢复并记录审计      |

测活不查询 IP 属性；IP 属性查询失败不等于节点失效。IPPure 风险接口与其他六家分类来源分别排队，风险查询失败不阻塞住宅判断。历史批次保持原样，当前状态按连接身份附加，出口变化不会沿用旧 IP 的属性。

## 调度

- 在用住宅节点测活、出口检测：默认 **300 秒**。自动退役节点每次退役只做一轮恢复核验。
- 普通在用节点测活、出口检测：默认 **1800 秒**。
- 每个节点按自己的最近检查时间排期，任务完成后不等待其他节点的一整轮。
- 住宅恢复先按真实出口 ASN 过滤明确的云／CDN／托管网络，批量结束已知机房候选；完成核验的退役记录长期保留标记，不重复入队。
- 调度查询先排除不属于本服务的节点；恢复调度只读取尚未建立恢复任务的自动退役节点，避免每 5 秒重复读取全部连接并插入已存在的任务。
- 分类与风险成功结果按 IP 缓存并在 24 小时后刷新；失败默认 30 分钟后重试，住宅优先任务失败 5 分钟后重试。分类证据保留每家来源的原始时间。
- 住宅／普通任务有独立 Redis 通道和固定工作槽位。恢复协调器每个通道最多允许 2 个完整验证在途，等待队列不会提前启动验证计时或创建探测任务；已准入的依赖重试优先投递。测活和出口服务各使用八个专用 Mihomo 实例，每条住宅／普通通道各四个，匹配 Compose 默认每通道四个工作协程；实例租约覆盖整轮检测，避免协程都显示执行中却排队等同一实例。慢风险请求只占用普通 IP 代理。独立的 `CONNECTIVITY_HIGH_CONCURRENCY`／`CONNECTIVITY_NORMAL_CONCURRENCY` 和 `IP_SERVICE_HIGH_CONCURRENCY`／`IP_SERVICE_NORMAL_CONCURRENCY` 分别控制两个服务的通道并发；单独启动进程时未设置则沿用通用并发配置。
- 来源配额通过 PostgreSQL 协调，至少间隔 1.5 秒，保存 `Retry-After`、IP-API 配额冷却时间，重启不清空冷却。

## 一次成功即恢复

每次复检都要求：当前退役记录属于自动退役、本轮 GitHub 连通、ipify 和 AWS HTTPS 回显的公网 IPv4 一致且区别于宿主出口、同 IP 的当前版本分类为住宅。分类之后再次经该节点核对出口。

全部通过即提交高优先级恢复任务，不累计连续成功次数，不等待第二轮或 30 分钟。生命周期服务在事务中再次核对出口版本、近期连通性、分类证据以及退役记录的 identity／时间／原因。人工禁用、出口变化、证据过期和已清理的原始快照都不能恢复。本轮通过、未通过或超时后保存结果和出口 IP 并结束，不再周期排期；相同退役记录只核验一轮。

API 每次请求直接查询当前注册表、连通性、出口和 IP 属性，不加载批次内容，也不运行后台全量投影。分页请求先用筛选、统计及排序字段选出当前页，再生成该页完整节点 JSON；统计与页面读取同一条 SQL 的数据库快照。打开的页面每 5 秒自动更新；隐藏页面暂停结果和服务状态轮询，请求未完成时不叠加请求。来源服务使用持久化批次游标和批量写入，空闲时不重复聚合或更新全部连接。默认“住宅 IP 优先”要求连通性和出口检查在 10 分钟内、分类在有效期内；用户显式选择速度、名称等排序时使用其选择。

## 队列与故障恢复

`node_explorer_work` 是 PostgreSQL 持久化投递记录；Redis sorted set 仅含 `任务 ID:代数`，不携带节点凭据。业务结果和下游任务在同一 PostgreSQL 事务中写入。后台每秒扫描到期或即将到期的任务，仅投递新任务、代数／时间／优先级变化的任务，以及超过 30 秒未发送的补偿任务。Redis 使用 pipeline 批量发送，整批成功后才批量登记；未变化任务不会每秒重复写入 Redis。周期调度每 250 条批量写入，先筛掉无需变化的记录，运行中的任务不覆盖。

Redis 丢失或重启不会丢失任务。未变化的排队任务按 30 秒间隔补偿；已领取任务在 3 分钟租约过期后可重新投递。消费者通过数据库 CAS 获取租约，执行上限 2 分钟；旧租约不能提交结果，重复消息不能重复恢复。任务优先级提升后，普通通道遗留消息也不能领取住宅任务。空队列轮询从 250 毫秒退避至最多 2 秒，发现消息即重置；因此空闲后的任务领取可能额外等待约 2 秒。跨库操作无需分布式事务。

完整源配置仍只存在 PostgreSQL 与探测进程内；公开 API 会去除连接凭据。恢复会话与其他已完成一次性任务保留 2 天，已完成恢复任务长期保留用于去重，生命周期审计保留 30 天，正在排期的任务不清理。

## 部署与升级

1. 备份现有 PostgreSQL；保留 `.env`、`release.env` 和订阅目录。
2. 更新 `compose.yaml`、其他使用中的 overlay、`Dockerfile` 和 `deploy/`。使用包含本次变更的镜像版本；旧版本不包含 `node-worker`。
3. 按 `.env.example` 配置 `PGHOST`、`PGPORT`、`PGDATABASE`、`PGUSER`、`PGPASSWORD`、`POSTGRES_NETWORK` 和 `SUBS_CHECK_HOST_DIR`。PostgreSQL 必须已在这个外部 Docker 网络中运行。
4. 停止旧的 `github-patrol` 和旧采集进程，再启动新服务，避免两套逻辑并行写状态。

```bash
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d --remove-orphans
bash deploy/compose.sh ps
curl --fail http://127.0.0.1:3001/api/services
```

`compose.yaml` 已包含全部 worker、十六个专用 Mihomo 和持久化 Redis，不暴露它们的宿主端口；只有来源服务挂载 subs-check 文件。旧的 `compose.pgsql.yaml`、`compose.ippure.yaml`、`compose.patrol.yaml`、`compose.patrol.pgsql.yaml` 空兼容 overlay 已从仓库删除；仍在 `COMPOSE_FILE` 中引用它们的旧部署需先移除这些引用再升级。`compose.qilan.yaml` 现在挂载到来源服务。现有更新脚本会一起更新／回滚六个应用服务。

没有 PostgreSQL 配置时启动会失败。运行时不再支持 SQLite，也不自动搬迁 SQLite 历史。旧 SQLite 卷不会被删除；需要保留旧历史时应单独迁移或继续保存备份。

首次启动自动创建新表和索引，保留原快照、IPPure 与退役表。新工作进程先从快照注册连接，再补测当前状态。可暂时设置 `LIFECYCLE_DRY_RUN=true` 查看建议审计，再设回 `false`；预演不修改退役记录。

## 本地运行和检查

```bash
npm run build:backend
npm run dev
./backend/node-worker -role source
./backend/node-worker -role connectivity
./backend/node-worker -role ip
./backend/node-worker -role residential
./backend/node-worker -role lifecycle
```

各命令分别在终端或进程管理器中运行。填写 `.env.example` 的 Redis、PostgreSQL 和四组专用控制器配置；住宅与普通通道不能共享控制器，不同服务的控制器所有权也有数据库锁保护。探测服务需要 `PGPOOL_MAX>=2`，默认每个服务最多 6 个连接。

`/api/services` 返回六个服务的心跳、已到期的待处理任务、未到期的定时等待任务、执行中任务、最久逾期等待时间、已完成核验数、机房 ASN 跳过数和保留期内的恢复数；未启动或心跳超过 90 秒的服务算异常。探测 worker 正常心跳每 10 秒写入，错误状态变化时立即写入；控制器所有权连接仍每秒检查。结果接口仅查询 IP 服务状态，服务列表按需汇总六个服务的任务计数。`node-worker -role <角色> -health` 可单独检查。`/api/health` 仍表示 HTTP API 存活。

数据库结构按版本在一个事务中迁移，六个服务共用迁移锁，已应用版本在后续启动时跳过 DDL。当前实时读取无需观察结果触发器；来源批次及退役状态仍保留增量追踪。

集成测试只读取显式测试变量，并为 PostgreSQL 每个测试创建独立 schema：

```bash
NODE_EXPLORER_TEST_PG=postgresql://user:password@127.0.0.1/test_db \
NODE_EXPLORER_TEST_REDIS=redis://127.0.0.1:6379/15 \
go -C backend test -race ./...
```

`internal/store/sqlite` 仅保留为旧行为测试夹具，不进入 API／worker 二进制的依赖。
