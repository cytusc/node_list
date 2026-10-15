# 住宅出口复核与恢复

住宅节点默认每 **5 分钟**复检；**一次完整验证通过就立即恢复**。不累计两次成功，不等待 30 分钟，不依赖普通节点整轮巡检。架构与部署见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)。

## 分类

按同一真实出口 IP 的有效证据判断，默认来源为 IPPure、IPinfo、IP-API、IPregistry、AbuseIPDB、ipapi.is 和 IP2Location。每家最多一票，使用类型优先，公司类型后备。失败、缺字段、限流弃权；`hosting=false` 不能推断住宅。最高票并列时，在并列项中依次优先住宅、商业宽带，其余平票为机房，全部弃权为未知。

`business` 映射商业宽带，`isp` 映射住宅，`hosting` 映射机房；其他明确用途保留原类型。使用类型和公司类型按 IPQuality 映射。VPN、代理、风险分数不直接当作机房票，也不凭运营商品牌猜测住宅。

只使用同 IP、24 小时内、当前分类版本的证据。每家选最新响应，包括最新未知或失败响应；旧成功不能覆盖新失败。保存 `classification.types`、`classification.evidence` 和来源时间，不通过刷新缓存时间延长原始证据有效期。IPPure 风险失败不阻塞其余六家分类。

## 自动恢复

仅对已知自动退役原因创建恢复任务，人工禁用排除。历史住宅优先每 300 秒检查，其他自动退役候选每 1800 秒检查。

本轮须满足：

1. 经目标连接访问 GitHub 返回 HTTP 200。
2. 经同一连接访问 ipify 和 AWS CheckIP，两个 HTTPS 公网 IPv4 回显一致，并区别于宿主出口。
3. 当前出口的有效分类为住宅；可复用未过期的同 IP 属性，缺失时提交高优先级属性查询。
4. 分类后再次核对出口，IP 和出口版本都不变化。
5. 生命周期事务再核对近期成功事实、有效源快照，以及精确匹配的 identity／retiredAt／reason。

全部满足即解除自动退役。IPv6、出口变化、连通失败、属性未知／机房、证据过期、源快照已删除都不恢复。本轮最长 3 分钟，超时结束后按节点类别再次排期；任务重试沿用同一会话，不把重复投递计为新实测。

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

本次代码修改尚未部署，不代表线上节点数量已经增加。
