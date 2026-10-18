# 住宅出口复核与恢复

在用住宅节点默认每 **5 分钟**复检；自动退役前须连续 3 轮对照确认失败，每轮至少间隔 2 分钟；历史住宅节点的临时核验失败会退避复测；**一次完整验证通过就立即恢复**。不累计两次成功，不等待 30 分钟，不依赖普通节点整轮巡检。架构与部署见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)。

## 分类

按同一真实出口 IP 的有效证据判断，默认来源为 IPPure、IPinfo、IP-API、IPregistry、AbuseIPDB、ipapi.is 和 IP2Location。每家最多一票，使用类型优先，公司类型后备。失败、缺字段、限流弃权；`hosting=false` 不能推断住宅。最高票并列时，在并列项中依次优先住宅、商业宽带，其余平票为机房，全部弃权为未知。

`business` 映射商业宽带，`isp` 映射住宅，`hosting` 映射机房；其他明确用途保留原类型。使用类型和公司类型按 IPQuality 映射。VPN、代理、风险分数不直接当作机房票，也不凭运营商品牌猜测住宅。

只使用同 IP、24 小时内、当前分类版本的证据。每家选最新响应，包括最新未知或失败响应；旧成功不能覆盖新失败。保存 `classification.types`、`classification.evidence` 和来源时间，不通过刷新缓存时间延长原始证据有效期。IPPure 风险失败不阻塞其余六家分类。

## 自动恢复

仅对已知自动退役原因创建恢复任务，人工禁用排除。历史住宅优先处理。以节点身份和退役时间标识持久化任务。每次核验保存结果、时间、出口 IP、累计核验次数和最近 16 次结果。历史住宅节点的连通失败、出口核验失败、实际出口变化、超时或分类未确认，按 5 分钟、15 分钟、1 小时、6 小时退避，之后最多每 6 小时复测。每次复测提升任务代数，使用全新探测会话；来源失效、人工禁用、明确非住宅或机房 ASN 不会因此重试。旧版已有临时失败记录也适用。

恢复核验先读取数据库中的真实出口 IP 和对应 ASN。命中已复核的云／CDN／托管机房 ASN 时，持久化 `skipped-datacenter-asn` 后结束，不创建隧道探测或住宅分类依赖。排队任务会批量标记跳过，不占用有限的核验槽位。新取得出口后也再次应用相同规则，生命周期恢复事务最终再检查一次，防止旧恢复任务绕过过滤。

规则见 `backend/internal/service/recovery_policy.go`，依据 2026-10-16 生产库 ASN／机构汇总制定，覆盖 Cloudflare、阿里云、Datacamp、M247、Scaleway、OVH、Amazon、Oracle、DigitalOcean、Hetzner、Vultr、IONOS 等明确云与托管网络。不会根据机构名称模糊匹配或机房标签多数自动封禁整个 ASN；HKT、中华电信、联通等接入网络，以及归属不明确或混合用途的网络继续正常核验。该规则仅限制住宅恢复，不改写已有分类或风险分数。

已有出口时不使用节点服务器地址代替出口。未知出口或缺少 ASN 的候选仍需先获得出口／属性，不能只靠配置猜测。最后一次已观测出口命中机房 ASN 的退役记录会结束核验，因此不会继续轮询它未来是否换到住宅出口。

本轮须满足：

1. 经目标连接访问 GitHub 返回 HTTP 200。
2. 经同一连接访问 ipify 和 AWS CheckIP，两个 HTTPS 公网 IPv4 回显一致，并区别于宿主出口。
3. 当前出口的有效分类为住宅；可复用未过期的同 IP 属性，缺失时提交高优先级属性查询。
4. 分类后再次核对出口，IP 和出口版本都不变化。
5. 生命周期事务再核对近期成功事实、有效源快照，以及精确匹配的 identity／retiredAt／reason。

全部满足即解除自动退役。IPv6、出口变化、连通失败、属性未知／机房、证据过期、源快照已删除都不恢复。本轮最长 3 分钟，超时记录为 `timed-out`，不标为核验通过；单轮内依赖等待沿用同一会话，退避后的新一轮使用新会话，不把重复投递计为新实测。

