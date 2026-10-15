# Node Explorer

使用 React、TypeScript、shadcn/ui 和 TanStack Table 展示 subs-check 检测结果及历史批次。参考 Qilan 的节点浏览布局，支持地区／协议／传输／特性筛选、搜索、速度排序、列设置、分页、详情和深浅主题。

## 开发

前端与构建需要 Node.js 22.13 或更新版本；后端是 Go，需要 Go 1.26 或更新版本。运行时的 SQLite 由后端自带（`modernc.org/sqlite`，纯 Go），不依赖 Node。

```bash
npm install
npm run dev
```

`npm run dev` 并行启动 Vite 和 `go -C backend run ./cmd/node-explorer`，打开 http://localhost:5173 。开发服务会代理 `/api` 到本地 3001 端口。

默认读取项目下的 `data/sub`，可在 `.env` 中设置 `SUBS_CHECK_DIR`。结果文件默认是该目录下的 `config/cache/config/results.json`。后端启动时立刻导入一轮，之后由文件监听（`fsnotify`）在结果文件变化后防抖导入，并每 2 分钟兜底扫描一次；监听不可用时（例如部分 bind mount 不上报事件）自动降级为每 30 秒轮询。按检测时间和内容去重后写入 SQLite，页面从数据库读取。即使无人打开页面也会积累历史。订阅文件用于匹配和归档节点来源；下载接口与归档 Node 版本保持一致，当前返回 404。

### Qilan 节点来源

容器运行时，在宿主机 `.env` 中配置会话文件路径，并在现有 `COMPOSE_FILE` 末尾追加 `:compose.qilan.yaml`：

```dotenv
QILAN_SESSION_HOST_FILE=/mnt/d/code/qilan/qilan-scraper/session.txt
COMPOSE_FILE=compose.yaml:compose.subs-check.yaml:compose.qilan.yaml
QILAN_REFRESH_INTERVAL_SECONDS=900
QILAN_USER_AGENT=Mozilla/5.0
```

采集请求会携带站点 Referer 和 User-Agent。遇到请求被拒绝时，可将 `QILAN_USER_AGENT` 设置为已登录浏览器的 User-Agent；会话文件仍只存 `session` 值，响应中的 `csrf_token` 不能替代登录会话。

然后运行 `bash deploy/compose.sh up -d node-explorer`。`compose.qilan.yaml` 将宿主机文件只读挂载到容器的 `/run/secrets/qilan-session.txt`，并设置 `QILAN_SESSION_FILE` 指向此路径。宿主机文件必须已存在且容器用户可读；其他服务器请填写该服务器上的实际路径。已有 PostgreSQL、IPPure 等 Compose 文件继续保留在 `COMPOSE_FILE` 中。

服务端可同时采集 Qilan 节点并将其作为独立历史批次展示。默认读取 `/mnt/d/code/qilan/qilan-scraper/session.txt`（文件只放 session Cookie 值）；也可设置 `QILAN_SESSION_FILE` 指向只含 Cookie 的文件，或通过 `QILAN_SESSION` 环境变量传入。Cookie 不写入仓库。采集每页最多 200 条，默认每 15 分钟刷新，可用 `QILAN_REFRESH_INTERVAL_SECONDS` 调整，最低 60 秒。Qilan 返回的分享链接保存在节点来源字段中；会话过期或服务拒绝请求时保留现有批次并记录服务端错误。

每个检测批次会同时保存节点的完整源配置 `rawConfig`，以及存在且匹配的真实分享链接 `rawLink`。两者包含连接凭据，与订阅下载一样应按私有订阅管理。检测结果与源配置尚未完成匹配时会保留已有批次并等待下次导入，避免导入残缺节点。历史配置不会被后续订阅覆盖；IP 检测优先使用已归档配置。正常启动和刷新只回填来源，不删除历史节点。

只有分享链接的 Qilan 节点也参与出口 IP 和 IPPure 检测，包括已归档的历史节点。检测器在本地将链接转换为 Mihomo 配置，无需节点存在于 subs-check 的 `all.yaml` 中。同一连接的新批次复用最近的出口检测结果，未检测节点优先补采；无法转换的链接会明确显示“连接配置不支持”。

历史清理通过 `node legacy/node-server/reconcile-sources.js` 显式执行（该脚本属于归档的 Node 实现，见下文“后端实现”），默认仅打印预演计数。先备份数据库并停止页面服务（避免采集器在节点重新排序时写入旧位置），用同一数据库环境运行 `node legacy/node-server/reconcile-sources.js --clash /data/sub/output/all.yaml --links /data/sub/output/base64.txt --prune`；可重复添加 `--clash` 提供历史源文件，或用 `--archive 发布时刻=文件.yaml` 提供有发布时间的历史版本以消除配置歧义。核对计数后加 `--apply`，只删除既没有可匹配链接也没有完整源配置的节点，保留批次 ID 和剩余节点的 IP 检测数据。维护脚本需要已有数据库配置：PostgreSQL 使用 `PGHOST` 等变量，SQLite 使用 `DATABASE_PATH`。

