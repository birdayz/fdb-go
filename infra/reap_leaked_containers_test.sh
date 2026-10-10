#!/bin/bash
# Drives infra/reap-leaked-containers.sh against stubbed docker, pgrep and ps:
# an earlier job's orphans go; bazel-remote and live jobs' containers stay.
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

# The stubs accept only the production invocations, so a changed call fails here.
# containers: "id started name image [policy]"; an absent policy is Docker's "".
cat >"$BIN/docker" <<'STUB'
#!/bin/bash
c="$REAPTEST_STATE/containers"
case "$*" in
  "ps --format {{.ID}}") awk '{ print $1 }' "$c" ;;
  "inspect -f {{.State.StartedAt}} {{.Name}} {{.Config.Image}} {{.HostConfig.RestartPolicy.Name}} "*)
    awk -v id="${!#}" '$1 == id { print $2, $3, $4, $5 }' "$c" ;;
  "rm -fv "*) echo "${!#}" >>"$REAPTEST_STATE/removed" ;;
  *) echo "unexpected: docker $*" >&2; exit 2 ;;
esac
STUB
# workers: "pid etimes" lines; an empty etimes is an unreadable age.
cat >"$BIN/pgrep" <<'STUB'
#!/bin/bash
[ "$*" = "-x Runner.Worker" ] || { echo "unexpected: pgrep $*" >&2; exit 2; }
awk '{ print $1 }' "$REAPTEST_STATE/workers"
STUB
cat >"$BIN/ps" <<'STUB'
#!/bin/bash
[ "$1 $2 $3" = "-o etimes= -p" ] || { echo "unexpected: ps $*" >&2; exit 2; }
awk -v pid="$4" '$1 == pid { print $2 }' "$REAPTEST_STATE/workers"
STUB
chmod +x "$BIN"/*

ago() { date -u -d "@$(($(date -u +%s) - $1))" +%Y-%m-%dT%H:%M:%S.000000000Z; }

# One case. $1 name; $2 the workers' etimes, space-separated ("" = no worker,
# "-" = one whose age is unreadable); $3 the removals expected; $4 optional
# string the output must contain.
run_case() {
  rm -f "$STATE/removed"
  : >"$STATE/workers"
  pid=4242
  for et in $2; do
    [ "$et" = - ] && et=
    echo "$pid $et" >>"$STATE/workers"
    pid=$((pid + 1))
  done
  REAPTEST_STATE="$STATE" PATH="$BIN:$PATH" bash "$SCRIPT" >"$STATE/out" 2>&1
  rc=$?
  got=$(sort "$STATE/removed" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')
  if [ "$rc" -eq 0 ] && [ "$got" = "$3" ]; then
    ok "$1 (removed: ${got:-none})"
  else
    bad "$1: exit $rc, removed '$got', want exit 0 and '$3'"
    sed 's/^/       /' "$STATE/out"
  fi
  if [ -n "${4:-}" ]; then
    grep -q "$4" "$STATE/out" && ok "$1: says \"$4\"" || bad "$1: output does not contain \"$4\""
  fi
}

echo "reap-leaked-containers:"
cat >"$STATE/containers" <<EOF
build $(ago 1800) /quirky_build foundationdb/build@sha256:c6133f no
ryuk $(ago 1800) /reaper_1a2b testcontainers/ryuk:0.13.0
cache $(ago 120000) /bazel-remote buchgr/bazel-remote-cache@sha256:8e17 always
live $(ago 30) /eager_fdb foundationdb/foundationdb:7.3.77 no
EOF
# Orphans with policy "no" and "" go; bazel-remote and the job's own container stay.
run_case "orphans, cache and live container" "60" "build ryuk" "removing quirky_build"
run_case "orphans newer than the worker" "1900" ""
# Two workers: the older job owns them, so the newer worker's start must not decide.
run_case "older of two workers decides" "60 1900" ""
# Fail closed, and say so.
run_case "no worker visible" "" "" "reaping nothing"
run_case "worker age unreadable" "-" "" "age unreadable"

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
