# 接口层切换 webman（2026-10-18）

## 决策

网页接口从 Go（`backend/internal/api` + `cmd/node-explorer`）**整体切换为 PHP/webman 2.2.4**
（`webman/` 目录），Go 侧接口代码**全部删除**，webman 成为接口的唯一实现。
采集、调度、存储仍在 Go（`node-worker`，五个角色不变）。前端 `src/App.tsx` 零改动。

切换前经过 55/55 用例逐字节比对（状态码、响应头、原始 body 字节全同），
比对工具 `webman/tools/api-parity.py` 按「不再保留 Go 参照」的决策一并删除，
契约结论固化在两套测试里（离线 93 断言 + 连库 70 断言）。

## 删除的 Go 代码

| 范围 | 文件 |
|---|---|
| 接口包 | `backend/internal/api/`（api.go、services.go + 5 个测试） |
| 接口入口 | `backend/cmd/node-explorer/main.go` |
| read-through store | `store.NewReadThrough` 及 `readThrough` 全部分支、`AggregateContext`/`GetContext`/`History`、`postgres/live.go`（live.sql 的 go:embed 迁至 services.go）、`sqlite.go`/`postgres.go` 的 History 实现、`ingest_integration_test.go` 改直查 count(*) |
| 服务状态 | `service.Status`/`Statuses()`（唯一调用方是已删的 node-explorer）、`status_test.go`、`live_test.go` |
| 分页类型 | `domain/pagination.go`（随接口包删除） |

删除前用「试移 + `go build ./...` + `go vet ./...`」验证无残留引用；
`go test -race ./...` 13 个包全绿（WSL ubuntu-gn，PG16 测试库）。

## webman 实现要点

- **读路径一条 SQL**：`LiveResults` 用 `MATERIALIZED` CTE 先收敛过滤/排序/分页，
  再 JOIN 一次取 payload。不能把 `node_explorer_live_nodes` 放进 JOIN 内侧按主键
  取值——生产实测那样写 `limit=50` 单请求 12 秒。
- **必须照抄的派生逻辑**：`IpClassification`（按 `classification.types` 投票重算
  state 并重写 attributes）；`connectivity`/`exit`/`lifecycle` 是结构体投影
  （字段序 + omitempty），不是 jsonb 透传。
- **心跳**：Go 版由 node-explorer 起 10s ticker 写 `node_explorer_service_status`。
  删除后看板 `api` 行会永远「服务尚未启动」。webman 改为 `Heartbeat` 惰性补写
  （`ServiceBoard::statuses()` 入口触发，两次写入至少间隔 10s，失败只记日志），
  页面轮询即驱动；页面全关 90s 后显示「心跳已过期」符合事实。
- **已知表示差异（有意不修）**：源文件显式写零值（`country: ""`、`media: []`、
  `reality: false`）时输出形态不同；生产 705/705 节点都带非空 country，不触发。

## 时区（本次明确）

`config/app.php` 的 `default_timezone` 默认 **`Asia/Shanghai`**（可用
`APP_TIMEZONE` 覆盖），Dockerfile `TZ=Asia/Shanghai` 一致。这只影响 PHP 侧
时间戳渲染偏移与日志时间；库里是 `timestamptz`，查询两侧都不经过它。
实测 `/api/services` 的 `updatedAt` 输出 `+08:00` 偏移。

## 部署变化

| 文件 | 变化 |
|---|---|
| `Dockerfile` | 新增 PHP 运行时 stage（php:8.4-cli-alpine + pcntl/sockets/pdo_pgsql/mbstring/opcache/tzdata），composer stage 按 lock 装 vendor；CMD `php start.php start`；HEALTHCHECK 探 `/api/health`；node-worker 仍是 Go 交叉编译 |
| `.dockerignore` | 放行 `webman/`（排除本地 vendor/runtime/composer.phar） |
| `compose.yaml` | node-explorer 无 command（用镜像默认 CMD），`tmpfs` 增加 `/app/webman/runtime`（read_only 下 Workerman 要写 pid/status/log，file 缓存也在 runtime/cache） |
| `release.yml` | 新增 PHP 测试步（setup-php 8.4 + composer + 离线 93 + 独立库集成 70）；冒烟容器加 `--tmpfs /app/webman/runtime:mode=0777`；tar 清单加 `webman`（排除 vendor/runtime/composer.phar） |
| `package.json` | `dev:api`/`start` → `php webman/start.php start`；`build:backend` → `build:worker`；新增 `test:api` |
| `.env.example`（根 + webman） | 增加接口段：`APP_TIMEZONE`、`API_WORKERS`、缓存秒数 |

**容器监听**：PHP 读 `API_HOST`（不是 Go 时代的 `HOST`），Dockerfile 设
`API_HOST=0.0.0.0`——否则容器里监听回环，compose 端口映射（DNAT 到容器网卡）
打不进来。本地开发默认 127.0.0.1 不受影响。

## 验证

- Go：`gofmt`（1.26.1，按 index LF 内容判定，Windows 工作区 CRLF 是 autocrlf 假象）
  + `go vet` + `go test -race ./...` 全绿。
- PHP：离线 93 断言（Windows PHP 8.5 + WSL PHP 8.4）；集成 70 断言（WSL PG16）。
- 真实起服务冒烟（WSL，PHP 8.4 + pdo_pgsql）：`/api/health` 200、`/api/results`
  返回夹具数据、`/api/services` 的 api 行 healthy 且心跳落库、`/api/history` 404、
  静态 404。日志与文件缓存确认落在 `runtime/`。
- YAML：`release.yml`/`compose.yaml` 解析通过；`npm run test:api` 通过。

## 未决与风险

- **本地无 Docker**：镜像构建与 read_only 冒烟未在本机验证，靠 CI（tag 触发）。
  CI 冒烟已显式加 runtime tmpfs；compose 用默认 1777（docker 文档记载的默认值）。
- 生产 `.env` 会覆盖 compose 默认值（历史踩过 PGPOOL_MAX）：上线时如需调
  `API_WORKERS`/`APP_TIMEZONE`，记得两边改。
- 索引里遗留的未合并条目（`docs/node-list-数据保留与清理策略-2026-10-17.md`，
  无 MERGE_HEAD 的陈旧冲突态）已按「保留工作区文件」解决。
