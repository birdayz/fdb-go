#!/bin/bash
# Tests infra/reap-leaked-containers.sh against stubbed `docker`, `pgrep`, `ps`
# and `free`, so it needs no daemon and no runner.
#
# The incident: a job timed out mid FDB C++ build; its runner killed Bazel, and
# the build container ran on into the next job, which the kernel OOM-killed.
# The cases pin both directions: the orphan goes, and neither the job's own
# containers nor the persistent bazel-remote cache are touched.
#
# Run: bash infra/reap_leaked_containers_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."

SCRIPT=infra/reap-leaked-containers.sh
BIN=$(mktemp -d)
STATE=$(mktemp -d)
trap 'rm -rf "$BIN" "$STATE"' EXIT

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
  printf '  FAIL %s\n' "$1"
  fail=1
}

# `docker` stub over $REAPTEST_STATE/containers, one "id started policy name image"
# line per running container; `rm` records the removal instead of performing one.
cat >"$BIN/docker" <<'STUB'
#!/bin/bash
c="$REAPTEST_STATE/containers"
case "$1" in
  ps) [ -s "$c" ] && awk '{ print $1 }' "$c" ;;
  inspect) awk -v id="${!#}" '$1 == id { print $2, $3, $4, $5 }' "$c" ;;
  rm) echo "${!#}" >>"$REAPTEST_STATE/removed" ;;
esac
exit 0
STUB
cat >"$BIN/pgrep" <<'STUB'
#!/bin/bash
[ -f "$REAPTEST_STATE/worker" ] && echo 4242
exit 0
STUB
cat >"$BIN/ps" <<'STUB'
#!/bin/bash
[ -s "$REAPTEST_STATE/etimes" ] && cat "$REAPTEST_STATE/etimes"
exit 0
STUB
cat >"$BIN/free" <<'STUB'
#!/bin/bash
echo "Mem: 7745 6600 200 36 900 1100"
STUB
chmod +x "$BIN"/*

ago() { date -u -d "@$(($(date -u +%s) - $1))" +%Y-%m-%dT%H:%M:%S.000000000Z; }

# One case. $1 name; $2 worker elapsed seconds ("" = no worker, "unreadable");
# $3 the removals expected, space-separated; $4 optional string the output must
# contain. The fixture is the containers file the caller wrote.
run_case() {
  rm -f "$STATE/removed" "$STATE/worker" "$STATE/etimes"
  case "$2" in
  "") ;;
  unreadable) touch "$STATE/worker" ;;
  *)
    touch "$STATE/worker"
    echo "$2" >"$STATE/etimes"
    ;;
  esac
  REAPTEST_STATE="$STATE" PATH="$BIN:$PATH" bash "$SCRIPT" >"$STATE/out" 2>&1
  got=$(sort "$STATE/removed" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')
  if [ "$got" = "$3" ]; then ok "$1 (removed: ${got:-none})"; else bad "$1: removed '${got}', want '$3'"; fi
  if [ -n "${4:-}" ]; then
    grep -q "$4" "$STATE/out" && ok "$1: says \"$4\"" || bad "$1: output does not contain \"$4\""
  fi
}

echo "reap-leaked-containers:"
cat >"$STATE/containers" <<EOF
build $(ago 1800) no /quirky_build foundationdb/build@sha256:c6133f
cache $(ago 120000) always /bazel-remote buchgr/bazel-remote-cache@sha256:8e17
live $(ago 30) no /eager_fdb foundationdb/foundationdb:7.3.77
EOF
# The orphan goes; bazel-remote (older, restart=always) and the job's own FDB
# container (newer than the worker) stay.
run_case "orphan build, cache and live container" 60 "build" "removing quirky_build"
# The same build container, started after the worker, is the job's own.
run_case "build newer than the worker" 1900 ""
# Fail closed, and say so: an unknown job start reaps nothing.
run_case "no worker visible" "" "" "reaping nothing"
run_case "worker age unreadable" unreadable "" "age unreadable"

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
