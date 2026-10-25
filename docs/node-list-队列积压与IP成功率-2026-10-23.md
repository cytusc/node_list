# 队列积压与 IP 数据成功率：根因与处置（2026-10-23）

## 现象

| 通道 | 到期积压 | 头部等待 | 吞吐 |
|---|---|---|---|
| attributes | 1576 | 2084s | 约 70/分钟 |
| risk | 1695 | 2257s | 约 50/分钟 |
| exit | 1110 | 1240s | 约 30/分钟 |

供应商成功率（近 30 分钟，以落库证据为准）：

| 供应商 | 成功 | 冷却 | 成功率 |
|---|---|---|---|
| ip-api | 953 | 1080 | 47% |
| ipinfo | 5 | 1972 | 0.2% |
| ippure | 843 | — | 42% |
| ipregistry / ipapi / ip2location / abuseipdb | 76 / 0 / 0 / 0 | 均约 1850 | 未在调用列表内，证据是历史残留 |

`IP_PROVIDERS=ipinfo,ip-api`，所以后四家的 `quota-cooldown` 是
`mergeAttributes` 一直保留下来的旧证据，不是当前在查询。

## 根因一：批量端点被要求跑两倍速

`PrimeIPAPIBatch` 用 1.6s 的间隔打 `ip-api.com/batch`，而免费额度下
`/batch` 的每分钟请求数远低于单 IP 端点。实测从容器内连打：

```
try1 200 X-Rl: 4 X-Ttl: 6      try2 200 X-Rl: 3      try3 429
```

窗口快耗尽时 `X-Rl` 已经是个位数，代码只在 `X-Rl == "0"` 时才退避，且
退避量是固定的 `batchInterval`，不看 `X-Ttl`。于是批量几乎每两三次就
429，而 429 写入的是和单 IP 路径同一个 `ip-api` 冷却键——单 IP 查询也被
一起冻结。两头都拿不到数据。

## 根因二：查不到就被判成「五分钟后再试」

`attributesRetryDelay` 原本只区分「有类型」和「没类型」：

```go
case typed == 0:
    if priority > 0 { return residential }  // 300s
    return 30 * time.Minute
```

配额被打满时 `typed` 恒为 0，于是一批地址按 5 分钟回流。3600 个地址里
约三分之一落在这个分支，等于每分钟制造上百次注定失败的请求，把 40 次/
分钟的配额继续吃光。**积压是自造的**：通道看起来在忙，实际在空转。

## 根因三：risk 通道对着宕机的镜像反复试探

`api.123169.xyz` 对所有来源都返回 `200 {"ok":false}` 或裸 403，属于镜像
自己上游挂了。但每个 risk 任务仍会串行试 3 个载体、再补一次直连，单任务
最多 40 秒；失败后住宅通道 5 分钟就重排。约 1120 个住宅任务在宕机期间
每分钟回流上百次。

另外 `risk()` 的直连兜底写成了 `if retried, e := ...; e == nil`，`e` 被
丢掉，`queryErr` 仍是 `errNoCarrier`。所以 v1.5.54 新增的
`upstream-refused` 从未真正落库，所有失败都显示成 `request-failed`。

## 处置

### v1.5.56

- 批量端点使用独立的 `ip-api-batch` 配额键，下限 4s。
- 两个端点都改为跟随 `X-Rl`/`X-Ttl`：剩余额度按窗口均摊，耗尽则等窗口
  重置，不再用固定间隔猜。
- 并发预取用 `singleflight` 合并。每个消费者原本各要一批 100 个地址，
  重复的批量请求正是配额耗尽的直接原因。
- `PendingAttributeIPs` 按 `due_at` 排序并强制包含当前 IP，让同一批地址
  被稳定覆盖，而不是每个消费者各取一个随机子集。
- `attributesRetryDelay` 区分「没答上」与「答了但没类型」：所有源都成功
  却没给出类型时答案稳定，不再 5 分钟后重问。
- attributes 消费者默认 1 → 3。

### v1.5.57

- `collector.IPPureCarrierIndependent`：`upstream-refused`、
  `signature-failed`、`http-403` 与出口地址无关，遇到即跳出载体循环并
  跳过直连；`timeout`、`429`、`5xx`、解析失败仍继续换载体。
- 直连兜底失败时保留其错误，`upstream-refused` 这才真正可见。

### v1.5.58

- 确定性失败的退避 30 分钟 → 2 小时。30 分钟下约 2500 个任务每分钟回流
  83 次，而通道只能处理 50 次，积压仍在涨。

## 效果

