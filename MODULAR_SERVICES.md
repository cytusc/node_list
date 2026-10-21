# 模块化服务

同一个仓库和镜像提供七个独立进程：API 查询是 PHP/webman（Workerman），其余六个是 Go `node-worker`。Redis 分发任务引用，PostgreSQL 保存快照、业务状态、任务投递记录和恢复审计。工作进程通过数据库事实与异步任务协作，不直接调用其他进程。

| 服务      | 命令                             | 唯一职责                                            |
| --------- | -------------------------------- | --------------------------------------------------- |
| API 查询 | `php start.php start`（镜像默认） | 实时查询当前状态、SQL 筛选排序和分页；不执行网络检测        |
| 来源导入  | `node-worker -role source`       | subs-check 文件监听、Qilan、快照入库、连接注册      |
| 测活      | `node-worker -role connectivity` | 连接 GitHub，记录连通性；确认失败后提交状态变更建议 |
| IP 属性   | `node-worker -role ip`           | 获取节点真实出口；按出口 IP 共享分类、ASN、风险属性 |
| 住宅恢复  | `node-worker -role residential`  | 优先复检自动退役节点；一次完整验证合格就请求恢复    |
| 非住宅恢复 | `node-worker -role normal-recovery` | 低频复检非住宅与机房节点，验证可用后请求恢复 |
| 生命周期  | `node-worker -role lifecycle`    | 校验最新事实，统一执行自动退役／恢复并记录审计      |

API 查询的实现在 `webman/`（详见该目录 README）：读取路径与采集器共用 `node_explorer_live_*` 视图，只读不写业务表；唯一写入是 `node_explorer_service_status` 的 `api` 行心跳（惰性补写，见下文）。容器里 `runtime/` 必须挂 tmpfs（`read_only: true` 下 Workerman 要写 pid/日志/文件缓存）。

测活不查询 IP 属性；IP 属性查询失败不等于节点失效。IPPure 风险接口与其他六家分类来源分别排队，风险查询失败不阻塞住宅判断。历史批次保持原样，当前状态按连接身份附加，出口变化不会沿用旧 IP 的属性。

## 调度

- 在用住宅节点测活、出口检测：默认 **300 秒**。历史住宅自动退役后的临时失败按 5 分钟、15 分钟、1 小时、6 小时退避复检。
- 普通在用节点测活、出口检测：默认 **1800 秒**。
- 非住宅恢复独立进程与 `normal-recovery` 队列：默认 **21600 秒（6 小时）**，可用 `NORMAL_RECOVERY_INTERVAL_SECONDS` 调整。首次至少等待退役后一个间隔，历史积压额外错开最多 5 分钟；失败后从本轮完成时间计算下一次。Compose 每 30 秒补充排期，每次最多 256 条，完整核验并发由 `NORMAL_RECOVERY_CONCURRENCY` 统一控制（默认 2，消费、投递与数据库准入一致；各副本必须同值），全部探测使用普通通道，不占住宅恢复额度。
- 每个节点按自己的最近检查时间排期，任务完成后不等待其他节点的一整轮。
- 住宅恢复先按真实出口 ASN 过滤明确的云／CDN／托管网络，批量结束已知机房候选；完成核验的退役记录长期保留标记；明确机房或非住宅的结果交给低频服务。
- 调度查询先排除不属于本服务的节点；恢复调度只读取尚未建立恢复任务的自动退役节点，避免每 5 秒重复读取全部连接并插入已存在的任务。
- 分类与风险成功结果按 IP 缓存并在 24 小时后刷新；失败默认 30 分钟后重试，住宅优先任务失败 5 分钟后重试。分类证据保留每家来源的原始时间。
单 Mihomo 架构及升级步骤见 [SINGLE_MIHOMO.md](SINGLE_MIHOMO.md)。快速测活 32 并发，住宅等待时预留 8 路；严格 GitHub 核验 8、出口 8、风险 2。所有任务使用管理服务租约，其他 worker 无控制器写权限。旧并发环境变量只供回滚配置使用。
- 来源配额通过 PostgreSQL 协调，至少间隔 1.5 秒，保存 `Retry-After`、IP-API 配额冷却时间，重启不清空冷却。

## 一次成功即恢复

住宅复检要求：当前退役记录属于自动退役、本轮 GitHub 连通、ipify 和 AWS HTTPS 回显的公网 IPv4 一致且区别于宿主出口、同 IP 的当前版本分类为住宅。分类之后再次经该节点核对出口。

全部通过即提交高优先级恢复任务，不累计连续成功次数，不等待第二轮或 30 分钟。生命周期服务在事务中再次核对出口版本、近期连通性、分类证据以及退役记录的 identity／时间／原因。人工禁用、出口变化、证据过期和已清理的原始快照都不能恢复。每轮保存结果、出口 IP、累计次数和最近 16 次核验历史。历史住宅的临时探测失败、出口变化、分类未确认或超时，按 5 分钟、15 分钟、1 小时、6 小时退避复测，提升任务代数并重新取得全部证据。人工禁用和已失效来源不重试；明确非住宅或机房节点转入低频恢复。退役前要求 3 轮对照确认失败、每轮至少间隔 2 分钟，成功或对照不足清零；失败候选按 2 分钟复测。

