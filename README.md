# Node Explorer

使用 React、TypeScript、shadcn/ui 和 TanStack Table 展示 subs-check 检测结果及历史批次。参考 Qilan 的节点浏览布局，支持地区／协议／传输／特性筛选、搜索、速度排序、列设置、分页、详情、深浅主题和订阅下载。

## 开发

需要 Node.js 22.13 或更新版本（使用内置 SQLite）。

```bash
npm install
npm run dev
```

打开 http://localhost:5173 。开发服务会代理 `/api` 到本地 3001 端口。

默认读取项目下的 `data/sub`，可在 `.env` 中设置 `SUBS_CHECK_DIR`。结果文件默认是该目录下的 `config/cache/config/results.json`，后端启动时及每 5 秒检查一次新结果，也会在刷新时检查；按检测时间和内容去重后写入 SQLite，页面从数据库读取。即使无人打开页面也会积累历史。下载来自 `output/all.yaml`、`output/mihomo.yaml`、`output/base64.txt`，包含完整订阅，不随页面筛选变化。

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

历史清理通过 `server/reconcile-sources.js` 显式执行，默认仅打印预演计数。先备份数据库并停止页面服务（避免采集器在节点重新排序时写入旧位置），用同一数据库环境运行 `node server/reconcile-sources.js --clash /data/sub/output/all.yaml --links /data/sub/output/base64.txt --prune`；可重复添加 `--clash` 提供历史源文件，或用 `--archive 发布时刻=文件.yaml` 提供有发布时间的历史版本以消除配置歧义。核对计数后加 `--apply`，只删除既没有可匹配链接也没有完整源配置的节点，保留批次 ID 和剩余节点的 IP 检测数据。维护脚本需要已有数据库配置：PostgreSQL 使用 `PGHOST` 等变量，SQLite 使用 `DATABASE_PATH`。

人工确认失效的连接可通过 `store.retireNodes(nodes, reason)` 软删除。软删除记录以连接配置摘要为键，隐藏节点的首页/聚合结果并跳过出口 IP 补采；历史批次仍可查阅。恢复时由维护人员删除对应的 retired identity 记录。生产使用 PostgreSQL 的 `node_explorer_retired_nodes` 表，SQLite 使用 `retired_nodes` 表。

### 定时 GitHub 巡检

在服务器 `.env` 的 `COMPOSE_FILE` 追加 `:compose.patrol.yaml`；使用 PostgreSQL 时还需追加 `:compose.patrol.pgsql.yaml`（并保留已有的 `compose.pgsql.yaml`）。运行 `bash deploy/compose.sh up -d github-patrol github-patrol-proxy` 启动独立巡检容器和专用 Mihomo，无需暴露额外主机端口。巡检读取与页面相同的数据库，扫描首页聚合列表中的连接；历史配置保留，已经退役的节点不再巡检。不要扩容该巡检服务或将其控制器指向 IPPure 使用的 Mihomo。

