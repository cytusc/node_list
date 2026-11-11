#!/usr/bin/env bash
# Daily PostgreSQL backup for the node_explorer database.
# Run from the deployment host (cron or manual). Read-only against the
# database; every failure exits non-zero so cron/systemd surfaces it.
#
# Database coordinates are read from
# the running app container with docker inspect, the dump runs in a temporary
# container on the app's network namespace, and credentials never appear in
# arguments, logs or this file.
#
# Environment overrides:
#   PG_BACKUP_SOURCE_CONTAINER  app container holding the PG* env (default node_list-node-explorer-1)
#   PG_BACKUP_IMAGE             client image with pg_dump (default postgres:18-alpine, matches the server)
#   PG_BACKUP_DIR               output directory (default <repo>/backups/postgres)
#   PG_BACKUP_KEEP              dumps to retain (default 14)
set -Eeuo pipefail
cd "$(dirname "$0")/.."

container=${PG_BACKUP_SOURCE_CONTAINER:-node_list-node-explorer-1}
image=${PG_BACKUP_IMAGE:-postgres:18-alpine}
backup_dir=${PG_BACKUP_DIR:-$PWD/backups/postgres}
keep=${PG_BACKUP_KEEP:-14}
mkdir -p "$backup_dir"
chmod 700 "$backup_dir"
backup_dir=$(realpath "$backup_dir")

# One backup at a time: overlapping cron runs must not race the retention pass.
exec 9>"$PWD/.pg-backup.lock"
flock -n 9 || { echo "pg-backup: another run holds the lock, skipping" >&2; exit 0; }

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

stamp=$(date -u +%Y%m%dT%H%M%SZ)
target="node-explorer-$stamp.dump"
# Custom format: compressed and selectively restorable.
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --network "container:$container" --env-file "$pg_env" \
  -v "$backup_dir:/backup" --entrypoint pg_dump "$image" \
  -Fc -f "/backup/$target"
# A dump that cannot be listed is not a backup: fail before retention runs.
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -v "$backup_dir:/backup" --entrypoint pg_restore "$image" \
  -l "/backup/$target" >/dev/null

# Retention: newest $keep dumps stay, older ones go. Names sort by stamp.
ls -1t "$backup_dir"/node-explorer-*.dump | tail -n +$((keep + 1)) | while read -r old; do
  rm -f -- "$old"
done
echo "pg-backup: $backup_dir/$target ($(du -h "$backup_dir/$target" | cut -f1)), keep=$keep"
