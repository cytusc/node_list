#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
# Compose reads COMPOSE_FILE from the server's .env; retain the old file set
# for installations that have not added this setting yet.
files=()
if [[ -z "${COMPOSE_FILE:-}" ]] && ! grep -Eq '^[[:space:]]*COMPOSE_FILE[[:space:]]*=' .env; then
  files=(-f compose.yaml -f compose.subs-check.yaml)
fi
# Removed services are absent from `up`, so stop their old containers explicitly.
# Restrict removal to this Compose project and the retired service label.
for argument in "$@"; do
  if [[ "$argument" == up ]]; then
    project=$(docker compose "${files[@]}" --env-file .env --env-file release.env config --format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])')
    [[ -n "$project" ]] || exit 1
    for retired_service in node-cleanup recovery dead-verify; do
      retired=$(docker ps -aq --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$retired_service")
      if [[ -n "$retired" ]]; then
        while IFS= read -r container; do docker rm -f "$container"; done <<< "$retired"
      fi
    done
    break
  fi
done
exec docker compose "${files[@]}" --env-file .env --env-file release.env "$@"
