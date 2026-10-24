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
