# webman 接口层

PHP / webman 负责登录、节点查询和服务监控。采集、测活、出口和属性处理由独立 Go 服务负责，网页接口不触发这些任务，也不修改节点可用状态。

## 节点查询

首页请求 `GET /api/results`，接口每次读取 PostgreSQL 的实时视图，返回节点列表、统计和筛选项。视图用 `node_explorer_node_registry.available=true` 过滤不可用节点，可用状态只由测活服务维护。

- 不读取结果文件，不保存网页快照，不缓存节点查询结果；响应带 `Cache-Control: no-store`。
- 不附带来源告警、采集器状态、批次时间或快照编号。
- 单次请求在一致的只读事务内先读取筛选字段，再按当页 identity 获取节点展示数据，避免逐行重复计算视图。
- 搜索、筛选、排序、分页由 `ResultsQueryParser` 校验参数后交给 `LiveResults` 处理。未提供 `page` 和 `limit` 时返回全部可用节点；分页上限 200。
- `NodePresenter` 投影展示字段并剥离原始链接、生成链接和原始配置等凭据。登录后通过专用复制接口获取节点链接。
- 数据库读取失败返回 503，前端清空旧列表并展示重试入口。

采集只维护当前节点与去重、来源记录；测活直接读取 registry。旧整批快照与成员定位表通过 `deploy/sql/017-remove-snapshots.sql` 删除。服务监控每次请求读取当前心跳，不缓存、不生成历史采样占位。

## 其他接口

- `/api/health`：健康检查。
- `/api/auth/login`、`/api/auth/logout`、`/api/auth/session`：登录与会话。
- `/api/services`：独立服务监控页面使用，实时读取当前心跳，页面每 15 秒重新请求。
- 其余路由见 `config/route.php`；旧历史与订阅下载接口不可用。

## 对外接口

`GET /api/open/v1/countries` 返回可用国家及节点数，`GET /api/open/v1/nodes?country=HK&mode=standard&page=1` 返回指定国家的可用节点与分享链接，每页固定 10 条。标准模式按住宅、风险度升序、人类比例占优、原生逐级排序。

接口使用独立 `OPEN_API_APP_ID / OPEN_API_SECRET` 和 HMAC-SHA256 验签，PostgreSQL 原子 nonce 防重放；未配置默认关闭。升级先执行 `deploy/sql/023-open-api.sql`，数据库账号需要 nonce 表读写权限。配置、签名规范、字段和调用示例见 [对外接口文档](../docs/open-api.md)。

`Heartbeat` 随请求节流写入 API 自身心跳，不影响节点状态。

## 本地运行与验证

```bash
cd webman
cp .env.example .env
composer install
php start.php start
php tests/run.php
php tests/auth.php
php tests/open-api.php
```

数据库配置使用 `PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD`。监听地址使用 `API_LISTEN`，或 `API_HOST/API_PORT`；进程数使用 `API_WORKERS`。

集成测试只能在专用测试数据库运行，夹具会清空测试表：

```bash
PGURL='postgresql://postgres:test_password@127.0.0.1:5432/node_explorer_test' bash tests/fixtures/load.sh
PGHOST=127.0.0.1 PGDATABASE=node_explorer_test PGUSER=postgres PGPASSWORD=test_password php tests/integration.php
PGHOST=127.0.0.1 PGDATABASE=node_explorer_test PGUSER=postgres PGPASSWORD=test_password php tests/open-api-integration.php
```

数据库表和实时视图以 `backend/internal/database/schema.sql` 为准。

## 部署

仓库根目录运行 `npm run build`，再执行 `bash webman/tools/sync-frontend.sh` 同步到 `public/`。生产容器由根目录 `Dockerfile` 构建，`compose.yaml` 启动 PHP 接口与独立 Go 服务。
