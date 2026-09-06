#!/bin/bash
# Tests the FDB container watcher as it is actually shipped: the script is
# EXTRACTED from .github/workflows/nightly-rowdiff.yml and executed against a
# stubbed `docker`, so this cannot drift from what the nightly runs and needs no
# daemon.
#
# The watcher exists because the forensics dump it feeds was measured capturing
# NOTHING on a night with a real cluster death: by the time any later step runs,
# the container has been removed, so evidence has to be taken while it is alive.
# Getting that right took several attempts, and each of the cases below is one of
# them:
#
#   - a 30-second `docker exec grep` cannot run after the container stops, and
#     fdbserver's death IS the container stopping, so the promoted snapshot was
#     always the pre-crash one.
#   - `tail -F` over a glob replaced it and was worse in two ways that only show
#     up when you run it: the glob expands ONCE at exec time, so a trace file
#     that does not exist yet is never captured at all, and a file created after
#     the attach — which is what rotation does — is never followed. It lost the
#     terminal line anyway, because `docker exec`'s pipe is torn down with the
#     container.
#   - copying the directory has none of those, because it re-globs every cycle.
#
# So the cases are: a late first file, a rotated second file, and the terminal
# line written as the container dies. Each was measured by hand first and each is
# here because a measurement that lives only in a scratch directory is a
# measurement nobody can re-run.
#
# WHAT THIS DOES NOT COVER, written first and from what was actually run rather
# than from what the cases happen to touch:
#
#   - the dump's loop over LIVE containers. The stub answers `phase=gone` by the
#     time the dump runs, so that loop iterates an empty set here. Its arms are
#     unexercised; only the trace-directory and last-inspect arms are driven.
#   - `docker exec` entirely — the stub refuses it, so the `df` sampler and the
#     in-container reads are unpinned.
#   - the watcher's deadline and its process-group teardown, which the workflow
#     comments describe and which were driven by hand against real processes.
#   - anything about a REAL fdbserver: every trace here is a fixture, so this
#     pins the plumbing and not the format it carries.
#
# Run: bash pkg/docscheck/rowdiff_watcher_suite.sh
set -uo pipefail
cd "$(dirname "$0")/../.."

SCRIPT=$(mktemp); BIN=$(mktemp -d); WORK=$(mktemp -d); STATE=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$BIN" "$WORK" "$STATE"' EXIT

# Extract the heredoc body the workflow writes as fdb-watch.sh.
awk '
  /^          cat > fdb-watch\.sh <<.SH.$/ { inb = 1; next }
  inb && /^          SH$/ { exit }
  inb { print substr($0, 11) }
' .github/workflows/nightly-rowdiff.yml > "$SCRIPT"
[ -s "$SCRIPT" ] || { echo "FATAL: extracted an empty watcher"; exit 1; }
# Sanity-check the EXTRACTION only. Asserting the behaviour's own text here would
# make a removed behaviour look like a broken test rather than a failing case.
grep -q 'fdb-watch.pid' "$SCRIPT" || { echo "FATAL: extraction has no pid handshake"; exit 1; }

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fail=1; }

# A `docker` stub with a container whose lifecycle the fixture drives through
# files in $STATE: `phase` is running|stopped|gone, and `logs/` is the trace
# directory `cp` copies out.
cat > "$BIN/docker" <<'STUB'
#!/bin/bash
phase=$(cat "$WATCHTEST_STATE/phase" 2>/dev/null || echo gone)
case "$1" in
  ps)
    [ "$phase" = gone ] || echo c1
    ;;
  inspect)
    [ "$phase" = gone ] && exit 1
    if [ "$phase" = running ]; then
      echo 'exit=0 oom=false running=true err= started=x finished=y'
    else
      echo 'exit=1 oom=false running=false err= started=x finished=y'
    fi
    ;;
  logs) sleep 3600 ;;
  exec) exit 1 ;;   # df sampling is not what these cases are about
  cp)
    # `docker cp c1:/var/fdb/logs/. dest/`
    [ "$phase" = gone ] && exit 1
    dest=$3
    mkdir -p "$dest"
    cp -r "$WATCHTEST_STATE/logs/." "$dest/" 2>/dev/null
    ;;
esac
exit 0
STUB
chmod +x "$BIN/docker"