人工确认失效的连接可通过 `store.retireNodes(nodes, reason)` 软删除。软删除记录以连接配置摘要为键，隐藏节点的首页/聚合结果并跳过出口 IP 补采；历史批次仍可查阅。人工退役节点恢复时由维护人员删除对应的 retired identity 记录；定时 GitHub 巡检退役的连接支持下述自动复检恢复。生产使用 PostgreSQL 的 `node_explorer_retired_nodes` 表，SQLite 使用 `retired_nodes` 表。

### 定时 GitHub 巡检

在服务器 `.env` 的 `COMPOSE_FILE` 追加 `:compose.patrol.yaml`；使用 PostgreSQL 时还需追加 `:compose.patrol.pgsql.yaml`（并保留已有的 `compose.pgsql.yaml`）。运行 `bash deploy/compose.sh up -d github-patrol github-patrol-proxy` 启动独立巡检容器和专用 Mihomo，无需暴露额外主机端口。巡检读取与页面相同的数据库，扫描首页聚合列表中的连接；历史配置保留，已经退役的节点由后续复检流程处理。不要扩容该巡检服务或将其控制器指向 IPPure 使用的 Mihomo。

启动后立即扫描；每轮结束后默认等待 30 分钟，再扫描更新后的节点列表。`GITHUB_PATROL_INTERVAL_SECONDS=1800`、`GITHUB_PATROL_CONCURRENCY=2`、`GITHUB_PATROL_TIMEOUT_SECONDS=12`、`GITHUB_PATROL_RETRY_SECONDS=10` 可在 `.env` 中调整。巡检优先使用完整配置 `rawConfig`，只有 `rawLink` 的节点（包括 Qilan 节点）在本地转换为 Mihomo 配置后参与检测；转换不会改变节点身份。无法转换或被 Mihomo 明确拒绝的配置单独跳过，不影响其他节点。两个节点并发，每次通过各自隔离代理访问 `https://github.com/`，HTTP 200 算成功；失败时等待 10 秒，用新代理连接再次确认。两次失败且服务器直连 GitHub、正常节点的代理对照均成功，才调用软删除。对照失败、没有正常代理、缺少可用连接配置或代理控制器异常时，不对未确认的节点做删除。所有请求结束后才重载代理配置，巡检不占用 IPPure 代理实例。

巡检每轮结束后还会复检自动退役的连接：距退役或上次复检至少 30 分钟才再次检查，连续两轮经该连接访问 GitHub 返回 HTTP 200 后解除退役标记，恢复首页展示和 IPPure 补采；失败清零连续成功次数。仅复检曾确认是住宅 IP、且因定时 GitHub 巡检或 IPPure 自动退役的连接；人工禁用记录保持禁用。复检时间、结果和成功次数保存在现有状态文件中，容器重启后继续使用；复检复用专用巡检代理，在正常巡检完成后执行。

巡检摘要保存到共享历史卷的 `github-patrol-status.json`，包含心跳时间、当前进度、完成时间、下次运行时间和成功/退役/暂缓/跳过计数；不包含完整连接配置。使用 `bash deploy/compose.sh logs --tail 20 github-patrol` 查看每轮汇总；容器健康检查读取心跳，巡检过程中每 30 秒更新。容器重启会立即重新巡检，已软删除的记录仍保留在数据库。控制器尚未启动或数据库暂时不可用时，本轮中止并在 60 秒后重试。

数据库默认保存为项目目录下的 `data/nodes.sqlite`，可通过 `DATABASE_PATH` 指定可写路径。节点来源字段保存在检测批次的 JSON payload 中，数据库备份包含订阅连接凭据。

自定义位置可设置 `SUBS_CHECK_DIR` 和可选的 `RESULTS_FILE`、`DATABASE_PATH` 环境变量：

```bash
SUBS_CHECK_DIR=/path/to/sub RESULTS_FILE=/path/to/results.json npm run dev
```

PowerShell：

```powershell
$env:SUBS_CHECK_DIR = 'D:\code\sub'
npm run dev
```

## 生产运行

```bash
npm run build          # 构建前端到 dist/
npm run build:backend  # 编译 backend/node-explorer 与 backend/github-patrol
npm start
```

