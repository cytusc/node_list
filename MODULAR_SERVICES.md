# 模块化服务

同一个 Go 仓库和镜像提供六个独立进程。Redis 分发任务引用，PostgreSQL 保存快照、业务状态、任务投递记录和恢复审计。工作进程通过数据库事实与异步任务协作，不直接调用其他进程。

| 服务      | 命令                             | 唯一职责                                            |
| --------- | -------------------------------- | --------------------------------------------------- |
| API／投影 | `node-explorer`                  | 查询、历史分页、当前节点投影；不执行网络检测        |
| 来源导入  | `node-worker -role source`       | subs-check 文件监听、Qilan、快照入库、连接注册      |
| 测活      | `node-worker -role connectivity` | 连接 GitHub，记录连通性；确认失败后提交状态变更建议 |
| IP 属性   | `node-worker -role ip`           | 获取节点真实出口；按出口 IP 共享分类、ASN、风险属性 |
| 住宅恢复  | `node-worker -role residential`  | 优先复检自动退役节点；一次完整验证合格就请求恢复    |
| 生命周期  | `node-worker -role lifecycle`    | 校验最新事实，统一执行自动退役／恢复并记录审计      |

测活不查询 IP 属性；IP 属性查询失败不等于节点失效。IPPure 风险接口与其他六家分类来源分别排队，风险查询失败不阻塞住宅判断。历史批次保持原样，当前状态按连接身份附加，出口变化不会沿用旧 IP 的属性。

## 调度

- 住宅节点测活、出口检测，以及历史住宅节点的自动恢复复检：默认 **300 秒**。
- 普通节点测活、出口检测，以及其他自动退役候选复检：默认 **1800 秒**。
- 每个节点按自己的最近检查时间排期，任务完成后不等待其他节点的一整轮。
- 分类与风险成功结果按 IP 缓存并在 24 小时后刷新；失败默认 30 分钟后重试，住宅优先任务失败 5 分钟后重试。分类证据保留每家来源的原始时间。
- 住宅／普通任务有独立 Redis 通道和固定工作槽位。测活和出口服务各使用两个专用 Mihomo 实例，慢风险请求只占用普通 IP 代理。
- 来源配额通过 PostgreSQL 协调，至少间隔 1.5 秒，保存 `Retry-After`、IP-API 配额冷却时间，重启不清空冷却。

## 一次成功即恢复

每次复检都要求：当前退役记录属于自动退役、本轮 GitHub 连通、ipify 和 AWS HTTPS 回显的公网 IPv4 一致且区别于宿主出口、同 IP 的当前版本分类为住宅。分类之后再次经该节点核对出口。

全部通过即提交高优先级恢复任务，不累计连续成功次数，不等待第二轮或 30 分钟。生命周期服务在事务中再次核对出口版本、近期连通性、分类证据以及退役记录的 identity／时间／原因。人工禁用、出口变化、证据过期和已清理的原始快照都不能恢复。失败的本轮结束后按节点类别再次排期。

API 每秒检查投影变化，打开的页面每 5 秒自动更新。默认“住宅 IP 优先”要求连通性和出口检查在 10 分钟内、分类在有效期内；用户显式选择速度、名称等排序时使用其选择。

## 队列与故障恢复

`node_explorer_work` 是 PostgreSQL 持久化投递记录；Redis sorted set 仅含 `任务 ID:代数`，不携带节点凭据。业务结果和下游任务在同一 PostgreSQL 事务中写入，后台每秒将待投递记录补发到 Redis。

Redis 丢失或重启不会丢失任务。消费者通过数据库 CAS 获取 3 分钟租约，执行上限 2 分钟；旧租约不能提交结果，重复消息不能重复恢复。任务优先级提升后，普通通道遗留消息也不能领取住宅任务。跨库操作无需分布式事务。

完整源配置仍只存在 PostgreSQL 与探测进程内；公开 API 会去除连接凭据。恢复会话与已完成一次性任务保留 2 天，生命周期审计保留 30 天，正在排期的任务不清理。

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

`compose.yaml` 已包含全部 worker、四个专用 Mihomo 和持久化 Redis，不暴露它们的宿主端口；只有来源服务挂载 subs-check 文件。旧的 `compose.pgsql.yaml`、`compose.ippure.yaml`、`compose.patrol.yaml`、`compose.patrol.pgsql.yaml` 成为空兼容 overlay，可从 `COMPOSE_FILE` 移除。`compose.qilan.yaml` 现在挂载到来源服务。现有更新脚本会一起更新／回滚六个应用服务。

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

`/api/services` 返回六个服务的心跳、待处理／执行中任务、最久等待时间和保留期内的恢复数；未启动或心跳超过 90 秒的服务算异常。`node-worker -role <角色> -health` 可单独检查。`/api/health` 仍表示 HTTP API 存活。

集成测试只读取显式测试变量，并为 PostgreSQL 每个测试创建独立 schema：

```bash
NODE_EXPLORER_TEST_PG=postgresql://user:password@127.0.0.1/test_db \
NODE_EXPLORER_TEST_REDIS=redis://127.0.0.1:6379/15 \
go -C backend test -race ./...
```

`internal/store/sqlite` 仅保留为旧行为测试夹具，不进入 API／worker 二进制的依赖。
