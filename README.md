# Node Explorer

React／TypeScript 节点浏览器与 PHP（webman）接口层，实时展示 subs-check 和 Qilan 节点的当前连通性及出口 IP 属性。支持筛选、分页、详情、列设置和深浅主题。

现在由六个应用服务运行：API、来源导入、独立节点测活、出口探测、IP 属性和 Mihomo 管理。**只有 `node-liveness` 测试并维护节点是否可用：兼容节点优先 Xray，其他节点及 Xray 失败回退 Mihomo。** 测活只依赖 PostgreSQL 和私有内核，不依赖 Redis、共享 manager 或其他业务服务，不执行恢复、退役和删除。

服务边界与升级见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)，测活实现见 [独立节点测活](docs/service-refactoring/10-node-liveness.md)。

## 开发与运行

需要 Node.js 22.13+、Go 1.26+、PHP 8.1+（含 pdo_pgsql／pcntl／sockets／mbstring 扩展）、PostgreSQL 和 Redis。复制 `.env.example` 为 `.env`，填写 PostgreSQL、Redis 及共享 Mihomo、manager 地址；接口另需 `cp webman/.env.example webman/.env` 并 `composer install`。

```bash
npm ci
composer install --working-dir=webman
npm run dev
```

开发命令启动 Vite 和 API，打开 http://localhost:5173 。另外分别运行来源和工作进程（测活需安装 Xray / Mihomo 并导出 PG* 环境变量）：

```bash
go -C backend run ./cmd/node-worker -role source
go -C backend run ./cmd/node-worker -role mihomo-manager
go -C backend run ./cmd/node-worker -role exit-provision
go -C backend run ./cmd/node-worker -role exit-attributes
go -C backend run ./cmd/node-liveness
```

生产构建使用 `npm run build && npm run build:worker`；接口用 `npm start`（= `php webman/start.php start`），工作进程用 `node-worker -role <角色>` 独立启动。运行时必须接入 PostgreSQL，不再使用 SQLite。测试夹具仍保留 SQLite，用于验证旧行为。接口测试：`npm run test:api`（离线断言，测试另需 pdo_sqlite 扩展）+ `php webman/tests/integration.php`（连库契约，见 `webman/README.md`）。

## 容器部署

复制 `.env.example`，填写已有 PostgreSQL 的连接信息、所在外部 Docker 网络 `POSTGRES_NETWORK` 和宿主订阅目录 `SUBS_CHECK_HOST_DIR`。数据库用户需要建表权限。设置 `release.env` 中 `IMAGE_TAG` 为包含本次模块化实现的版本。

```bash
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d --remove-orphans
bash deploy/compose.sh ps
curl --fail http://127.0.0.1:3001/api/health
```

默认 `COMPOSE_FILE=compose.yaml:compose.subs-check.yaml`，包含六个应用服务、Redis、一个共享 Mihomo 和上游 subs-check。已有 subs-check 时可只使用 `compose.yaml`。数据库是已有外部服务，不自动新建。只有来源服务挂载订阅目录；所有代理和 Redis 都不发布宿主端口。Web 默认绑定 `127.0.0.1:3001`，可用 `BIND_ADDRESS`／`WEB_PORT` 调整。

旧的 PG／IPPure／巡检 overlay 已删除（它们曾是为旧部署保留的空兼容文件，生产自 v1.4.x 起已不在 `COMPOSE_FILE` 中引用）。升级应停止旧巡检与采集进程，更新 Compose 文件，保留数据库与旧 SQLite 卷；SQLite 历史不会自动迁移。详见模块化升级说明。

`deploy/node-list.service`、`deploy/node-list-update.service` 和 timer 可安装到 systemd。`deploy/update.sh` 会同步更新六个应用服务，健康检查失败时回滚对应版本。`NODE_EXPLORER_AUTO_UPDATE=false` 可关闭应用自动更新。

## 来源与当前状态

