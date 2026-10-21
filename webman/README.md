# webman 接口层

node_list 网页接口的 **PHP / webman 实现**，目前是接口的**唯一实现**——
Go 版的接口层（`backend/internal/api` 与 `cmd/node-explorer`）已经删除，
采集、调度、存储仍在 Go（`node-worker`），本实现只负责 HTTP 面。

接口契约来自 Go 版，且经过逐字节比对固化：同一份路径、状态码、JSON 字段名与顺序、
中文文案、`Cache-Control`，前端 `src/App.tsx` 不需要任何改动。

> 这不是把 Go 代码逐行翻译成 PHP。Go 那边的"读一份内存快照再在内存里过滤分页"
> 搬不过来，也不可能搬——本实现按 webman 的方式重新组织了职责划分（见下面
> 「为什么这样写」），只有**必须一条 SQL 一个快照**的那段查询是照抄的。

---

## 接口面

| 路径 | 方法 | 状态码 | 响应 | 缓存头 |
|---|---|---|---|---|
| `/api/health` | GET | 200 | `{"status":"ok"}` | 无 |
| `/api/results` | GET | 200 | 实时视图 + `sourceWarning` + `ippureCollector` | `no-store` |
| `/api/downloads` | ANY | 404 | `{"error":"订阅下载暂不可用。"}` | 无 |
| `/api/download` | ANY | 404 | `{"error":"订阅下载暂不可用。"}` | 无 |
| `/api/*` | ANY | 404 | `{"error":"接口不存在。"}` | 无 |
| `/`、`/**` | GET | — | `public/` 下的前端产物 | 由框架静态处理决定 |

`/api/results` 的参数语义（契约来自 Go，含几个反直觉的地方）：

- **既没有 `page` 也没有 `limit` → 整份返回**，`page=0 / limit=0`，且**所有筛选/排序参数被忽略**。
  这是 Go `parseResultsQuery` 的早退行为，前端首屏依赖它。
- `limit` 上限 200；`page`/`limit` 必须是正整数；`page` 越界时夹到最后一页（`least/greatest`）。
- `sort` 白名单：`preferred`（或空）+ 9 个枚举键；`direction` 只接受 `asc`/`desc`，缺省 `desc`。
- `features` 只接受 `tls`/`reality`/`udp`，多值是 **AND**；`media` 多值是**包含**判定。
- 任何不合法输入 → 400 `{"error":"节点分页、筛选或排序参数无效。"}`。
- `snapshot` 参数 → 400 `{"error":"仅支持查询当前节点状态。"}`。

服务状态面板及 `/api/services` 已移除。节点结果的采集器状态和来源告警由 `ResultsStatus` 读取；结果请求仍驱动 API 心跳（至少间隔 10 秒）。

---

## 目录结构与职责

```
start.php                 入口：读 .env → support\App::run()
support/bootstrap.php     启动脚本（复用框架内同一份实现，见文件注释）
config/
  route.php               路由表：/api 路由组 + 访问日志中间件 + 兜底
  api.php                 业务契约参数（分页上限、投票版本、缓存 TTL）——不是框架配置
  server.php / database.php / cache.php / log.php / static.php / exception.php / container.php
  bootstrap.php           启动期钩子（现为空，故意留白以免有人往 start.php 里加代码）
app/
  controller/
    ResultsController.php   GET /api/results：参数校验 → 查询 → 合并附属信息
    HealthController.php    GET /api/health（不带 no-store）
    DownloadController.php  /api/download(s) 的 404 占位
    StaticController.php    GET / → public/index.html（框架静态处理不映射目录）
  service/
    ResultsQueryParser.php  查询参数 → 类型化 ResultsQuery（白名单唯一入口）
    LiveResults.php         实时视图读取：一条 SQL 完成元数据/facets/过滤/分页/载荷
    NodePresenter.php        节点 JSON 形状投影（复刻 Go 的 marshalJSONShape）
    ResultsStatus.php        采集器状态 + 上游告警
    Heartbeat.php            api 服务心跳（惰性写入，替代 Go 的 10s ticker）
    ReadCache.php            PSR-16 缓存包装：记住 null、失败不缓存、缓存故障降级
    JsonResponse.php         JSON 响应（charset + no-store）
    ApiError.php             业务异常（状态码 + 错误码）
    Condition.php            查询条件值对象（SQL 片段 + 绑定量成对）
    Country.php              地区代码 → 中文名（由 tools/gen-country-names.py 从 Go 生成）
    IpClassification.php     网络类型投票 → state / attributes（移植自 Go 的 ipcheck/voting.go）
    ResultsQuery.php         readonly DTO
    enum/                    SortKey / Feature / FacetKind / Direction
  model/ServiceStatus.php   只读 Eloquent 模型（只在 sourceWarning 这种单行读上用）
  exception/ApiErrorHandler.php  统一异常 → JSON
  middleware/AccessLog.php  路由组级访问日志（PSR-15）
  functions.php             env() 助手（webman 本身没有）
tests/
  run.php                   离线单测（不需要数据库）
  integration.php           连真实 PG（node_explorer_live_* 视图）的契约测试
  fixtures/                 最小 schema + 7 个节点的夹具 + 一键导入脚本
tools/
  gen-country-names.py      从 backend/internal/domain/countries.go 重新生成 Country.php
  sync-frontend.sh          仓库根 dist/ → public/
```