`npm start` 直接执行 `./backend/node-explorer`（静态二进制，无运行时依赖）。服务会自行定位仓库根目录以读取 `dist/` 和 `.env`，因此从仓库根目录或 `backend/` 目录启动都可以。打开 http://localhost:3001 。服务默认仅监听本机。`API_PORT` 可修改后端端口；开发时需同步修改 Vite 代理地址。结果和节点凭据不复制到前端仓库，也不进入构建产物。

## 后端实现

后端为 Go 模块 `github.com/cytusc/node_list/backend`，承担全部服务与调度：HTTP API、结果文件导入、SQLite/PostgreSQL 持久化与 SQL 投影、IPPure 逐节点采集、Qilan 采集、GitHub 巡检，以及连接退役与恢复。

原先的 Node.js 后端已归档到 `legacy/node-server/`，不再参与构建、运行或调度，仅保留两点用途：作为行为基准（`backend/tools/api-parity.py` 用它做逐接口比对）和运维脚本（`retire-nodes.js`、`reconcile-sources.js`）。归档目录中的 `*.test.js` 仍可用 `npm test` 运行，用于核对 Go 实现是否与既有语义一致。归档代码自带 `ARCHIVED` 标记，不要再向其中添加功能。

模块划分：

| 目录 | 职责 |
| --- | --- |
| `cmd/node-explorer` | 主进程：HTTP 服务、文件监听、采集与巡检调度 |
| `cmd/github-patrol` | 独立巡检容器 |
| `internal/api` | HTTP 接口与请求校验 |
| `internal/domain` | 节点模型、连接身份、排序与地区名 |
| `internal/store` | 快照聚合、导入、退役状态 |
| `internal/store/sqlite` | 本地数据库适配器与内存分页 |
| `internal/store/postgres` | PostgreSQL 适配器与增量 SQL 投影 |
| `internal/transport` | 按协议分流的探测引擎路由 |
| `internal/collector` `internal/qilan` `internal/patrol` | 各采集与巡检任务 |
| `internal/source` | 结果文件监听 |

`cmd/` 下另有四个只在核对时使用的开发命令，不进入镜像、不参与发布：`idcheck`、`idscan` 与 `linkdiff` 比对连接身份与来源匹配，`sortchk` 比对排序结果。用 `go -C backend run ./cmd/<名字>` 执行。

## 容器部署与发布

GitHub Actions 在推送 `v*.*.*` 标签后构建并发布 `ghcr.io/cytusc/node_list`，支持 `linux/amd64` 和 `linux/arm64`。流程执行 Go 与归档 Node 测试、生产构建和已发布镜像的启动检查，再创建 GitHub Release 与生产文件压缩包。镜像只包含 Go 静态二进制和前端 `dist/`，不包含 Node 运行时，生产文件包解压后无需 `npm ci`。

在安装 Docker Compose 的机器上运行：

```bash
SUBS_CHECK_HOST_DIR=/mnt/d/code/sub IMAGE_TAG=v1.2.1 docker compose pull
SUBS_CHECK_HOST_DIR=/mnt/d/code/sub IMAGE_TAG=v1.2.1 docker compose up -d
docker compose ps
curl --fail http://127.0.0.1:3001/api/health
```

PowerShell / Docker Desktop 可设置：

```powershell
$env:SUBS_CHECK_HOST_DIR = 'D:/code/sub'
$env:IMAGE_TAG = 'v1.2.1'
docker compose pull
docker compose up -d
```

打开 http://localhost:3001 。目录必须已存在，容器只读挂载整个目录，以便读取 subs-check 后续替换的结果文件。独立的 `node-history` Docker 卷保存 SQLite 数据库，更新或重启容器会保留历史；请勿使用 `docker compose down -v`，这会删除历史卷。健康检查仅表示 Web 服务正常，检测结果尚未生成时页面仍会显示空状态。Linux 挂载文件须允许容器内非 root 用户读取。

默认只绑定 `127.0.0.1`。需要局域网访问时设置 `BIND_ADDRESS=0.0.0.0`；`WEB_PORT` 修改宿主机端口。公网部署应通过带鉴权的 HTTPS 反向代理访问。

更新时设置新 `IMAGE_TAG` 后再次执行 `docker compose pull` 和 `docker compose up -d`；回滚时设置之前的版本标签并执行相同命令。镜像不包含用户结果和订阅文件。

从 Node 版本升级到 Go 版本时，更新服务器的 `compose.patrol.yaml`，将巡检启动命令改为 `["/usr/local/bin/github-patrol"]`，健康检查改为 `["CMD", "/usr/local/bin/github-patrol", "--health"]`。保留 `.env`、`release.env`、数据库卷和订阅目录，然后运行 `bash deploy/compose.sh up -d --no-deps node-explorer github-patrol`。镜像也兼容旧配置中的 `node server/github-patrol-service.js` 及其 `--health` 调用，会直接执行 Go 巡检程序；兼容入口不支持执行其他 JavaScript。

