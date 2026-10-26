# 节点采集：生产数据与第三方库评估

采样：**2026-10-26 20:20:05（Asia/Shanghai）**。PostgreSQL 可重复读只读事务，设置只读及语句超时，读取表结构、注册记录、来源成员、链接档案。未更新生产数据或进行节点连通性测试。

“来源成员”指 registry 的 `(identity, snapshot_id)` 与 snapshot_members 匹配，不表示来源在线或节点可用。原始配置、链接和凭据不进入仓库；[JSON 证据](02-source-ingest-evidence.json) 只含计数、字段名和工具版本。

## 节点记录字段

`node_explorer_node_registry` 共 **11** 列：

| 字段 | 类型 | 当前生产默认值/说明 |
| --- | --- | --- |
| identity | text | 主键 |
| node | jsonb | 摘要、完整配置、链接等 |
| snapshot_id | integer | 来源快照定位 |
| node_index | integer | 快照内位置 |
| historical_residential | boolean | false；不属采集维护范围 |
| available | boolean | **true**，生产仍为旧默认 |
| source_at | timestamptz | -infinity |
| first_seen_at | timestamptz | now() |
| last_available_at | timestamptz，可空 | 测活事实 |
| incarnation | text | 随机 MD5；并发代际 |
| connection_key | text | 空字符串 |

本地独立测活迁移已调整新节点默认不可用，生产尚未部署；新采集须显式初始化未测，不依赖生产旧默认值。

## 节点与链接规模

| 指标 | 全部注册记录 | 匹配来源成员 |
| --- | ---: | ---: |
| 记录数 | 8,942 | 1,069 |
| available=true | 916 | 916 |
| 有 rawConfig | 6,963 | 451 |
| 有原始 rawLink | 2,195 | 785 |
| 无原始、仅生成链接 | 3,400 | 278 |
| 两种链接都没有 | 3,347 | 6 |

另有 1,334 个快照，声明节点总数 1,069，成员表 1,069 条。7,873 条注册记录不匹配来源成员，不能据此自动删除。

| 协议 | 全部 | 来源成员 | 全部缺失两种链接 |
| --- | ---: | ---: | ---: |
| HTTP | 6,174 | 262 | 3,057 |
| SOCKS5 | 577 | 16 | 287 |
| VLESS | 1,276 | 425 | 2 |
| VMess | 283 | 57 | 1 |
| Shadowsocks | 254 | 166 | 0 |
| Hysteria2 | 202 | 82 | 0 |
| Trojan | 151 | 51 | 0 |
| TUIC | 16 | 2 | 0 |
| AnyTLS | 7 | 6 | 0 |
| Hysteria v1 | 2 | 2 | 0 |

来源成员中缺失两种链接的是 HTTP 4 条、SOCKS5 2 条。没有原始链接的 6,747 条均有 rawConfig，补链应从配置转换，不能从摘要猜认证参数。

HTTP 全部没有原始链接，291 条配置启用 TLS；来源成员 HTTP 中 **244/262** 条启用 TLS。全库以普通 HTTP 为主，当前来源成员却以 TLS 为主，不能统一输出普通 HTTP 链接。

## 去重证据

| 方法 | 全部记录 | 来源成员 | 判断 |
| --- | --- | --- | --- |
| 旧 connection_key | 8,446 条非空、8,244 个值；132 组重复、202 条冗余 | 1,033 条非空、880 个值；115 组重复、153 条冗余 | 仅候选，不能作为迁移真值 |
| 协议+小写地址+端口 | 8,033 个端点、909 条冗余 | 555 个端点、514 条冗余 | 不安全，不能当唯一键 |
| 完整 rawConfig 仅去 name/sub_tag，RFC 8785 + SHA-256 | 6,963 条配置中 1 组重复、1 条冗余 | 451 条配置中 1 组重复、1 条冗余 | 第一期严格基线，不覆盖无配置记录 |
| Sub-Store 转成 Mihomo 配置后比较 | 60 组重复、61 条冗余 | 55 组重复、56 条冗余 | 规范化候选，仍需检查字段丢失 |

同端点具有多个不同旧连接键的组分别为 **253 / 217**，不能只看地址端口合并。

旧键候选数量不是安全删除数量；来源成员 880 个键加 36 个空键得到 916，与 available=916 只是数值巧合，不证明连接等价。不同方法覆盖的记录范围不同，不能直接相减推断节点丢失。

## 第三方库实测