启动后立即扫描；每轮结束后默认等待 30 分钟，再扫描更新后的节点列表。`GITHUB_PATROL_INTERVAL_SECONDS=1800`、`GITHUB_PATROL_CONCURRENCY=2`、`GITHUB_PATROL_TIMEOUT_SECONDS=12`、`GITHUB_PATROL_RETRY_SECONDS=10` 可在 `.env` 中调整。两个节点并发，每次通过各自隔离代理访问 `https://github.com/`，HTTP 200 算成功；失败时等待 10 秒，用新代理连接再次确认。两次失败且服务器直连 GitHub、正常节点的代理对照均成功，才调用软删除。对照失败、没有正常代理、缺少完整配置或代理控制器异常时，不对未确认的节点做删除。所有请求结束后才重载代理配置，巡检不占用 IPPure 代理实例。

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
npm run build
npm start
```

打开 http://localhost:3001 。服务默认仅监听本机。`API_PORT` 可修改后端端口；开发时需同步修改 Vite 代理地址。结果和节点凭据不复制到前端仓库，也不进入构建产物。

## 容器部署与发布

GitHub Actions 在推送 `v*.*.*` 标签后构建并发布 `ghcr.io/cytusc/node_list`，支持 `linux/amd64` 和 `linux/arm64`。流程执行后端测试、生产构建和已发布镜像的启动检查，再创建 GitHub Release 与生产文件压缩包。生产文件包解压后需执行 `npm ci --omit=dev` 才能使用 `npm start`。

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

默认只绑定 `127.0.0.1`。需要局域网访问时设置 `BIND_ADDRESS=0.0.0.0`；`WEB_PORT` 修改宿主机端口。页面可下载含连接凭据的完整订阅，公网部署应通过带鉴权的 HTTPS 反向代理访问。

更新时设置新 `IMAGE_TAG` 后再次执行 `docker compose pull` 和 `docker compose up -d`；回滚时设置之前的版本标签并执行相同命令。镜像不包含用户结果和订阅文件。

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

页面聚合展示所有已保存批次的节点，按协议、服务器、端口及连接参数去重，同一节点保留检测时间最近的一次结果（缺少检测时间时使用入库时间）。仅在历史批次出现的节点也会保留，详情显示该节点的检测时间；历史记录不代表节点当前仍可用。历史不自动删除，请定期备份所选数据库并关注磁盘空间。运行中备份 SQLite 时应使用 SQLite backup 工具，或停服务后备份整个数据库目录（包含 WAL 文件）。

Qilan 采集在入库前按分享链接的连接内容去重，跨分页重复时保留检测时间最新的记录。历史聚合也使用此规则，忽略链接中的显示名称、查询参数顺序，以及来源 ID、延迟、状态等检测信息；不同凭据或连接参数仍保留为独立节点。已有历史批次无需删除，更新服务后聚合展示会按新规则合并。

结果文件缺失或暂时损坏时，已入库的历史仍然可读取，页面会提示正在展示已保存结果。过去已经被覆盖且没有备份的文件无法恢复。后台只能采集服务运行期间仍在文件中的结果；服务停机期间被覆盖的批次，或两次采集之间被快速覆盖的结果无法补录。无需修改 subs-check，继续只读挂载其结果目录即可。

## 数据含义

- 节点数量是跨批次去重后的节点数，并非输入总数，无成功率和延迟指标。
- 速度沿用 subs-check 的显示规则：原始 KB/s 除以 1024 显示为 MB/s。
- TLS、UDP、Reality 是配置特性，不代表独立验证结果。
- 服务检测关闭时不显示解锁筛选；启用后保留原始平台标签，空标签表示未返回解锁标签，不推断具体失败原因。
- 未生成结果、零节点、无筛选匹配与读取错误分别展示；刷新失败保留上次已加载结果。
- 订阅文件独立生成，可能落后于结果快照；下载菜单展示文件更新时间。聚合展示时下载仍为当前输出文件，不提供聚合订阅。

## 验证

```bash
npm test
npm run build
npm run test:e2e
# 使用独立测试数据库验证 PostgreSQL（会创建并删除专用测试 schema）
TEST_POSTGRES_URL=postgresql://user:password@localhost/test_db npm test
```

浏览器测试使用 Playwright。若本机没有 Chromium，可执行 `npx playwright install chromium`；也可通过 `PLAYWRIGHT_CHROMIUM_EXECUTABLE` 指定现有浏览器路径。测试覆盖真实结果读取、筛选、分页、下载、异常状态、服务解锁和移动布局。

shadcn/ui 组件源码位于 `src/components/ui`，来源于官方 `new-york-v4` registry，保留其 MIT 许可证。

`GET /api/results?view=aggregate` 返回聚合节点和 `batchCount`，每个节点带有 `checkedAt`、`snapshotId` 及检测功能状态。原有 `/api/results`（最新批次）、`/api/results?snapshot=<id>` 和 `/api/history` 接口仍可使用。

### IPPure 逐节点检测

在 `.env` 的 `COMPOSE_FILE` 末尾追加 `:compose.ippure.yaml`，例如：

```dotenv
COMPOSE_FILE=compose.yaml:compose.subs-check.yaml:compose.ippure.yaml
```

使用包含本次改动的页面镜像，然后运行 `deploy/compose.sh up -d`。额外的 Mihomo 容器只用于 IPPure，控制端口和代理端口不映射到宿主机。采集直接发送 HTTP 请求，不启动 Chromium，页面镜像不再安装浏览器。不要将检测器连接到其他应用正在使用的 Mihomo，因为采集器会替换其完整配置。

采集器每 30 秒检查当前批次，优先使用已归档的完整代理配置，否则匹配 `output/all.yaml`。通过待检测节点优先访问 IPv4 专用入口 `https://api-ipv4.ip.sb/ip` 确认真实 IPv4 出口，失败后依次尝试 AWS CheckIP、ipify、ifconfig.me、icanhazip 和 IP.SB 的通用入口；节点实测中这些服务的可达性不同，AWS CheckIP 能返回此前 IP.SB 失败节点的 IPv4。ipify 官方 API 文档见 https://www.ipify.org/ ，AWS CheckIP 入口为 https://checkip.amazonaws.com/ 。所有请求均使用同一节点专属代理；只接受 IPv4 作为风险接口查询目标，绝不使用服务器地址替代或直连回退。随后轮换已成功查询的节点作为传输代理，按出口 IP 调用 `/api/info/ip-basic/<IP>`、`/api/info/ip-risk/<IP>`，再按 ASN 调用 `/api/info/asn/botclass/<ASN数字>`。客户端通过单连接代理会话处理 `x-k`、`x-t` 动态签名，挑战响应与签名重试复用同一条连接；旧签名方式下已耗尽重试次数的失败记录会获得新一轮重试机会。校验返回 IP、数据完整性和百分比；HTTP 200 的 `ok:false` 仍视为失败。429 和服务端错误最多退避重试 2 次，每次等待不超过 5 秒。