### Linux 开机启动与定时更新

服务器完整部署使用两个独立容器：官方 `ghcr.io/beck-8/subs-check:1.6.5` 检测数据，页面镜像只读展示；检测服务没有宿主机端口映射。上游项目、源码与 GPL-3.0 许可证见 https://github.com/beck-8/subs-check 。通过文件共享集成，不将上游源码复制进页面镜像。

将项目放在 `/root/node_list`，首次建立共享目录和配置：

```bash
mkdir -p data/config data/output
cp deploy/subs-check.example.yaml data/config/config.yaml
chown -R 1000:1000 data
chmod 700 data/config
```

只在首次安装时复制示例，升级时保留实际配置。检测容器与页面均使用 UID 1000，保证可读取上游以 `0600` 保存的结果。示例使用三个上游公共订阅源，每 120 分钟检测一次；为 1 GB VPS 设置较低并发和下载量，可在 `data/config/config.yaml` 的 `sub-urls` 替换自己的订阅。

在 `.env` 设置数据目录、绑定地址和端口，例如：

```dotenv
SUBS_CHECK_HOST_DIR=/root/node_list/data
BIND_ADDRESS=127.0.0.1
WEB_PORT=3100
```

在 `release.env` 写入以下版本：

```dotenv
IMAGE_TAG=v1.2.1
SUBS_CHECK_TAG=1.6.5
```

然后执行：

```bash
docker compose -f compose.yaml -f compose.subs-check.yaml --env-file .env --env-file release.env pull
cp deploy/node-list.service deploy/node-list-update.service deploy/node-list-update.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now node-list.service node-list-update.timer
systemctl start node-list-update.service
systemctl list-timers node-list-update.timer
journalctl -u node-list-update.service
```

定时器每小时分别检查页面和 subs-check 的最新正式 Release，增加最多 5 分钟随机延迟。新版本先拉取镜像，等待 Docker 健康检查通过后才记录版本；启动或健康检查失败时恢复旧版本。检查失败不改变当前容器。更新仅替换镜像，不自动拉取或执行远端仓库脚本。部署尚未发布的本地镜像时，在 `.env` 设置 `NODE_EXPLORER_AUTO_UPDATE=false` 暂停页面更新，subs-check 仍自动更新；正式镜像发布后恢复为 `true`。容器日志最多保留 3 个 10 MB 文件。subs-check 更新会中断正在进行的一轮检测，但保留上次完成的结果。

### 使用现有 PostgreSQL（1Panel）

将 `.env.example` 复制为部署服务器的 `.env`，填写实际配置。`npm start` 和开发后端会加载项目根目录 `.env`；Docker Compose 通过环境变量和挂载映射注入配置，不把 `.env` 打包进镜像。

设置 `PGHOST` 后改用 PostgreSQL；不设置时继续使用 SQLite。应用启动会检查连接并创建 `node_explorer_snapshots` 表，连接失败直接退出，不会悄悄退回 SQLite。用户需要目标数据库的连接和建表权限。

在服务器 `.env` 添加以下配置，填写实际数据库名、密码与 PostgreSQL 容器所在的网络名：

```dotenv
PGHOST=实际PostgreSQL容器名
PGPORT=5432
PGDATABASE=实际数据库名
PGUSER=实际用户名
PGPASSWORD=实际密码
POSTGRES_NETWORK=实际Docker网络名
```

`.env` 已被 Git 忽略，请仅保存在部署服务器。使用单独环境变量无需拼接数据库 URL，密码中的特殊字符不需要 URL 编码。

```bash
docker compose -f compose.yaml -f compose.pgsql.yaml --env-file .env up -d
```

同时运行本项目的 subs-check 容器时追加 `-f compose.subs-check.yaml`。在服务器 `.env` 设置 `COMPOSE_FILE=compose.yaml:compose.subs-check.yaml:compose.pgsql.yaml`，systemd 和定时更新会通过 `deploy/compose.sh` 自动沿用该文件组合。仅运行页面时设为 `compose.yaml:compose.pgsql.yaml`。升级部署脚本后重新安装 `deploy/node-list.service` 并执行 `systemctl daemon-reload`。

容器必须加入 PostgreSQL 所在的外部 Docker 网络，才能使用 `.env` 中配置的 `PGHOST:PGPORT`。容器内的 `127.0.0.1` 指向容器自身。若用 `npm start` 在 PostgreSQL 所在宿主机运行，可将 `PGHOST` 改为 `127.0.0.1`，其余配置相同。

