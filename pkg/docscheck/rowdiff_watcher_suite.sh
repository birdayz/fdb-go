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
#   - the container log stream the watcher backgrounds with `docker logs -f`,
#     which the dump tails and the upload ships. The watcher cases DO create the
#     file as a side effect, and no case reads it or asserts on it: the stub's
#     `logs` arm sleeps without emitting a byte, so what it holds here is the
#     stub's silence rather than anything a container said.
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
  logs)
    # `-f` FOLLOWS (the watcher's stream); `--tail` RETURNS (the dump's read).
    # Sleeping for BOTH wedges the dump's live-container loop the moment a case
    # leaves a container running, which is how this arm was found to be wrong.
    case " $* " in *" -f "*) sleep 3600 ;; *) echo "(stub container stdout)" ;; esac
    ;;
  exec) exit 1 ;;   # df sampling is not what these cases are about
  cp)
    # `docker cp c1:/var/fdb/logs/. dest/`
    # The container can vanish BETWEEN the loop's inspect and its copy; this
    # knob is that window, which no phase can express on its own.
    [ -f "$WATCHTEST_STATE/cpfail" ] && exit 1
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

# A COPY THAT FAILS must publish nothing. `mkdir` before `docker cp`, with the
# copy's failure swallowed, exposes `fdb-logs-$c` either way — and the dump then
# reads an empty directory, which it cannot tell from a healthy cluster that
# wrote no fatal trace. Worse than a missing directory, because the alarm below
# accepts a last-inspect as evidence and the trace section stays blank while
# LOOKING like it was read. The container vanishing between the loop's inspect
# and its copy is that window, and it is what the `cpfail` knob is.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
echo running > "$STATE/phase"; touch "$STATE/cpfail"
if start_watcher 1; then
  sleep 4
  if ls -d "$WORK"/fdb-logs-* >/dev/null 2>&1; then
    bad "a failed copy publishes no trace directory (found $(ls -d "$WORK"/fdb-logs-* | tr '\n' ' '))"
  else
    ok "a failed copy publishes no trace directory"
  fi
  # And the staging directory must not be left behind either — a dot-prefixed
  # leftover is invisible to the dump's glob but still litters the workspace the
  # upload globs.
  if ls -d "$WORK"/.fdb-logs-* >/dev/null 2>&1; then
    bad "a failed copy leaves no staging directory"
  else
    ok "a failed copy leaves no staging directory"
  fi
  # The same copy SUCCEEDING then publishes, so the case above is about the
  # failure and not about the watcher never getting that far.
  rm -f "$STATE/cpfail"
  sleep 4
  grep -rq 'Severity="40"' "$WORK"/fdb-logs-*/ 2>/dev/null \
    && ok "the copy publishes once it succeeds" \
    || bad "the copy publishes once it succeeds"
  stop_watcher
else
  bad "the watcher never recorded a pid (cpfail case)"
fi
rm -f "$STATE/cpfail"
# Hand the container back GONE. The sections below are written against an
# absent container, and a case that leaves one running silently retargets them.
echo gone > "$STATE/phase"

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

# The NO-DIRECTORY run, which is the other half of the same instrument and was
# the r58 defect one loop over. `|| echo` hung on the `for` cannot fire when the
# glob matches nothing: the name stays literal, `[ -d ]` is false, `continue` is
# the last command and the loop exits 0. So a night where the copier never made
# a directory printed NEITHER a directory NOR the fallback — the trace section
# was simply absent, which reads as "nothing fatal" rather than "nothing looked".
# The sibling `fdb-df-*` loop fired its fallback from the identical shape only
# because `[ -e ] &&` leaves a non-zero status, so the two shapes had to be
# driven separately or the working one would vouch for the broken one.
rm -rf "$DWORK"; mkdir -p "$DWORK"
( cd "$DWORK" && PATH="$BIN:$PATH" WATCHTEST_STATE="$STATE" bash "$DUMP" >/dev/null 2>&1 )
for want in "(no fdb-logs-* directories)" "(no fdb-df-*.txt)" "(no fdb-container-*.log)"; do
  if grep -qF "$want" "$DWORK/fdb-forensics.txt" 2>/dev/null; then
    ok "an empty capture says \"$want\""
  else
    bad "an empty capture says \"$want\""
  fi
done

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

# The TAG GUARD, which sits ABOVE the body each of these steps runs and so falls
# outside every block extracted for them. It exists because the forensics step
# once hardcoded `:7.3.77` in its container filter — a stale ancestor filter
# emptying a loop, which is the exact failure that step exists to report on — so
# a missing pin has to be loud rather than silently matching nothing. That makes
# it the arm here whose absence would reinstate a SHIPPED bug.
#
# There are TWO copies, one per step, and covering one is how a suite reports the
# guard as covered while the other stays free to be deleted. Measured on this
# file at 18 arms: deleting the forensics copy reddens exactly one arm, and
# deleting the WATCHER copy reddened NONE until the cases below were driven over
# both. So the extraction takes the step.
GUARD=$(mktemp); GWORK=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$BIN" "$WORK" "$STATE" "$DUMP" "$DWORK" "$ALARM" "$AWORK" "$GUARD" "$GWORK"' EXIT
mkdir -p "$GWORK"

# $1 step name, $2 the literal line FOLLOWING the guard in that step. Terminating
# on a structural line after the guard rather than on the guard's own text is
# what lets a DELETED guard fail a CASE instead of failing the extraction — the
# same reason the sanity check below reads the pin and not the guard.
guard_cases() {
  awk -v step="      - name: $1" -v term="$2" '
    $0 == step { armed = 1 }
    armed && /^          ver="/ { inb = 1 }
    inb && $0 == term { exit }
    inb { print substr($0, 11) }
  ' .github/workflows/nightly-rowdiff.yml > "$GUARD"
  [ -s "$GUARD" ] || { echo "FATAL: extracted an empty tag guard for '$1'"; exit 1; }
  grep -q 'FDB_VERSION=' "$GUARD" || { echo "FATAL: tag-guard extraction for '$1' has no pin read"; exit 1; }

  printf 'test --test_env=FDB_VERSION=7.3.77\n' > "$GWORK/.bazelrc"
  if ( cd "$GWORK" && bash "$GUARD" >/dev/null 2>&1 ); then
    ok "$1: a pinned .bazelrc passes the tag guard"
  else
    bad "$1: a pinned .bazelrc passes the tag guard"
  fi

  printf 'test --test_output=errors\n' > "$GWORK/.bazelrc"
  ( cd "$GWORK" && bash "$GUARD" > "$GWORK/out" 2>&1 )
  rc=$?
  if [ "$rc" != 0 ] && grep -q 'no FDB_VERSION' "$GWORK/out"; then
    ok "$1: an unpinned .bazelrc fails the tag guard loudly"
  else
    bad "$1: an unpinned .bazelrc fails the tag guard loudly (rc=$rc)"
  fi
}

guard_cases "Capture FDB container forensics" "          {"
guard_cases "Watch the FDB container while it is alive" '          echo "watching image foundationdb/foundationdb:$ver"'

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
