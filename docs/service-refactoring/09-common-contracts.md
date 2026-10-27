# 公共数据契约

服务仅通过 PostgreSQL 事实表读取输入，不互相投递任务或触发流程。

| 数据 | 唯一业务写入者 | 读取者 |
| --- | --- | --- |
| 来源批次、节点配置、链接与严格指纹 | node-source | 测活、出口、API |
| available、connectivity、last_available_at | node-liveness | 出口、属性、API |
| exits | node-exit | 属性、API |
| ip_attributes | node-attributes | API |
| 各服务自有尝试表、心跳 | 对应服务 | 自身调度、API 心跳读取 |

新节点由采集以默认 available=false 创建；已有节点重入不得覆盖测活事实。identity 保留既有节点关联；strict-v1 配置指纹用于新采集去重，incarnation 用于防止旧结果写入已替换节点。测活直接读取 registry 当前配置，出口 version 防止属性错配，不能合并成一个版本字段。

共享表与只读视图由部署执行 `backend/internal/database/schema.sql`，服务启动只维护各自的调度表。旧库执行 `deploy/sql/016-retire-legacy.sql`，先停全部旧写入者并备份。没有 Redis、通用任务表、恢复或自动删除服务。

配置拒绝、内核故障、取消与节点不可达分别处理。只有有效测活结论可以变更 available。监控只允许聚合字段，原始节点凭据、供应商错误和进程配置不进入公开响应。

回归使用隔离 PostgreSQL、私有真实内核与 PHP 契约测试；禁止以单元测试结果宣称生产连通性已验证。