来源只导入配置，新节点等待测活服务验证，重复导入不覆盖可用状态。测活周期默认 30 分钟，异常默认 60 秒重试，间隔从每个节点完成后计算。出口与属性服务读取可用节点，各自维护出口、分类和风险事实；它们的失败不会把节点判为不可用。

IP 分类使用多家供应商独立投票，保留来源、时间和明细。页面默认住宅优先，详情分别展示测活、出口和属性时间。`/services` 和 `/api/services` 展示六个应用服务心跳、测活两组队列和在途数、出口 / 属性 / 风险队列以及小时采样。心跳和队列采样不输出节点配置或任务载荷。

## API 与后端

- `/api/results?page=1&limit=50`：实时查询当前节点并分页，默认住宅优先；支持 `sort=preferred` 或速度、名称等显式排序。
- `/api/results`：实时查询全部当前节点。历史接口已移除，历史路径返回 404，snapshot 参数返回 400。
- `/api/health`：HTTP API 存活。worker 使用 `node-worker -role <角色> -health`。

后端 `internal/liveness` 与 `cmd/node-liveness` 为独立测活服务；`internal/service` 和 `cmd/node-worker` 提供 source、exit-provision、exit-attributes、mihomo-manager。`internal/taskqueue` 为出口和属性提供 Redis 引用队列，`internal/store/postgres` 为业务存储，`internal/domain` 为身份、分享链接和分类模型。HTTP 接口在 `webman/`。

旧 Node 运行时与重复调度器已移除；身份兼容样例保留在 Go 测试夹具中。`npm test` 运行当前 PHP API 离线契约测试；`npm run test:backend` 验证 Go。集成测试使用显式 `NODE_EXPLORER_TEST_PG`／`NODE_EXPLORER_TEST_REDIS`，不自动连接生产环境。

GitHub Actions 执行构建、Go race 测试、webman 离线与连库契约测试、归档行为测试，并验证 API 和独立测活服务镜像。容器包含 PHP 接口运行时（含前端静态资源）与 Go worker 二进制。发布不会自动执行当前工作区部署。

发布流程中，测试与镜像构建并行执行；`linux/amd64` 和 `linux/arm64` 分别使用 GitHub 原生 runner 构建，不经过 QEMU。每个架构都执行 API、node-liveness 和 Mihomo manager 冒烟检查，全部测试通过后才合并双架构镜像清单、更新版本／`latest` 标签并创建 Release。前端产物通过 Actions artifact 传给发布任务，无需再次编译。

Docker 构建缓存保存到同一 GHCR 镜像的 `buildcache-amd64`／`buildcache-arm64` 标签，供不同版本 tag 复用；这些标签仅用于缓存，不用于部署。首次运行或依赖变更需要重建相应缓存层，后续发布可复用 PHP 扩展、依赖安装等结果。工作流沿用 `GITHUB_TOKEN` 的 `packages: write` 权限，无需新增密钥。

### 全库出口 IP 属性清洗