切换存储不会自动迁移 SQLite 的历史，旧数据库卷会保留；PostgreSQL 会采集当前文件并积累后续批次。请通过 PostgreSQL 自身的备份工具备份历史。

## 历史存储

页面聚合展示所有已保存批次的节点，按协议、服务器、端口及连接参数去重，同一节点保留检测时间最近的一次结果（缺少检测时间时使用入库时间）。仅在历史批次出现的节点也会保留，详情显示该节点的检测时间；历史记录不代表节点当前仍可用。SQLite 历史不自动删除；PostgreSQL 默认保留 7 天，可用 `SNAPSHOT_RETENTION_DAYS` 调整，负值禁用自动清理。请定期备份所选数据库并关注磁盘空间。运行中备份 SQLite 时应使用 SQLite backup 工具，或停服务后备份整个数据库目录（包含 WAL 文件）。

Qilan 采集在入库前按分享链接的连接内容去重，跨分页重复时保留检测时间最新的记录。历史聚合也使用此规则，忽略链接中的显示名称、查询参数顺序，以及来源 ID、延迟、状态等检测信息；不同凭据或连接参数仍保留为独立节点。已有历史批次无需删除，更新服务后聚合展示会按新规则合并。

结果文件缺失或暂时损坏时，已入库的历史仍然可读取，页面会提示正在展示已保存结果。过去已经被覆盖且没有备份的文件无法恢复。后台只能采集服务运行期间仍在文件中的结果；服务停机期间被覆盖的批次，或两次采集之间被快速覆盖的结果无法补录。无需修改 subs-check，继续只读挂载其结果目录即可。

## 数据含义

- 节点数量是跨批次去重后的节点数，并非输入总数，无成功率和延迟指标。
- 速度沿用 subs-check 的显示规则：原始 KB/s 除以 1024 显示为 MB/s。
- TLS、UDP、Reality 是配置特性，不代表独立验证结果。
- 服务检测关闭时不显示解锁筛选；启用后保留原始平台标签，空标签表示未返回解锁标签，不推断具体失败原因。
- 未生成结果、零节点、无筛选匹配与读取错误分别展示；刷新失败保留上次已加载结果。
- 订阅文件独立生成，可能落后于结果快照；当前 API 不提供订阅下载。

## 验证

```bash
npm run test:backend   # go vet + go test -race，含接口契约与排序回归
npm test               # 归档 Node 实现的语义基准测试
npm run build
npm run test:e2e
```

SQLite 与 PostgreSQL 都通过数据库变更版本同步独立采集、巡检进程的写入。无变化时只读取版本；有变化时读取受影响批次及其完整 IPPure 结果集，同时同步退役与恢复。文件导入、缓存刷新与本进程写入分别串行化，避免旧数据覆盖新结果。

Go 回归覆盖文件重写／替换、并发导入、跨进程同步、采集索引、Mihomo 实例配置与占用、巡检状态并发写入。PostgreSQL 集成测试默认跳过；显式设置 `NODE_EXPLORER_TEST_PG` 指向测试数据库后，`npm run test:backend` 会同时验证真实投影、凭据过滤与锁释放。测试为各用例创建独立 schema，测试账号需具备建 schema 权限；发布流程已启用该检查。

接口契约测试（`backend/internal/api`）断言 `/api/results`、`/api/history`、`/api/health` 的完整 JSON 键集合——多出一个键（泄漏的投影标记、内部 ID）和少一个键同样是回归。

需要逐接口核对 Go 与归档 Node 实现的输出时，用自带夹具起两个服务再比对：

```bash
python backend/tools/make-parity-fixture.py /tmp/parity/nodes.sqlite
cp /tmp/parity/nodes.sqlite /tmp/parity/node.sqlite
DATABASE_PATH=/tmp/parity/node.sqlite  API_PORT=3011 node legacy/node-server/index.js &
DATABASE_PATH=/tmp/parity/nodes.sqlite API_PORT=3012 ./backend/node-explorer &
python backend/tools/api-parity.py
```

两侧必须读取内容相同的数据库。脚本覆盖 27 种查询形态（分页、排序、各筛选、两种视图、各错误码），全部一致才算通过。

> 节点及 IPPure 数据在 Go 中保留源 JSON 的字段存在性：缺失字段保持缺失，显式空字符串、空数组、空对象、`false`、`0` 和 `null` 在存储及 API 输出时保留，后续更新以当前字段值为准。公开 API 仍完全移除 `rawLink` 和 `rawConfig`，快照指纹也排除这两个凭据字段。此前已经被省略且写入数据库的零值无法从现有记录推断恢复，需要从仍保留这些字段的原始源数据重新导入。