| 指标 | 处置前 | 处置后 |
|---|---|---|
| ip-api 成功率 | 47% | 92% |
| attributes 到期积压 | 1576 | 0 |
| attributes 头部等待 | 2084s | 0s |
| risk 失败可辨识度 | 全部 `request-failed` | `upstream-refused` 为主 |

## 仍未解决：缺少能给出 ISP 类型的稳定数据源

`ip-api` 只能给出 `hosting`/`proxy`/`mobile` 三个布尔位，**无法正面判定
住宅**。正面判定原本依赖 ipinfo 的 `as_type`，而 `ipinfo.io/widget/demo`
已经对所有来源 429，免费 `/json` 端点只返回 `org`，不含 `as_type`。

实测可用的替代（从生产容器内）：

| 端点 | 结果 |
|---|---|
| `ipinfo.io/widget/demo/<ip>` | 429 |
| `ipinfo.check.place/<ip>?db=ipapi` | 403（Cloudflare 页） |
| `api.ipregistry.co/<ip>`（带 Origin/Referer） | 200，含 `connection.type`/`company.type` |
| `api.ipregistry.co`（演示 key） | 约 6 次后 429，配额太小 |
| `api.ipapi.is`、`ipwho.is`、`freeipapi.com` | 200，但免费档不含 ASN 类型 |

**建议**：申请 IPinfo 免费 token（50k 次/月），在 `.env` 设
`IPINFO_TOKEN=...`。代码路径已经支持——有 token 时走
`ipinfo.io/<ip>/json`，响应里的 `asn.type` 会被 `ParseIPinfo` 解析成
`ASNType`，`typed` 立刻从 0 变 1，属性刷新窗口也从 6 小时回到 24 小时。
这是目前唯一不需要改代码、又能恢复住宅判定的做法。

## 追加：失败查询覆盖正确信息导致的数据丢失（v1.5.60）

### 现象

香港/台湾住宅节点成批消失。抽样 `1.36.11.143`（AS4760 HKT Limited，
香港电讯盈科住宅宽带）当前证据为：

```
ip-api    hosting=false   outcome=non-hosting   0 分（弃权）
ipinfo    quota-cooldown                       0 分（弃权）
ippure    request-failed                       0 分（弃权）
```

三源全弃权 → `state=unknown`。`LiveResults::isPreferred()` 只放行
`state==='residential'`，节点被直接过滤掉。

### 根因：失败行顶掉了成功行

`ipcheck.Score()` 按 provider 取**窗口内最新的一行**投票，不区分这行是
答案还是失败。于是同 provider 一次 `request-failed`/`quota-cooldown` 会
顶掉上一次成功答案：

- 该 provider 的 `Types` 票消失 → 汇总类型从「住宅IP」回落；
- `Result.Evidence` 只剩失败行，而 `mergeAttributes` 的 `all` 正是由
  `v.Classification.Evidence` 拼出来的 → **成功证据被永久丢弃**，
  后续每次合并都带着这份残缺历史，损失不可逆；
- `recoveryASN` 要求 `queryStatus=='success'`，也随之失去 ASN 证据。

`mergeAttributes` 另有两处同类问题：

- `v = *verdict`：风险查询成功时整体替换记录，把行里其它写入方的字段
  一并抹掉；
- `RiskQueryCheckedAt` 在失败时被刷成当前时间，让一份旧的成功结论看起来
  像刚刚查过，同时把 `RiskQueryStatus` 从 `success` 改成 `query-failed`。

在 IPPure 成功率只有 24%–35% 的环境里，任何一次刷新都可能命中失败，
住宅判定因此逐步被"洗白"成 unknown。

### 处置

- `Score`：每个 provider 分别跟踪「窗口内最新已答复行」与「最新一次尝试」。
  投票只用已答复行，失败行随结果一起保存用于诊断，**不能顶掉答案**；
  已答复行仍受 24h 新鲜度约束，过期照常不投票。
- `mergeAttributes`：新增 `applyRisk`，成功结论只覆盖风险字段
  （状态/ASN/组织/属性/分数/流量占比），不再整体替换；证据改为累加，
  每个 provider 至多保留「一条答案 + 一次尝试」，负载有界。
- `RiskQueryCheckedAt` 只随成功前进，新增 `RiskQueryAttemptAt` 记录尝试
  时间；`missingTrafficRatio` 的退避改看尝试时间。

### 已经丢掉的数据：`attributes-repair`

预防只能止损。状态回落到 unknown 的记录不会自己想起来，但
`node_explorer_node_registry.historical_residential` 还在。新增命令：

```bash
# 默认只读，只统计
PGHOST=... go run ./cmd/attributes-repair
# 入队，默认单次 200 条，priority=0 走普通通道
PGHOST=... go run ./cmd/attributes-repair -apply -limit 200
```

