# node-source 实现与验收记录

更新：2026-10-27。快照依赖已移除，当前清理范围见 [实时查询清理](12-realtime-query-cleanup.md)。

原实现验收记录（2026-10-26）：第三方解析、链接生成、严格去重与独立事务入库已完成本地实现和隔离验证；尚未提交、部署或对生产执行新迁移。前端未修改。

## 独立运行与写入边界

入口为 `backend/cmd/node-source`，仅依赖 PostgreSQL 和已配置的来源。Go 负责获取、调度、健康与事务；本地 Node.js 子进程调用第三方转换库。无需 API、Redis、manager、测活、出口或属性服务，不启动代理内核，不发布测活任务。

采集只创建当前连接输入、去重和来源记录，新 registry 行显式 `available=false`。已有 available、last_available_at、incarnation、historical_residential、connectivity、exits、ip_attributes、work 不由采集更新。不接受来源声明的可用、延迟、测速、出口和属性事实。完整来源批次只替换本来源成员关系，不删除 registry 节点或其他服务数据。

来源支持 Qilan、三个现有公开代理列表、subs-check 的 all.yaml/base64.txt、`SOURCE_SUBSCRIPTIONS` 中按行配置的 HTTP(S) 订阅。YAML 与 URI 导出分别记来源，不通过旧 results.json 做名称匹配或上游测活筛选。上游导出本身可能只含成功节点，需要完整覆盖时应直接配置原订阅。

## 转换与补链

转换器位于 `services/node-source-converter/`，固定 Sub-Store 2.44.2 官方制品，启动校验 SHA-256；保留 AGPL-3.0 许可、制品及对应源码链接。依赖固定 canonicalize 2.1.0、yaml 2.8.1、dotenv 16.4.7 并提交 npm 锁文件。部署镜像固定 Node.js 22.14.0。

只调用 parse/produce，不运行 Sub-Store HTTP 应用、远程处理器或用户脚本。库初始化的缓存文件放临时目录，转换子进程退出后清理；Go 同样为子进程建立临时工作目录，取消或超时后删除。日志屏蔽库可能输出的原始链接，子进程不继承 PostgreSQL 和供应商凭据。配置中拒绝库私有 `_` 字段，避免把来源数据当成本地文件选项。此为独立子进程和资源限制，并非操作系统文件访问沙箱。

每批最多 20,000 项，输入最多 32 MiB、输出最多 64 MiB；子进程 V8 堆上限 192 MiB，等待与执行合计超时 45 秒，同一进程最多一个转换子进程。数据库批次事务超时 5 分钟。超限、启动失败和超时只返回固定错误，不输出凭据。

| 输入 | 处理 |
| --- | --- |
| 有原始 URI | 第三方解析并验证基本端点及必需认证，保留原始字节 |
| HTTP 基础配置 | 用标准 WHATWG URL 生成，支持认证编码、IPv6、TLS；回读验证 |
| 其他协议配置 | 第三方生成 URI，回读到 Mihomo 配置并逐项验证源字段 |
| 配置与原 URI 冲突 | 原 URI 只作冲突证据；配置必须能另外生成验证链接才入主表 |
| 无法表达未知字段、SNI 等扩展 | 不为补链删字段，记录 unrepresentable 并拒绝主表 |
| 缺必要参数、无法解析 | 记录 incomplete / invalid / unsupported，拒绝主表 |

回归覆盖 HTTP、SOCKS5、SS、Trojan、VMess WS、VLESS gRPC/Reality、Hysteria2、TUIC、AnyTLS，以及认证、插件、未知 query、未知配置字段、JSON/YAML/base64。原始 URI 可解析和可保留，不代表导出器支持该协议配置的无损补链，也不代表节点可联通；联通性仍由独立测活负责。

HTTP 仅使用标准库，不手写分享协议。SOCKS 回读中的缺省与空认证仅作为链接验证等价，指纹仍区分两种配置。未建立规则的字段差异不放行。旧 domain 分享链接工具仍供其他现有读者使用，采集不再调用它们。

## 去重、来源和兼容

指纹版本为 `strict-v1/sub-store-2.44.2`。完整配置仅删除顶层 name/sub_tag，RFC 8785 规范化后 SHA-256；认证、TLS、传输、显式默认值与未知字段全部保留。

