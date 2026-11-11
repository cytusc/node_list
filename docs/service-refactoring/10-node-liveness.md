# 独立节点测活服务

实施日期：2026-10-26。本文替代旧 manager 巡检、recovery、dead-verify 中关于测活和可用状态维护的设计。实现参考 `/Users/hoyo/code/me/ip_list` 的私有 Xray 进程模式，以及 [内核评估](../kernel-evaluation/xray-mihomo-fallback-2026-10-26.md)；未沿用旧测活执行器。

## 服务边界

`node-liveness` 是唯一执行节点测活、写入 `node_explorer_connectivity` 和更新注册表 `available` 的常驻服务。它只依赖 PostgreSQL 中的节点配置及来源成员关系，独立启动私有 Xray / Mihomo 进程，不依赖 Redis、manager、出口核验、IP 属性、住宅分类或其他业务队列。

采集服务只导入节点，新节点 `available=false`；重复导入不修改此字段。出口和属性服务可读取测活事实，但不能改变它，也不能向测活服务投递任务。测活成功即可标记可用，无需出口、住宅属性或恢复审批；失败不会退役、删除节点或写 tombstone。不可用节点持续按相同规则复测。

已删除 recovery、normal recovery、dead-verify、malformed-clean、connectivity-bench、manager sweeper、fast-delay 和共享 manager 的 delay 接口，以及来源 TCP 准入、恢复依赖、生命周期变更和自动判死删除流程。测活直接读取 registry，不依赖历史表、快照成员或其他服务任务；出口与属性服务独立运行。

## 测试流程

1. 读取来源仍存在的注册节点；按到期时间调度，每批按 3:1 交错分配可用与其他节点；一组不足时另一组补满。配置摘要或 incarnation 改变立即重新排队。
2. 解析原始配置或分享链接。保守筛选可转换配置：HTTP、SOCKS5、现代 Shadowsocks、VMess、带 TLS / Reality 的 VLESS、Trojan；支持已建模的 TCP、WS、gRPC、XHTTP 选项。
3. 未知协议、未知选项、插件、跳过证书验证、明文 VLESS 等直接使用 Mihomo。兼容性白名单以 `internal/liveness/config.go` 为准，不把历史评估的 91.1% 配置接受率当作实际成功率承诺。
4. Xray 成功即保存；Xray 任意失败由 Mihomo 补测，只保存一次最终结果，并记录回退原因。
5. 先请求 `https://www.gstatic.com/generate_204`，失败后经同一代理补测 `https://cp.cloudflare.com/generate_204`。主目标最多使用剩余期限的一半，备用目标使用余下期限；任一成功即可。必须 GET、HTTP 204、完整读取有界响应，不跟随重定向。Go 客户端验证目标 TLS；节点本身的传输选项保持其原始含义。
6. 最终 Mihomo 请求失败时进行有时限的双目标直连对照；结果在当前进程共享缓存 15 秒，合并并发检查。对照失败记基础设施错误，保留上次可用状态。目标本身或主机出口故障可能因此延迟不可用判定。

配置错误、内核未启动、内核异常退出和基础设施错误只更新尝试记录，不将节点标为不可用。解析失败保留固定诊断码，不输出原始链接或密钥。同配置同原因的配置错误复用指数退避，上限一小时；配置或 incarnation 改变重置计数。基础设施错误按短重试间隔加 0–20% 抖动。可用性结果采用布尔状态，不延续旧恢复、住宅或自动删除策略。

## 并发、超时与写入

两组独立工作池，默认 Xray 4 路、Mihomo 4 路；每个任务拥有私有配置目录、内核和代理入口。Xray 使用 Unix socket HTTP 入口；Mihomo 使用随机回环端口和每任务认证。内核日志不输出，临时配置权限 0600，结束强制回收进程和临时目录。

Mihomo 监听端口打开时内部 tunnel 可能仍暂停，因此不以 TCP 端口可连接作为就绪条件。每个进程先通过代理请求私有回环 HTTP 服务，专用精确地址 / 端口规则只将该就绪请求直连；收到 204 后才发起节点测试。此检查不访问节点、不启用控制器，且包含在同一个内核超时中。真实内核重复测试已覆盖这一启动竞态。