浏览器测试使用 Playwright。若本机没有 Chromium，可执行 `npx playwright install chromium`；也可通过 `PLAYWRIGHT_CHROMIUM_EXECUTABLE` 指定现有浏览器路径。测试覆盖真实结果读取、筛选、分页、下载、异常状态、服务解锁和移动布局。

shadcn/ui 组件源码位于 `src/components/ui`，来源于官方 `new-york-v4` registry，保留其 MIT 许可证。

首页通过 `GET /api/results?view=aggregate&page=1&limit=50` 按页获取节点，翻页、更改每页数量、筛选和排序会重新请求接口。PostgreSQL 模式直接查询持久化的 `node_explorer_current_nodes` 节点表，在 SQL 中执行筛选、排序、计数和 `LIMIT/OFFSET`，仅读取当前页的节点内容，不在请求中扫描历史快照或等待文件同步。`total` 为筛选后的数量，`totalNodes` 为聚合总数，`page`、`limit` 为实际页码及每页数量，`summary` 和 `facets` 为全量统计和筛选选项。页码从 1 开始，`limit` 范围为 1–200，超出最后一页会回到最后一页。SQLite 模式仍使用原有聚合及分页逻辑。

首页聚合保留所有未禁用、未软删除的历史节点，不因来源停止更新或后续订阅中缺失而隐藏。节点可用性由巡检判断；确认不可用后按既有规则软删除，恢复后重新展示。分页和完整聚合使用同一节点保留规则。

PostgreSQL 启动时自动创建查询表、索引及变更触发器，首次构建从现有历史数据回填，不删除快照、完整源配置或 IPPure 记录。后端在进程内维护已解码批次缓存，来源文件变化或数据库出现新批次、IPPure 更新、节点退役或恢复时才刷新查询表，包括其他采集或巡检进程写入的数据。后续仅重读发生变更的批次，并仅写入变化的查询表记录；排序位置和全量统计随刷新一起原子提交。刷新期间继续提供上一份完整结果，数据库故障时保留上一份结果并重试，因此页面数据相对数据库有一个刷新周期及计算时间的延迟。中文自然排序（ICU 等价的数字感知排序）和缺失值末尾排序在构建时生成排序序号，避免数据库排序规则改变分页结果。排序规则变化时通过投影内的 `ranking` 版本标记自动触发一次性重建。完整聚合及历史候选扫描同样在进程内执行，不阻塞 HTTP 请求。

筛选参数包括 `q`（名称、服务器、SNI、国家及中文地区名）、`country`、`type`、`network`、`features`、`media`（多选值用逗号分隔）及 `minSpeed`（MB/s）；排序使用 `sort` 和 `direction=asc|desc`。返回的 `batchCount` 为聚合批次数，每个节点带有 `checkedAt`、`snapshotId` 及检测功能状态。不传分页参数的 `/api/results?view=aggregate` 保留原有完整结果格式。原有 `/api/results`（最新批次）、`/api/results?snapshot=<id>` 和 `/api/history` 接口仍可使用。

### IPPure 逐节点检测

在 `.env` 的 `COMPOSE_FILE` 末尾追加 `:compose.ippure.yaml`，例如：

```dotenv
COMPOSE_FILE=compose.yaml:compose.subs-check.yaml:compose.ippure.yaml
```

使用包含本次改动的页面镜像，然后运行 `deploy/compose.sh up -d`。额外的 Mihomo 容器只用于 IPPure，控制端口和代理端口不映射到宿主机。采集直接发送 HTTP 请求，不启动 Chromium，页面镜像不再安装浏览器。不要将检测器连接到其他应用正在使用的 Mihomo，因为采集器会替换其完整配置。