# $1 = the periodic copy interval in seconds. Cases that want the periodic path
# pass 1; the case that isolates the EXIT-TRANSITION copy passes a number larger
# than the case's own lifetime, so the periodic loop cannot be what captured
# anything. Without that knob the two paths are indistinguishable, which is
# exactly what a first mutation run showed: deleting the exit copy changed
# nothing, because at a 1s interval the periodic loop reached the same file.
start_watcher() {
  rm -rf "$WORK"; mkdir -p "$WORK"
  cp "$SCRIPT" "$WORK/fdb-watch.sh"; chmod +x "$WORK/fdb-watch.sh"
  # A short deadline so a case cannot hang the suite.
  sed -i -e 's/^deadline=.*/deadline=$(( $(date +%s) + 30 ))/' -e "s/^\\( *\\)sleep 60\$/\\1sleep ${1:-1}/" "$WORK/fdb-watch.sh"
  ( cd "$WORK" && WATCHTEST_STATE="$STATE" FDB_IMAGE_TAG=x PATH="$BIN:$PATH" \
      setsid nohup ./fdb-watch.sh >/dev/null 2>&1 & )
  for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s "$WORK/fdb-watch.pid" ] && return 0; sleep 1; done
  return 1
}
stop_watcher() {
  [ -s "$WORK/fdb-watch.pid" ] && kill -TERM -"$(cat "$WORK/fdb-watch.pid")" 2>/dev/null
  sleep 1
}

echo "rowdiff fdb-watch:"

# The container appears with an EMPTY logs directory and the first trace file
# arrives later — the startup race that made `tail -F` capture nothing forever.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"; echo running > "$STATE/phase"
if start_watcher 1; then
  sleep 3
  echo '<Event Severity="10" file="one"/>' > "$STATE/logs/trace.001.xml"
  sleep 3
  # ROTATION: a second file, created after the watcher attached.
  echo '<Event Severity="10" file="two"/>' > "$STATE/logs/trace.002.xml"
  sleep 3
  grep -rq 'file="one"' "$WORK"/fdb-logs-*/ 2>/dev/null \
    && ok "a trace file created after the watcher attached is captured" \
    || bad "a trace file created after the watcher attached is captured"
  grep -rq 'file="two"' "$WORK"/fdb-logs-*/ 2>/dev/null \
    && ok "a ROTATED second trace file is captured" \
    || bad "a ROTATED second trace file is captured"

  # The terminal line, written as the container dies: appended and the phase
  # flipped to stopped in the same breath, so only a read of a STOPPED container
  # can see it.
  echo '<Event Severity="40" Type="SharedTLogFailed"/>' >> "$STATE/logs/trace.002.xml"
  echo stopped > "$STATE/phase"
  sleep 3
  grep -rq 'Severity="40"' "$WORK"/fdb-logs-*/ 2>/dev/null \
    && ok "the terminal Severity=40 line is captured after the container stops" \
    || bad "the terminal Severity=40 line is captured after the container stops"
  [ "$(grep -c 'EXITED' "$WORK/fdb-watch.log" 2>/dev/null)" = 1 ] \
    && ok "the exit transition is logged exactly once" \
    || bad "the exit transition is logged exactly once (got $(grep -c 'EXITED' "$WORK/fdb-watch.log" 2>/dev/null))"

  # Removal: the last inspect must SURVIVE it, per container.
  echo gone > "$STATE/phase"
  sleep 3
  ls "$WORK"/fdb-last-inspect-*.txt >/dev/null 2>&1 \
    && ok "the last inspect survives removal" \
    || bad "the last inspect survives removal"
  grep -q 'GONE' "$WORK/fdb-watch.log" 2>/dev/null \
    && ok "removal is logged" || bad "removal is logged"
  stop_watcher
else
  bad "the watcher never recorded a pid"
fi

# The EXIT-TRANSITION copy, isolated. The periodic interval is longer than this
# case lives, so the periodic loop cannot be what captured anything — and the
# container is REMOVED immediately after stopping, which is the shape that makes
# the exit copy the only reader that ever gets there. Without this case the exit
# copy is unpinned: deleting it changes nothing at a 1s interval, because the
# periodic loop reaches the same file within a cycle.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"; echo running > "$STATE/phase"
echo '<Event Severity="10" file="pre"/>' > "$STATE/logs/trace.001.xml"
if start_watcher 3600; then
  sleep 2
  # The fatal line and the stop, in the same breath.
  echo '<Event Severity="40" Type="SharedTLogFailed"/>' >> "$STATE/logs/trace.001.xml"
  echo stopped > "$STATE/phase"
  sleep 4
  echo gone > "$STATE/phase"
  sleep 2
  grep -rq 'Severity="40"' "$WORK"/fdb-logs-*-exit/ 2>/dev/null \
    && ok "the exit-transition copy alone captures the terminal line" \
    || bad "the exit-transition copy alone captures the terminal line"
  stop_watcher
else
  bad "the watcher never recorded a pid (exit-copy case)"
fi