筛选条件：`available` 且 `historical_residential` 且当前非 residential，
且证据里**没有**任何一条明确的机房答复（`outcome` 为
`datacenter`/`provider-conflict`，或 `hosting=true`）。已被正确改判为
机房的节点因此不会被反复重查。

服务器上没有 Go，静态编译后拷进容器运行：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/attributes-repair ./cmd/attributes-repair
scp /tmp/attributes-repair root@<host>:/tmp/
docker cp /tmp/attributes-repair node_list-node-explorer-1:/tmp/
docker exec node_list-node-explorer-1 /tmp/attributes-repair          # 只读
docker exec node_list-node-explorer-1 /tmp/attributes-repair -apply   # 入队
```

### 回归用例

- `ipcheck.TestFailedLookupCannotEvictAnAnswer`：`request-failed` /
  `quota-cooldown` / `http-error` 三种失败都不能顶掉同 provider 的答案，
  且答案要能经 `Result.Evidence` 往返后继续生效。
- `ipcheck.TestNewerAnswerReplacesOlderAnswer`：更新的答案仍可改判。
- `service.TestFailedRefreshKeepsResidentialEvidence`：住宅结论与
  「住宅IP」标签在失败刷新后保持不变，成功证据仍在数组里。
- `service.TestFailedRiskRefreshPreservesScoreAndRecordsFailure`：失败
  只移动尝试时间，不动状态/分数/结论时间。

## 追加：死节点二验与删除（v1.5.62）

### 现象

注册表 47141 个 available 节点里 45614 个（96.8%）连通性不可达；它们每 30 分钟
被全量 sweep 探一遍，同时被 normal-recovery 每 6 小时起评估一次，全都在做注定
失败的探测。`node-cleanup` 服务心跳正常、队列却是空的，`delete` 事件停在
10-23 03:16。

### 根因：闸门写死，条件永远不成立

`scheduleCleanup` 的入队条件是
`greatest(first_seen_at, coalesce(last_available_at, first_seen_at)) < now() - interval '7 days'`。
而 `first_seen_at` 是 10-18 加列迁移
（`ALTER TABLE ... ADD COLUMN IF NOT EXISTS first_seen_at timestamptz NOT NULL DEFAULT now()`）
统一刷新的：**最老的节点只有 6 天**，7 天窗口一天都不可能满足，所以队列永远为空，
二验从未发生过。另有三处吞吐上限：单次调度 `LIMIT 256`（配合 30 秒周期 ≈ 512 个/小时）、
`Publish` 给 `node-cleanup` 的 lane 深度写死为 **1**（同一时刻只放一个任务出队，
其余任务永远排在窗口外）、以及 strict 探针 lane 在内核里只有 8 路容量。

### 机制本身是完整的（未改动）

`cleanup` 处理器已经是「二验 + 删除」：

1. 年龄闸门通过后建 `cleanup:<id>:<generation>` 会话，投一次 `KindLive` 探针；
2. `live()` 连续两次连通性失败，且**对照节点可达**（直连与控制节点各一次）才置
   `ConfirmedDead`——只有「我们网络正常、这个节点真的连不上」才算确认死亡；
3. `deleteUnavailable` 校验探针在 1 分钟内、会话结果与传参一致、注册表行锁 +
   快照表 EXCLUSIVE 锁、`first_seen/last_available` 仍满足窗口、无在跑的探针、
   没有比探针更新的连通性观察；
4. 通过后删 connective/exits/retired/probe_results/work/registry，写
   `deleted_nodes` 水印与 `delete` 生命周期事件，并把该 identity 从所在快照里摘除。

`recur` 会升 generation，所以每次延后重试都会拿到全新会话与全新探针，不会复用
过期证据。

### 改动

| 位置 | 改动 |
|---|---|
| `cleanupMinAge()` | `NODE_CLEANUP_MIN_AGE_SECONDS` 取代写死的 7 天（默认仍 604800），三处 SQL 全部参数化，删除事件原因记录实际窗口 |
| `cleanupBatch()` | `NODE_CLEANUP_BATCH`（默认 256，上限 20000）。`Number()` 有 1–64 的并发语义，批量必须单独读 |
| `laneConcurrency` | `RECOVERY_LIVE_CONCURRENCY`（connectivity lane）与 `NODE_CLEANUP_CONCURRENCY` 可配，默认 2；删除吞吐 ≈ 探针 lane 并发 × 单价 |
| `services.sql` | `node_explorer_work((payload->>'identity')) WHERE state <> 'done'`，清理否决与准入 busy 检查原本每次任务全表扫 15 万行账本 |

### 线上参数与预期

服务器 `.env`：`NODE_CLEANUP_MIN_AGE_SECONDS=86400`、`NODE_CLEANUP_BATCH=2048`、
`NODE_CLEANUP_CONCURRENCY=2`、`RECOVERY_LIVE_CONCURRENCY=8`。

按 41346 个「超过 24 小时未见/不可用」的节点、单次探针约 15 秒（两次连通性检查
之间固定等下 `PROBE_RETRY_SECONDS`）估算，8 路探针约 1900 个/小时，积压约一天
排空；之后进入稳态——源批次重新发现的节点要重新走「3 轮失败 → 24 小时 →
受控探针」才会再次删除。

### 回归用例

- `TestCleanupWindowIsConfigurable`：2 天的节点在 24 小时窗口下必须入队；
- `TestCleanupDefaultsKeepAWeekOfGrace`：同样的节点在默认窗口下不得入队；
- `TestCleanupBatchReader`：默认 256、可配 2048、非法值与上限钳制；
- 既有 `TestCleanupDeletionVetoes` 的 11 个否决场景与
  `TestCleanupPhysicallyDeletesAndPreservesOtherNodes` 继续通过。

### 上线实测：第二个卡点是 Publish 的 lane 深度

v1.5.62 放开窗口后，`node_explorer_work` 里 2 分钟积了 12288 个 `node-cleanup`
任务，但 12287 个 `due_at` 已过期却始终 `attempts=0` —— 一个都没被领取。原因在
`Publish`：

```sql
WHERE lane_position <= CASE ... WHEN pending.kind='node-cleanup' THEN 1 ... END
 - coalesce(occupied.running,0)