---

## 为什么这样写（不是 Go 的逐行翻译）

Go 版本的读取路径是「把整份快照加载进内存，然后在内存里过滤、排序、分页」。
PHP 侧如果照做，每个请求要拉全部节点，代价不可接受。所以职责是这样切的：

| 关注点 | Go 的做法 | 本实现的做法 |
|---|---|---|
| 过滤/排序/分页 | 内存遍历 | **一条 SQL**（`LiveResults`）内完成，数据库只回一页 |
| 参数校验 | 命令式 if 链 | `ResultsQueryParser` 收口白名单 + PHP 8.1 枚举 `tryFrom` |
| 排序键 → SQL | `map[string]string` | `SortKey` 枚举，`match` 穷尽分支，新增键漏写即编译期报错 |
| 布尔筛选 | `map[string]bool` 白名单 | `Feature` 枚举（`column()`/`label()`） |
| facet 标签 | `switch` 里的 `strings.ToUpper`/`CountryName` | `FacetKind::label()`，分桶顺序由枚举保证 |
| 查询条件拼接 | `[]any` + `&args` 引用传参 | `Condition` 值对象（片段与绑定成对，不可能错位） |
| 缓存 | 进程内 `ttlCache`（每进程一份） | `support\Cache`（file 驱动，**多 worker 共享**），失败不缓存 |
| 单表读一行 | 裸 SQL | Eloquent `ServiceStatus::query()->find()` |
| 异常 → 响应 | 手写 `writeJSON` | `ApiError` + 全局 exception handler，控制器里没有 try/catch |
| 依赖获取 | 构造函数 | **控制器方法参数注入**（webman 按类型从容器解析） |
| 访问日志 | middleware 包一层 | 路由组中间件（静态资源不写日志） |
| 服务心跳 | 10s ticker 无条件写 | 跟着请求惰性写（`Heartbeat`，跨平台行为一致） |

SQL 那段是刻意照抄 Go 的 `liveRows` + `QueryPage`：它用 `MATERIALIZED` CTE
先把过滤/排序收敛成行号表，再 JOIN 一次取 payload。**不能**把
`node_explorer_live_nodes` 放进 JOIN 内侧按主键逐行取值——那是视图逐行重算，
生产实测 `limit=50` 时单请求 12 秒。改这段之前先读 `LiveResults` 顶部的注释。

---

## 本地运行

```bash
cd webman
cp .env.example .env          # 按需改 PG* 与 API_PORT
composer install
php start.php start           # 前台运行；Linux 可加 -d 后台
```

环境变量与采集器同名同义（compose 的 x-environment 直接复用）：
`PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD`、`API_PORT`、`IPCHECK_VOTING_VERSION`。
额外有 `API_LISTEN`（整段监听地址）、`API_WORKERS`、`API_*_CACHE_SECONDS`、
`APP_TIMEZONE`（默认 `Asia/Shanghai`，只影响 PHP 侧时间渲染偏移）。

```bash
php tests/run.php             # 93 条断言，无需数据库
```

集成测试需要一个可写的 PG 库（表与视图直接执行 `backend/` 的 `services.sql`
与 `live.sql`，不在本目录复制 DDL——与 Go 侧共用一份定义）：

```bash
PGURL="postgresql://postgres:test_password@127.0.0.1:5432/node_explorer_test" \
  bash tests/fixtures/load.sh
PGHOST=127.0.0.1 PGDATABASE=node_explorer_test PGUSER=postgres PGPASSWORD=... \
  php tests/integration.php   # 70 条断言
```

