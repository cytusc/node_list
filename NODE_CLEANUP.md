# 长期失活节点清理

`dead-verify` 是唯一常驻核验与删除服务。它用私有 Mihomo 对每批候选执行实际协议握手，以 DIRECT 或近期可用节点证明出口正常。内核配置拒绝与控制器故障不构成死亡证明。旧 `node-cleanup` 调度及 TCP 判死实现已移除。

候选筛选与删除保护共用 `NODE_CLEANUP_MIN_AGE_SECONDS`（默认 604800 秒）。节点自首次出现或最后成功起超过该窗口才可删除；来源重新导入不会刷新成功时间。`--min-age` 可收紧候选范围，不允许小于删除窗口。

最终事务检查配置摘要、incarnation、一分钟内的受控死亡证明、执行中探测、新于证明的连通性观察及最小存活池（`NODE_CLEANUP_MIN_POOL=500`）。握手结束时立即记录证明时间，等待删除并发槽位不会使旧证据变新。门禁拒绝仍保存有效核验记录；缓存带有配置摘要和实例标识，配置变化或重建后重新检查。

删除写入 tombstone 和审计，保留共享 IP 属性及其他节点数据；批量压缩另行清理快照副本。源冷却及 tombstone 年龄继承继续生效。`LIFECYCLE_DRY_RUN=true` 或 `--dry-run` 禁止删除及压缩。服务使用数据库单实例锁，重复容器不会启动第二套私有内核。

```sh
bash deploy/compose.sh up -d dead-verify
bash deploy/compose.sh exec dead-verify node-worker -role dead-verify -health
```

可配置批量 `DEAD_VERIFY_LIMIT=400`、握手并发 `DEAD_VERIFY_WORKERS=96`、删除并发 `DEAD_VERIFY_DELETERS=8`、超时 `DEAD_VERIFY_TIMEOUT_SECONDS=6`、复查间隔 `DEAD_VERIFY_RECHECK_SECONDS=21600`。策略与并发默认值未调整。

升级步骤和旧任务的迁移范围见 [MODULAR_SERVICES.md](MODULAR_SERVICES.md)。
