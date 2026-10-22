# Node Explorer

React／TypeScript 节点浏览器与 PHP（webman）接口层，实时展示 subs-check 和 Qilan 节点的当前连通性及出口 IP 属性。支持筛选、分页、详情、列设置和深浅主题。

现在由七个独立服务运行：API 查询（PHP/webman，实现见 `webman/`）、来源导入、出口探测、IP 属性、恢复与生命周期、节点清理、Mihomo 管理与全量巡检（后六个是 Go `node-worker`）。**PostgreSQL 保存业务数据，Redis 分发任务；API 只查询当前状态，全量连通性巡检由 `mihomo-manager` 在上一轮结束后默认间隔 30 分钟启动下一轮；`recovery` 消费住宅和普通节点的复核、恢复及生命周期任务，失败后按退避间隔重试。** 全量巡检更新检测证据，不能替代恢复任务；人工禁用不会自动恢复。

完整的模块职责、任务契约、故障恢复、部署与升级步骤见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)，住宅判定规则见 [RESIDENTIAL_RECOVERY.md](RESIDENTIAL_RECOVERY.md)。

## 开发与运行

需要 Node.js 22.13+、Go 1.26+、PHP 8.1+（含 pdo_pgsql／pcntl／sockets／mbstring 扩展）、PostgreSQL 和 Redis。复制 `.env.example` 为 `.env`，填写 PostgreSQL、Redis 及四组专用 Mihomo 地址；接口另需 `cp webman/.env.example webman/.env` 并 `composer install`。

