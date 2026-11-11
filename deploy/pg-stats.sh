#!/usr/bin/env bash
# Append one line of table row counts and database size for growth tracking.
# Run from the deployment host once a day (cron). Read-only; the log lets the
# archival decisions in the roadmap observe before they touch data.
#
# Also appends per-table total sizes (indexes included) to
# backups/postgres-table-sizes.log: row counts alone hid a 249MB rollback table
# for weeks (ip_type_clean_backup, 2026-10), sizes make silent growth visible.
#
# Credential handling matches deploy/pg-backup.sh: coordinates come from the
# app container via docker inspect, the query runs in a temporary container.
#
# Environment overrides:
#   PG_STATS_SOURCE_CONTAINER  app container holding the PG* env (default node_list-node-explorer-1)
#   PG_STATS_IMAGE             client image with psql (default postgres:18-alpine)
set -Eeuo pipefail
cd "$(dirname "$0")/.."

container=${PG_STATS_SOURCE_CONTAINER:-node_list-node-explorer-1}
image=${PG_STATS_IMAGE:-postgres:18-alpine}
stats_dir=$PWD/backups
mkdir -p "$stats_dir"
chmod 700 "$stats_dir"
stats_file=$stats_dir/postgres-stats.log
sizes_file=$stats_dir/postgres-table-sizes.log

pg_env=$(mktemp)
trap 'rm -f "$pg_env"' EXIT
chmod 600 "$pg_env"
python3 - "$container" "$pg_env" <<'PYENV'
import json, subprocess, sys
allowed = {"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSSLMODE"}
data = json.loads(subprocess.check_output(["docker", "inspect", sys.argv[1]]))[0]
with open(sys.argv[2], "w") as f:
    for value in data["Config"]["Env"]:
        if value.split("=", 1)[0] in allowed:
            if "\n" in value: raise SystemExit("Unsupported newline in environment value")
            f.write(value + "\n")
PYENV

# Format: utc-timestamp|registry|available|ip_attributes|db_size
row=$(docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --network "container:$container" --env-file "$pg_env" \
  --entrypoint psql "$image" -X -q -t -A -F'|' -v ON_ERROR_STOP=1 -c \
  "select (select count(*) from node_explorer_node_registry),
          (select count(*) from node_explorer_node_registry WHERE available),
          (select count(*) from node_explorer_ip_attributes),
          pg_size_pretty(pg_database_size(current_database()))")
printf '%s|%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$row" >> "$stats_file"

# Format: utc-timestamp|table:size table:size ... (top 8 by total size, indexes included)
sizes=$(docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --network "container:$container" --env-file "$pg_env" \
  --entrypoint psql "$image" -X -q -t -A -F'|' -v ON_ERROR_STOP=1 -c \
  "SELECT string_agg(format('%s:%s', relname, pg_size_pretty(sz)), ' ' ORDER BY sz DESC)
     FROM (SELECT relname, pg_total_relation_size(relid) AS sz
             FROM pg_stat_user_tables
            WHERE schemaname='public' AND relname LIKE 'node_explorer%'
            ORDER BY sz DESC LIMIT 8) t")
printf '%s|%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$sizes" >> "$sizes_file"
echo "pg-stats: appended to $stats_file and $sizes_file"