**Windows + WSL 里的 PG 有个坑**：WSL2 的 localhost 端口转发只在有 `wsl.exe`
客户端存活时有效，`wsl` 命令一退出，Windows 侧连 127.0.0.1:5432 就可能
`Connection refused`（WSL 内部 `pg_isready` 却是通的）。跑测试时先挂一个保活进程：

```bash
wsl -d <distro> -- sleep 300 &     # 撑住转发
```

集成测试会清空 `node_explorer_service_status` 表，验证无心跳时结果附属字段的响应。仅在测试数据库运行。

---

## 部署

1. **同步前端产物**：webman 的静态根是 `public/`，而前端仍是原来的 Vite 构建。

   ```bash
   npm run build
   bash webman/tools/sync-frontend.sh
   ```

   漏了这一步的表现是「接口正常、打开首页 404」。

2. **容器化**：仓库根的 `Dockerfile` 在同一镜像里构建 PHP 接口进程与 Go
   `node-worker`，`compose.yaml` 的 `node-explorer` 服务即本实现（command 为
   `php /app/start.php start`）。`runtime/` 挂 tmpfs，见 compose 里该服务的
   `tmpfs` 段——漏了会在 `read_only: true` 下起不来。

3. **进程管理（非容器）**：`php start.php start -d`（Workerman 自带守护），
   或交给 systemd。Linux 上 `API_WORKERS` 默认 4，Windows 强制 1（Workerman 的限制）。

4. **日志**：`runtime/logs/` 下按天切割保留 7 天（`config/log.php`），
   访问日志同样落在这里。容器化部署请把 `runtime/` 挂成卷或定期清理，
   否则 `stdout.log` 会随访问量线性增长。

5. **数据库**：必须能在启动时读到 `node_explorer_live_*` 视图；
   视图由 `backend/` 的 `live.sql` 维护，本实现只读不建。

---

## 已知且有意保留的差异

以下条目是 Go 版接口被删除前**逐字节比对**（55/55 用例：状态码、响应头、
原始 body 字节全部相同）确认过的契约细节，比对工具已随 Go 版一并退场，
结论固化在两套测试的断言里。改动输出形状前先读这里：

- **JSON 键序**：`jsonb` 的键序由 PostgreSQL 决定（按长度再按字节）；
  `media` 之类的内层对象不要假设原始顺序。
- **每个 JSON 响应末尾有一个 `\n`**（Go 的 `json.NewEncoder` 行为）。
- **错误体带业务码时键序是 `{"code","error"}`**（Go 用 map 序列化 → 字母序）。
- **失败响应也带 `no-store`**：Go 在 handler 入口就设好了响应头，报错时也在。
- **时间戳的时区写法**：按「本地时区当前偏移」渲染（UTC 部署出 `…Z`，
  +08 部署出 `+08:00`），零值哨兵在 +08 下会是 `0000-12-31T23:54:17+08:00`。
  前提：`config/app.php` 的 `default_timezone` 与部署时区一致。
- **`GET /`**：由 `StaticController` 显式映射到 `index.html`；
  `GET /index.html` 直接 200（Go 的 `http.FileServer` 会 301 到 `./`）。页面结果一致。
- **显式空值**：`"media": []` 这类「键在、容器空」的形态输出 `[]`，`null` 输出 `null`，
  与 Go 解码后的 nil/非 nil 切片语义一致。真正会被省略的只有 `omitempty` 且值为零的标量。
- **目录列举**：`dist` 里没有 `index.html` 时 Go 会输出文件列表，本实现返回 404
  （那是信息泄露，不打算复刻）。

---

## 排错备忘

| 现象 | 原因 |
|---|---|
| 启动即报 `Failed opening required .../support/bootstrap.php` | 缺 `support/bootstrap.php`（webman 骨架文件），见该文件注释 |
| `Declaration of ...process(support\Request ...) must be compatible` | PSR-15 中间件签名必须用 `Webman\Http\Request` / `Webman\Http\Response`（父类参数、父类返回） |
| 服务看板数字全是 0 / 字段互相覆盖 | pdo_pgsql 对无别名的同名表达式列会互相覆盖，聚合 SQL 必须给每列加别名 |
| bool 字段恒为 true | pdo_pgsql 返回 `'t'/'f'`，`(bool)'f'` 是 `true`，必须显式判 |
| `country` 全是 `unknown` | 视图 `country` 列的归一在 SQL 层，别在 PHP 里再算一遍 |
| 优选排序失效 | 出口证据要满足 10 分钟新鲜度；集成测试会把时间戳推到 2 分钟前 |