使用 [Sub-Store 2.44.2](https://github.com/sub-store-org/Sub-Store/releases/tag/2.44.2) 官方 [ESM 模块](https://github.com/sub-store-org/Sub-Store/releases/download/2.44.2/proxy-utils.esm.mjs)，AGPL-3.0。源码提交 `c1018115a7eee4cdcb9042cce5e94afda41e48ba`。

制品 SHA-256：`ed870a8728056663acee57629472261a83d0277ba7ff3b8ec8b8f0fe98d52847`。实验环境 Node.js 22.14.0，补充 dotenv@16.4.7，安装关闭生命周期脚本；这不是最终生产运行时选型。

### 原始 URI 解析

2,195 条中 **2,171 条**解析为单节点；Trojan 23 条、VMess 1 条返回空，这 24 条均有来源成员关系。返回空不意味着网络不可用，不能据此删除。尚未逐条定位输入问题或库兼容问题，实施时需要建立受限样例回归。

### 配置导出与回读

6,963 条 rawConfig 全部能经库解析成单个节点。对没有原始链接的 6,747 条导出：

| 协议 | 样本 | URI 生成并回读单节点 | 未生成 URI |
| --- | ---: | ---: | ---: |
| HTTP | 6,174 | 0 | 6,174 |
| SOCKS5 | 562 | 562 | 0 |
| VLESS | 4 | 4 | 0 |
| VMess | 2 | 2 | 0 |
| Shadowsocks | 2 | 2 | 0 |
| Trojan | 2 | 2 | 0 |
| AnyTLS | 1 | 1 | 0 |
| 合计 | 6,747 | 573 | 6,174 |

URI producer 没有 HTTP 导出分支，因此不能单靠 Sub-Store 完成补链。HTTP 中 **5,879** 条仅含地址、端口、可选认证等基础字段，可以进入标准 URL 适配候选；来源成员只有 **17** 条。此项仅为字段筛选，尚未验收跨客户端兼容。

对生成 URI 的 573 条，将输入及回读结果均转为 Mihomo 配置、去展示和库内部元数据后，严格相同 **2** 条，字段有差异 **571** 条。差异不都代表语义变化，例如缺省认证与空字符串、隐式 TLS、别名或默认值，也不能一概视为无害。

差异涉及 SOCKS 用户名/密码、skip-cert-verify、SNI、packet-encoding 等，必须逐项建立等价规则。不能为提高补链通过率直接删除字段。

同时含 rawConfig 和原始 URI 的记录也有差异，例如 VLESS 的 WS/gRPC/encryption、Hysteria2 的 auth/obfs/带宽。保留原始 URI 的同时，不能默认它和现存配置完全等价。

### 历史链接档案

共 9,324 条。缺少两种链接的记录中，仅 2 条通过非空旧 connection_key 找到档案候选，均为 generated，仍需等价校验。档案无法解决主要的 HTTP/SOCKS 补链问题。

## 方法与限制

1. 一致性只读事务读取 registry、匹配成员、链接档案和字段定义；原始、仅生成、均缺失三类互斥计数。
2. 严格去重采用 `canonicalize@2.1.0`（Apache-2.0），仅移除配置顶层 name/sub_tag，再用 SHA-256，不套用旧连接键规则。
3. 原始链接调用官方 `parse(rawLink)`；配置调用 `parse(JSON.stringify({proxies:[rawConfig]}))`，要求恰好单节点。
4. 无原始链接配置调用 `produce(deepClone(parsed), 'URI')` 后回读；两侧使用 `produce(deepClone(proxies), 'mihomo', 'internal')` 比较。库会修改输入，调用前复制。直接配置导出的对照统计也保存在 JSON 的 direct_conversion。
5. 比较仅忽略展示和库内部元数据，不主动排除未知业务参数。但库规范化本身可能损失字段，所以结果仅列为候选。

本次验证解析、表达和去重风险，没有测试节点连通性或服务吞吐，没有将所有字段差异判为错误，没有实际补链或合并数据。数据仅代表采样时点。

## 决策

完整配置作为核心输入，原始链接作为不可覆盖的来源证据。使用第三方解析转换，加上完整性及回读验证；首期严格去重，语义合并按规则推进。HTTP 用标准 URL 补足基础格式，按用户后续指示，无法无损补链且没有有效原始链接的配置拒绝进入节点主表，等待重新采集。实施边界、清理范围与 TODO 见 [重写方案](02-source-ingest.md)。
