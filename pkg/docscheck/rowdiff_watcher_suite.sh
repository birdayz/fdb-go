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
#   - the ATOMICITY the publish rests on. The copy lands in a dot-prefixed
#     staging directory and is published by renaming it onto `$launch-$gen`, a
#     name that never pre-exists, so `rename(2)` on one filesystem makes a
#     generation's appearance indivisible. No arm lands a signal inside a RENAME
#     — that property is relied on. Signals inside the two SLOW operations either
#     side of it are driven: inside the copy (the `slowcp` knob) and inside the
#     retiring deletion (the `slowrm` shim). What the arms drive is what the
#     construction is for — a failed copy publishes nothing, one generation
#     survives, a deletion cut short leaves nothing published behind, and no
#     staging outlives the copier.
#   - the TERM arm of the watcher's trap. It makes the exit prompt and explicit,
#     and it is NOT what makes the EXIT trap reachable: removing it reddens
#     nothing, and measured directly, a group SIGTERM with an EXIT trap set and
#     no TERM trap still ran that trap 5 times out of 5 on bash 5.3.15. An
#     earlier version of this entry justified the arm from documented behaviour
#     nobody here had run, which is the substitution this file exists to catch.
#   - the trash DELETION, as distinct from the retire rename. Dropping the rename
#     is driven — a deletion cut short then leaves a published generation behind,
#     which the slow-`rm` shim makes deterministic. Dropping the DELETION reddens
#     nothing, because the trap removes every dot-prefixed leftover on the way
#     out. It is kept anyway: a trap only runs at exit, and a four-hour lane
#     should not accumulate a retired generation per cycle until then.
#   - TWO copiers for one container. `$c` is `docker ps -aq … | head -1`, so with
#     two containers in a lane the older one can be re-selected after the newer
#     goes and get a SECOND copier while its first is still looping. Generation
#     names are scoped by a parent-shell launch counter so the two cannot collide,
#     but no case here starts a second container at all — the stub serves exactly
#     one. The scoping is reasoned, not driven.
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

# `rm` shim. Slow ONLY for the names the retire path deletes, so a case can put
# the stop signal inside a deletion deterministically. Everything else delegates
# straight through, so the shim cannot perturb the rest of the run.
cat > "$BIN/rm" <<'STUB'
#!/bin/bash
if [ -f "$WATCHTEST_STATE/slowrm" ]; then
  for a in "$@"; do
    case "$a" in
      .fdb-logs-*.trash|fdb-logs-*.[0-9]*) sleep 3; break ;;
    esac
  done
fi
exec /usr/bin/rm "$@"
STUB
chmod +x "$BIN/rm"

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
    # A slow copy, so the stop signal is GUARANTEED to land inside it rather
    # than landing there once in a thousand runs. Without this knob the staging
    # arms pass because the window is narrow, not because it is closed — which
    # is what a review measured by widening exactly this call.
    [ -f "$WATCHTEST_STATE/slowcp" ] && sleep 3
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
  # CONFIRM the group is gone rather than sleeping once and assuming it. Three
  # arms read the workspace after this returns, and a stop that has not landed
  # turns each of them into a race against a loop that is still writing. The
  # workflow's own stop step escalates the same way, so this mirrors what ships.
  [ -s "$WORK/fdb-watch.pid" ] || return 0
  pgid=$(cat "$WORK/fdb-watch.pid")
  kill -TERM -"$pgid" 2>/dev/null
  for _ in $(seq 1 40); do
    kill -0 -"$pgid" 2>/dev/null || return 0
    sleep 0.25
  done
  kill -KILL -"$pgid" 2>/dev/null
  sleep 0.5
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
  # `[ -s ]`, not `ls`: the alarm below counts an inspect as evidence only when it
  # is NON-EMPTY, so an arm that accepts a zero-byte file accepts one the alarm
  # would reject — the suite and the instrument disagreeing about what counts.
  [ -s "$(ls "$WORK"/fdb-last-inspect-*.txt 2>/dev/null | head -1)" ] \
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
  # Read AFTER the quiesce: staging exists for a moment on every copy, so a live
  # check reports a failure whenever it lands in that moment — a flake, not a
  # finding.
  if ls -d "$WORK"/.fdb-logs-*-exit.new >/dev/null 2>&1; then
    bad "the exit-transition copy leaves no staging directory"
  else
    ok "the exit-transition copy leaves no staging directory"
  fi
else
  bad "the watcher never recorded a pid (exit-copy case)"
fi