采集器每 30 秒检查当前批次，优先使用已归档的完整代理配置，否则匹配 `output/all.yaml`。通过待检测节点优先访问 IPv4 专用入口 `https://api-ipv4.ip.sb/ip` 确认真实 IPv4 出口，失败后依次尝试 AWS CheckIP、ipify、ifconfig.me、icanhazip 和 IP.SB 的通用入口；节点实测中这些服务的可达性不同，AWS CheckIP 能返回此前 IP.SB 失败节点的 IPv4。ipify 官方 API 文档见 https://www.ipify.org/ ，AWS CheckIP 入口为 https://checkip.amazonaws.com/ 。每个出口查询入口使用独立连接，单次超时 8 秒；第一轮全部失败后，再用新连接重试 IP.SB IPv4、AWS 和 ipify，共最多 11 次尝试。记录每次查询入口、耗时和错误类型；旧版未取得地址的失败记录优先重新检测。所有请求均使用同一节点专属代理；只接受 IPv4 作为风险接口查询目标，绝不使用服务器地址替代或直连回退。随后轮换已成功查询的节点作为传输代理，按出口 IP 调用 `/api/info/ip-basic/<IP>`、`/api/info/ip-risk/<IP>`，再按 ASN 调用 `/api/info/asn/botclass/<ASN数字>`。客户端通过单连接代理会话处理 `x-k`、`x-t` 动态签名，挑战响应与签名重试复用同一条连接；旧签名方式下已耗尽重试次数的失败记录会获得新一轮重试机会。校验返回 IP、数据完整性和百分比；HTTP 200 的 `ok:false` 仍视为失败。429 和服务端错误最多退避重试 2 次，每次等待不超过 5 秒。

默认每批并发 3 个节点，`.env` 中的 `IPPURE_CONCURRENCY` 可设为 1～10；小内存服务器建议保持 3。每批为目标节点和最多 3 个可用查询代理创建独立 Mihomo mixed listeners，各入口固定绑定节点，整批任务结束后才重载配置，避免并发出口串用。`IPPURE_PROXY_URL` 的原端口保留为拒绝入口，从下一端口开始分配隔离入口，须额外预留最多“并发数 + 3”个连续内部端口；Docker 不发布这些端口。每轮采集按出口 IP 合并重复查询、按 ASN 合并人机流量比请求，失败缓存会删除；新一轮重新建缓存，避免沿用旧数据。API 地址默认为 `https://api.123169.xyz`，可用 `IPPURE_API_ORIGIN` 覆盖。