`ip-type-clean` 覆盖 `node_explorer_ippure_results` 中所有有效出口 IP，包括历史批次和已退役节点；按 IP 去重，不使用节点服务器地址替代出口。它不探测节点当前出口，也不覆盖 IPPure 风险分数。默认实时请求 IPPure 和六家外部类型来源；通过 `--query-proxy` 指定独立检测代理。每次刷新仅使用本轮结果投票，失败、限流或缺少有效类型的来源弃权，备份只用于回滚。`--refresh=false` 仅用于显式重算现有证据，不属于实时清洗。

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C backend build -o ip-type-clean ./cmd/ip-type-clean
bash deploy/ip-type-clean.sh --query-proxy http://audit-proxy:7890 --ip 51.158.105.199        # 单 IP 预览
bash deploy/ip-type-clean.sh --query-proxy http://audit-proxy:7890 --apply # 全库逐 IP 实时查询全部七家并清洗
```

在部署主机执行包装脚本；它从应用容器安全读取数据库配置，通过临时容器运行，报告写入宿主机 `ip-type-clean-reports/`。可通过 `IP_TYPE_CLEAN_CONTAINER`、`IP_TYPE_CLEAN_BINARY`、`IP_TYPE_CLEAN_REPORT_DIR` 覆盖默认值。二进制也可直接使用 PostgreSQL 标准环境变量运行。

默认不写数据库；`--apply` 在每个 IP 的事务中锁定当前记录，先将原始 JSON 备份至 `node_explorer_ip_type_clean_backup`（按运行 ID、批次、节点索引唯一），再仅更新 `classification` 和 `attributes`。保留节点、检测时间、风险分数、真实出口及其他字段，避免覆盖并发采集结果。失败重跑会重新读取当前数据，数据库互斥锁阻止两个清洗任务同时运行。JSONL 报告记录每个 IP 的证据、来源缺失和更新数量；终端出现 `COMPLETE` 才表示该次全量处理结束。主标签采用独立来源票数最多的类型；最高票并列时，仅在并列项中按住宅 IP、商业宽带的顺序优先判定，其余平票判为机房 IP，全部弃权时显示“类型未知”，各来源原始类型保留在明细中。

节点七天失效清理与可用时间记录：[NODE_CLEANUP.md](NODE_CLEANUP.md)。

### 队列容量与并发

`MIHOMO_EXIT_CONCURRENCY` 控制出口总执行容量（默认 8，范围 1–64），Compose 将同一值传给 manager 和出口 worker。`EXIT_PROVISION_HIGH_CONCURRENCY`、`EXIT_PROVISION_NORMAL_CONCURRENCY` 控制两个优先级通道的消费者上限（各默认 8）；两个通道在领取任务前共享总容量，空闲通道不占用容量，等待中的消费者按先后顺序获得空位。非 Compose 启动时，消费者参数名为 `IP_SERVICE_HIGH_CONCURRENCY`、`IP_SERVICE_NORMAL_CONCURRENCY`，并需保持 manager 与 worker 的总容量配置一致。

容量不足会触发按任务池共享的指数退避（带随机抖动，上限 30 秒），已领取任务延后 5–10 秒重新入队。出口池拥塞不会阻塞其他池，也不会阻止已有租约校验和释放。`NORMAL_RECOVERY_CONCURRENCY` 同时限制普通恢复消费者和包括等待前置检测在内的在途评估，不再强制扩大到 128；升级前已开始的评估仍继续完成。基础设施异常不作为节点探测失败触发长期退避。

调优时先观察实际完成量、到期任务的最长等待时间、容量拒绝和 CPU 限流，再小幅提高总容量及消费者上限。排队数量包含未来复验任务，退避导致到期数量减少也不等于任务完成。

恢复评估缺失检测结果时会重建已完成的子任务，并使用新任务代数屏蔽旧投递；不会改写正在执行或等待退避的子任务。尚未取得检测结果的总等待上限为 10 分钟，超出后记录 `capacity-timeout`，释放评估占用并在 1 分钟后重新评估，不增加节点失败次数。已取得的检测结果仍遵守原有 3 分钟有效期和出口复核要求。

出口 worker 在取得共享容量后，最多连续三个领取机会优先从持久化队列领取到期的恢复出口子任务，第四次回到普通 Redis 队列；恢复任务沿用原来的高/普通优先级，不挤占为住宅定义的队列语义。专用部分索引避免扫描整个出口积压，所有领取仍校验节点、任务代数和租约。完成评估时取消该轮尚未运行的子任务，避免留下无主的排队工作。

普通出口查询取得首个有效 IPv4 后会取消同批剩余请求并回收它们，再最多用 1 秒补查一次可选的国家信息；某批全部失败时仍继续后续区域查询入口。最近成功的查询入口保存在出口记录中，供下一轮优先尝试。普通出口连续失败按基础间隔指数退避，最长 6 小时；新连接实例或更新的成功连通性证据可提前重试，恢复评估的独立出口证明不受该退避约束。出口记录包含实际探测耗时和连续失败次数，用于区分处理变快与单纯推迟任务。

manager 的控制器健康检查仍每 5 秒执行，全量注册表读取改为单独的 `MIHOMO_REGISTRY_POLL_SECONDS`（默认 30 秒）。增量重载只对新增或变更的连接配置批量校验，未变更配置沿用本进程已验证的结果；仍需排空租约并最终安装完整配置，不能保证零暂停。运行心跳分别记录等待租约耗时、整个暂停耗时和控制器配置写入次数。

自动更新使用 ETag 条件查询，并在 GitHub 返回 403 配额耗尽或 429 时，将额度重置时间/Retry-After 写入 `.update-state`，之后的定时检查会在冷却期内跳过请求并明确报告延后。临时网络错误和 5xx 也会延后；失败响应不会进入版本 JSON 解析，更不会改写当前版本或触发降级。正常更新、完整服务清单和失败回滚规则保持不变。

健康检查遇到瞬时超时或非 200 响应时，会再用独立的 3 秒预算复核；注册代理返回 404 仍立即判定内核状态丢失，连续失败才触发全局失效和恢复。并发健康复核串行化且等待可取消，避免扫描高峰时放大控制器请求。manager 的启动宽限调整为 120 秒，更新命令等待上限为 180 秒，以容纳初次全量配置加载；运行中的真实故障判定不受启动宽限影响。

## 登录与复制节点

节点列表和服务监控需要登录；对应 API 同样校验会话，未登录返回 401。未配置账号时默认关闭数据访问，健康检查仍公开。使用 shadcn/ui 登录表单，支持错误提示、退出及会话过期重新登录。

在 Compose 使用的 `.env`（直接运行 webman 时为 `webman/.env`）配置 `AUTH_USERNAME`、`AUTH_PASSWORD_HASH`。先用以下命令生成密码哈希，输入至少 12 字节密码后按回车、Ctrl-D：

```bash
php webman/tools/hash-password.php
```

将结果用**单引号**包围后填入，例如 `AUTH_PASSWORD_HASH='$2y$…'`，避免 dotenv / Compose 对 `$` 插值。不要提交实际密码或哈希。生产通过 HTTPS 访问并保持 `AUTH_COOKIE_SECURE=true`；本地 HTTP 开发需设为 `false`。配置后重启 API（Compose 需重建 `node-explorer` 容器）。账号配置只传给 API，不进入前端构建。

会话为 HttpOnly、SameSite=Strict Cookie，固定 8 小时有效，容器重建或修改账号 / 密码哈希后失效。每个直连来源 IP 每 15 分钟最多尝试 10 次登录；反向代理下同一代理连接共享额度，不信任客户端提供的转发头。多副本部署需共享会话和限流存储；当前适用于单 API 容器、多 worker 部署。登录阻止匿名读取，不能阻止拥有有效账号的人采集。

列表每行和详情均提供「复制节点链接」。`POST /api/nodes/link` 在登录校验后按节点 identity 读取单个有效节点，优先复制数据库保存的原始分享链接 `rawLink`（逐字保留上游原文）；上游仅提供 IP、端口或 Clash 配置时，由采集服务转换并保存 `generatedLink`，接口直接读取已保存的链接。普通列表响应剥离 `rawLink` / `generatedLink` / `rawConfig`，复制响应禁止缓存。

链接保留规则：原文与生成链接必须分开存储，禁止用生成值覆盖 `rawLink`；同一批次后来收到的原文必须补入，不能因指纹去重而跳过。`node_explorer_node_links` 独立保存原文及生成链接，不随快照压缩、节点清理级联删除。升级至数据库版本 14 后会重新处理保留快照，为配置节点补充可转换的链接；订阅文件仍有的原文会按完整配置和凭据匹配恢复，不能仅按 IP/端口或节点名关联。无法无损转换的选项会记录固定错误码并保留配置，不生成缺少连接参数的链接。链接属于敏感凭据，禁止记录到日志或普通列表响应。