# The DUMP block, which is a different piece of the same instrument and was not
# covered until it broke. A refactor emptied `[ -d "$d" ]` to `[ -d "" ]` in the
# loop that reads the watcher's copied trace directories: always false, so the
# body never ran, and the `|| echo "(no …)"` fallback did not fire either because
# the `for` still exits 0. A night with a dead cluster then reads exactly like a
# clean one — the empty-set false green, inside the change whose whole point was
# letting the dump read that directory.
#
# So the block is extracted and run the same way the watcher is, against a
# fixture holding what the watcher would have written.
DUMP=$(mktemp); DWORK=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$BIN" "$WORK" "$STATE" "$DUMP" "$DWORK"' EXIT
# Anchor on the STEP, not on the first ten-space brace in the file. There are
# three such braces; taking the first works only while this block stays first,
# and a new block above it would silently reroute the extraction while both
# guards below still passed — an instrument measuring the wrong thing, which is
# the failure this suite exists to catch.
awk '
  /^      - name: Capture FDB container forensics$/ { armed = 1 }
  armed && /^          \{$/ { inb = 1 }
  inb { print substr($0, 11) }
  inb && /^          \} > fdb-forensics\.txt/ { exit }
' .github/workflows/nightly-rowdiff.yml > "$DUMP"
[ -s "$DUMP" ] || { echo "FATAL: extracted an empty dump block"; exit 1; }
grep -q 'fdb-logs-' "$DUMP" || { echo "FATAL: dump extraction has no trace-directory arm"; exit 1; }
# And that it is THIS block: `=== host ===` opens the forensics dump and nothing
# else in the file. Without it a wrong-block extraction that happened to mention
# fdb-logs- would pass the guard above.
grep -q '=== host ===' "$DUMP" || { echo "FATAL: dump extraction picked the wrong block"; exit 1; }

mkdir -p "$DWORK/fdb-logs-c1-exit"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$DWORK/fdb-logs-c1-exit/trace.001.xml"
echo 'ts c1 exit=1 oom=false' > "$DWORK/fdb-last-inspect-c1.txt"
# The extracted block ENDS with `} > fdb-forensics.txt`, so its output lands in
# that file rather than on stdout — which is the artifact the night is read from,
# and therefore the right thing to assert against.
( cd "$DWORK" && PATH="$BIN:$PATH" WATCHTEST_STATE="$STATE" bash "$DUMP" >/dev/null 2>&1 )
if grep -q 'Severity="40"' "$DWORK/fdb-forensics.txt" 2>/dev/null; then
  ok "the dump reads the trace directory the watcher wrote"
else
  bad "the dump reads the trace directory the watcher wrote"
fi
if grep -q 'fdb-last-inspect-c1.txt' "$DWORK/fdb-forensics.txt" 2>/dev/null; then
  ok "the dump reads the per-container last inspect"
else
  bad "the dump reads the per-container last inspect"
fi

# The empty-capture ALARM, which sits below the dump and decides whether a night
# that captured nothing is reported as a defect or as a quiet night. It is pinned
# here because it has already been wrong once in a way nothing noticed: it wrote
# an `::error::` annotation and did not exit non-zero, so the step stayed GREEN
# while announcing it had captured nothing.
ALARM=$(mktemp); AWORK=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$BIN" "$WORK" "$STATE" "$DUMP" "$DWORK" "$ALARM" "$AWORK"' EXIT
awk '
  /^          have_inspect=0$/ { inb = 1 }
  inb { print substr($0, 11) }
  inb && /^          fi$/ { exit }
' .github/workflows/nightly-rowdiff.yml > "$ALARM"
[ -s "$ALARM" ] || { echo "FATAL: extracted an empty alarm block"; exit 1; }
grep -q 'captured NOTHING' "$ALARM" || { echo "FATAL: alarm extraction has no error arm"; exit 1; }

alarm_case() { # $1 name  $2 forensics content  $3 inspect content ("" = none)  $4 sweep  $5 paging  $6 want-rc
  rm -rf "$AWORK"; mkdir -p "$AWORK"
  printf '%s\n' "$2" > "$AWORK/fdb-forensics.txt"
  [ -n "$3" ] && printf '%s\n' "$3" > "$AWORK/fdb-last-inspect-c1.txt"
  ( cd "$AWORK" && SWEEP_OUTCOME="$4" PAGING_OUTCOME="$5" bash "$ALARM" > out 2>&1 )
  rc=$?
  if [ "$rc" = "$6" ]; then ok "$1 (rc=$rc)"; else bad "$1: rc=$rc, want $6"; fi
}

# The arm that was broken: nothing captured on a night a sweep FAILED must fail
# the step, not merely annotate it.
alarm_case "empty capture + deep sweep failed exits non-zero" '=== host ===' '' failure success 1
# The PAGING lane counts too — reading only the un-paged one printed "nothing to
# explain tonight" on a night with a death, one lane over.
alarm_case "empty capture + PAGING sweep failed exits non-zero" '=== host ===' '' success failure 1
# And a genuinely quiet night must stay quiet.
alarm_case "empty capture + both sweeps green stays green" '=== host ===' '' success success 0
# Evidence present, from either source, is not an empty capture.
alarm_case "a live container in the dump is evidence" '=== inspect c1 ===' '' failure success 0
alarm_case "a watcher inspect is evidence" '=== host ===' 'ts c1 exit=1' failure success 0

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