# The exit copy FAILING. The arm above cannot see the failure branch's cleanup —
# on a copy that succeeds the staging directory is consumed by the rename, so
# deleting that branch's `rm -rf` changes nothing and the arm stays green. This
# was measured: the first version of that arm was vacuous against exactly the
# mutation it was written for. So the failure is driven directly, with `cpfail`
# held across the transition, and both halves are asserted — nothing staged is
# left behind, and the watcher SAYS the copy did not happen rather than leaving
# a reader to infer it from an absent directory.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"; echo running > "$STATE/phase"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
touch "$STATE/cpfail"
if start_watcher 3600; then
  sleep 2
  echo stopped > "$STATE/phase"
  sleep 4
  stop_watcher
  if ls -d "$WORK"/.fdb-logs-*-exit.new >/dev/null 2>&1; then
    bad "a FAILED exit copy leaves no staging directory"
  else
    ok "a FAILED exit copy leaves no staging directory"
  fi
  grep -q 'NOT copied at exit' "$WORK/fdb-watch.log" 2>/dev/null \
    && ok "a FAILED exit copy says so in the watcher log" \
    || bad "a FAILED exit copy says so in the watcher log"
else
  bad "the watcher never recorded a pid (failing-exit case)"
fi
rm -f "$STATE/cpfail"; echo gone > "$STATE/phase"

# THE PUBLISH IS ALL-OR-NOTHING, and what it publishes is one COMPLETE snapshot.
#
# `mkdir` before `docker cp` with the failure swallowed exposes `fdb-logs-$c`
# either way, and the dump then reads an empty directory it cannot tell from a
# healthy cluster that wrote no fatal trace — worse than a missing directory,
# because the alarm below accepts a last-inspect as evidence, so the trace
# section stays blank while LOOKING like it was read. The container vanishing
# between the loop's inspect and its copy is that window, and it is what the
# `cpfail` knob is: no container phase can express it, since `inspect` has to
# succeed and `cp` has to fail in the same cycle.
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
  # The same copy SUCCEEDING then publishes, so the arm above is about the
  # failure and not about the watcher never getting that far.
  rm -f "$STATE/cpfail"
  sleep 4
  grep -rq 'Severity="40"' "$WORK"/fdb-logs-*/ 2>/dev/null \
    && ok "the copy publishes once it succeeds" \
    || bad "the copy publishes once it succeeds"

  # Let several more cycles run, then QUIESCE before looking at staging or
  # counting generations. Both are mid-cycle states of a loop that recreates its
  # staging directory every second: asserting against a live copier reports a
  # failure whenever the check lands between the `mkdir` and the rename, which is
  # a flake and not a finding. Stop first, then read.
  sleep 3

  # END ON A FAILING CYCLE. Without this the last cycle SUCCEEDS, and a
  # successful publish consumes its staging directory by renaming it — so the
  # periodic failure branch's `rm -rf` could be deleted and the staging arm below
  # would still be green. Measured: with the case ending on a success, that
  # mutation reddened nothing. The failing cycle is what gives the arm its teeth,
  # and it is the same vacuity the exit path's arm had.
  # …and make that last cycle SLOW, so the stop lands mid-copy. This is what
  # gives the staging arm below its teeth: the copy loop is asleep almost all
  # of the time, so a signal at a random instant essentially never lands in the
  # window, and five clean runs cannot tell p=0 from p a thousandth.
  touch "$STATE/slowcp"
  touch "$STATE/cpfail"
  sleep 3
  stop_watcher
  rm -f "$STATE/slowcp"

  # ONE generation, not a growing pile. The publish renames staging onto a name
  # carrying a per-cycle COUNTER, with `mv -T` so it can never nest, and the
  # previous generation is removed only AFTER the new one is in place — so the
  # glob never sees zero and never sees two. Without the prune this grows one
  # entry per cycle for the whole lane.
  gens=$(ls -d "$WORK"/fdb-logs-c1.* 2>/dev/null | wc -l)
  if [ "$gens" = 1 ]; then
    ok "exactly one published generation survives the prune"
  else
    bad "exactly one published generation survives the prune (found $gens)"
  fi
  # The surviving generation must hold the traces, not be an empty husk. The
  # count arm above passes for a directory that exists and holds nothing, which
  # is the exact reading — "present, therefore captured" — the whole dump exists
  # to make impossible.
  if grep -rq 'SharedTLogFailed' "$WORK"/fdb-logs-c1.*/ 2>/dev/null; then
    ok "the surviving generation still holds its traces"
  else
    bad "the surviving generation still holds its traces"
  fi
  # The published generation must hold the traces DIRECTLY. A publish that nested
  # instead of replacing leaves them one level down, where the dump's `grep -r`
  # still finds them but the file list the dump prints does not — and the
  # generation count above still reads 1, so nesting is invisible to it.
  if find "$WORK"/fdb-logs-c1.* -mindepth 1 -type d 2>/dev/null | grep -q .; then
    bad "the published generation holds no nested directory ($(find "$WORK"/fdb-logs-c1.* -mindepth 1 -type d | tr '\n' ' '))"
  else
    ok "the published generation holds no nested directory"
  fi
  # And nothing dot-prefixed is left behind by THIS loop. The glob covers the
  # periodic loop's whole namespace — its staging AND its `.trash`, which is
  # deliberate — while excluding the exit loop's, since `.fdb-logs-c1.*` does not
  # match `.fdb-logs-c1-exit.new`. Both loops stage under `.fdb-logs-*`, so a
  # SHARED glob makes a leak in either fire whichever arm happens to run, which
  # is how a periodic leak was once reported as an exit one.
  if ls -d "$WORK"/.fdb-logs-c1.* >/dev/null 2>&1; then
    bad "the PERIODIC copier leaves no staging directory"
  else
    ok "the PERIODIC copier leaves no staging directory"
  fi
