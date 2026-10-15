#!/usr/bin/env bash
# Run from the deployment host. The command defaults to a read-only preview.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
container=${IP_TYPE_CLEAN_CONTAINER:-node_list-node-explorer-1}
clean_binary=${IP_TYPE_CLEAN_BINARY:-$PWD/backend/ip-type-clean}
if [[ ! -f "$clean_binary" && -f "$PWD/ip-type-clean" ]]; then clean_binary=$PWD/ip-type-clean; fi
if [[ ! -x "$clean_binary" ]]; then
  echo "Build first: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C backend build -o ip-type-clean ./cmd/ip-type-clean" >&2
  exit 1
fi
clean_binary=$(realpath "$clean_binary")
report_dir=${IP_TYPE_CLEAN_REPORT_DIR:-$PWD/ip-type-clean-reports}
mkdir -p "$report_dir"
chmod 700 "$report_dir"
report_dir=$(realpath "$report_dir")
clean_env=$(mktemp)
trap 'rm -f "$clean_env"' EXIT
chmod 600 "$clean_env"
# Do not print database credentials or pass them through process arguments.
python3 - "$container" "$clean_env" <<'PYENV'
import json, subprocess, sys
allowed = {"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSSLMODE",
           "IPINFO_TOKEN", "IP_API_PRO_KEY", "IPREGISTRY_API_KEY", "ABUSEIPDB_API_KEY"}
data = json.loads(subprocess.check_output(["docker", "inspect", sys.argv[1]]))[0]
with open(sys.argv[2], "w") as f:
    for value in data["Config"]["Env"]:
        if value.split("=", 1)[0] in allowed:
            if "\n" in value: raise SystemExit("Unsupported newline in environment value")
            f.write(value + "\n")
PYENV
image=$(docker inspect --format '{{.Image}}' "$container")
report_name="clean-$(date -u +%Y%m%dT%H%M%S)-$$.jsonl"
docker run --rm --user 0 --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --network "container:$container" --env-file "$clean_env" \
  -v "$clean_binary:/usr/local/bin/ip-type-clean:ro" -v "$report_dir:/reports" \
  --entrypoint /usr/local/bin/ip-type-clean "$image" --output "/reports/$report_name" "$@"
echo "Report: $report_dir/$report_name"
