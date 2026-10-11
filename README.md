# Node Explorer

使用 React、TypeScript、shadcn/ui 和 TanStack Table 展示 subs-check 最近一轮检测结果。参考 Qilan 的节点浏览布局，支持地区／协议／传输／特性筛选、搜索、速度排序、列设置、分页、详情、深浅主题和订阅下载。

## 开发

需要 Node.js 22 或更新版本。

```bash
npm install
npm run dev
```

打开 http://localhost:5173 。开发服务会代理 `/api` 到本地 3001 端口。

默认在 Windows 读取 `D:\code\sub`，在 WSL/Linux 读取 `/mnt/d/code/sub`。结果文件默认是该目录下的 `config/cache/config/results.json`，每次刷新都会重新读取。下载来自 `output/all.yaml`、`output/mihomo.yaml`、`output/base64.txt`，包含完整订阅，不随页面筛选变化。

自定义位置可设置 `SUBS_CHECK_DIR` 和可选的 `RESULTS_FILE` 环境变量：

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
SUBS_CHECK_HOST_DIR=/mnt/d/code/sub IMAGE_TAG=v1.0.0 docker compose pull
SUBS_CHECK_HOST_DIR=/mnt/d/code/sub IMAGE_TAG=v1.0.0 docker compose up -d
docker compose ps
curl --fail http://127.0.0.1:3001/api/health
```

PowerShell / Docker Desktop 可设置：

```powershell
$env:SUBS_CHECK_HOST_DIR = 'D:/code/sub'
$env:IMAGE_TAG = 'v1.0.0'
docker compose pull
docker compose up -d
```

打开 http://localhost:3001 。目录必须已存在，容器只读挂载整个目录，以便读取 subs-check 后续替换的结果文件。健康检查仅表示 Web 服务正常，检测结果尚未生成时页面仍会显示空状态。Linux 挂载文件须允许容器内非 root 用户读取。

默认只绑定 `127.0.0.1`。需要局域网访问时设置 `BIND_ADDRESS=0.0.0.0`；`WEB_PORT` 修改宿主机端口。页面可下载含连接凭据的完整订阅，公网部署应通过带鉴权的 HTTPS 反向代理访问。

更新时设置新 `IMAGE_TAG` 后再次执行 `docker compose pull` 和 `docker compose up -d`；回滚时设置之前的版本标签并执行相同命令。镜像不包含用户结果和订阅文件。

## 数据含义

- 节点数量是本轮保留节点数，并非输入总数；无成功率、延迟和历史趋势。
- 速度沿用 subs-check 的显示规则：原始 KB/s 除以 1024 显示为 MB/s。
- TLS、UDP、Reality 是配置特性，不代表独立验证结果。
- 服务检测关闭时不显示解锁筛选；启用后保留原始平台标签，空标签表示未返回解锁标签，不推断具体失败原因。
- 未生成结果、零节点、无筛选匹配与读取错误分别展示；刷新失败保留上次已加载结果。
- 订阅文件独立生成，可能落后于结果快照；下载菜单展示文件更新时间。

## 验证

```bash
npm test
npm run build
npm run test:e2e
```

浏览器测试使用 Playwright。若本机没有 Chromium，可执行 `npx playwright install chromium`；也可通过 `PLAYWRIGHT_CHROMIUM_EXECUTABLE` 指定现有浏览器路径。测试覆盖真实结果读取、筛选、分页、下载、异常状态、服务解锁和移动布局。

shadcn/ui 组件源码位于 `src/components/ui`，来源于官方 `new-york-v4` registry，保留其 MIT 许可证。
