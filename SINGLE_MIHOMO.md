# 共享 Mihomo 与独立清理核验

当前常规探测共享内核；长期失活核验由 `dead-verify` 私有内核执行。服务划分与升级以 [MODULAR_SERVICES.md](MODULAR_SERVICES.md) 为准。

管理进程 `node-worker -role mihomo-manager` 独占 PostgreSQL 会话锁与控制器密钥。工作进程只访问内部 9080 API，共享固定 mixed 7890 入口；每个节点以稳定身份哈希用户名认证，IN-USER 规则绑定到该节点，末尾 MATCH,REJECT。不要将控制器或 mixed 端口发布到公网。

全部注册节点（含退役及清理候选）常驻内核。配置摘要不含展示名称；注册表稳定 5 秒后暂停新租约，排空后重载。单节点解析错误被隔离，未知加载失败恢复上一份配置并每 5 秒重试。租约不可续期、最长 150 秒，包含 incarnation、摘要和内核代次。严格证明写入及生命周期提交都持有租约；内核故障、重启、过期或配置变更要求重新评估。

快速检测为 gstatic generate_204、2 秒超时，32 路全局容量，住宅等待时为其保留 8 路，空闲允许借用。失败复核 8 路、出口 8 路、风险 2 路独立限流。快速结果保存在每节点最新一条记录；仅接受 HTTP 204，成功后持有租约写入连通性证明并更新 last_available_at，无需访问 GitHub。退役快速失败结束本轮；活跃失败必须经过两次 204 请求、重试间隔、直连与正常节点对照。204 成功本身不能恢复节点，仍需真实出口及相应属性核验。非住宅最多 128 个评估在途，取得证明或复核执行容量后才开始 3 分钟期限。普通测活和非住宅恢复间隔默认 20 分钟，住宅保持 5 分钟；出口监控保持 30 分钟。每 5 秒从数据库重新扫描可调度节点，管理进程每 5 秒刷新完整注册表，新增节点自动纳入。20 分钟是调度目标，实际完成时间取决于节点数、重载及核验背压。

Redis 按分钟聚合完成、成功、失败、异常数，保留 2 小时。面板一分钟显示上一个完整分钟，一小时包含当前分钟。管理服务展示加载数、隔离数、配置代次、重载次数及最后耗时；原严格恢复统计独立保留。

## 升级及回滚

1. 备份服务器 Compose、.env、release.env 和当前镜像 ID，保留旧代理容器。禁用自动更新定时器，避免观察期间覆盖验证镜像。
2. 设置随机 MIHOMO_MANAGER_TOKEN 和 MIHOMO_CONTROLLER_SECRET；设置 LIFECYCLE_DRY_RUN=true（默认），同时禁止新增退役、恢复和物理删除。保留原数据库、Redis、网络和采集器配置。
3. 停止 connectivity、ip-service、residential-recovery、normal-recovery、node-lifecycle、node-cleanup，再启动 shared-mihomo 和 mihomo-manager，健康后启动 exit-provision、exit-attributes、recovery 与 dead-verify。旧清理角色不再启动。上游 subs-check 不变。
4. 检查每节点真实路由、快速与严格证明、服务错误和管理健康，连续观察至少 30 分钟。记录快速与严格吞吐、住宅等待、恢复成功率、异常率及 CPU/内存。仅在证明准确后设置 LIFECYCLE_DRY_RUN=false 并重建生命周期与清理 worker。
5. 串线或错误删除证明出现时立即停止生命周期及清理 worker，停止新探测，恢复备份 Compose/.env/release.env 和旧镜像，再恢复旧 worker。数据库新增表兼容旧版本，无需回滚 schema。验收后才删除旧代理容器。

## 可复现检查

```sh
cd backend
NODE_EXPLORER_TEST_PG=postgres://... NODE_EXPLORER_TEST_REDIS=redis://... go test -race ./...
go vet ./...
MIHOMO_TEST_BINARY=/path/to/mihomo go test -race ./internal/transport -run Real
MIHOMO_TEST_BINARY=/path/to/mihomo MIHOMO_TIMEOUT_BENCH=1 go test -race ./internal/transport -run Timeout -v
```

真实内核隔离测试涵盖并发认证路由、无认证、错误认证、规则未匹配；控制器测试涵盖隔离坏配置、回滚、排空、过期、旧代次和重启。1000 个均耗尽 2 秒的可控代理用例在 32 并发下首轮须小于 90 秒；这不代表生产出口及风险核验速度。

## CPU 预算

共享内核默认限制为 1 核，管理服务默认 0.5 核，可通过 MIHOMO_CPUS / MIHOMO_MANAGER_CPUS 调整。32 路网络并发不变，满负载完成时间仍需实测。任务期限查询与在途评估计数使用专用索引，避免每次轮询扫描历史任务的 JSON。生产新增索引应先通过 CREATE INDEX CONCURRENTLY 安装，再部署包含 schema v6 的镜像，以减少数据库写入阻塞。

## 后台服务单实例锁

八个 node-worker 角色（采集、测活、出口、住宅恢复、普通恢复、生命周期、清理和 Mihomo 管理）启动时，按数据库 schema 与服务名称取得 PostgreSQL advisory session lock。锁在调度、消费任务或启动管理 HTTP 服务前取得；第二个同名服务立即报 `service already running: <服务名>` 并退出，不写心跳或消费任务。不同角色独立运行，角色内部并发保持不变。健康检查不申请该锁。

每个角色增加一个专用数据库连接，不占业务连接池的容量。每 2 秒检查持锁连接，单次超时 2 秒；发现连接失效后取消服务工作，等待其退出，再关闭连接。锁不自动重连，进程交由容器重启策略重启。正常退出、崩溃或数据库断开后，PostgreSQL 自动释放锁，不需要清理 PID 文件；网络中断后的接管以数据库确认旧会话结束为准。

升级时先停止旧 worker，再重建并启动新 worker；所有实例必须运行含此锁的版本，旧版本不会识别新锁。锁保护使用同一数据库和 schema 的后台 worker；PHP API 由 Workerman 管理多进程，Mihomo 控制器原有独占锁继续保留。第三方 subs-check 采集器不在此锁范围内。
