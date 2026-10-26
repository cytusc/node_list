# Node Explorer

React／TypeScript 节点浏览器与 PHP（webman）接口层，实时展示 subs-check 和 Qilan 节点的当前连通性及出口 IP 属性。支持筛选、分页、详情、列设置和深浅主题。

现在由五个应用服务运行：API、来源导入、独立节点测活、出口探测、IP 属性。**只有 `node-liveness` 测试并维护节点是否可用：兼容节点优先 Xray，其他节点及 Xray 失败回退 Mihomo。** 测活只依赖 PostgreSQL 和私有内核，不依赖 Redis、共享 manager 或其他业务服务，不执行恢复、退役和删除。

服务边界与升级见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)，测活实现见 [独立节点测活](docs/service-refactoring/10-node-liveness.md)。

## 开发与运行

需要 Node.js 22.14+、Go 1.26+、PHP 8.1+（含 pdo_pgsql／pcntl／sockets／mbstring 扩展）和 PostgreSQL。复制 `.env.example` 为 `.env`，填写 PostgreSQL 连接配置；接口另需 `cp webman/.env.example webman/.env` 并 `composer install`。

```bash
npm ci
composer install --working-dir=webman
npm run dev
```

开发命令启动 Vite 和 API，打开 http://localhost:5173 。另外分别运行来源和工作进程（测活需安装 Xray / Mihomo 并导出 PG* 环境变量）：

```bash
npm --prefix services/node-source-converter ci --ignore-scripts
go -C backend run ./cmd/node-source
go -C backend run ./cmd/node-exit
go -C backend run ./cmd/node-attributes
go -C backend run ./cmd/node-liveness
```

生产构建使用 `npm run build && npm run build:source && npm run build:exit && npm run build:attributes`；接口用 `npm start`（= `php webman/start.php start`），采集用 `node-source`，出口用 `node-exit`，属性用 `node-attributes`，测活用 `node-liveness` 独立启动（`go -C backend build -o node-liveness ./cmd/node-liveness`）。采集额外需要 Node.js >=22.14.0 与锁定的转换器依赖（镜像已包含）；实现说明见 [采集服务文档](docs/service-refactoring/02-source-ingest-implementation.md)。运行时必须接入 PostgreSQL，不再使用 SQLite。PHP 离线测试使用内存 SQLite，生产无 SQLite 存储。接口测试：`npm run test:api`（离线断言，测试另需 pdo_sqlite 扩展）+ `php webman/tests/integration.php`（连库契约，见 `webman/README.md`）。

## 容器部署

复制 `.env.example`，填写已有 PostgreSQL 的连接信息、所在外部 Docker 网络 `POSTGRES_NETWORK` 和宿主订阅目录 `SUBS_CHECK_HOST_DIR`。数据库用户需要建表权限。设置 `release.env` 中 `IMAGE_TAG` 为包含本次模块化实现的版本。

```bash
bash deploy/compose.sh config --quiet
bash deploy/compose.sh pull
bash deploy/compose.sh up -d
bash deploy/compose.sh ps
curl --fail http://127.0.0.1:3001/api/health
```

默认 `COMPOSE_FILE=compose.yaml:compose.subs-check.yaml`，包含五个应用服务和可选上游 subs-check。已有 subs-check 时可只使用 `compose.yaml`。数据库是已有外部服务，不自动新建。只有来源服务挂载订阅目录；测活和出口服务使用私有内核，不发布代理端口。Web 默认绑定 `127.0.0.1:3001`，可用 `BIND_ADDRESS`／`WEB_PORT` 调整。

首次启动执行 `psql -v ON_ERROR_STOP=1 -f backend/internal/database/schema.sql`。旧版本升级必须先停机备份，再执行 `psql -v ON_ERROR_STOP=1 -f deploy/sql/016-retire-legacy.sql`；详见 [清理与迁移记录](docs/service-refactoring/11-project-cleanup.md)。

`deploy/node-list.service`、`deploy/node-list-update.service` 和 timer 可安装到 systemd。`deploy/update.sh` 会同步更新五个应用服务，健康检查失败时回滚对应版本。`NODE_EXPLORER_AUTO_UPDATE=false` 可关闭应用自动更新。

## 来源与当前状态

来源只导入配置，新节点等待测活服务验证，重复导入不覆盖可用状态。测活周期默认 30 分钟，异常默认 60 秒重试，间隔从每个节点完成后计算。出口与属性服务读取可用节点，各自维护出口、分类和风险事实；它们的失败不会把节点判为不可用。

IP 分类使用多家供应商独立投票，保留来源、时间和明细。页面默认住宅优先，详情分别展示测活、出口和属性时间。`/services` 和 `/api/services` 展示五个应用服务心跳、测活两组队列和在途数；退役的队列和小时采样返回空值。心跳和队列采样不输出节点配置或任务载荷。

## API 与后端

- `/api/results?page=1&limit=50`：实时查询当前节点并分页，默认住宅优先；支持 `sort=preferred` 或速度、名称等显式排序。
- `/api/results`：实时查询全部当前节点。历史接口已移除，历史路径返回 404，snapshot 参数返回 400。
- `/api/health`：HTTP API 存活。采集使用 `node-source -health`；出口使用 `node-exit -health`；属性使用 `node-attributes -health`；测活使用 `node-liveness -health`。