```bash
npm ci
composer install --working-dir=webman
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

生产构建使用 `npm run build && npm run build:worker`；接口用 `npm start`（= `php webman/start.php start`），工作进程用 `node-worker -role <角色>` 独立启动。运行时必须接入 PostgreSQL，不再使用 SQLite。测试夹具仍保留 SQLite，用于验证旧行为。接口测试：`npm run test:api`（离线断言）+ `php webman/tests/integration.php`（连库契约，见 `webman/README.md`）。

## 容器部署

复制 `.env.example`，填写已有 PostgreSQL 的连接信息、所在外部 Docker 网络 `POSTGRES_NETWORK` 和宿主订阅目录 `SUBS_CHECK_HOST_DIR`。数据库用户需要建表权限。设置 `release.env` 中 `IMAGE_TAG` 为包含本次模块化实现的版本。

```bash
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d --remove-orphans
bash deploy/compose.sh ps
curl --fail http://127.0.0.1:3001/api/health
```

默认 `COMPOSE_FILE=compose.yaml:compose.subs-check.yaml`，包含七个应用服务、Redis、一个共享 Mihomo 和上游 subs-check。已有 subs-check 时可只使用 `compose.yaml`。数据库是已有外部服务，不自动新建。只有来源服务挂载订阅目录；所有代理和 Redis 都不发布宿主端口。Web 默认绑定 `127.0.0.1:3001`，可用 `BIND_ADDRESS`／`WEB_PORT` 调整。

旧的 PG／IPPure／巡检 overlay 已删除（它们曾是为旧部署保留的空兼容文件，生产自 v1.4.x 起已不在 `COMPOSE_FILE` 中引用）。升级应停止旧巡检与采集进程，更新 Compose 文件，保留数据库与旧 SQLite 卷；SQLite 历史不会自动迁移。详见模块化升级说明。

`deploy/node-list.service`、`deploy/node-list-update.service` 和 timer 可安装到 systemd。`deploy/update.sh` 会同步更新七个应用服务，健康检查失败时回滚对应版本。`NODE_EXPLORER_AUTO_UPDATE=false` 可关闭应用自动更新。

## 来源与当前状态

来源服务监听 `SUBS_CHECK_DIR` 下 `config/cache/config/results.json`、`output/all.yaml` 和 `output/base64.txt`，防抖导入，并周期兜底。`RESULTS_FILE` 可覆盖结果文件路径。来源批次按检测内容去重；同步游标持久化，仅在批次新增或修改时更新连接注册表，不在空闲时遍历全部批次。

Qilan 来源在 `COMPOSE_FILE` 追加 `:compose.qilan.yaml`，设置 `QILAN_SESSION_HOST_FILE` 为宿主会话文件；挂载到来源服务，文件只含 session Cookie 值。可设置 `QILAN_REFRESH_INTERVAL_SECONDS`（默认 900，至少 60）、`QILAN_USER_AGENT`。非容器运行支持 `QILAN_SESSION` 或 `QILAN_SESSION_FILE`。`FEISHU_WEBHOOK_URL` 是可选的会话过期／恢复通知配置。Cookie 和节点凭据不进入公开 API。

历史快照保存完整 `rawConfig` 或分享链接，作为后续检测的连接依据。同一连接跨批次按配置身份合并，当前连通性、出口和 IP 属性按身份保存；页面每次请求直接关联这些当前记录，在 SQL 中筛选、排序和分页，不读取来源批次的完整内容。数据库备份包含连接凭据，按私有订阅管理。

注册表额外按实际 Mihomo 连接参数合并等价配置和分享链接，忽略备注及未启用 TLS 时无效的 TLS 参数；不同凭据、有效 SNI 或传输路径分别保留。重复记录仅停止展示和调度，原身份、配置及检测记录仍保留。出口探测通过节点代理并发访问 IPIP（`myip.ipip.net`）、IP.SB、AWS、ipify 等查询站，一批超时后仍尝试后续站点；已检测连通但缺少出口 IP 的节点优先补查。

IP 属性的六个来源并行查询，缓存锁只保护内存数据；同一上游的请求按限额串行，等待可取消。共用 `ipinfo.check.place` 的公共查询共享额度和拒绝冷却，配置个人 AbuseIPDB 凭据后使用独立额度。HTTP 401/403 冷却 15 分钟，429 遵循 `Retry-After`，网络请求失败冷却 30 秒；冷却通过 PostgreSQL 在工作进程间共享。被拒绝或限流的来源弃权，不据此判定节点类型；此机制减少无效请求，不能解除上游封禁，稳定完整的属性仍依赖可用的查询源或有效账号额度。

分享链接兼容 TUIC v4 token、TUIC v5 凭据及 VLESS 自定义用户 ID，`splithttp` 按 `xhttp` 解析。缺少凭据、Reality 公钥或内核不支持的配置会保存明确的拒绝原因并退避重试；探测失败保留历史出口，但清除本次验证状态。

IPPure 人机比例接口明确返回空数据时记为“暂无数据”，6 小时后重查；查询失败记为独立错误，30 分钟后重试，不生成比例值。基础信息或风险查询失败保留上次成功结果，同时记录最近一次查询的状态、时间和原因。

首页展示保留期内所有未退役连接。PostgreSQL 默认保留 7 天快照，`SNAPSHOT_RETENTION_DAYS` 设为负值禁用自动清理；没有有效源快照的连接不会被恢复。人工退役由维护操作写入 `node_explorer_retired_nodes`；自动恢复只匹配已知的自动退役原因和原退役时间。

新增公共代理来源默认由 `source-ingest` 每 30 分钟拉取，启动时立即同步：

| 来源标签 | 上游项目 | 导入范围 |
| --- | --- | --- |
| `proxio-all` | [proxio.io / proxy-list](https://github.com/proxio-io/proxy-list) | `all.json` 中的 HTTP、CONNECT、SOCKS5 |
| `Jakee-http` | [Jakee8718 / Free-Proxies](https://github.com/Jakee8718/Free-Proxies) | `proxy/http.txt` |
| `xyzs996-all` | [xyzs996 / free-proxy-health-list](https://github.com/xyzs996/free-proxy-health-list) | `proxies/all/data.json` 中的 HTTP、CONNECT、SOCKS5 |

可通过 `SOURCE_PROVIDERS` 选择来源（Compose 默认 `subs-check,proxio-all,Jakee-http,xyzs996-all`），通过 `PUBLIC_PROXY_REFRESH_INTERVAL_SECONDS` 调整周期（默认 1800，小于 60 时回退默认值）。修改后重建来源服务。来源名称写入节点的 `subTag`，同一来源内去重；跨来源连接沿用注册表的连接合并规则。JSON 保留协议信息，避免把无协议的 `all.txt` 误当 HTTP。上游的 HTTPS 表示 HTTP CONNECT，导入配置不启用代理端 TLS；SOCKS4 当前检测内核不支持，跳过。上游地区仅作声明保存，不使用上游住宅标签或服务器 IP 代替本系统的出口检测和 IP 分类。请求失败、数据格式错误或空列表时不写入新快照，其他来源继续运行。这里导入完整支持列表，不按“亚太家园”数量或地区筛选；实际可用及住宅数量以本系统检测为准。proxio 数据由 [proxio.io](https://proxio.io) 提供，采用 CC BY 4.0。

## 调度与展示

全量巡检通过共享 Mihomo 检查 GitHub 连通性并保存当前状态；严格复核另有宿主直连和参考代理对照。自动退役要求跨周期连续失败及有效代际证据。出口服务独立读取出口，按真实出口 IP 合并属性查询，不使用节点服务器地址替代出口。

全量连通性巡检按完成时间计算下一轮，默认间隔 1800 秒；常规出口检查默认间隔也是 1800 秒。巡检在后台单轮运行，配置检查继续按 5 秒节拍执行；测活整轮（含结果事务提交）与配置重载使用进程互斥锁：有配置变化时合并待加载配置，优先完成本轮并落库，再重载最新配置；来源刷新不会取消本轮。每轮结束后空闲 30 分钟再启动下一轮（`SWEEP_INTERVAL_SECONDS=1800`），重载不重置间隔。停机或轮次超时仍可取消。取消、接口忙及基础设施错误不累计节点失败；证据写入须通过内核代际、节点实例及配置摘要校验。服务心跳包含巡检阶段、候选数、有效观察数、探测/提交耗时及下轮启动时间 `next_run_at`。自动退役须连续 3 轮失败、每轮至少相隔 2 分钟；历史住宅节点的临时恢复失败按 5 分钟、15 分钟、1 小时、6 小时退避复测，每轮使用新证据并保留结果。各类检测有独立工作槽位，底层复用共享 Mihomo；风险接口请求使用独立的 risk 容量。分类与风险成功结果 24 小时后刷新，失败有独立重试。分类按原始来源时间判断有效性，风险接口失败不阻塞其他来源分类与住宅恢复。

默认查询 IPPure、IPinfo、IP-API、IPregistry、AbuseIPDB、ipapi.is、IP2Location。每家最多一票，使用类型优先、公司类型后备，失败或无有效类型时弃权。最高票并列时，在并列项中依次优先住宅 IP、商业宽带，剩余平票判为机房 IP；全部弃权为类型未知。来源详情与独立时间保存在 `classification` 中。

页面默认“住宅 IP 优先”，要求近期可用、出口检查新鲜且分类有效；用户选择其他排序后沿用其选择。页面自动刷新，详情分别展示测活、出口检查和属性时间。服务状态面板及 `/api/services` 已移除。

## API 与后端

- `/api/results?page=1&limit=50`：实时查询当前节点并分页，默认住宅优先；支持 `sort=preferred` 或速度、名称等显式排序。
- `/api/results`：实时查询全部当前节点。历史接口已移除，历史路径返回 404，snapshot 参数返回 400。
- `/api/health`：HTTP API 存活。worker 使用 `node-worker -role <角色> -health`。

后端主要目录：`backend/internal/service` 为工作进程和事务边界，`internal/taskqueue` 为 Redis 引用队列，`internal/store/postgres` 为业务存储和当前状态 SQL 查询，`internal/domain` 为身份、分类与排序模型，`internal/transport` 为 Native／Mihomo 隔离通道。`cmd/node-worker` 按角色启动独立服务（source／exit-provision／exit-attributes／recovery／node-cleanup／mihomo-manager）；HTTP 接口已改为 `webman/` 下的 PHP/webman 实现（同一镜像内运行，命令 `php start.php start`）。切换期的一次性分析命令（idcheck/idscan/linkdiff/sortchk）与旧 patrol 入口已完成使命并删除。

`legacy/node-server/` 已归档，仅作为兼容行为基准及旧数据维护参考，不参与运行时调度。`npm test` 运行归档行为测试；`npm run test:backend` 验证 Go。集成测试使用显式 `NODE_EXPLORER_TEST_PG`／`NODE_EXPLORER_TEST_REDIS`，不自动连接生产环境。

GitHub Actions 执行构建、Go race 测试、webman 离线与连库契约测试、归档行为测试，并验证 API 和独立生命周期 worker 镜像。容器包含 PHP 接口运行时（含前端静态资源）与 Go worker 二进制。发布不会自动执行当前工作区部署。

发布流程中，测试与镜像构建并行执行；`linux/amd64` 和 `linux/arm64` 分别使用 GitHub 原生 runner 构建，不经过 QEMU。每个架构都执行 API、recovery 和 Mihomo manager 冒烟检查，全部测试通过后才合并双架构镜像清单、更新版本／`latest` 标签并创建 Release。前端产物通过 Actions artifact 传给发布任务，无需再次编译。

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
