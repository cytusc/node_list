# 独立出口 IP 属性服务

状态：已重写并完成本地验收，尚未部署。日期：2026-10-26。

入口：[node-attributes](../../backend/cmd/node-attributes/main.go)。实现：[internal/attributes](../../backend/internal/attributes)。Compose 名称保留 `exit-attributes`，命令改为 `node-attributes`。

## 服务边界

仅依赖 PostgreSQL 与供应商 HTTP API。自行轮询缺失数据；没有 Redis 队列、manager 调用、代理节点租约、下游任务、恢复或退役联动。属性请求直接从服务所在服务器发出，目标明确携带出口 IP，不通过待测节点转发，也不重新探测出口。

只写 `node_explorer_ip_attributes`、自有 `node_explorer_attribute_attempts`、`node_explorer_attribute_limits` 和自身 `node_explorer_service_status` 心跳。注册表、节点可用状态、出口事实、测活、生命周期、历史快照和通用任务均只读或不访问。住宅分类结果不再回写 `historical_residential`。

复用现有 `ipcheck` 供应商客户端与分类模型、`collector` 的 IPPure 协议客户端和 ASN 缓存；HTTP 重试使用现有第三方 `go-retryablehttp`，PostgreSQL 使用 `pgx/v5`。没有另写供应商协议、通用任务系统或代理内核。

## 输入与补缺规则

每秒有空闲 worker 时，扫描注册表和出口事实，满足以下全部条件才入选：

1. 至少一个节点 `available=true`。
2. 出口 `verified=true`、无错误、payload 与列中的 IP 一致。
3. 出口 incarnation 等于当前注册表 incarnation，避免复用旧节点记录的出口。
4. IP 是有效公网地址；排除私网、回环、链路本地、组播、保留/文档地址以及 CIDR、zone 和映射 IPv4 表示。
5. 该 IP 缺少分类或风险结果，且自有重试时间已到。

旧出口没有 incarnation 或 verified 时暂不处理，等待独立出口服务重新核验。注册表是可用状态的唯一输入，不要求历史快照或旧 connectivity 记录存在。不用节点服务器地址代替出口 IP。

以 IP 为去重键，多节点共用一个 IP 只查询、保存一份属性。分类与风险分别判断缺失：

| 组件 | 已有数据判定 | 缺失时行为 |
| --- | --- | --- |
| 网络分类 | classification.types 非空，或历史 attributes 含明确网络类型标签 | 调用配置的 ipcheck 来源，汇聚网络类型和来源证据 |
| IPv4 风险 | status=success | 调用 IPPure，补 ASN、组织、国家、来源、评分、标签和可选流量统计 |
| IPv6 风险 | 当前供应商不支持 | 不发送风险请求；仍可补网络分类 |

住宅、机房、商业、移动等明确类型都算已获取；不强制住宅。只有失败、配额冷却、空证据或“类型未知”不算成功分类。已有的成功组件不重新查询、不按 TTL 周期刷新，只有缺失组件重试。可选 ASN 流量统计的 no-data / query-failed 不会让已成功的风险评分反复重查。此服务不承诺对已有属性长期刷新；其职责就是补缺。

失败保留已有成功信息和对应查询时间；风险尝试状态另存。分类成功但风险失败时只会再次请求风险，反之亦然。类型证据与风险结果保持现有读取结构兼容；分类仍使用现有来源汇聚规则，不以一次 HTTP 成功或 unknown 结果代替明确类型。

## 调度、限流与提交

- 默认 3 个 worker、每 IP 总预算 45 秒，最多在途 worker 数量的任务；同 IP 不并发。
- 缺失组件失败或未完成后默认冷却 1800 秒，自有尝试表持久化，重启不会立即重试。
- 供应商客户端保留来源隔离、缓存、配额头和 Retry-After；冷却时间另存自有 PostgreSQL 表，重启保留。Redis 限流依赖已删除。
- IPPure 请求序列全局串行，成功序列间隔至少 1.5 秒；失败后对该上游冷却 30 分钟，避免为每个 IP 重复放大故障。HTTP 429/5xx 重试仍受单 IP 总预算约束。
- 执行前重检是否仍符合输入条件。提交前重新查询并对仍在使用该 IP 的一个可用节点及其出口行加共享锁，持有到提交。
- 若最后一个可用使用者已失活、换 IP 或 incarnation 不匹配，丢弃迟到结果；若另一个可用节点仍使用同 IP，结果仍可保存。
- 使用与旧属性 worker 一致的 PostgreSQL advisory lock，拒绝双实例；事实写入和心跳全部经持锁独占连接串行执行。所有权连接丢失后旧实例不能提交。
- 每 5 秒心跳，30 秒过期；退出取消并等待全部任务，自身心跳标记停止。供应商失败不导致节点变不可用。

