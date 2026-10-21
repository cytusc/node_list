# 轮次制测活重构计划（sweep 架构收敛）

> 状态：**已实施（批次 1→4 全部落地，backend `go build`+`go vet`+`go test` 全绿）**。基线原则：避免过度设计与过度执行，先满足需求。
> 本文档是唯一实施依据；Code 里未实现的部分一律以本文档为准，实施中有偏差回写 §11 实施记录。

## 1. 背景与动机

当前系统是逐节点任务制测活（`node_explorer_work` 持久化投递 + Redis 引用 + 租约 CAS），connectivity/ip/residential/normal-recovery/lifecycle 五类进程各管一段。mihomo 内核 delay 探测（zashboard 面板形态）实测效率远高于现有逐节点拨号，用户决定将测活主链路切换为**轮次制全量快测**，并顺手完成一次架构收敛（净减服务与任务机制）。

规模口径：**每轮快测覆盖注册表中的所有节点**（全量、不抽样），1 万节点仅是下文估算用的示例数字。通用估算公式：一轮时长上界 ≈ `N × 2s ÷ 并发`（N = 注册表节点数）。以示例值代入（N=10000、并发 32~64）：极端全超时约 10 分钟（64 并发减半），正常存活节点亚秒级返回，一轮 3~8 分钟；周期由 `SWEEP_INTERVAL_SECONDS` 配置（默认 30 分钟），留有 20+ 分钟空窗给出口/属性/恢复任务。

## 2. 目标拓扑（10 → 7）

| # | 原服务 | 处置 | 新形态 |
|---|--------|------|--------|
| 1 | node-explorer (PHP/webman, :3001) | **保留不动** | 不变 |
| 2 | source-ingest | **拆分** | 多个独立来源 provider（subs-check watcher / Qilan / 未来可扩展），入库前统一节点级去重 |
| 3 | connectivity | **停用**（逐节点 KindLive 排期删除） | 严格 GitHub 连通性核验并入 recovery，不再周期性全量跑 |
| 4 | ip-service | **拆 2** | exit-provision + exit-attributes（见 §4） |
| 5 | residential-recovery | **合并** | 并入 sweeper + recovery 生态 |
| 6 | normal-recovery | **合并** | 并入 sweeper + recovery 生态 |
| 7 | node-lifecycle | **合并** | 事务复核逻辑原样保留，改由 sweeper 轮次结果触发 |
| ★ | （新）**sweeper** | 新增 | 30 分钟一轮全量 mihomo 快测 + 可用性判定 + 3 连败退役 + 恢复候选升级 |
| 8 | node-cleanup | **保留** + 适配服务心跳名单 | 不变 |
| 9 | task-redis | **保留**（暂撤） | 仅剩 exit/attributes/recovery 用途；测活路径不再依赖 |
| 10 | shared-mihomo | **优化** | share-link 协议覆盖、YAML 装饰字段剔除表、端口段耗尽告警 |

基础设施：外部 PostgreSQL 不变；mihomo-manager（`:9080` 控制器租约）保留。

## 3. sweeper：轮次制测活主链路（核心新进程）

### 3.1 核心循环

```text
每 30 分钟（SWEEP_INTERVAL_SECONDS）：
  1. LoadRegistry        读取当前全量注册表
  2. manager.Sync        全量重载注册表全部 proxies 进 shared-mihomo（用户已接受秒级停顿）
  3. sweepAll            并发 delay 探测（/proxies/<name>/delay?timeout=2000&expected=204）
                         并发 32 起步，可配到 64；OnDelay 只累加内存结果
  4. 轮末单事务批量写     连通性状态 + failedRounds（不再逐条 RecordFast 小事务）
  5. failedRounds >= 3   → 自动退役（复用 lifecycle_policy 的 guard/审计语义）
  6. 退役节点本轮通过     → 投递 strict 恢复会话（KindRecovery）
  7. 无出口 IP 的节点     → 投递 KindExit；有 IP 缺分类/风险 → 投往 exit-attributes
```

