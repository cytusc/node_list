#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
# Compose reads COMPOSE_FILE from the server's .env; retain the old file set
# for installations that have not added this setting yet.
files=()
if [[ -z "${COMPOSE_FILE:-}" ]] && ! grep -Eq '^[[:space:]]*COMPOSE_FILE[[:space:]]*=' .env; then
  files=(-f compose.yaml -f compose.subs-check.yaml)
fi
exec docker compose "${files[@]}" --env-file .env --env-file release.env "$@"
