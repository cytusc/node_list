# 扫描超时、补测与 204 判定

本文记录 2026-10-22 的源码修正，部署是否生效需以运行镜像为准。

## 超时预算

Mihomo 的 URL 测速仍请求 `timeout=2000`。控制器请求保留 12 秒预算，以容纳 DNS、拨号等开销；随后校验租约及本次检测记录最多 3 秒。Manager 统一限制容量等待、测速和校验合计最多 15 秒，调用方取消仍生效。扫描不再另设 8 秒超时，避免在有效观测返回前截断。

外层请求超时、容量不足及基础设施错误不构成节点死亡证据。新测试使用 8.1 秒返回的控制器，覆盖成功及明确失败均可保留。历史被 8 秒限制丢弃的节点数量无法由旧版汇总日志还原；不能把所有缺失观测都归因于这一超时。

## 补测与状态

全量扫描完成并成功提交后，缺失有效观测的节点进入内存补测集合。首次补测在轮次结束 30 秒后调度，连续无进展按 30、60、120、240、300 秒退避，上限 5 分钟；有有效观测后恢复 30 秒。调度还受主循环约 5 秒轮询、内核健康及配置重载影响。

补测重新读取注册表，只选择尚未完成的 identity，使用当前 incarnation/config；已退出来源的节点自然移除。已取得明确成功或失败的节点不会重复补测，避免重复累积失败轮次。明确拒绝的节点配置计入 `rejected`，不进入快速补测，等待后续全量轮次重新检查。正常全量扫描仍按完成时间加 `SWEEP_INTERVAL_SECONDS`（默认 1800 秒）安排，补测不能推迟该时间。

只有数据库提交成功才更新补测集合。事务失败或取消不清除尚待补测的节点。补测集合不持久化，进程重启后按原全量扫描调度重新建立。

管理状态中的 `sweep` 新增：

- `kind`: `full` 或 `retry`。
- `skipped` / `rejected` / `pending`: 本批缺失观测、配置拒绝、提交后待补测数量。
- `skip_reasons`: `busy`、`timeout`、`config_changed`、`config_rejected`、`cancelled`、`infrastructure` 的数量。
- `outcome=partial`: 本批有节点未获得有效观测，不再标记为 `completed`。
- `next_run_at`: 下一次全量扫描或补测的计划时间。

`candidates` 和 `observations` 表示当前批次；`kind=retry` 时不是上一轮全量覆盖率。状态里的 observations 是采集的有效观测数，最终落库仍经过 incarnation、配置、代次和时间戳校验。

## Mihomo v1.19.27 的 204 语义

上游 [`adapter/adapter.go`](https://github.com/MetaCubeX/mihomo/blob/v1.19.27/adapter/adapter.go) 的 `URLTest` 将 `expected=204` 的匹配结果写入 `extra[url].alive/history`，但不会因 HTTP 状态不匹配而返回错误。上游 [`hub/route/proxies.go`](https://github.com/MetaCubeX/mihomo/blob/v1.19.27/hub/route/proxies.go) 的 `/delay` 路由只根据错误和延迟是否为 0 决定响应。因此，200/403 可以得到成功响应；亚毫秒 204 因延迟取整为 0，又可以得到 503。

Manager 现在复用租约校验取得的 proxy 详情，以 `extra[测速URL].alive` 判断成功，并要求最新 history 的时间不早于本次测速请求的开始时间。缺少、损坏或过期记录作为基础设施错误补测。顶层 `alive` 和 `/delay` 的 HTTP 200 均不能单独证明目标返回 204；合法的 0ms 成功可被保留。

同一 identity 的测速串行收集证据，冲突请求得到 busy；不同 identity 继续并发。调用方取消时立即返回，但在后台有界排空原控制器请求后，才允许同一 identity 再次测速，避免取消请求的迟到结果覆盖下一次检测。没有增加第二次目标网络探测。

真实内核用例更名为 `TestRealManagerDelayRequiresExpected204`，纳入现有 CI 的 `-run TestRealManager`。用例覆盖立即响应和延迟响应下的 200/204/301/403/500，并重复 204 验证恢复。除此之外还覆盖超时、缺失/过期记录、同节点并发、取消后排空、补测退避、配置替换、节点退出、提交失败及全量周期不被推迟。

## 运行层边界

这些修改不调整 manager CPU 配额，也不清空出口检测队列。CPU 限流和队列到期积压须在部署后通过真实吞吐、扫描覆盖率及限流采样继续评估，测试通过不等同于生产积压已经消除。
