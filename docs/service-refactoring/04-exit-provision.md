# 独立出口 IP 获取服务

更新：2026-10-26。代码已实现、本地验收通过，尚未部署生产。Compose 名称沿用 `exit-provision`，入口改为 `node-exit`。

## 唯一职责与边界

读取 `node_explorer_node_registry.available=true` 的节点，通过该节点的私有 Mihomo 代理请求出口回显，维护出口 IP 及自身执行记录。可用性由独立测活服务维护，出口服务只读可用状态。

依赖只有 PostgreSQL、Mihomo 二进制和公开 HTTPS 回显源。无需 Redis、共享 Mihomo、manager、测活进程或属性进程在线。共享数据库是输入输出契约，不通过任务、通知或 RPC 调用其他业务服务。启动不运行通用业务迁移；部署需预先建立注册表、出口表与服务心跳表。

| 数据 | 权限与用途 |
| --- | --- |
| `node_explorer_node_registry` | 只读节点配置、身份、incarnation、available；不要求关联历史快照 |
| `node_explorer_exit_attempts` | 自有表，保存配置摘要、最近尝试、下一次到期时间与结果；启动自动建表 |
| `node_explorer_exits` | 写出口事实，兼容现有 API 的字段和版本契约 |
| `node_explorer_service_status` | 只维护 `exit-provision` 自身心跳 |

不写 connectivity、available、historical_residential、IP 属性/风险，也不创建、修改或清理通用工作任务。不会因出口失败删除、退役节点。

入口：[main.go](../../backend/cmd/node-exit/main.go)。实现：[exitprobe](../../backend/internal/exitprobe/)。公共内核启动与存储配置适配：[proxyprocess](../../backend/internal/proxyprocess/)。测活仅复用公共进程基础设施，不与出口业务互相调用。

## 执行与结果语义

1. 每秒补充到期任务，只选择可用节点；在内存中按 identity 排除在途重复，同一时刻最多执行 `EXIT_WORKERS` 个任务。
2. 开始执行前重新检查 available、incarnation 和节点 JSON 摘要。配置读取沿用已存在的 `rawConfig` / `rawLink` / `generatedLink` 兼容适配，不新增协议解析实现。
3. 为每个任务启动一个私有 Mihomo。配置目录私有、配置文件权限 0600；子进程日志丢弃，避免暴露节点凭证。入站只监听回环且带认证，没有共享控制器。通过专用回环请求确认 tunnel 就绪，不能仅以 TCP 端口打开作为就绪依据。
4. 全部公网请求固定路由到唯一节点，无直连降级。唯一 DIRECT 规则仅用于本地就绪端口。依次尝试 ipify、AWS CheckIP、icanhazip，每个最多 5 秒；从内核启动到最后一次请求共享总超时预算。
5. 仅接受 HTTP 200 的单个公网 IPv4/IPv6 字面量，拒绝跳转、超大响应、多个地址、私网、回环、链路本地、CGNAT 和常见保留地址。不补查国家或住宅属性，不与宿主 IP 比较；宿主与代理出口相同本身不是错误。
6. 提交事务先锁定注册节点，重新检查可用性、incarnation 和配置摘要。节点失效、换代、修改或删除后，旧结果和旧重试记录一起丢弃。
7. 成功写入 `verified=true`；出口改变递增 version，同 IP 刷新不增版本。失败保留上次 IP 供诊断、清除 verified、记录固定错误码与连续失败次数，不改变节点可用状态。
8. 成功/失败分别持久化下一次执行时间；重启继续遵守冷却。实例或配置改变立即重新到期。新服务首次启动会重新获取全部可用节点出口，不继承旧通用队列的退避。

服务沿用旧出口 worker 的 PostgreSQL advisory lock 键，拒绝重复实例和旧新入口同时启动。所有写入串行使用持锁连接，失去该连接后无法提交，心跳或下一次写入失败会停止服务并取消、等待所有私有内核退出。正常退出标记 running=false。数据库故障由进程管理器重启，不写成节点死亡。

这是出口观测，不是多来源投票认证：每轮接受首个有效 HTTPS 回显，不承诺来源间一致性或同时获取双栈地址。本轮没有进行生产吞吐或成功率基准测试。

## 配置与运行

| 参数 | 默认值 | 范围 |
| --- | --- | --- |
| `EXIT_WORKERS` | 4 | 1–32 |
| `EXIT_TIMEOUT_SECONDS` | 25 | 5–120 |
| `EXIT_INTERVAL_SECONDS` | 1800 | 10–86400 |
| `EXIT_RETRY_SECONDS` | 300 | 10–86400 |
| `MIHOMO_BIN` | mihomo | 已安装内核路径 |

读取标准 PG* 环境变量，包括 PGSSLMODE；业务连接池固定为 2，另有 1 条所有权/写入连接。不继承 `MIHOMO_MANAGER_URL`、Redis、住宅优先级或供应商密钥。非法数值启动报错。

```sh
npm run build:exit
./node-exit
./node-exit -health
```

`-health` 只查询心跳，不启动内核、不获得任务所有权、不建表。5 秒续写心跳；运行中且 30 秒内有心跳才健康。

## 删除与解耦清单

- 删除 `node-worker -role exit-provision`、旧 exit/rejectedExit 探测和通用任务调度。
- 删除出口优先级通道、共享容量准入、旧出口超时与退避配置及对应旧契约测试。
- 删除住宅 quick verdict、自动属性/风险投递、风险缺失补投及住宅历史标记更新。
- 删除 collector 中无调用的旧出口回显、国家查询、来源优先排序等实现；collector 仅保留 IPPure 查询职责。
- 删除旧 `exit-prune` 命令，避免另一路定期删除出口事实；失败诊断由出口自身账本维护。
- Compose 出口服务改为 `node-exit`，不再依赖 Redis 或 manager；镜像与 npm 脚本新增入口。

属性服务现已改为 `node-attributes`，自行扫描可用节点当前已验证出口并补缺，详见 [独立属性服务](05-exit-attributes.md)。出口与属性之间没有任务投递或调用。

## 验收与上线

本地使用隔离 PostgreSQL schema 和回环回显服务，未连接生产、未部署：

- [x] 不可用节点不进入候选，执行前再次检查，不调用探测器。
- [x] 提交前变不可用、换 incarnation 或改配置时，出口和尝试记录均不落库。
- [x] 失败保留旧 IP、清除 verified、保持节点可用状态；成功刷新和 IP 变化版本正确。
- [x] 在没有快照、通用任务、属性表的最小数据库夹具中正常工作。
- [x] 重复实例被拒绝，持锁连接断开后旧写者无法提交，新实例可接管。
- [x] 最大并发、取消后等待任务退出、停止心跳状态正确。
- [x] 真实 Mihomo 经受控 HTTP 节点获取 HTTPS 回显；节点停止后不会直连成功。
- [x] 全后端 race 测试、vet 和部署脚本单元测试通过；包含共享启动代码的 Xray/Mihomo 测活回归。
- [ ] 构建生产镜像并执行容器验收（本机无 Docker）。
- [ ] 部署后测量到期队列等待、获取成功率、延迟、CPU/内存与失败类型。

升级先备份并停止所有旧写入者，执行 v16 清理迁移后再部署新入口。旧任务表与 Redis 已删除，回退旧版本必须恢复停机数据库备份；详见 [清理记录](11-project-cleanup.md)。