默认每批并发 3 个节点，`.env` 中的 `IPPURE_CONCURRENCY` 可设为 1～10；小内存服务器建议保持 3。每批为目标节点和最多 3 个可用查询代理创建独立 Mihomo mixed listeners，各入口固定绑定节点，整批任务结束后才重载配置，避免并发出口串用。`IPPURE_PROXY_URL` 的原端口保留为拒绝入口，从下一端口开始分配隔离入口，须额外预留最多“并发数 + 3”个连续内部端口；Docker 不发布这些端口。每轮采集按出口 IP 合并重复查询、按 ASN 合并人机流量比请求，失败缓存会删除；新一轮重新建缓存，避免沿用旧数据。API 地址默认为 `https://api.123169.xyz`，可用 `IPPURE_API_ORIGIN` 覆盖。

查询结果保持原有展示格式：ASN 编号和组织、IP 来源、IP 属性、IPPure 系数及文字等级、人机流量比。没有 ASN 流量统计时显示“统计数据不足”，系数越低越纯净。出口探测额外支持 Cloudflare `/cdn-cgi/trace`，记录实际返回地址的查询服务，备用服务的结果不会标为 IP.SB。所有 IPv4 探测失败但取得 IPv6 时，保留该 IPv6 并显示“仅 IPv6，暂无评分”；已获取出口但风险接口失败时显示“风险查询失败”，没有地址时显示“出口探测失败”。IPv6 不会发送到仅支持 IPv4 的风险评分接口，也不会因此被判定为失效节点。

软删除节点按完整连接配置标记退役，首页聚合视图与后续 IP 补采均排除它们，历史批次保留原配置。采集器保留节点在原始批次中的数据库下标，过滤退役节点不会改变结果写入位置；节点退役后，已经发出的请求也不能再覆盖其检测记录。

旧采集器在退役后写入、且位于被过滤记录之后的结果可能存在下标错位。新版读取时把这部分旧结果视为待重新核验，按原始完整配置重新探测；数据库中的原始批次和旧检测记录保留，新结果核验后覆盖对应检测字段。

结果独立关联原批次和节点，保存到 SQLite/PostgreSQL；不会改变原始快照的去重指纹。列表可按系数排序，电脑列表以独立列展示 ASN 组织、IP 来源、IP 属性、系数等级及人机流量比，各列可单独显示、隐藏和排序；手机卡片按字段展示，系数和人机比例配有绿红渐变条，节点详情展示全部指标和检测时间。刷新页面可读取采集进度。已有成功结果缺少 ASN 组织时自动补采当前批次，补采失败保留原指标，间隔 30 分钟最多重试 3 次；完整成功结果不重复请求；失败间隔 30 分钟重试，最多 3 次，重启不会重置次数；切换到 HTTP 接口采集后，旧方式失败的节点获得独立的 3 次重试机会。已获取的出口 IP 会随失败结果保存，重试时可以复用。新检测批次重新采集，聚合列表显示节点最新批次的 IPPure 结果。历史批次有完整代理配置的失败、待检测及信息不完整记录也会补采，优先处理聚合列表正在展示的记录，并复用历史已成功节点作为查询代理；旧版 IPv6 失败记录重新探测 IPv4 并获得新一轮重试机会。仅能获取 IPv6 的节点显示风险评分接口暂不支持 IPv6，出口连接失败与接口查询失败分别记录。已被覆盖的完整代理配置不会从历史展示字段重建。订阅尚未生成或无法匹配的节点等待后续检查。

非容器运行时设置 `IPPURE_CONTROLLER`、`IPPURE_PROXY_URL`；控制器配置了密钥时同时设置 `IPPURE_CONTROLLER_SECRET`。仅设置其中一个地址不会启动采集。多个页面实例不要共用一个检测代理，以免切换节点相互干扰。IPPure 接口签名变化、限流或节点故障均可能导致检测失败，页面会保留失败状态。
