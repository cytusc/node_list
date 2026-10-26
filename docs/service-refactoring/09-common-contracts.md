# 跨服务公共契约与任务底座 TODO

状态：待实施。优先于大规模移动代码，防止七个服务分别重构出不同的身份、任务和证据语义。

依据：[Repository](../../backend/internal/service/repository.go)、[Runner](../../backend/internal/service/runner.go)、[配置](../../backend/internal/service/config.go)、[任务队列](../../backend/internal/taskqueue/redis.go)、[PostgreSQL adapter](../../backend/internal/store/postgres/postgres.go)。

## TODO

- [ ] **COMMON-01 / P0：数据所有权表**。列出节点身份、原始链接/快照、注册表、连通性、出口、IP 属性、退役、恢复、永久删除、任务和监控的写入入口、事务范围、版本键及读取方。区分唯一执行所有者与合法的多个事实生产者，不强行断言连通性只有一个写入者。
- [ ] **COMMON-02 / P0：持久任务协议**。记录 PostgreSQL 账本与 Redis 引用投递关系；定义发布、领取、lease、generation、优先级、依赖等待、完成与重试状态。审查抽取独立任务模块的接口，避免各服务自行实现不同的 lease 规则。保证重复/过期引用、发布失败、消费者崩溃、锁所有权丢失不会造成过时提交。
- [ ] **COMMON-03 / P0：执行与领域分层**。明确 Runner 公共执行设施与各角色用例接口，按职责拆分目前共享的 probes、schedule、repository；不以拆文件代替设计边界，不盲目把事务跨服务远程化。统一 manager 错误分类、探测证据和版本信息。
- [ ] **COMMON-04 / P0：配置、迁移与健康契约**。建立逐角色配置表及有效范围，区分默认值、Compose 覆盖和生产实际值；明确迁移责任、启动就绪、角色锁、controller 锁、存活、心跳与进度的不同语义。当前多个启动入口会进入 adapter 初始化，应先核实迁移互斥与兼容策略再决定集中迁移，不直接判断为故障。
- [ ] **COMMON-05 / P1：回归与上线模板**。复用现有身份/分享链接 fixture、租约代际、巡检证据、恢复和删除保护测试；新增测试针对改变的行为及并发风险。规定脱敏固定样本、版本记录、性能基线、隔离集成环境、灰度观察与回退条件。

## 必须保留的现有约束

identity/ConnectionKey、config digest、incarnation 和任务 generation 各有用途，不能合并成一个模糊版本号。对外节点链接必须保留来源；配置归一化不能静默放宽 TLS。共享 controller 单一所有权、租约隔离和角色锁丢失后取消/等待在途工作继续有效。

任务账本在 PostgreSQL，Redis 不是唯一真相；删除权保留在受保护 dead-verify 路径。配置拒绝、内核异常、容量不足、取消和有效节点失败需要区分。dry-run 不是完全只读保证。监控只暴露允许的聚合字段，不能直接返回 worker payload 或原始错误。

## 验收

先交付数据所有权表、任务状态图、逐角色配置/健康表和兼容迁移方案，再开始大范围移动实现。后端行为变更执行 `npm run test:backend`；涉及 PHP/前端、部署分别增加对应检查。真实 PostgreSQL/Redis/Mihomo 集成需明确环境、版本、样本和跳过项，不将单元测试通过表述为生产联通性已验证。