## 配置与启动

```sh
go -C backend run ./cmd/node-attributes
go -C backend run ./cmd/node-attributes -health
npm run build:attributes
```

| 变量 | 默认值 | 约束/用途 |
| --- | --- | --- |
| PGHOST / PGPORT / PGDATABASE / PGUSER / PGPASSWORD | 标准 PG 环境 | PostgreSQL 16+；独立小连接池 |
| ATTR_WORKERS | 3 | 1～32 |
| ATTR_TIMEOUT_SECONDS | 45 | 5～120 秒，单 IP 总预算 |
| ATTR_RETRY_SECONDS | 1800 | 10～86400 秒，仅缺失组件 |
| IP_PROVIDERS | 空=现有全部来源 | ipinfo,ip-api,ipregistry,abuseipdb,ipapi,ip2location 子集 |
| IPPURE_API_ORIGIN | https://api.123169.xyz | 直连风险供应商 |
| IPINFO_TOKEN / IP_API_PRO_KEY / IPREGISTRY_API_KEY / ABUSEIPDB_API_KEY | 空 | 对应来源凭证 |

显式配置的 IP_PROVIDERS 若没有任何有效来源，启动失败。供应商现有客户端负责解析、认证和重试；部分公共源可能拒绝服务器出口，请配置供应商凭证或可用来源，不通过重新引入节点租约解决。

不运行共享 schema 迁移。启动只建立自有属性输出、尝试和冷却表；注册表、出口和公共心跳表需预先按共享契约建立。

## 删除与精简

- 删除旧 node-worker 的 exit-attributes 角色和 Config / Runner / admission / probes / schedule / paths / observations / limits 中的旧任务执行实现。
- 删除风险承载节点筛选、manager 风险租约、代理重试和直连兜底链，改为唯一直接查询路径。
- 删除属性/风险任务自续期、优先级通道、任务补投、住宅历史标记回写和无调用的 Redis taskqueue / ratelimit 包及依赖。
- 删除旧 attributes-repair 命令；删除覆盖历史和不可用节点属性的 ip-type-clean 命令及部署脚本，避免继续绕过本服务写属性。
- 删除无调用的 IPPure carrier 判断和仅供旧清洗使用的类型查询入口。
- 保留供应商适配器，旧只读审计命令已删除。四个独立业务服务均不再依赖 manager；后续按用户要求删除 manager 及共享 Mihomo / Redis 部署，见 [退役记录](11-project-cleanup.md)。

## 验收与切换

隔离 PostgreSQL 测试不建立 snapshots、connectivity、work、Redis 或 manager，供应商测试使用受控响应，不消耗生产配额。

- [x] 只选可用、已验证、当前 incarnation 的有效公网出口；共享 IP 去重。
- [x] 已完成组件不重查；失败重试冷却；商业分类与 IPv6 行为。
- [x] 最后可用使用者失活、换 IP、换 incarnation、出口失效时不提交；其他可用使用者仍存在时可以提交。
- [x] 不写节点可用状态，不产生任务或依赖快照；部分成功字段保留。
- [x] 供应商冷却持久化、重复实例拒绝、失锁禁止提交、停止心跳失效。
- [x] 并发上限、执行前复检、取消后等待全部任务退出。
- [x] 全后端 race 测试、go vet、10 项部署脚本测试、新二进制构建及 Compose 属性依赖断言通过。
- [ ] 生产镜像及容器验收（本机无 Docker）、生产配额/直连成功率观测和部署。

切换先停止旧 exit-attributes worker，再部署新二进制。旧 attributes / risk 工作表记录和 Redis 引用不再消费，不阻塞自有扫描；本轮不连接生产删除历史队列或属性。回退必须先停新服务再恢复旧版本，不能混跑两套写入者。保留新增表可供回退后排查。
