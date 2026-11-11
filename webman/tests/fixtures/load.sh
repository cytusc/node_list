#!/usr/bin/env bash
# 在测试用 PostgreSQL 上重建 node_list 的最小 schema 并灌入集成测试数据。
#
# 前置：一个可写的库（默认 node_explorer_test），以及 backend/ 的 SQL 源文件。
# 用法：
#   PGURL="postgresql://postgres:test_password@127.0.0.1:5432/node_explorer_test" \
#     bash tests/fixtures/load.sh
#
# 设计说明：schema 与视图直接执行 backend/internal/database/schema.sql，
# 不在 webman 里复制一份 DDL —— 两边共用同一份定义才能真正比出差异。
set -euo pipefail

PGURL="${PGURL:-postgresql://postgres:test_password@127.0.0.1:5432/node_explorer_test}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
psql "$PGURL" -v ON_ERROR_STOP=1 -q -f "$ROOT/../backend/internal/database/schema.sql"

# 种子数据里的 __NOW2M__ 必须替换成真实时间戳：优选位判定要求连通性/出口证据
# 在 45 分钟内，写死的时间戳会随时间失效。
NOW2M="$(python3 -c 'from datetime import datetime,timedelta,timezone; print((datetime.now(timezone.utc)-timedelta(minutes=2)).strftime("%Y-%m-%dT%H:%M:%SZ"))')"
RESOLVED="$(mktemp)"
sed "s/__NOW2M__/$NOW2M/g" "$HERE/seed.sql" > "$RESOLVED"
printf '== %s (__NOW2M__ -> %s)\n' "$HERE/seed.sql" "$NOW2M"
psql "$PGURL" -v ON_ERROR_STOP=1 -q -f "$RESOLVED"
rm -f "$RESOLVED"

psql "$PGURL" -c "select count(*) as live_nodes from node_explorer_live_nodes"
