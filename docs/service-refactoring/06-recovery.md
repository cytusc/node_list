# recovery：恢复与生命周期重构 TODO

> 实施更新：测活统一迁至 [独立节点测活服务](10-node-liveness.md)。manager 不再测活；recovery 和 dead-verify 已删除。本文保留为实施前规划记录，冲突部分不再适用。

状态：待实施。优先级 P1。前置：COMMON-01～03、MGR-01～03、出口/属性契约稳定。

## 当前职责与依据

住宅与普通退役节点恢复、依赖探测和生命周期已合并到同一个服务。消费 `recovery`、`normal-recovery`、`fast-delay`、`connectivity`、`lifecycle`，因此不能按服务名误认为只运行一种检测。永久删除归 dead-verify。

依据：[runner.go](../../backend/internal/service/runner.go)、[recovery.go](../../backend/internal/service/recovery.go)、[normal_recovery.go](../../backend/internal/service/normal_recovery.go)、[lifecycle_policy.go](../../backend/internal/service/lifecycle_policy.go)。普通恢复已有分散调度/退避，不能重构成所有退役节点同时高频重试。

## TODO

- [ ] **REC-01 / P1**：画出退役、住宅/普通评估、等待依赖、恢复与终止的状态表，列明每个转换所需证据、写入者和幂等键；区分事实更新与动作命令。
- [ ] **REC-02 / P1**：拆分评估策略、依赖协调、探测执行和生命周期提交，保持现有单服务部署。把依赖关系显式建模，减少通过共享 Runner/Payload 隐式耦合。
- [ ] **REC-03 / P1**：统一依赖会话、证据有效期、配置代际和 lease 丢失后的终止规则；陈旧成功不能恢复新配置，陈旧失败不能退役当前节点。
- [ ] **REC-04 / P1**：整理住宅/普通队列的准入、退避、超时预算与公平性，区分等待依赖、等待内核、有效评估失败；保留已开始评估的推进能力及普通恢复错峰。
- [ ] **REC-05 / P1**：梳理 dry-run 的覆盖范围和可观测输出，明确哪些任务/证据可写、哪些退役/恢复动作被抑制。不要把 dry-run 当作数据库完全只读。
- [ ] **REC-06 / P2**：提供每类候选数、等待阶段、评估年龄、恢复成功率及证据不足原因，先让状态机可解释，再决定是否需要进一步拆部署。

## 验收与回退

以状态转换场景验证并发巡检、出口变化、取消、依赖超时、旧任务复投、恢复与删除竞争；不得使用基础设施故障累加死亡证据。执行后端检查及隔离 PostgreSQL/Redis 集成。保留现有策略默认值；状态或 Payload 升级采用旧版本可读的渐进迁移，不在同一次上线更改退役阈值。