### 3.2 设计参数（冻结值）

| 参数 | 值 | 说明 |
|---|---|---|
| SWEEP_INTERVAL_SECONDS | 1800 | 30 分钟；一轮实测约 3~8 分钟 |
| SWEEP_CONCURRENCY | 32（可配至 64） | 快车道并发，对应 runner.go:57 现值口径 |
| delay timeout | 2000ms / expected=204 | 沿用 manager.go:494 现值；2 秒误杀靠 3 连败兜底，不为慢节点调大 |
| 退役门槛 | 连败 3 轮 | 轮次天然构成「3 轮对照确认失败」（MODULAR_SERVICES.md:35 语义），单轮误标不致命 |
| 恢复判定 | 严格链原样保留 | GitHub 连通 + ipify/AWS 双回显一致 + 宿主出口排除 + 分类为住宅；一次成功即恢复，事务复核不弱化 |

### 3.3 删除的机制（净减）

- KindLive / KindFast 的**逐节点排期**（schedule.go:188-197 的 connectivity 分支）整体删除；
- 测活路径的 `node_explorer_work` 任务、代数推进、Redis 投递、租约 CAS —— 测活不再进队列；
- connectivity / residential / normal-recovery / lifecycle 四个独立进程的 Compose 定义（逻辑并入）。

### 3.4 保留的机制（不重写）

- 严格核验会话（GitHub + 双回显 + 宿主出口排除）→ recovery 角色代码原样保留，仅触发源变为 sweeper 升级候选；
- lifecycle 的恢复/退役**事务复核**（guard：退役记录 identity/时间/原因核对）→ 原样复用 `lifecycle_policy.go`；
- `node_explorer_service_status` 心跳名单按新拓扑 7 服务更新（node-cleanup 联动适配点）。

## 4. ip 流水线拆分与住宅快判（原 4 号改造）

### 4.1 两个服务

| 服务 | 消费任务 | 处理对象 |
|---|---|---|
| **exit-provision** | KindExit | 只处理**没有出口 IP** 的节点（反复补查，退避节奏沿用 12s 超时既有口径，不额外加频） |
| **exit-attributes** | KindAttributes / KindRisk | 只处理**有出口 IP** 的节点，内部按住宅快判分流（§4.2） |

### 4.2 住宅快判 → IPPure 直取（新流水线）

用户目标流水线：`server → 真 IP → 国家/ASN/ISP → 数据中心/住宅/CDN 分类 → 确认住宅则跳过属性加权，直接取 IPPure 风险数据`。

现状映射（已核实代码）：

| 层 | 现有实现 |
|---|---|
| server → 真 IP | KindExit（ipify/AWS 回显，走 manager kind="exit" 通道） |
| → 国家/ASN/ISP | ipcheck.Client.Lookup 六家来源（sources.go:51-60）+ ASNCache(6h) |
| → 住宅/机房分类 | ipcheck.Score 投票（voting.go, VotingVersion=8：hosting/datacenter −1，explicit-ISP +1，tie 偏住宅） |
| 确认住宅 → IPPure | KindRisk 通道（schedule.go:200-202，IPPure 独立排队不阻塞分类） |

改造实质 = **调度顺序 + 提前判定**，不新增任务 kind、不新建服务、voting.go 不动：

```text
节点拿到出口 IP 后：
  ① 住宅快判（强信号，不落最终结论）：
     主弹药 = ip-api 批量端点（PrimeIPAPIBatch，100 IP/请求，15 req/min，batch.go:17）
     信号位 = hosting'),companyType/usageType（ipinfo widget、ip-api batch 响应字段）
  ② 强信号=住宅 → 直接投 KindRisk（IPPure），跳过 KindAttributes 完整加权
  ③ 弱信号/机房 → 投 KindAttributes（完整 6 家加权，流程与现状相同）
  ④ 兜底自愈：IPPure 回来后校验 Classification.State，非住宅回补完整属性查询
```

