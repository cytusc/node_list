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

数据库默认保存为项目目录下的 `data/nodes.sqlite`，可通过 `DATABASE_PATH` 指定可写路径。数据库只保存页面展示字段，不保存订阅连接凭据。

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

使用包含本次改动的页面镜像，然后运行 `deploy/compose.sh up -d`。额外的 Mihomo 容器只用于 IPPure，控制端口和代理端口不映射到宿主机。页面镜像包含 Chromium，检测时的内存占用会增加。不要将检测器连接到其他应用正在使用的 Mihomo，因为采集器会替换其完整配置。

采集器每 30 秒检查当前批次，读取 `output/all.yaml` 中的完整配置，按节点名、协议、服务器、端口匹配，逐个切换代理，用 Chromium 访问 `https://ippure.com/`，展开 ASN 并提取 ASN 编号、所属组织（如 YUUSEI LTD）、人机流量比、IP 来源、IP 属性、IPPure 系数及文字等级。所有页面请求走该节点，无法连接时不会回退直连。无 ASN 流量统计时显示“统计数据不足”；0、15、25、40、50、70、100 是展示刻度，不是额外检测字段。系数越低越纯净。

结果独立关联原批次和节点，保存到 SQLite/PostgreSQL；不会改变原始快照的去重指纹。列表可按系数排序，电脑列表以独立列展示 ASN 组织、IP 来源、IP 属性、系数等级及人机流量比，各列可单独显示、隐藏和排序；手机卡片按字段展示，系数和人机比例配有绿红渐变条，节点详情展示全部指标和检测时间。刷新页面可读取采集进度。已有成功结果缺少 ASN 组织时自动补采当前批次，补采失败保留原指标，间隔 30 分钟最多重试 3 次；完整成功结果不重复请求；失败间隔 30 分钟重试，最多 3 次，重启不会重置次数。新检测批次重新采集，聚合列表显示节点最新批次的 IPPure 结果。旧批次仅保留已采集结果，已被覆盖的完整代理配置不会从历史展示字段重建。订阅尚未生成或无法匹配的节点等待后续检查。

非容器运行时设置 `IPPURE_CONTROLLER`、`IPPURE_PROXY_URL`、`PLAYWRIGHT_CHROMIUM_EXECUTABLE`；控制器配置了密钥时同时设置 `IPPURE_CONTROLLER_SECRET`。仅设置其中一个地址不会启动采集。多个页面实例不要共用一个检测代理，以免切换节点相互干扰。IPPure 验证页面、限流、节点故障或页面结构变化均可能导致检测失败，页面会保留失败状态。
