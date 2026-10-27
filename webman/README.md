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

采集服务使用的原始数据批次与节点位置仍保留在数据库中，供采集和测活定位输入；它们不属于网页历史查询功能。

## 其他接口

- `/api/health`：健康检查。
- `/api/auth/login`、`/api/auth/logout`、`/api/auth/session`：登录与会话。
- `/api/services`：独立服务监控页面使用，保留 15 秒监控缓存。
- 其余路由见 `config/route.php`；旧历史与订阅下载接口不可用。

`Heartbeat` 随请求节流写入 API 自身心跳，不影响节点状态。

## 本地运行与验证

```bash
cd webman
cp .env.example .env
composer install
php start.php start
php tests/run.php
php tests/auth.php
```

数据库配置使用 `PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD`。监听地址使用 `API_LISTEN`，或 `API_HOST/API_PORT`；进程数使用 `API_WORKERS`。

集成测试只能在专用测试数据库运行，夹具会清空测试表：

```bash
PGURL='postgresql://postgres:test_password@127.0.0.1:5432/node_explorer_test' bash tests/fixtures/load.sh
PGHOST=127.0.0.1 PGDATABASE=node_explorer_test PGUSER=postgres PGPASSWORD=test_password php tests/integration.php
```

数据库表和实时视图以 `backend/internal/database/schema.sql` 为准。

## 部署

仓库根目录运行 `npm run build`，再执行 `bash webman/tools/sync-frontend.sh` 同步到 `public/`。生产容器由根目录 `Dockerfile` 构建，`compose.yaml` 启动 PHP 接口与独立 Go 服务。
