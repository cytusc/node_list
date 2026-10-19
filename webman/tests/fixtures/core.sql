-- 集成测试的最小 schema。
--
-- 只包含 Go 侧内联在 backend/internal/store/postgres/postgres.go 里的两张核心表
-- （snapshots / retired_nodes）；其余表与视图由 backend 的 services.sql、live.sql
-- 直接执行，避免在本仓库复制一份会漂移的 DDL。
--
-- 用法见 tests/integration.php。

CREATE TABLE IF NOT EXISTS node_explorer_snapshots (
  id SERIAL PRIMARY KEY,
  fingerprint TEXT NOT NULL UNIQUE,
  checked_at TEXT,
  imported_at TEXT NOT NULL,
  node_count INTEGER NOT NULL,
  payload TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS node_explorer_retired_nodes (
  identity TEXT PRIMARY KEY,
  retired_at TEXT NOT NULL,
  reason TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS node_explorer_ippure_results (
  snapshot_id INTEGER NOT NULL REFERENCES node_explorer_snapshots(id),
  node_index INTEGER NOT NULL,
  payload TEXT NOT NULL,
  PRIMARY KEY (snapshot_id, node_index)
);
