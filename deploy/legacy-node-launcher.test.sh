#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
export PATROL_TEST_ARGS="$test_dir/args"
export PATH="$test_dir:$PATH"
cat > "$test_dir/github-patrol" <<'SH'
#!/bin/sh
printf '%s\n' "$#" "$@" > "$PATROL_TEST_ARGS"
exit "${PATROL_TEST_EXIT:-0}"
SH
chmod +x "$test_dir/github-patrol"

for script in server/github-patrol-service.js ./server/github-patrol-service.js /app/server/github-patrol-service.js; do
  sh deploy/legacy-node-launcher.sh "$script"
  test "$(head -n 1 "$PATROL_TEST_ARGS")" = 0
  sh deploy/legacy-node-launcher.sh "$script" --health
  diff -u <(printf '1\n--health\n') "$PATROL_TEST_ARGS"
done

# Forward arguments intact and propagate the Go process's failure status.
status=0
PATROL_TEST_EXIT=73 sh deploy/legacy-node-launcher.sh server/github-patrol-service.js 'an argument with spaces' --health || status=$?
test "$status" = 73
diff -u <(printf '2\nan argument with spaces\n--health\n') "$PATROL_TEST_ARGS"

for script in '' server/other.js -e; do
  rm -f "$PATROL_TEST_ARGS"
  status=0
  sh deploy/legacy-node-launcher.sh "$script" > "$test_dir/stdout" 2> "$test_dir/stderr" || status=$?
  test "$status" = 127
  test ! -e "$PATROL_TEST_ARGS"
  test ! -s "$test_dir/stdout"
  test -s "$test_dir/stderr"
done
printf '%s\n' 'Legacy patrol launcher checks passed.'