```

`node-cleanup` 的投递深度写死为 1：最多只把一个任务放进 Redis，正在跑一个时
`1 - 1 = 0`，一个都不放。按每次探针约 15 秒算，这条 lane 每小时只能处理约
240 个节点，41346 个积压要跑一周。同一时刻第二个消费者也是空转。

改动（v1.5.63）：`NODE_CLEANUP_INFLIGHT`（默认 32）取代写死的 1，作为 Publish
的 lane 深度。strict 探针 lane 的内核容量本来就是 8 路，深度 32 已足够让它一直
有活干，再深只会增加轮询账本的次数。同时把「探针未就绪」的轮询间隔从 1 秒放宽
到 5 秒——strict 探针本身就要两次连通性检查，1 秒轮询只是在刷账本。

另一个本可以避免的坑：一开始用「在途任务数上限」限制准入，但延后一小时的任务
（活节点每小时复验一次）也算在途，会把窗口占满，导致新候选一小时进不来。准入
只负责把候选填进账本，真正限制并发的是 Publish 的 lane 深度。

### 第三个闸门：Claim 里的单飞准入（v1.5.64）

Publish 放开后仍不动：Redis 里明明有 12 个 ref，`attempts` 还是 0。原因在
`Claim` 的准入子查询：

```sql
AND (SELECT count(*) FROM node_explorer_work active
     WHERE active.kind=node_explorer_work.kind AND active.state<>'done'
       AND active.payload ? 'startedAt') < CASE WHEN kind='normal-recovery' THEN $6
       WHEN kind='node-cleanup' THEN 1 ELSE 2 END