所有含原始 URI 的输入额外加入去展示 fragment 的 URI 身份材料，保留未知参数；VMess 的 base64 内部可能含展示名，首期保留整条 URI。因此同一连接的 URI 与纯配置、不同 URI 写法或 VMess 改名可能仍产生两个节点。此策略接受部分重复，不做未经证明的语义合并。来源别名和原始字节单独保留。

新增六张自有表：

| 表（均带 node_explorer_ 前缀） | 责任 |
| --- | --- |
| source_connections | 版本、指纹、完整 canonical、稳定 registry identity，唯一约束 |
| source_batches | 来源、输入摘要、数量与 complete/partial/rejected 状态 |
| source_occurrences | 来源批次位置、identity、别名、current 关系 |
| source_links | 原始/生成链接、验证或冲突状态与转换版本 |
| source_rejections | 批次位置和原因代码，不保存拒绝原文 |
| source_identity_aliases | 唯一匹配历史 identity 的增量采用记录 |

批次在同一 PostgreSQL 事务内写入，使用事务互斥锁和唯一约束保证多写者并发幂等。摘要命中核对完整 canonical，差异整批回滚。批次摘要不含抓取时间，完全重放不重复写入节点和来源记录；重放旧完整批次会重新激活该来源对应成员关系。partial/rejected 批次也保持幂等并返回固定警告，健康检查不会将部分失败视为完全正常。

新 identity 为 `src1:<fingerprint>`。纯配置输入只有在历史 rawConfig 严格相等且恰好一条、没有其他绑定时才采用旧 identity，不批量合并旧记录。别名表记录采用关系。测活与 API 直接读取 registry，不依赖批次成员或快照定位；不调用这些服务。节点配置不整行覆盖，仅更新时间，必要时补原始链接。来源失效不等于节点不可用。

转换 canonical、节点配置、链接表均可能包含认证信息，应沿用数据库与备份访问权限；拒绝码、心跳及仓库证据不包含链接和凭据。批次证据目前持续保留，尚无自动历史清理策略。

## 历史样本离线验证

使用之前保存的生产只读导出，采样时间 2026-10-26 12:20:05 UTC，共 8,942 条。它早于无链接生产清理，**不代表当前线上数量**。本次没有重新连接生产，也未执行测活、补写或删除。

| 指标 | 数量 |
| --- | ---: |
| 可接收记录 | 8,385 |
| 保留原始链接 | 1,946 |
| 生成并回读验证链接 | 6,439 |
| 无法无损表达 | 513 |
| 参数缺失 | 15 |
| 无效输入 | 29 |
| 接收记录的唯一指纹 | 8,206 |
| 同指纹重复出现 | 179 |

完整脱敏计数见 [统计 JSON](02-source-ingest-implementation-evidence.json)。这 179 次重复只表示新转换规则下的候选，不等于可以删除 179 条现有生产数据。旧 identity → 新指纹的逐条审阅映射保留在本机私有审计目录，不纳入仓库；生产启动不扫描全表自动合并。

## 验证与上线

已完成第三方转换器 8 项回归测试；Go 全后端 race 测试、vet、独立 PostgreSQL 集成测试、真实私有内核回归；部署脚本 10 项测试和 PHP API 144 / auth 34 项断言。数据库覆盖并发重复、跨来源别名与链接、批次重放、部分/空/拒绝批次、摘要冲突整批回滚、历史唯一采用和多候选不合并、读者位置修复、其他服务事实不变、独立服务启停、转换失败脱敏和取消。

本地运行先执行 `npm --prefix services/node-source-converter ci --ignore-scripts`，确保 Node.js >=22.14.0，再运行 `go -C backend run ./cmd/node-source`。可用 SOURCE_NODE_BIN / SOURCE_CONVERTER 指定绝对路径。健康检查为 `node-source -health`；来源失败或部分失败返回异常心跳。仅测试运行可设 SOURCE_PROVIDERS=none；正常部署应配置实际来源。

Docker、Compose 与 CI 已纳入转换器和固定依赖，构建上下文排除宿主 node_modules。共享表仍需部署时预先准备；服务只创建上述自有表，不运行共享迁移。当前环境无法运行 Docker，镜像构建和容器内 Node 动态依赖需在 CI 验证；本地没有声称完成镜像构建。

上线尚待：备份并确认旧采集写者已停，准备共享表、构建镜像，隔离冒烟后启动新 source-ingest，观察来源健康、接收/拒绝批次与资源。无需修改前端。回退时先停新写者，保留新表与映射，避免两个版本同时写入。