非住宅复检复用相同的 GitHub 连通性和两轮真实出口核验，但不要求住宅分类，也不因机房 ASN 跳过；分类和风险查询异步刷新。生命周期服务按恢复模式复核，仍检查当前退役记录、来源、连通性、出口版本和近期二次出口证明。

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
curl --fail http://127.0.0.1:3001/api/health
```

`compose.yaml` 已包含全部 worker、十六个专用 Mihomo 和持久化 Redis，不暴露它们的宿主端口；只有来源服务挂载 subs-check 文件。旧的 `compose.pgsql.yaml`、`compose.ippure.yaml`、`compose.patrol.yaml`、`compose.patrol.pgsql.yaml` 空兼容 overlay 已从仓库删除；仍在 `COMPOSE_FILE` 中引用它们的旧部署需先移除这些引用再升级。`compose.qilan.yaml` 现在挂载到来源服务。现有更新脚本会一起更新／回滚七个应用服务。

没有 PostgreSQL 配置时启动会失败。运行时不再支持 SQLite，也不自动搬迁 SQLite 历史。旧 SQLite 卷不会被删除；需要保留旧历史时应单独迁移或继续保存备份。

首次启动自动创建新表和索引，保留原快照、IPPure 与退役表。新工作进程先从快照注册连接，再补测当前状态。可暂时设置 `LIFECYCLE_DRY_RUN=true` 查看建议审计，再设回 `false`；预演不修改退役记录。

## 本地运行和检查

```bash
npm run build:worker
npm run dev
./node-worker -role source
./node-worker -role connectivity
./node-worker -role ip
./node-worker -role residential
./node-worker -role normal-recovery
./node-worker -role lifecycle
```

`npm run dev` 会同时拉起 Vite 与 PHP 接口（`npm run dev:api` = `php webman/start.php start`，首次需 `cp webman/.env.example webman/.env` 并 `composer install`）。接口的离线测试：`php webman/tests/run.php`；连库集成测试见 `webman/README.md`。

各命令分别在终端或进程管理器中运行。填写 `.env.example` 的 Redis、PostgreSQL 和四组专用控制器配置；住宅与普通通道不能共享控制器，不同服务的控制器所有权也有数据库锁保护。探测服务需要 `PGPOOL_MAX>=2`，默认每个服务最多 8 个连接（控制器所有权常驻 1 个，剩余供任务租约与写入使用）。慢车道（risk/attributes/lifecycle/recovery）消费者默认每通道 1 个，可用 `<角色前缀>_<KIND>_CONCURRENCY` 或通用 `<KIND>_CONCURRENCY` 调整；ip-service 的 Compose 默认 `IP_SERVICE_RISK_CONCURRENCY=2`。

服务状态面板及 `/api/services` 已移除。结果接口仍查询 IP 采集状态和来源告警。探测 worker 正常心跳每 10 秒写入，错误状态变化时立即写入；控制器所有权连接仍每秒检查。`node-worker -role <角色> -health` 可单独检查。`/api/health` 表示 HTTP API 存活。

数据库结构按版本在一个事务中迁移，七个服务共用迁移锁，已应用版本在后续启动时跳过 DDL。当前实时读取无需观察结果触发器；来源批次及退役状态仍保留增量追踪。

集成测试只读取显式测试变量，并为 PostgreSQL 每个测试创建独立 schema：

```bash
NODE_EXPLORER_TEST_PG=postgresql://user:password@127.0.0.1/test_db \
NODE_EXPLORER_TEST_REDIS=redis://127.0.0.1:6379/15 \
go -C backend test -race ./...
```

`internal/store/sqlite` 仅保留为旧行为测试夹具，不进入 API／worker 二进制的依赖。

节点成功时间持久化与独立七天失效清理服务见 [NODE_CLEANUP.md](NODE_CLEANUP.md)。

非住宅恢复先完成本轮 GitHub 连通性检测，再创建两轮强出口核验；连通性失败不消耗出口探测容量。12 秒检测超时、10 秒后第二次连通性复测、对照验证、ipify/AWS 一致性、宿主出口排除、最终出口版本与事务校验保持不变。普通测活通道使用二十个独立 Mihomo 实例和二十个工作协程；生产恢复并发可分档从 4 提至 8、16，为日常巡检保留四个槽位。恢复默认值仍为 2，生产按容量显式配置。监测完整核验实际吞吐、探测到期积压、证明时效和成功节点巡检延迟；扩大恢复额度时必须同步评估探测池容量，不能只增加协程而复用同一控制器。

服务看板的累计核验包含重新排期的历史结果；执行中包含等待前置检测的完整核验会话；近一小时吞吐按最近 16 次核验历史统计（极短自定义间隔可能截断）；积压预计仅按近期吞吐估算当前到期工作，不包含未来重试或新增节点。并发扩容刚完成时，一小时滚动速率会逐渐反映新容量。
