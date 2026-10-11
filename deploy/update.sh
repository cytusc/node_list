#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
exec 9>.update.lock
flock -n 9 || exit 0

compose() { docker compose --env-file .env --env-file release.env "$@"; }
old_tag=$(sed -n 's/^IMAGE_TAG=//p' release.env)
new_tag=$(curl --fail --silent --show-error --retry 2 --max-time 30 \
  https://api.github.com/repos/cytusc/node_list/releases/latest | \
  python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')
[[ "$old_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$new_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
if [[ "$new_tag" == "$old_tag" ]]; then
  echo "Already running $old_tag"
  exit 0
fi
echo "Updating $old_tag -> $new_tag"
IMAGE_TAG="$new_tag" compose pull
healthy() {
  local container state
  for attempt in $(seq 1 60); do
    container=$(compose ps -q node-explorer)
    state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    [[ "$state" == healthy ]] && return 0
    [[ "$state" == unhealthy || "$state" == exited ]] && return 1
    sleep 2
  done
  return 1
}
if IMAGE_TAG="$new_tag" compose up -d --wait --wait-timeout 120 && healthy; then
  printf 'IMAGE_TAG=%s\n' "$new_tag" > release.env.tmp
  mv release.env.tmp release.env
  echo "Updated to $new_tag"
else
  echo "Update failed; rolling back to $old_tag" >&2
  IMAGE_TAG="$old_tag" compose up -d --wait --wait-timeout 120
  exit 1
fi
