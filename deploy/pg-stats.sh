#!/usr/bin/env bash
# Append one line of table row counts and database size for growth tracking.
# Run from the deployment host once a day (cron). Read-only; the log lets the
# archival decisions in the roadmap observe before they touch data.
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

# Format: utc-timestamp|current_nodes|retired_nodes|snapshots|ippure_results|db_size
row=$(docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --network "container:$container" --env-file "$pg_env" \
  --entrypoint psql "$image" -X -q -t -A -F'|' -v ON_ERROR_STOP=1 -c \
  "select (select count(*) from node_explorer_current_nodes),
          (select count(*) from node_explorer_retired_nodes),
          (select count(*) from node_explorer_snapshots),
          (select count(*) from node_explorer_ippure_results),
          pg_size_pretty(pg_database_size(current_database()))")
printf '%s|%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$row" >> "$stats_file"
echo "pg-stats: appended to $stats_file"