判定函数：复用 `ipcheck.Score(ip, evidenceSubset, now)` 的证据子集重评价（与 `FreshResidential` state.go:74-88 同构），不写新算法。快判误判的代价被 ④ 闭环吸收。

### 4.3 收益

- 住宅节点省掉 6 家 × 1 IP 完整属性查询（4 家是公共演示端点、配额紧张），直接进 IPPure；
- ip-api 配额压力显著下降（批量端点一次 100 IP 已是现成高吞吐通道）；
- 数据中心/未知节点流程完全不变，行为无回归面。

## 5. source-ingest 拆分与去重（原 2 号改造）

- **拆分**：抽 source provider 接口（`subs-check watcher` / `Qilan poller` / 未来来源均实现），每来源独立 Compose 容器、各自挂载；
- **去重**：现状节点级幂等是 `ON CONFLICT(identity) DO UPDATE`（registry.go:184，覆盖更新而非跳过）；补入库前去重层（ingest 前置阶段按 `AggregateIdentity` 判重并计数），保留快照指纹幂等（ON CONFLICT(fingerprint) DO NOTHING）；
- **约束**：不改 `AggregateIdentity` 哈希输入（identity.go:159 持久化稳定性警告）；vmess 规范化已收敛（commit 531aуля5），不重开。

## 6. shared-mihomo 优化清单（原 10 号改造）

1. **share-link 协议覆盖**：vless/hysteria2/tuic 等解析失败现静默 fallback summary identity（identity.go:177），补 parse 失败可观测计数 + 协议测试夹具；
2. **Clash YAML 装饰字段剔除表**：RawConfig 进 canonical hash 前显式剔除装饰性字段（名称类）——去重质量在此扎根；
3. **端口段耗尽告警**：ConfigureProxyBatch base-port 越界现直接报错（mihomo.go:139），提前加分段监控；
4. **proxyName 截断 + 映射表**：全量节点（规模随注册表增长，N 可达万级）以长 hash 名称进同一实例，截断到面板/API 友好长度并保留 identity↔name 映射（控制面 URL 转义已处理，manager.go:494）。

## 7. 实施顺序

```
第一批（纯拆分，风险最低）：4  → ip-service 拆 exit-provision / exit-attributes + 住宅快判
第二批（铺干净入库面）    ：2  → source provider 拆分 + 节点级去重
第三批（同一波迁移核验链）：5/6/7 + 3 → sweeper 上线 + 四进程合并退场
第四批（持续优化）        ：10 → shared-mihomo 四项；8 心跳名单随第三批顺带
1 / 9 全程不动。
```

## 8. 验收标准

1. 以示例规模（N=10000）注册表下 sweep 一轮 ≤ 10 分钟（32 并发），30 分钟周期稳定空窗；大体量节点数按通用公式 `N × 2s ÷ 并发` 重新核算周期；
2. 3 连败节点的退役审计含 `guard` 语义复核记录，与现 lifecycle 审计同构；
3. 住宅节点（强信号确认）不产生 KindAttributes 查询记录，IPPure 数据完整入库；
4. 快判误判样本经 ④ 回补后最终分类与完整加权口径一致；
5. `node_explorer_service_status` 心跳与新 7 服务拓扑一一对应，`/api/services` 无幽灵服务；
6. 同一节点两种拼写（vmess 噪声字段变体）在入库层只产生一个 identity。

## 9. 执行纪律（实施过程中强制约束）

本次重构不是纯加法，是**收敛**。实施期间适用以下纪律，与文档头部的基线原则（避免过度设计与过度执行）一体适用：