```

`node-cleanup` 同样是写死的 1：**整个系统同时只允许一个带 `startedAt` 的清理任务
存在**。当时正好有一个任务的探针结果没落库（`cleanupGuard` 在节点配置变更后否决），
它每 5 秒重试一次、`startedAt` 一直挂着，把唯一的准入位锁死，于是四万多个任务
一个都领不到。

改动：该处也用 `NODE_CLEANUP_INFLIGHT`（`$7`）；另给「探针迟迟不落库」加了时间
上限——`startedAt` 超过 15 分钟仍未就绪就 `deferCycle`（`recur` 会清掉
`startedAt`，把准入位还回去），避免一个坏任务永久占位。

教训：`node-cleanup` 的并发在三层被写死过（Publish 深度、Claim 准入、消费者数），
改并发时必须三层一起看；判断「队列是否在动」要看 `attempts`、`published_at` 与
`due_at` 三者的关系，只看 `state` 会被「全部 queued」骗过去。

### 第四个瓶颈：共享内核的 strict lane 与「本地直连」的正确做法（v1.5.65）

前三个闸门拆掉后，队列真的会动了（在途 96、删除 ~7 个/分钟），但立刻撞上共享内核
的硬上限：`manager.go` 里 `strict: 8` 是**代码常量**（exit/risk 都有环境变量，
strict 没有），而单次 strict 探针是「两次 12 秒超时 + 10 秒间隔 + 对照节点」，
黑洞地址要 ~60 秒。8 路 × 60 秒 = 480 个/小时，四万积压要 90 小时；把在途提到 96
只会让 `connectivity` 队列堆到 61 个，不影响吞吐。

**关于「本地用 mihomo 独立绕过系统代理测一下」**：方向是对的，但有个陷阱必须先说清。
在本机直连测试是**无效**的——本机默认路由是 `utun4`，TUN 在本地代答所有 TCP，
连保留地址 `192.0.2.1:443` 都能 1 毫秒「连上」。抽 24 个被判死的节点在本机直连，
24/24 全部「成功」，延迟 1–2 毫秒，全是本地代答。所以探测必须在**容器侧**发起
（容器没有 TUN，直连走真实路由），或者在本地严格绕开 TUN 的路由。

因此 v1.5.65 把二次验证换成**容器内直连拨号**：

- `tcpVerifiable`：只对 TCP 传输生效（http/https/socks5/ss/ssr/vmess/vless/trojan/anytls）；
  hysteria2/tuic/hysteria 走 UDP，继续用内核探针。
- `verifyByDial`：先用一条最近可达的 TCP 节点做**出口对照**（拨通它证明本容器
  出网正常），再对目标做两次 6 秒直连，间隔 1 秒。两次都失败且对照成功 → 写入
  `ConnectivityState{Reachable:false, ConfirmedDead:true, ProbeTarget:"tcp-dial"}`
  作为新的受控证据，随后仍走**原来的 `deleteUnavailable` 全部门禁**（探针 1 分钟内、
  会话匹配、注册表行锁 + 快照表 EXCLUSIVE 锁、年龄窗口、无在跑探针、无更新观察）。
- 拨通（活着）→ `recordAvailable` 刷新可用时间并 `recur` 一小时，交回恢复通道。
- 不经内核、不占租约，单节点从 ~60 秒降到 ~13 秒，且并发由
  `NODE_CLEANUP_CONCURRENCY` 决定。

线上：`NODE_CLEANUP_CONCURRENCY=16`，约 4400 个/小时，四万积压约 10 小时排空；
UDP 那 1800 个仍走内核路径，慢但量小。

回归用例：`TestTCPVerifiableProtocols`（协议白名单）、
`TestCleanupDialVerificationDecidesByEndpoint`（活端点保留 / 死端点删除，走完整
`cleanup` 处理器与删除门禁）。

## 追加：死节点批量核验服务（v1.5.66）

### 为什么还要一个独立 mihomo

v1.5.65 的容器内直连解决了一半问题：TCP 端口不通的节点十几秒就能判死。但
Cloudflare 前置的 vless 节点（`172.67.*`、`104.24.*`、`162.159.*`）**TCP 永远连得上**，
后端早就死了也只能判「活」，于是每小时被延后一次。这类节点只能靠完整握手判死，
而共享内核的 strict lane 是写死的 8 路、单节点约 60 秒 —— 换句话说，生产内核不是
为「四万节点一次核验」设计的，也不该为它让路。

### 结构

新增服务 `dead-verify`（同一个镜像，自带 mihomo 二进制）：

1. `VerifiedCandidates`：可用 + 当前不可达 + 超过年龄窗口 + 无在跑探针 +
   最近 `recheck`（默认 6 小时）内没被核验过，按 `first_seen_at` 取一批（默认 400）。
2. `LoadVerificationProxies`：把本批节点（外加 3 个**出口对照**）渲染成 mihomo 配置，
   `PUT /configs?force=true` 灌进**自带的 mihomo**（独立控制器、独立端口，不碰生产内核）。
3. `ProxyDelay`：对每个 proxy 名调 `GET /proxies/<name>/delay`，让 mihomo 真正握手到
   `generate_204`；超时/非 200 就是「没答上」。默认 96 路并发、单节点 6 秒。
4. 出口对照：整批里只要有一个对照节点握手成功，才承认这批判定；否则整批作废
   （`egress control failed: wave discarded`）。这样容器出网故障不会变成批量误删。
5. 判定为死的节点交给 `Repository.DeleteVerifiedDead` —— 写一条
   `ProbeTarget=batch-verify` 的新鲜证据，然后调用**同一个** `deleteUnavailable`：
   探针 1 分钟内、配置摘要与行内一致、年龄窗口、无在跑探针、无更高优先级的
   新观察、注册表行锁 + 快照表 EXCLUSIVE 锁，一个都不少。`-dry-run` 只记录不删除。

### 关键取舍

- **判定与删除分离**：核验器只产证据，删除永远由门禁代码决定，和队列路径共用一份实现。
- **按 `recheck` 去重**：证据写在 `dead-verify:<md5(identity)>` 会话下，同一节点 6 小时内
  不会被反复核验，波次自然向前推进而不是反复测队头。
- **删除并发单独限流**（默认 8）：删除事务要拿快照表排他锁，并发太高会拖住导入和 sweep。
- 镜像里只有 mihomo 二进制（不做路由、不要 geo 库），配置由核验器自己渲染。

### 核验器上线的两处返工（v1.5.68）

- **对照索引错位**：波次把 3 个对照节点放在最前面，但配置渲染会跳过没有
  server/port 的节点，一旦跳过，`names[0..2]` 就不再是对照。日志表现为
  「egress control failed: wave discarded」而候选明明是好的。改为完全按
  `picked` 判断哪些名次属于对照、哪些属于候选。
- **单个非法代理拖垮整批**：mihomo 对不支持的参数返回 400 并整批拒绝，响应体
  里带 `proxy N:`。现在解析该下标、剔除该节点后重试（最多 4 次），一个坏节点
  不会让整批作废。

### 核验器实测（v1.5.69，上线后 1 小时）

- 一波 200 个候选：`alive=134 dead=58 deleted=18`，其余被门禁否决，原因已可读：
  `lock timeout 26 / not-safe 7 / guard 7`。
- 注册表 47235 → 45729，一小时内删掉约 1500 个，约 **1000 个/小时**，比之前
  队列路径的 150 个/小时快了一个量级。

**剩下唯一的瓶颈是删除事务本身**：`removeSnapshotIdentities` 每删一个节点都要重写
它所在的整张快照（快照内所有节点的 `node_index` 重新编号），实测单个删除事务
持有快照表 EXCLUSIVE 锁 1–3 秒。这意味着删除吞吐的上限是 `1/hold_time` ≈ 1800 个/小时，
再多的并发也只是排队（而且排队会撞上 3 秒的 `lock_timeout`）。

下一步的优化（未做）：把同一张快照的候选攒成一批、一次事务删完，快照只重写一次；
或者让批量路径跳过索引重排，只靠 `deleted_nodes` 水印阻止旧快照回流
（`syncRegistry` 的 `x.source_at<=d.deleted_at` 已经覆盖这一点）。这两条都能把
删除从 O(快照大小) 降回 O(1)。

### 决定性的那一处：窗口参数没传给核验器（v1.5.69）

`dead-verify` 只拿到 `DEAD_VERIFY_MIN_AGE_SECONDS`，没拿到 `NODE_CLEANUP_MIN_AGE_SECONDS`；
而删除门禁 `deleteUnavailable` 自己也会复核年龄窗口。于是候选按 24 小时选出、
门禁按默认 7 天判断 —— 95 个全被判「not-safe」。compose 里已给两侧显式传同一个值。

### 性能优化：删除降到 O(1)，快照压缩转后台（v1.5.70 – v1.5.72）

删除的瓶颈原本不在探测，而在删除本身：`removeSnapshotIdentities` 每删一个节点都要
重写它所在的整张快照、重排全快照的 `node_index`，单事务持排他锁 1–3 秒。于是删除
吞吐被锁在约 1800/小时，加并发只会互相撞 `lock_timeout`（实测 26/58 个死节点卡在
锁等待）。

改动：

- **删除不再碰快照**：`deleteUnavailable` 增加 `compact` 开关，批量路径传 false——
  既不重写 payload，也不抢 `LOCK TABLE ... EXCLUSIVE`（注册表 advisory 锁已经和
  source fold 串行化）。节点仍写 `deleted_nodes` 水印，回流由 `syncRegistry` 的
  `x.source_at<=d.deleted_at` 挡住。删除从此与快照大小无关。
- **压缩转后台按批**：`CompactDeleted` 每次取 400 个「已 tombstone 但仍在快照里」的
  identity，一次事务重写涉及到的快照；幂等，核验器每 5 分钟跑一次。
- **判活也落证据**：判活原来什么都不写，下一波又被同一个候选查询捞出来重测；随着
  死节点减少，这批活节点会吃掉整波容量。现在写入 `Reachable` 证据并刷新
  `last_available_at`，既进 recheck 去重窗口，也离开年龄窗口。
- 修 `removeSnapshotIdentities` 的边界：整张快照被清空时 `oldIndices` 编码成 NULL，
  `=ANY(NULL)` 匹配不到任何行，成员与 ippure 索引永远删不掉（压缩器会死循环）。

实测（400 个/批）：`alive=109 dead=285 deleted=266`，删除率 **15/分钟 → 56/分钟**
（约 3400/小时），注册表两小时从 47235 降到 45248。四万积压预计十余小时排空，
之后进入稳态。

**运维注意**：镜像自带 `HEALTHCHECK`（探 PHP 的 3001 端口），dead-verify 必须显式
`healthcheck: disable: true`，否则永远 unhealthy，会把 `up -d --wait` 的整批更新
拖垮并触发回滚（实测发生过一次）。心跳由程序每 20 秒写一次 `service_status`。

## 追加：候选池为什么会空（v1.5.73）

### 现象

`dead-verify` 日志刷 `wave did not load: names=3 picked=3 controls=3 candidates=0`，
CPU 只有 0.02%，删除率归零。看起来像卡死，其实是**没有候选可测**。

逐层拆开候选查询（`available AND reachable=false AND 年龄窗口`）：

| 条件 | 剩余 |
|---|---|
| `available` | 25868 |
| `+ reachable=false` | 24438 |
| `+ 超过 24 小时未见/不可用` | **8** |

原因：这 24438 个节点里有 **24382 个的 `last_available_at` 就在今天 00:01**
——它们是今天凌晨成批变成不可达的，还在 24 小时宽限期里。另有 8686 个的
`first_seen_at` 是 10-24 00:00（源批次重新发现的节点，identity 变了，算新节点）。
也就是说**这一轮的死节点是「新鲜死亡」，不是历史垃圾**，核验器没有活干是正常
的，不是故障。

一处顺带的可用性问题：`envInt` 要求值大于 0，所以
`DEAD_VERIFY_MIN_AGE_SECONDS=0` 会被静默回落成 86400，运维想「忽略窗口抽样」
时拿不到候选。改用 `envAllowZero`，0 现在真的表示忽略年龄门禁。

### 第二个问题：出口对照自证失败

忽略窗口抽样（`-min-age=0 -dry-run`，150 个候选）立刻撞上
`egress control failed: wave discarded (3 controls tried)`。原因是对照池来自
`connectivity.reachable=true` 的节点，而**这批节点恰恰是假活**——Cloudflare
前置的端点 TCP 永远连得上、后端早就死了。用假活节点当出口基准，等于用被测对象
证明自己没问题。

改用 mihomo 内置的 **DIRECT 出站**做对照（`GET /proxies/DIRECT/delay`）：

- 容器自己能握手到 `generate_204` → 本批的失败判定才成立；
- 完全不依赖库里的 `reachable` 标记，也不受假活污染；
- 实测容器直连 gstatic / cloudflare 均正常；
- DIRECT 不可达时才退回注册表对照，兼容只能经代理出网的部署。

### 环境事实

宿主 **4 核**、load average 6.18（`1Panel-postgresql` 一个容器占 257% CPU），
核验器自身 CPU 0.02%。所以「提 CPU / 提并发」在这个节点上没有收益——它是在等
候选、等对照，不是在算。真正的容量瓶颈依次是：候选池（时效）→ 出口对照
（已修）→ 注册表 advisory 锁（已把并发降到 6）。

### 真正的吞吐杀手：一个从未被创建的索引（v1.5.74）

删除节奏恢复后仍只有 **1.1 个/秒**（单事务约 0.9 秒），而且时间分布极其均匀——
不是抢锁成簇，是事务本身慢。逐条查执行计划：

```
Delete on node_explorer_work (actual time=639.9..639.9 rows=0)
  ->  Seq Scan on node_explorer_work (actual time=639.9..639.9)
        Filter: kind <> '{attributes,risk}' AND (payload->>'identity') = 'x'
        Rows Removed by Filter: 107840    Buffers: read=117742
