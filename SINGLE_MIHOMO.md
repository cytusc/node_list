# 共享 Mihomo 的范围

共享内核仅服务出口 IP 核验和 IP 风险查询。`mihomo-manager` 负责配置重载、节点身份认证路由、租约和内核代次校验；工作进程通过内部管理 API 获取出口 / 风险容量。控制器和 mixed 端口不应发布到公网。

节点测活已迁至完全独立的 `node-liveness`，为每个任务启动私有 Xray 或 Mihomo；共享 manager 不再提供 delay 接口、全量巡检或恢复调度。节点可用状态只由独立测活服务更新。

部署边界见 [服务说明](MODULAR_SERVICES.md)，测活设计、配置、升级和验收见 [独立节点测活](docs/service-refactoring/10-node-liveness.md)。原快速 / 严格测活通道、recovery、dead-verify、自动生命周期治理和删除保护配置已移除，不应继续按旧文档启动。
