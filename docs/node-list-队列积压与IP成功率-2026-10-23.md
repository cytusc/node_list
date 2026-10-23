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