```

**每删一个节点要全表扫 10.8 万行、1.2 GB 的 work 表，670 毫秒。**

两层原因，缺一不可：

1. 之前加的 `node_explorer_work_identity` 是**部分索引**
   （`WHERE state <> 'done'`），而清理用的这条 DELETE 只按 identity 过滤、
   不带 state 条件 → 条件不匹配，索引永远用不上；
2. 更致命的是它**从来没被创建过**——`Migrate` 只在 `schemaVersion` 变化时
   执行 `services.sql`，而改 SQL 时忘了 bump 版本。改 SQL 不 bump 版本是
   **静默失效**：服务照常启动，语句一行都没跑。

修复：

- 索引改为全量 `(payload->>'identity')`（DELETE 无 state 条件）；
- `schemaVersion` 从函数内常量提到包级并 bump 到 11，线上用
  `CREATE INDEX CONCURRENTLY` 立刻补齐（不锁表）；
- 四个迁移用例原本硬编码 `version=10`，改为引用 `schemaVersion`，以后 bump
  不必再动测试。

实测（同一波、同样 400 个候选）：

| | 索引前 | 索引后 |
|---|---|---|
| 单次 DELETE | 670 ms | **0.03 ms** |
| 删除速率 | 1.1 个/秒 | **9–12 个/秒** |
| 一波结果 | deleted=315（21 个 lock timeout） | deleted=392，**vetoed=0** |
| 10 分钟删除量 | ~660 | **5405** |

配合「宽限期收到 6 小时」（`NODE_CLEANUP_MIN_AGE_SECONDS` 与
`DEAD_VERIFY_MIN_AGE_SECONDS` 必须同值，否则门禁按默认 7 天把候选全部否决——
这个坑踩过一次），2.2 万积压从「十余小时」变成约半小时。

判断「队列是否在动」的正确姿势也因此补全了：**看 attempts、published_at、
due_at 三者的关系，而不是 state**；排查「慢」时先 `EXPLAIN (ANALYZE, BUFFERS)`
那条真正在跑的语句，别猜锁。

### 一个坏节点拖垮整波（v1.5.75）

索引修好后速率到 12 个/秒，然后突然归零，日志每 5 秒一条：

```
wave failed: load verification proxies: controller answered 400
```

mihomo 对整批配置是**全或无**：任何一个 proxy 的字段不被支持，就 400 拒绝
整批。原实现的补救是解析响应体里的 `proxy N:` 下标、剔除该节点再重试——但：

1. 正则锚定在行首（`^proxy ([0-9]+):`），而实际响应体在不同版本里会包成
   `{"message":"proxy 12: ..."}` 或别的形式，解析失败；
2. 解析失败就直接 `return err`，于是**同一个坏节点每一波都把整批带走**，
   删除完全停摆，而且候选查询的最老优先顺序不变，坏节点永远排在最前面。

修复：

- 正则去掉行首锚定；
- **解析不出下标时二分定位**：全量已知被拒、任何包含坏节点的子集也必然被拒，
  所以 `log2(n)` 次 PUT 就能定位（400 个候选约 9 次），剔除后重试，上限 24 次；
- 非 400 和最终的 400 都把响应体截断进错误信息，不再只留一个状态码。

教训：**外部服务返回「整批失败」时，不要依赖它的错误文案来定位**。文案会变、
会被包装、会本地化；二分是唯一稳的定位手段，代价只是 log n 次请求。

## 追加：稳态运行与「不可解析配置」的处置（v1.5.76）

### 一小时的实测

| 指标 | 数值 |
|---|---|
| 删除总量（1 小时） | 18770 |
| 峰值速率 | 16–18 个/秒（一波 400 个 18 秒删 372） |
| 注册表 | 31613 → 11325 |
| 剩余不可达 | 9445，其中 **9411 在 6 小时宽限期内** |
| 过窗口的候选 | 34（被下面的「不可解析配置」卡住） |

9411 这个数说明系统已经进入**稳态拉锯**：sweep 每轮探到可用就把
`last_available_at` 刷新，宽限期重新计时；到期后核验器再测一次，判死删除、
判活继续刷新。稳态核验量 ≈ 宽限期内的数量 / 6 小时，删除量 ≈ 其中真死的比例。

### 卡在 34 个的原因：配置无法被内核解析

日志每 5 秒一条，40 分钟 350 次：

```
wave failed: controller answered 400: {"message":"proxy 0: ss cf.090227.xyz:443
initialize v2ray-plugin error: 'mux' expected type 'bool', got unconvertible type 'string'"}
```

v1.5.75 把响应体打进日志，才第一次看到真正的原因。三个叠加缺陷：

1. **`len(config.Proxies) <= 1` 把整批判死**：剔除到只剩一个（恰好是坏的）时
   直接返回错误，于是每波从零重来，永远失败；
2. **文案里的 index 不可信**：剔除后它仍报同一个 slot（该 host 在库里其实是
   vless），照它剔除会连带丢掉健康邻居；
3. **剔除掉的节点没有记录**：候选按最老优先，下一波还是它排队头。

改法：

- 剔到 0 不再报错，改为返回 `dropped`（原始下标）由调用方收尾；
- 同一 index 连续出现两次即判定文案不可信，改用二分；
- 对 `dropped` 里的候选写一条 `ProbeTarget=config-rejected` 的**中性证据**
  （绝不判死），它因此进入 recheck 窗口、6 小时内不再占位。

原则没变：**配置问题只影响「能不能测」，不影响「死没死」**。判死依据始终是
真实握手 + DIRECT 出口对照 + `deleteUnavailable` 的全部门禁。