else
  bad "the watcher never recorded a pid (cpfail case)"
fi
rm -f "$STATE/cpfail"
# Hand the container back GONE. The sections below are written against an
# absent container, and a case that leaves one running silently retargets them.
echo gone > "$STATE/phase"

# `mv -T` DRIVEN, not asserted. An earlier version of this file recorded `-T` in
# the not-covered list on the grounds that the generation counter already makes
# the target unique, so nothing could reach the nesting case. That was true of
# the PERIODIC path and false of the EXIT one, whose destination is a fixed name:
# occupy it and the publish must REFUSE rather than nest. Measured with plain
# `mv`: the copy lands at `fdb-logs-c1-exit/.fdb-logs-c1-exit.new` and the log
# still says "copied complete at exit" — silent success over misplaced evidence.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"; echo running > "$STATE/phase"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
if start_watcher 3600; then
  sleep 1
  # Occupy the exit destination with a NON-EMPTY directory before the transition.
  mkdir -p "$WORK/fdb-logs-c1-exit"
  echo '<Event Severity="10" file="squatter"/>' > "$WORK/fdb-logs-c1-exit/old.xml"
  echo stopped > "$STATE/phase"
  sleep 4
  stop_watcher
  if [ -d "$WORK/fdb-logs-c1-exit/.fdb-logs-c1-exit.new" ]; then
    bad "an occupied exit destination is REFUSED, not nested into"
  else
    ok "an occupied exit destination is REFUSED, not nested into"
  fi
  grep -q 'NOT copied at exit' "$WORK/fdb-watch.log" 2>/dev/null \
    && ok "a refused exit publish is reported as NOT copied" \
    || bad "a refused exit publish is reported as NOT copied"
else
  bad "the watcher never recorded a pid (occupied-exit case)"
fi
echo gone > "$STATE/phase"

# THE RETIRE RENAME, DRIVEN. `rm -rf` is interruptible, so a generation is
# renamed to a dot-prefixed trash name — atomically — before deletion touches it.
# Deleting it directly instead reddens nothing UNLESS a deletion is actually cut
# short, which is why an earlier version of this file wrongly recorded the rename
# as covered by the generation-count arm: that arm catches the retire being
# skipped entirely, not the retire being done unsafely. The `slowrm` shim makes
# the interrupt land inside the deletion every time.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"; echo running > "$STATE/phase"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
if start_watcher 1; then
  sleep 3
  touch "$STATE/slowrm"
  sleep 2
  stop_watcher
  rm -f "$STATE/slowrm"
  gens=$(ls -d "$WORK"/fdb-logs-c1.* 2>/dev/null | wc -l)
  if [ "$gens" -le 1 ]; then
    ok "a deletion cut short leaves no published generation behind"
  else
    bad "a deletion cut short leaves no published generation behind (found $gens: $(ls -d "$WORK"/fdb-logs-c1.* | tr '\n' ' '))"
  fi
else
  bad "the watcher never recorded a pid (slow-rm case)"
fi
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
# Assert the CONTENT, not the header. `grep -q 'fdb-last-inspect-c1.txt'` matches
# the `--- $i` line the loop prints BEFORE `cat`, so deleting the `cat` reddens
# nothing and the arm reports that the dump "reads" a file it only NAMED. The
# same husk as the generation-count arm, in the assertion rather than the
# fixture: presence taken for content.
if grep -q 'exit=1 oom=false' "$DWORK/fdb-forensics.txt" 2>/dev/null; then
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
# file as committed, at 33 arms: deleting the forensics copy reddens exactly one
# arm, and deleting the WATCHER copy reddens exactly one — the other one — where
# before the extraction named a step and deleting the watcher's reddened NONE.
# (An earlier version of this sentence said "at 18 arms", which was the count of
# an uncommitted intermediate. A population no committed revision ever had is
# worse than no population at all: it cannot be seen to go stale.)
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
