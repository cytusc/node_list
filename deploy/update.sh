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
  local lookup_status
  if new_tag=$(python3 deploy/github_release.py "$repository" .update-state); then
    :
  else
    lookup_status=$?
    if [[ "$lookup_status" == 75 ]]; then
      echo "$service release check deferred; keeping $old_tag"
      return 0
    fi
    return "$lookup_status"
  fi
  if [[ "$service" == node-explorer ]]; then
    # Never downgrade a manually deployed newer tag, including a hotfix suffix.
    new_tag=$(python3 - "$old_tag" "$new_tag" <<'PYVERSION'
import re, sys
def version(tag, current=False):
    suffix = r"(?:-[A-Za-z0-9._-]+)?" if current else ""
    match = re.fullmatch(r"v([0-9]+)\.([0-9]+)\.([0-9]+)" + suffix, tag)
    if not match:
        raise ValueError("Invalid stable version: " + tag)
    return tuple(map(int, match.groups()))
current, latest = sys.argv[1:]
print(latest if version(latest) > version(current, current=True) else current)
PYVERSION
) || return 1
  fi
  if [[ "$new_tag" == "$old_tag" ]]; then
    echo "$service already running $old_tag"
    return 0
  fi
  [[ "$new_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  if [[ "$service" == subs-check ]]; then new_tag=${new_tag#v}; fi
  [[ "$old_tag" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9._-]+)?$ ]] || return 1
  if [[ "$new_tag" == "$old_tag" ]]; then
    echo "$service already running $old_tag"
    return 0
  fi
  local targets=("$service")
  if [[ "$service" == node-explorer ]]; then
    # Read the deployed service topology instead of assuming legacy roles.
    mapfile -t targets < <(compose config --format json | python3 -c '
import json,sys
config=json.load(sys.stdin)
for name, service in config["services"].items():
    if service.get("image", "").startswith("ghcr.io/cytusc/node_list:"):
        print(name)
')
    [[ ${#targets[@]} -gt 0 ]] || return 1
  fi
  echo "Updating $service $old_tag -> $new_tag"
  compose_tag "$variable" "$new_tag" pull "${targets[@]}" || return 1
  if compose_tag "$variable" "$new_tag" up -d --no-deps --wait --wait-timeout 180 "${targets[@]}" && healthy "$service"; then
    sed "/^$variable=/d" release.env > release.env.tmp
    printf '%s=%s\n' "$variable" "$new_tag" >> release.env.tmp
    chmod 600 release.env.tmp
    mv release.env.tmp release.env
    echo "Updated $service to $new_tag"
  else
    echo "Update failed; rolling back $service to $old_tag" >&2
    compose_tag "$variable" "$old_tag" up -d --no-deps --wait --wait-timeout 180 "${targets[@]}" || return 1
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
