#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
exec 9>.update.lock
flock -n 9 || exit 0

compose() { bash deploy/compose.sh "$@"; }
compose_tag() {
  env "$1=$2" bash deploy/compose.sh "${@:3}"
}
healthy() {
  local container state
  for attempt in $(seq 1 60); do
    container=$(compose ps -q "$1")
    state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    [[ "$state" == healthy ]] && return 0
    [[ "$state" == unhealthy || "$state" == exited ]] && return 1
    sleep 2
  done
  return 1
}
update_one() {
  local service=$1 variable=$2 repository=$3 old_tag new_tag
  old_tag=$(sed -n "s/^$variable=//p" release.env)
  new_tag=$(curl --fail --silent --show-error --retry 2 --max-time 30 \
    "https://api.github.com/repos/$repository/releases/latest" | \
    python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])') || return 1
  [[ "$new_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  if [[ "$service" == subs-check ]]; then new_tag=${new_tag#v}; fi
  [[ "$old_tag" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  if [[ "$new_tag" == "$old_tag" ]]; then
    echo "$service already running $old_tag"
    return 0
  fi
  local targets=("$service")
  if [[ "$service" == node-explorer ]]; then
    targets=(node-explorer source-ingest mihomo-manager connectivity ip-service residential-recovery normal-recovery node-cleanup node-lifecycle)
  fi
  echo "Updating $service $old_tag -> $new_tag"
  compose_tag "$variable" "$new_tag" pull "${targets[@]}" || return 1
  if compose_tag "$variable" "$new_tag" up -d --no-deps --wait --wait-timeout 120 "${targets[@]}" && healthy "$service"; then
    sed "/^$variable=/d" release.env > release.env.tmp
    printf '%s=%s\n' "$variable" "$new_tag" >> release.env.tmp
    chmod 600 release.env.tmp
    mv release.env.tmp release.env
    echo "Updated $service to $new_tag"
  else
    echo "Update failed; rolling back $service to $old_tag" >&2
    compose_tag "$variable" "$old_tag" up -d --no-deps --wait --wait-timeout 120 "${targets[@]}" || return 1
    return 1
  fi
}
failed=0
if [[ "${NODE_EXPLORER_AUTO_UPDATE:-}" == false ]] || grep -Eq '^NODE_EXPLORER_AUTO_UPDATE=(false|0)$' .env; then
  echo "node-explorer automatic updates disabled by .env"
else
  update_one node-explorer IMAGE_TAG cytusc/node_list || failed=1
fi
update_one subs-check SUBS_CHECK_TAG beck-8/subs-check || failed=1
exit "$failed"