后端 `internal/liveness` 与 `cmd/node-liveness` 为独立测活服务；`internal/ingestion` 与 `cmd/node-source` 为独立采集服务；`internal/exitprobe` 与 `cmd/node-exit` 为独立出口服务；`internal/attributes` 与 `cmd/node-attributes` 为独立属性服务；共享 manager 与通用 worker 入口已删除。`internal/database/schema.sql` 定义共享表和视图，`internal/domain` 仅保留共享 JSON 模型与历史链接解码。HTTP 接口在 `webman/`。

旧 Node 运行时与重复调度器已移除。`npm test` 运行当前 PHP API 离线契约测试；`npm run test:backend` 验证 Go。集成测试使用显式 `NODE_EXPLORER_TEST_PG`，不自动连接生产环境。

GitHub Actions 执行构建、Go race 测试、webman 离线与连库契约测试，并验证 API 和独立测活服务镜像。容器包含 PHP 接口运行时（含前端静态资源）与 Go worker 二进制。发布不会自动执行当前工作区部署。

发布流程中，测试与镜像构建并行执行；`linux/amd64` 和 `linux/arm64` 分别使用 GitHub 原生 runner 构建，不经过 QEMU。每个架构都执行 API 和 node-liveness 冒烟检查，全部测试通过后才合并双架构镜像清单、更新版本／`latest` 标签并创建 Release。前端产物通过 Actions artifact 传给发布任务，无需再次编译。

Docker 构建缓存保存到同一 GHCR 镜像的 `buildcache-amd64`／`buildcache-arm64` 标签，供不同版本 tag 复用；这些标签仅用于缓存，不用于部署。首次运行或依赖变更需要重建相应缓存层，后续发布可复用 PHP 扩展、依赖安装等结果。工作流沿用 `GITHUB_TOKEN` 的 `packages: write` 权限，无需新增密钥。

### 独立出口 IP 属性补全

使用 `node-attributes`，只处理可用节点当前已验证的公网出口，按 IP 去重、分类与风险分别补缺。已有成功属性不周期刷新；失败冷却重试。仅依赖 PostgreSQL 与供应商 HTTP API，不依赖 Redis、manager 或代理节点。旧全库属性清洗和任务修复入口已删除，详见 [服务契约与验收](docs/service-refactoring/05-exit-attributes.md)。

### 队列容量与并发

出口通过 `node-exit` 独立运行，只读取 `available=true` 的节点。`EXIT_WORKERS` 控制私有 Mihomo 最大进程数（默认 4），`EXIT_TIMEOUT_SECONDS` 限制启动与回显总耗时（默认 25 秒）；成功后 1800 秒、失败后 300 秒重试，可用 `EXIT_INTERVAL_SECONDS` / `EXIT_RETRY_SECONDS` 配置。出口失败保留上次地址并清除验证标记，不改变可用状态，不触发属性、风险或其他服务任务。独立账本持久化下一次执行时间，重启无需 Redis 补投。详见 [出口服务设计与验收](docs/service-refactoring/04-exit-provision.md)。

共享 manager、共享 Mihomo 和 Redis 部署已退役；四个业务服务独立运行。详见 [退役记录](docs/service-refactoring/11-project-cleanup.md)。

## 登录与复制节点

节点列表和服务监控需要登录；对应 API 同样校验会话，未登录返回 401。未配置账号时默认关闭数据访问，健康检查仍公开。使用 shadcn/ui 登录表单，支持错误提示、退出及会话过期重新登录。

在 Compose 使用的 `.env`（直接运行 webman 时为 `webman/.env`）配置 `AUTH_USERNAME`、`AUTH_PASSWORD_HASH`。先用以下命令生成密码哈希，输入至少 12 字节密码后按回车、Ctrl-D：

```bash
php webman/tools/hash-password.php
```

将结果用**单引号**包围后填入，例如 `AUTH_PASSWORD_HASH='$2y$…'`，避免 dotenv / Compose 对 `$` 插值。不要提交实际密码或哈希。生产通过 HTTPS 访问并保持 `AUTH_COOKIE_SECURE=true`；本地 HTTP 开发需设为 `false`。配置后重启 API（Compose 需重建 `node-explorer` 容器）。账号配置只传给 API，不进入前端构建。

会话为 HttpOnly、SameSite=Strict Cookie，固定 8 小时有效，容器重建或修改账号 / 密码哈希后失效。每个直连来源 IP 每 15 分钟最多尝试 10 次登录；反向代理下同一代理连接共享额度，不信任客户端提供的转发头。多副本部署需共享会话和限流存储；当前适用于单 API 容器、多 worker 部署。登录阻止匿名读取，不能阻止拥有有效账号的人采集。

列表每行和详情均提供「复制节点链接」。`POST /api/nodes/link` 在登录校验后按节点 identity 读取单个有效节点，优先复制数据库保存的原始分享链接 `rawLink`（逐字保留上游原文）；上游仅提供 IP、端口或 Clash 配置时，由采集服务转换并保存 `generatedLink`，接口直接读取已保存的链接。普通列表响应剥离 `rawLink` / `generatedLink` / `rawConfig`，复制响应禁止缓存。

链接保留规则：原文与生成链接分开保存。采集服务使用固定版本 Sub-Store 完成第三方解析、生成和回读验证，再按严格配置指纹去重；来源链接历史存入 `node_explorer_source_links`。旧 `node_explorer_node_links` 已备份退役，现有 registry / snapshots 中的节点链接保持不变。链接属于敏感凭据，禁止记录到日志或普通列表响应。