核验结果保存在恢复任务的 `recoveryResult` 中：`skipped-datacenter-asn` 表示机房 ASN 已跳过，`eligible` 表示可请求恢复，`not-residential` 表示明确未满足住宅条件，`classification-unconfirmed` 表示分类尚不确定，`probe-failed` 表示首轮探测失败，`exit-probe-failed` 表示最终出口核验失败，`exit-changed` 表示实际出口或版本变化，`timed-out` 表示未在时限内完成。出口请求失败不会再被空 IP 比较误报为出口变化；错误包含代理或宿主路径、回显服务和脱敏后的失败类型。已完成任务保留，服务重启保留退避时间和历史。

测活每轮仍先尝试两次 GitHub 请求，并核对宿主和参考节点。只有连续 3 轮对照确认失败，且首末轮至少相隔 4 分钟，才允许生命周期服务退役。前两轮失败后每 2 分钟复测；成功或对照不足清零，超过 2 小时未续接的失败不累计。旧版单轮 `confirmedDead` 记录缺少多轮证据，不能直接触发退役。

恢复只改变退役标记，不伪造测速结果或新快照。每次状态变更保存生命周期审计，页面在投影刷新后自动显示。`LIFECYCLE_DRY_RUN=true` 只记录建议，不实际修改退役记录。

## 来源配置与配额

可选密钥为 `IPINFO_TOKEN`、`IP_API_PRO_KEY`、`IPREGISTRY_API_KEY`、`ABUSEIPDB_API_KEY`。未配置时沿用公开 widget／演示／公共转发入口；缺密钥不直接关闭恢复。`IPPURE_API_ORIGIN` 设置风险接口地址。

来源查询通过宿主 HTTP 客户端进行，配额由 PostgreSQL 跨工作进程协调：至少间隔 1.5 秒，IP-API 遵守 `X-Rl`／`X-Ttl`，429 遵守 `Retry-After`。本进程同时保留短期请求缓存。同 IP 多节点共享持久化属性与任务，缓存不能替代每轮真实隧道检查。

## 独立审计

`backend/cmd/residential-audit` 只读 PostgreSQL，使用独立临时 Mihomo。报告不包含节点配置或账户密钥。

默认 `--scope all` 读取全部历史候选并按连接身份去重；`--scope historical-residential` 限定历史住宅。先用 `--inventory` 查看清单。实测默认 `--limit 200`，用 `--offset` 分批，`--limit 0` 选择全部；精确复测可用 `--ids`。

审计使用相同的 `IPINFO_TOKEN`、`IP_API_PRO_KEY`、`IPREGISTRY_API_KEY`、`ABUSEIPDB_API_KEY`。`--input` 仅重新查询分类，不刷新 `probedAt`，不能制造第二轮实测；缺少检测时间的旧报告不能取得恢复资格。`--evidence` 合并前轮证据。`recoveryEligible` 仅表示历史审计器的只读证据条件，审计工具自身不修改退役表。

## 官方接口文档

- [IPinfo Company API](https://ipinfo.io/developers/company-api)
- [IPinfo Privacy API](https://ipinfo.io/developers/privacy-api)
- [IP-API JSON 与配额说明](https://ip-api.com/docs/api:json)

## 非住宅低频恢复

`normal-recovery` 是独立服务和持久化任务类型，默认每 6 小时复检一次（`NORMAL_RECOVERY_INTERVAL_SECONDS=21600`），完整核验并发固定为 1，探测任务仅使用普通通道。首次至少等待退役后一个间隔，旧积压额外错开最多 5 分钟；失败从完成时间计算下一次，重启不改变排期。每 30 秒最多补充 256 个候选。

从未确认住宅的自动退役节点，以及住宅复检明确得到 `not-residential`／`skipped-datacenter-asn` 的节点进入该服务。仍在执行的住宅核验先完成后交接；住宅临时故障继续保留高频退避恢复。非住宅恢复不要求住宅分类，不排除机房 ASN，但同样要求 GitHub 可访问、ipify／AWS HTTPS 公网 IPv4 一致且区别于宿主，以及二次出口和版本一致。分类与风险查询异步执行，失败不阻止普通节点恢复。

人工禁用、来源快照删除、连接不可用、退役记录变更、出口变化和过期证明仍受事务校验保护。恢复结果保留最近 16 次核验历史；服务看板单独显示「非住宅节点恢复」的心跳和任务计数。