单内核启动及 HTTP 请求合计默认最多 12 秒；一次 Xray 加回退可消耗两个内核期限，最终负面结果另有最多 4 秒直连对照。Mihomo 慢任务不会占用 Xray 槽位。调度使用有界待处理列表和背压，数据库每次读取最多 1024 条，当前在途少于 256 时才继续拉取；间隔是单节点完成后的最早重测时间，不承诺全库固定时间完成。

专用 PostgreSQL 会话持有 schema 范围的单实例 advisory lock。结果及心跳使用同一持锁连接写入，连接丢失后不会通过连接池继续提交。事务锁定当前身份、incarnation、配置摘要和来源成员，过期任务结果丢弃。有效结果原子更新连通性与 `available`；异常仅写自有 `node_explorer_liveness_attempts`。每秒写入心跳和两组队列长度、在途数、完成计数。

## 运行和配置

镜像包含 Xray 26.10.21、Mihomo 1.19.27。本地需将两者加入 PATH，或指定 `XRAY_BIN` / `MIHOMO_BIN`。数据库连接使用标准 PG* 环境变量，支持 libpq 风格 TLS 设置；二进制不自动读取 `.env`。

```sh
go -C backend build -o /tmp/node-liveness ./cmd/node-liveness
/tmp/node-liveness
/tmp/node-liveness -health
```

| 配置 | 默认值 | 范围 |
| --- | --- | --- |
| LIVENESS_XRAY_WORKERS | 4 | 1–64 |
| LIVENESS_MIHOMO_WORKERS | 4 | 1–64 |
| LIVENESS_TIMEOUT_SECONDS | 12 | 1–120 |
| LIVENESS_INTERVAL_SECONDS | 1800 | 10–86400 |
| LIVENESS_RETRY_SECONDS | 60 | 10–3600 |

服务使用 2 个输入连接和 1 个持锁写入连接。健康检查要求 30 秒内有心跳，且输入查询没有错误；心跳健康不代表外部目标或所有节点可用。

## 升级与回退

先备份数据库及部署配置。停止旧 recovery、dead-verify 和旧 manager，再更新全部应用镜像及 Compose；旧 manager 不认识新所有权约定，禁止并存。`deploy/compose.sh up` 按当前 project 标签移除旧 recovery / dead-verify / node-cleanup 容器。

共享表由部署显式准备，本次升级需在更新测活服务后执行 `psql -v ON_ERROR_STOP=1 -f deploy/sql/019-publish-verdict.sql`；独立测活仅创建自有尝试表。现有 availability 和观察事实保留；首页同时要求正面测活结论、匹配当前配置摘要与 incarnation、六小时内的测活记录及已核验出口。缺少摘要的旧记录需重新测活后才能展示。监控页显示各隐藏原因的当前数量，以及本次测活服务启动以来的结果计数。

发布后检查独立心跳、每种最终结果计数、最老待测时间、回退比例、CPU / 内存和实际每轮耗时。需要回退时先停 node-liveness，再恢复完整旧版本，不能混用两套写入者。本次仅本地实施和验证，未连接生产库写入、未部署。

## 验收记录

- [x] 两组独立队列，慢 Mihomo 不阻塞 Xray。
- [x] Xray 失败补测，只提交一次最终结果。
- [x] 内核启动卡住时超时退出、回收进程。
- [x] Xray 26.10.21 / Mihomo 1.19.27 真实回环代理成功与失败测试连续 30 轮通过，验证请求确实经过上游代理；Xray 协议 / 传输配置矩阵通过内核检查。
- [x] 直连对照失败不能产生不可用结论；目标 TLS 证书不可信时拒绝请求。
- [x] 隔离 PostgreSQL 验证唯一实例、有效结果写入、异常保留状态、配置 / incarnation 变更丢弃旧结果、来源失效不再调度。
- [x] 重新导入不能更改可用状态。
- [x] Web 集成测试确认历史退役记录不能隐藏测活判定为可用的节点。
- [x] 全量 Go 竞态测试、go vet、前端构建、PHP 离线 / 认证 / PostgreSQL 集成测试、部署脚本测试通过。
- [ ] 生产节点成功率、吞吐和资源基线：部署后测量，不能由本地回环测试代替。