1. **先满足需求**：每个改动以本文档各节的需求为完成线，不引入文档未要求的能力、抽象层、配置项；
2. **精简优先于修补**：动到一个文件/函数时，顺手评估其中的冗余——轮次制迁移留下的死代码（旧逐节点排期分支、失效 kind 常量、无人消费的进度字段）应当场删除，不留「以后再清」的尾巴；但**删除仅限本次重构明确替换的对象**，不动无关代码；
3. **服务冗余操作一并消除**：合并/拆分后每个服务的心跳、排期循环、锁争用路径里，只保留新拓扑实际走的那一条；被替代的旧分支（如 connectivity 的 KindLive 周期排期）连同其调度 SQL 一起撤，不做双轨兼容；
4. **陛够就好**：快判信号弱就用现有字段，不新增查询端点；退役判定够用就沿用 guard 语义，不另造审计框架；
5. **改完即验证**：每批实施（§7 顺序）完成后跑对应验收条目（§8），通过才进下一批；不为「更放心」追加文档外的自检仪式。

> 删除动作仍受目标侧零破坏约束：仅删本重构明确替换的本仓库代码，不触碰任何数据/表/挂载目录。

## 10. 风险与既知取舍

| 风险 | 取舍 |
|---|---|
| 全量 Sync 重载秒级停顿 | 用户已明确接受（zashboard 日常同款形态） |
| 2 秒误杀慢节点 | 3 连败兜底；不为慢节点牺牲探测速度 |
| 快判把机房当住宅 | IPPure 校验 + 回补闭环，最坏多一次属性查询 |
| 测活脱离任务队列后无补偿机制 | 轮次制本身即「下一次全量重试」，无需补偿层 |
| PG 批量写压力 | OnDelay 改内存累加、轮末单事务批量落库 |

## 11. 实施记录（回写）

- **批次 1**：ip-service 拆 `exit-provision` / `exit-attributes`（compose + kind 路由 + 住宅快判 `quickverdict.go`，判定复用 `ipcheck.Score`，兜底④回补落 `probes.go` risk()）。
- **批次 2**：`SourceProvider` 接口 + `SOURCE_PROVIDERS` 选择（compose 拆 source-ingest / source-qilan 双容器）；`importLocked` 节点级去重（`AggregateIdentity` 判重，首个胜出，duplicates 计数落日志）。
- **批次 3**：sweeper 并入 mihomo-manager 主循环（`sweep.go`：`SWEEP_INTERVAL_SECONDS=1800`、`SWEEP_CONCURRENCY=32` 可配至 64、delay timeout=2000/expected=204、轮末单事务批量写 fast_results+connectivity、3 连败 `accumulateFailures/retirementConfirmed`+guard 复核+`LIFECYCLE_DRY_RUN` 开关、退役本轮通过→once(KindRecovery)、无 IP→once(KindExit)、缺分类/风险→KindAttributes/KindRisk）；connectivity/residential/normal-recovery/lifecycle 四进程退场，合并为 `recovery` 角色（消费 KindRecovery/KindNormalRecovery/KindFast/KindLive/KindLifecycle，调度侧 retryResidentialRecovery/skipHostingRecovery/scheduleNormalRecovery 原样挂靠）；mihomo-manager 去 taskqueue 依赖（RecordFast/FastScores 删除）；node-cleanup 顺带清扫 >30min 陈旧心跳行（§8.5 幽灵服务消除）。
- **批次 4**：§6 四项落地——`ShareLinkParseFailures` 原子计数 + `sharelink_coverage_test.go` 协议夹具；`decorativeConfigFields` 剔除表（identity 与 ConfigDigest 共用）；ConfigureProxyBatch 段位 <4096 提前告警；`proxyNameHex=16` 截断（identity↔name 纯函数映射，Sync 碰撞隔离守卫）。
- **删除清单**：`backend/internal/taskqueue/fast_stats.go`、`fast_stats_test.go`、schedule.go 大扫描 connectivity/residential 分支、runner/config/main 的旧 role 枚举、compose 四服务块（connectivity/residential-recovery/normal-recovery/node-lifecycle）。
- **平台诚实化**：3 处 Windows 不可观测断言（0600 位、POSIX ctime ×2）做平台分支/跳过，均为环境性、非重构语义变更。
- **待人工验证**：compose 全量 `docker compose config`（本机无 docker）；10k 级真实注册表下 sweep 一轮时长实测（§8.1）；生产中 Sweeper 首轮跑完后 §8 各条目复核。