IP 类型属性按 [xykt/IPQuality](https://github.com/xykt/IPQuality/blob/main/ip.sh) 的映射规则处理 ASN/使用类型、公司类型与 AbuseIPDB `usageType`：`business`/`Commercial` 为商业宽带，`isp`/`Fixed Line ISP` 为住宅 IP，`hosting`/`Data Center/Web Hosting/Transit` 为机房 IP，`Mobile ISP` 为移动网络，并识别教育、政府、银行、组织、军事、图书馆、CDN、搜索引擎和保留地址等用途。IP-API 的 `mobile=true`、`hosting=true` 分别识别移动、机房属性；`hosting=false` 不推断为住宅。来源缺少字段时不生成类型，有未识别的类型值时显示其他网络。

每家来源的使用类型和公司类型分别保存在 `classification.types`，节点详情显示全部 7 家。每家最多一票，优先使用使用类型，缺少时使用公司类型；请求失败、缺少类型的来源弃权。有效票中票数最多的类型作为汇总结果，最高票并列时统一判为住宅 IP；全部弃权时显示“类型未知”。各来源原始判断保留在明细中，住宅恢复采用同一汇总判定。

默认查询 7 家来源：IPPure、IPinfo、IP-API（ip-api.com）、IPregistry、AbuseIPDB、ipapi.is 和 IP2Location。脚本“IP 类型属性”的 5 家来源均已接入；IP-API 与 ipapi.is 是独立服务。ipapi.is 和 IP2Location 分别复用脚本的 `https://ipinfo.check.place/{IP}?db=ipapi` 与 `?db=ip2location`，按 `asn.type`/`company.type` 和 `usage_type`/`as_info.as_usage_type` 分别保存使用类型与公司类型，无需额外密钥。IPinfo 使用脚本同款公开 widget 接口；IPregistry 按脚本从 `https://ipregistry.co` 提取公开演示 Key，无法提取时使用脚本的演示默认值，并带官网 Origin/Referer 查询 `api.ipregistry.co`；AbuseIPDB 使用脚本的 `https://ipinfo.check.place/{IP}?db=abuseipdb` 转发入口。演示 Key 获取结果会缓存，避免每个 IP 重复抓取官网。设置 `IPINFO_TOKEN`、`IPREGISTRY_API_KEY`、`ABUSEIPDB_API_KEY` 时优先使用对应个人凭据和官方接口，Compose 采集和巡检容器均会接收这些配置。公开入口限流、失败或缺少字段时按未知处理。

查询结果保持原有展示格式：ASN 编号和组织、IP 来源、IP 属性、IPPure 系数及文字等级、人机流量比。没有 ASN 流量统计时显示“统计数据不足”，系数越低越纯净。出口探测额外支持 Cloudflare `/cdn-cgi/trace`，记录实际返回地址的查询服务，备用服务的结果不会标为 IP.SB。所有 IPv4 探测失败但取得 IPv6 时，保留该 IPv6 并显示“仅 IPv6，暂无评分”；已获取出口但风险接口失败时显示“风险查询失败”，没有地址时显示“出口探测失败”。IPv6 不会发送到仅支持 IPv4 的风险评分接口，也不会因此被判定为失效节点。

软删除节点按完整连接配置标记退役，首页聚合视图与后续 IP 补采均排除它们，历史批次保留原配置。采集器保留节点在原始批次中的数据库下标，过滤退役节点不会改变结果写入位置；节点退役后，已经发出的请求也不能再覆盖其检测记录。

旧采集器在退役后写入、且位于被过滤记录之后的结果可能存在下标错位。新版读取时把这部分旧结果视为待重新核验，按原始完整配置重新探测；数据库中的原始批次和旧检测记录保留，新结果核验后覆盖对应检测字段。

结果独立关联原批次和节点，保存到 SQLite/PostgreSQL；不会改变原始快照的去重指纹。列表可按系数排序，电脑列表以独立列展示 ASN 组织、IP 来源、IP 属性、系数等级及人机流量比，各列可单独显示、隐藏和排序；手机卡片按字段展示，系数和人机比例配有绿红渐变条，节点详情展示全部指标和检测时间。刷新页面可读取采集进度。已有成功结果缺少 ASN 组织时自动补采当前批次，补采失败保留原指标，间隔 30 分钟最多重试 3 次；完整成功结果不重复请求；失败间隔 30 分钟持续重试，重启保留累计次数；切换检测方式后，旧方式失败的节点获得新一轮重试机会。已获取的出口 IP 会随失败结果保存，重试时可以复用。新检测批次重新采集，聚合列表显示节点最新批次的 IPPure 结果。历史批次有完整代理配置的失败、待检测及信息不完整记录也会补采，优先处理聚合列表正在展示的记录，并复用历史已成功节点作为查询代理；旧版 IPv6 失败记录重新探测 IPv4 并获得新一轮重试机会。仅能获取 IPv6 的节点显示风险评分接口暂不支持 IPv6，出口连接失败与接口查询失败分别记录。已被覆盖的完整代理配置不会从历史展示字段重建。订阅尚未生成或无法匹配的节点等待后续检查。

非容器运行时设置 `IPPURE_CONTROLLER`、`IPPURE_PROXY_URL`；控制器配置了密钥时同时设置 `IPPURE_CONTROLLER_SECRET`。仅设置其中一个地址不会启动采集。多个页面实例不要共用一个检测代理，以免切换节点相互干扰。IPPure 接口签名变化、限流或节点故障均可能导致检测失败，页面会保留失败状态。

### 全库出口 IP 属性清洗

`ip-type-clean` 覆盖 `node_explorer_ippure_results` 中所有有效出口 IP，包括历史批次和已退役节点；按 IP 去重，不使用节点服务器地址替代出口。它不探测节点当前出口，也不刷新 IPPure 风险分数。它使用六家外部查询来源和已保存的 IPPure 类型；公共接口失败时只复用同一 IP 在 24 小时内的来源证据，保留各来源时间，过期数据不算新结果。

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C backend build -o ip-type-clean ./cmd/ip-type-clean
bash deploy/ip-type-clean.sh --ip 51.158.105.199        # 单 IP 预览
bash deploy/ip-type-clean.sh --refresh=false           # 全库已有证据预览
bash deploy/ip-type-clean.sh --refresh=false --apply   # 全库已有证据清洗
bash deploy/ip-type-clean.sh --apply                   # 全库联网补查并清洗
```

在部署主机执行包装脚本；它从应用容器安全读取数据库配置，通过临时容器运行，报告写入宿主机 `ip-type-clean-reports/`。可通过 `IP_TYPE_CLEAN_CONTAINER`、`IP_TYPE_CLEAN_BINARY`、`IP_TYPE_CLEAN_REPORT_DIR` 覆盖默认值。二进制也可直接使用 PostgreSQL 标准环境变量运行。

默认不写数据库；`--apply` 在每个 IP 的事务中锁定当前记录，先将原始 JSON 备份至 `node_explorer_ip_type_clean_backup`（按运行 ID、批次、节点索引唯一），再仅更新 `classification` 和 `attributes`。保留节点、检测时间、风险分数、真实出口及其他字段，避免覆盖并发采集结果。失败重跑会重新读取当前数据，数据库互斥锁阻止两个清洗任务同时运行。JSONL 报告记录每个 IP 的证据、来源缺失和更新数量；终端出现 `COMPLETE` 才表示该次全量处理结束。主标签采用独立来源票数最多的类型；最高票并列时判为住宅 IP，全部弃权时显示“类型未知”，各来源原始类型保留在明细中。
