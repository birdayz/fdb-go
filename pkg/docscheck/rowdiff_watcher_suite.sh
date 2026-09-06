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
# WHAT THIS DOES NOT COVER — and these are TWO lists, not one.
#
# They were one heading, and that hid something. The list was
# 4 entries when this file was committed and reached 10; all four originals
# survived verbatim, while every entry anyone actually picked up became an arm on
# the first attempt. Three of those four are in the second list below. So the
# risk was never that the list is too long to read — it is that a shape which
# CANNOT be driven from here and a shape that merely has not been look identical,
# so the closable ones inherit the permanence of the structural ones.
#
# STRUCTURAL — cannot be reached from a stub, and saying so is the value:
#
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
#     nothing, and measured on the platform that matters — bash 5.2.21 in
#     `ubuntu:24.04` (the CONTAINER; the fleet boots Hetzner's same-release
#     `ubuntu-24.04` rolling label per `infra/main.tf`, which is not the same
#     artifact and which Hetzner refreshes) — a group SIGTERM
#     with an EXIT trap set and NO TERM trap still ran that trap 5 times out of 5
#     (and 5 of 5 on the dev box's 5.3.15). An earlier version of this entry
#     justified the arm from documented behaviour nobody here had run, which is
#     the substitution this file exists to catch; the version after that measured
#     it only on the laptop, which is the same substitution one step smaller.
#   - the trash DELETION, as distinct from the retire rename. Dropping the rename
#     is driven — a deletion cut short then leaves a published generation behind,
#     which the slow-`rm` shim makes deterministic. Dropping the DELETION reddens
#     nothing, because the trap removes every dot-prefixed leftover on the way
#     out. It is kept anyway: a trap only runs at exit, and a four-hour lane
#     should not accumulate a retired generation per cycle until then.
#   - a counter CROSSING between two copiers, and so the NESTING that `mv -T`
#     refuses on the periodic path. Doubly unreachable: a launch-site guard means
#     a re-selected container gets no second copier, so there is nothing to
#     cross, and even without it both copiers sleep the same interval with the
#     second starting later, so the first leads by a fixed margin forever.
#     Crossing needs asymmetric cycle times, which the stub cannot make without
#     telling the two apart. It is why an obvious-looking arm was DELETED rather
#     than kept: `mv -T` -> `mv` on the periodic publish reddens NOTHING, so a
#     "no nested directory" assertion there is green with the bug fully present.
#     On the EXIT path the destination is a fixed name, a collision IS reachable,
#     and the nesting is driven — that asymmetry is why one path has the arm and
#     the other has this entry.
#   - anything about a REAL fdbserver: every trace here is a fixture, so this
#     pins the plumbing and not the format it carries.
#
# A QUEUE — each is a small fixture change away, and is a NEXT ARM, not a
# permanent exemption. Everything moved out of this half so far took one attempt:
#   - the outage window RESETTING after a successful inspect. The main loop's
#     `docker inspect --format` bypasses `still_there`, so without an explicit
#     reset there, isolated one-poll outages accumulate across a lane until the
#     backstop trips and reports a live container removed. The reset is in place;
#     mutating it out reddens nothing, because every case here produces one
#     CONTINUOUS outage rather than many separated by successes. The arm needs a
#     flapping knob — fail every Nth call rather than for a window — and then a
#     case whose outages span more than the backstop while never being
#     continuous for it.
#
#   - the dump's loop over LIVE containers. The stub answers `phase=gone` by the
#     time the dump runs, so that loop iterates an empty set here. Its arms are
#     unexercised; only the trace-directory and last-inspect arms are driven.
#   - the dump's IN-CONTAINER reads (`docker exec … grep Severity=40`, and its
#     `df`). The stub answers `docker exec … df`, which is what gave the df
#     SAMPLER an observable and let its retry wiring be driven at all; every
#     other `exec` shape is still refused, so the dump's own in-container arms
#     are unpinned.
#   - the watcher's deadline and its process-group teardown, which the workflow
#     comments describe and which were driven by hand against real processes.
#   - the container log stream the watcher backgrounds with `docker logs -f`,
#     which the dump tails and the upload ships. The watcher cases DO create the
#     file as a side effect, and no case reads it or asserts on it: the stub's
#     `logs` arm sleeps without emitting a byte, so what it holds here is the
#     stub's silence rather than anything a container said.
#   - the ORPHAN-RETIRE log line. When `mv -T` on a predecessor fails, the old
#     generation stays published while `prevgen` advances past it, so nothing
#     points at it again — the line says so instead of leaving it silent.
#     Mutating it reddens nothing: the fixtures cannot make that rename fail
#     (the destination is dot-prefixed and unique, and the source is a directory
#     this loop just created). It is drivable with an `mv` shim that refuses a
#     `.trash` destination, the same trick as the `rm` shim, and that is the next
#     arm to write rather than a reason it cannot be written.
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
#
# Two limits, both deliberate and both silent if they stop holding: it delegates
# to `/usr/bin/rm` by absolute path (there is no other `rm` on the image the
# runner uses), and it matches RELATIVE `fdb-logs-*` arguments only, because the
# watcher cd-s into the workspace and names them relatively. If the watcher ever
# passed absolute paths the shim would go inert rather than fail, and the arm it
# feeds would pass for the wrong reason.
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
# Per-container phase, falling back to the shared one. A second container is what
# makes TWO copiers for ONE container reachable, and no single global phase can
# express it: the older container has to stay RUNNING while the newer one is
# removed.
cphase() {
  if [ -n "$1" ] && [ -f "$WATCHTEST_STATE/phase-$1" ]; then
    cat "$WATCHTEST_STATE/phase-$1"
  else
    cat "$WATCHTEST_STATE/phase" 2>/dev/null || echo gone
  fi
}
phase=$(cphase "")
case "$1" in
  ps)
    # Two shapes, and they answer different questions.
    #
    #   `docker ps -aq --filter ancestor=<image>`  — who is there? (selection)
    #   `docker ps -aq --filter id=<id>`           — is THIS one there? (liveness)
    #
    # The liveness shape is what lets the watcher tell a REMOVED container (rc 0,
    # empty) from an UNREACHABLE daemon (rc 1), which `docker inspect` cannot.
    # The daemon-outage knob therefore lives here, and it is aimed: `POLLER` is
    # exported by each caller, so a case can blip ONE poller. Without that aim
    # the knob hit whichever polled first — all three callers issue the same
    # command — and an arm could pass having blipped a poller it was not about.
    if [ -f "$WATCHTEST_STATE/outage-until" ] &&
       [ "$(date +%s)" -lt "$(cat "$WATCHTEST_STATE/outage-until")" ] &&
       { [ ! -f "$WATCHTEST_STATE/outage-poller" ] ||
         [ "$(cat "$WATCHTEST_STATE/outage-poller")" = "${POLLER:-}" ]; }; then
      exit 1   # daemon unreachable: rc 1, no output
    fi
    want=""
    for a in "$@"; do
      case "$a" in id=*) want=${a#id=} ;; esac
    done
    if [ -n "$want" ]; then
      [ "$(cphase "$want")" = gone ] || echo "$want"
    else
      # `docker ps -aq` lists NEWEST FIRST, which is what makes `head -1` flip
      # between containers as they come and go — the whole mechanism under test.
      if [ -f "$WATCHTEST_STATE/second" ] && [ "$(cphase c2)" != gone ]; then
        echo c2
      fi
      [ "$(cphase c1)" = gone ] || echo c1
    fi
    ;;
  inspect)
    # The main loop reads state with `docker inspect --format`; that call is NOT
    # routed through `still_there`, so failing it is a separate knob. It is the
    # shape that produced a FALSE `GONE`: inspect flaky, container present.
    if [ -f "$WATCHTEST_STATE/inspect-outage-until" ] &&
       [ "$(date +%s)" -lt "$(cat "$WATCHTEST_STATE/inspect-outage-until")" ]; then
      exit 1
    fi
    # `docker inspect <id> [--format <fmt>]` — the id is FIRST at every call site
    # in the watcher (`docker inspect "$c"`, `docker inspect "$seen" --format …`).
    # An earlier version took `${@: -1}`, the LAST argument, under a comment
    # claiming the opposite shape: for every `--format` call it then used the
    # FORMAT STRING as the container id, `cphase` fell back to the global phase,
    # and the per-container fixture was half-inert with nothing reporting it.
    id="$2"
    p=$(cphase "$id")
    [ "$p" = gone ] && exit 1
    if [ "$p" = running ]; then
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
  exec)
    # `docker exec <id> df -h <path>` — the df sampler's only observable. It used
    # to be refused outright, which left the sampler with nothing to assert on:
    # reverting its retry wiring reddened no arm at all. Answering it makes
    # `fdb-df-<id>.txt` a thing that either keeps being refreshed or stops.
    case " $* " in
      *" df "*) echo "Filesystem Size Used Avail Use% Mounted"; echo "/dev/x 98G 54G 39G 59% /var/fdb/data" ;;
      *) exit 1 ;;
    esac
    ;;
  cp)
    # A daemon outage is GLOBAL: if `docker ps` cannot reach it, neither can
    # `docker cp`. Modelling the outage only in the `ps` arm made the copier's
    # SKIP signal undrivable — without the skip the body runs, and against a
    # still-working `cp` it succeeds, so nothing distinguishes skipping from
    # not skipping.
    if [ -f "$WATCHTEST_STATE/outage-until" ] &&
       [ "$(date +%s)" -lt "$(cat "$WATCHTEST_STATE/outage-until")" ]; then
      exit 1
    fi
    # A slow copy, so the stop signal is GUARANTEED to land inside it rather
    # than landing there once in a thousand runs. Without this knob the staging
    # arms pass because the window is narrow, not because it is closed — which
    # is what a review measured by widening exactly this call.
    [ -f "$WATCHTEST_STATE/slowcp" ] && sleep 3
    # `docker cp <id>:/var/fdb/logs/. dest/`
    # The container can vanish BETWEEN the loop's inspect and its copy; this
    # knob is that window, which no phase can express on its own.
    [ -f "$WATCHTEST_STATE/cpfail" ] && exit 1
    src=${2%%:*}
    [ "$(cphase "$src")" = gone ] && exit 1
    dest=$3
    mkdir -p "$dest"
    cp -r "$WATCHTEST_STATE/logs/." "$dest/" 2>/dev/null
    ;;
esac
exit 0
STUB
chmod +x "$BIN/docker"

# $1 = the poller interval in seconds, applied to BOTH pollers. The df sampler's
# `sleep 30` was outside this override for a long time, which left its whole
# retry branch untested at speed: reverting its wiring reddened no arm, because
# in a few seconds it never polled twice.
#
# $2 = the daemon-outage BACKSTOP in seconds, default 600 — overridable for the
# same reason the interval is: the backstop is 10 minutes in production, so the
# branch that decides "outage, not removal" is unreachable in a test without it,
# and that branch is the one that must not write a GONE line.
#
# $1 = the poller interval in seconds. Cases that want the periodic path
# pass 1; the case that isolates the EXIT-TRANSITION copy passes a number larger
# than the case's own lifetime, so the periodic loop cannot be what captured
# anything. Without that knob the two paths are indistinguishable, which is
# exactly what a first mutation run showed: deleting the exit copy changed
# nothing, because at a 1s interval the periodic loop reached the same file.
start_watcher() {
  rm -rf "$WORK"; mkdir -p "$WORK"
  cp "$SCRIPT" "$WORK/fdb-watch.sh"; chmod +x "$WORK/fdb-watch.sh"
  # A short deadline so a case cannot hang the suite.
  sed -i -e 's/^deadline=.*/deadline=$(( $(date +%s) + 30 ))/' \
    -e "s/^maxOutageSeconds=.*/maxOutageSeconds=${2:-600}/" -e "s/^\\( *\\)sleep 60$/\\1sleep ${1:-1}/" -e "s/^\\( *\\)sleep 30$/\\1sleep ${1:-1}/" "$WORK/fdb-watch.sh"
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

  # THE COPY-FAILURE LOG IS A TRANSITION, NOT A LINE. The copier logs once when
  # it starts failing and once when it recovers; logging every cycle would bury
  # the line that matters under hundreds, which is the trap the exit-transition
  # log is written around. So assert the COUNTS: exactly one of each after a
  # failing stretch that recovered. Dropping the `[ -z "$copyfail" ]` guard gives
  # five, which is the mutation that a presence check would not have caught.
  nf=$(grep -c 'periodic copy FAILING' "$WORK/fdb-watch.log" 2>/dev/null)
  nr=$(grep -c 'periodic copy recovered' "$WORK/fdb-watch.log" 2>/dev/null)
  [ "$nf" = 1 ] && ok "a failing copy stretch is logged exactly once" \
                || bad "a failing copy stretch is logged exactly once (got $nf)"
  [ "$nr" = 1 ] && ok "the recovery is logged exactly once" \
                || bad "the recovery is logged exactly once (got $nr)"

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
rm -f "$STATE/cpfail" "$STATE/outage-until" "$STATE/outage-poller"
# Hand the container back GONE. The sections below are written against an
# absent container, and a case that leaves one running silently retargets them.
rm -f "$STATE/outage-until" "$STATE/outage-poller"
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
rm -f "$STATE/outage-until" "$STATE/outage-poller"
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
  rm -f "$STATE/outage-until" "$STATE/outage-poller"
  gens=$(ls -d "$WORK"/fdb-logs-c1.* 2>/dev/null | wc -l)
  # `= 1`, not `-le 1`: zero means the copier published NOTHING, which is the
  # empty-set false green this whole file is built around — and it was sitting
  # inside the arm added to close a real defect.
  if [ "$gens" = 1 ]; then
    ok "a deletion cut short leaves no published generation behind"
  else
    bad "a deletion cut short leaves no published generation behind (found $gens: $(ls -d "$WORK"/fdb-logs-c1.* | tr '\n' ' '))"
  fi
else
  bad "the watcher never recorded a pid (slow-rm case)"
fi
rm -f "$STATE/outage-until" "$STATE/outage-poller"
echo gone > "$STATE/phase"

# EXHAUSTING THE BACKSTOP IS STILL NOT A REMOVAL. The two ways `still_there`
# returns 1 are not interchangeable to the main loop: one is a proven absence,
# the other is a daemon that never answered. Writing `GONE (removed…)` for the
# second asserts a removal that did not happen, at a time nothing happened — the
# defect this whole design removed, reappearing at the backstop, one line below a
# message that correctly calls it an outage. The backstop is 600s in production,
# so it is overridden here; without that this branch is unreachable and the
# `gone_reason` split is untested.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
rm -f "$STATE/second" "$STATE/phase-c1" "$STATE/phase-c2"
echo running > "$STATE/phase"
if start_watcher 1 2; then
  sleep 2
  echo main > "$STATE/outage-poller"
  echo $(( $(date +%s) + 6 )) > "$STATE/outage-until"
  echo $(( $(date +%s) + 6 )) > "$STATE/inspect-outage-until"
  sleep 8
  rm -f "$STATE/outage-until" "$STATE/outage-poller" "$STATE/inspect-outage-until"
  stop_watcher
  nb=$(grep -c 'NOT a removal' "$WORK/fdb-watch.log" 2>/dev/null)
  ng=$(grep -c 'GONE (removed' "$WORK/fdb-watch.log" 2>/dev/null)
  if [ "$nb" -ge 1 ] && [ "$ng" = 0 ]; then
    ok "an exhausted outage backstop is reported as an outage, not a removal"
  else
    bad "an exhausted outage backstop is reported as an outage, not a removal (outage lines=$nb, GONE lines=$ng)"
  fi
else
  bad "the watcher never recorded a pid (backstop case)"
fi
rm -f "$STATE/outage-until" "$STATE/outage-poller" "$STATE/inspect-outage-until"
echo gone > "$STATE/phase"

# A DAEMON OUTAGE MUST NOT BE LOGGED AS A REMOVAL. The main loop's `GONE` line is
# what a triaging reader uses to DATE the cluster's death, so a false one is
# worse than a missing one: it asserts a removal, at a time, that did not happen,
# and it clears `seen` so the same live container is "re-selected" a tick later.
# Measured on the shipped shape with the container running throughout:
# `watching c1` / `c1 GONE` / `watching c1`. The knob is AIMED at this poller —
# all three issue the same command, so an unaimed knob proves nothing about
# which one was hit.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
rm -f "$STATE/second" "$STATE/phase-c1" "$STATE/phase-c2"
echo running > "$STATE/phase"
if start_watcher 1; then
  sleep 2
  echo $(( $(date +%s) + 3 )) > "$STATE/inspect-outage-until"
  # The df sampler must ride out the SAME outage: its file keeps being refreshed
  # across it. It had no observable at all until the stub answered `docker exec
  # … df`, which is why reverting its retry wiring reddened nothing — and its
  # `sleep 30` sat outside the interval override, so in a few seconds it never
  # polled twice either. Both had to be fixed before this could be an arm.
  dfbefore=$(stat -c %Y "$WORK/fdb-df-c1.txt" 2>/dev/null || echo 0)
  sleep 5
  rm -f "$STATE/inspect-outage-until"
  dfafter=$(stat -c %Y "$WORK/fdb-df-c1.txt" 2>/dev/null || echo 0)
  stop_watcher
  ngone=$(grep -c 'GONE (removed' "$WORK/fdb-watch.log" 2>/dev/null)
  if [ "$ngone" = 0 ]; then
    ok "a daemon outage is not logged as a removal"
  else
    bad "a daemon outage is not logged as a removal (got $ngone GONE lines for a live container)"
  fi
  if [ "$dfafter" != 0 ] && [ "$dfafter" != "$dfbefore" ]; then
    ok "the df sampler survives an outage and keeps sampling"
  else
    bad "the df sampler survives an outage and keeps sampling (before=$dfbefore after=$dfafter)"
  fi
else
  bad "the watcher never recorded a pid (main-outage case)"
fi
echo gone > "$STATE/phase"

# A TRANSIENT `docker inspect` FAILURE MUST NOT END CAPTURE. The copier's loop
# condition used to BE its liveness test, so one blip — with the container alive
# and `cp` still working — stopped trace collection permanently and said nothing.
# Measured on the shipped shape before the fix: the last generation stayed put
# over the following seconds where the control advanced, and no line was written
# anywhere. It is the failure mode of the overloaded box that this watcher
# exists to explain, which is what makes it worth an arm rather than a note.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
rm -f "$STATE/second" "$STATE/phase-c1" "$STATE/phase-c2"
echo running > "$STATE/phase"
if start_watcher 1; then
  sleep 3
  before=$(ls -d "$WORK"/fdb-logs-c1.* 2>/dev/null | sort | tail -1)
  echo copier > "$STATE/outage-poller"
  echo $(( $(date +%s) + 2 )) > "$STATE/outage-until"
  sleep 5
  after=$(ls -d "$WORK"/fdb-logs-c1.* 2>/dev/null | sort | tail -1)
  nfail=$(grep -c 'periodic copy FAILING' "$WORK/fdb-watch.log" 2>/dev/null)
  if [ -n "$after" ] && [ "$before" != "$after" ]; then
    ok "a transient inspect failure does not end periodic capture"
  else
    bad "a transient inspect failure does not end periodic capture (stuck at $(basename "${before:-none}"))"
  fi
  # A TOLERATED OUTAGE SKIPS THE BODY. `still_there` sets `$misses` while it is
  # riding one out, and the poller reads it to skip — without that the copy is
  # attempted against an unreachable daemon, fails, and logs `periodic copy
  # FAILING (inspect still succeeds)`, which is false in the exact state it
  # prints in. The skip is the difference between a quiet outage and a log full
  # of a diagnosis that contradicts itself.
  if [ "$nfail" = 0 ]; then
    ok "a tolerated outage skips the copy instead of failing it"
  else
    bad "a tolerated outage skips the copy instead of failing it (got $nfail FAILING lines)"
  fi
  # And a REAL removal must still end it, loudly — otherwise the tolerance has
  # traded a false stop for a loop that never stops, which is this file's
  # signature failure.
  grep -q 'inspect failed' "$WORK/fdb-watch.log" 2>/dev/null \
    && bad "a single blip did not report the copier as ended" \
    || ok "a single blip is not reported as the container ending"
  # A REMOVAL MUST END THE COPIER — and the assertion is that it ENDED, not that
  # it SAID so. Those are different claims and only one of them is the point: a
  # `still_there` whose final `return 1` became `return 0` keeps polling forever
  # after removal and emits the same line on EVERY poll, so an arm that greps for
  # the message passes with the copier spinning. Measured — that one-line
  # mutation left all arms green. Counting the line separates them: a copier that
  # ends logs it once, a copier that spins logs it every cycle.
  #
  # Removal is now decided by `docker ps -aq --filter id=` returning EMPTY with
  # rc 0, so it is immediate rather than waiting out a miss budget.
  rm -f "$STATE/outage-until" "$STATE/outage-poller"
  echo gone > "$STATE/phase"
  sleep 4
  nend=$(grep -c 'is gone; ending periodic copy' "$WORK/fdb-watch.log" 2>/dev/null)
  if [ "$nend" = 1 ]; then
    ok "a removed container ends the copier exactly once"
  else
    bad "a removed container ends the copier exactly once (got $nend)"
  fi
  stop_watcher
else
  bad "the watcher never recorded a pid (blip case)"
fi
rm -f "$STATE/outage-until" "$STATE/outage-poller"
echo gone > "$STATE/phase"

# TWO COPIERS FOR ONE CONTAINER, which is what makes the generation name need a
# LAUNCH scope and not only a cycle scope.
#
# `$c` is `docker ps -aq … | head -1` and a copier is launched whenever `$c`
# differs from `$seen`, so the sequence below puts two of them on c1:
#
#   1. only c1 exists          -> head -1 = c1, copier #1 for c1
#   2. c2 appears (newer)      -> head -1 = c2, copier for c2; c1's is STILL LOOPING
#   3. c2 is removed           -> head -1 = c1 again, and c1 != seen (=c2),
#                                 so a SECOND copier starts for c1
#
# With a per-loop counter alone both copiers mint `fdb-logs-c1.1`, `…2`, `…3` and
# race on the same names: `mv -T` refuses, so copies are dropped rather than
# silently nested, but the evidence is lost either way. The launch counter lives
# in the PARENT shell, so the second copier cannot reuse the first's numbers.
rm -rf "$STATE/logs"; mkdir -p "$STATE/logs"
echo '<Event Severity="40" Type="SharedTLogFailed"/>' > "$STATE/logs/trace.001.xml"
rm -f "$STATE/second" "$STATE/phase-c1" "$STATE/phase-c2"
echo running > "$STATE/phase"
if start_watcher 1; then
  sleep 3
  # c1 EXITS while it is the watched container, so its transition is logged once.
  echo stopped > "$STATE/phase-c1"
  sleep 3
  # c2 appears and takes over `head -1`; c1 is still LISTED (docker ps -a shows a
  # stopped container), so `head -1` can come back to it.
  echo running > "$STATE/phase-c2"; touch "$STATE/second"
  sleep 3
  # c2 goes; `head -1` reverts to c1, which is where a re-selection resets state
  # that should be per container.
  echo gone > "$STATE/phase-c2"
  sleep 4
  stop_watcher
  # ONE EXIT TRANSITION PER CONTAINER, ACROSS A RE-SELECTION. `seen` and
  # `exited` are reset outside the launch guard, so a re-selected container that
  # had ALREADY exited gets its transition logged a second time — and the second
  # pass finds the exit destination occupied, so the log then says the trace was
  # NOT copied, for a container whose trace is sitting right there. A triaging
  # reader is told the evidence is missing and the container was removed, and
  # both halves are false.
  ntrans=$(grep -c 'c1 EXITED' "$WORK/fdb-watch.log" 2>/dev/null)
  if [ "$ntrans" = 1 ]; then
    ok "a re-selected container's exit is logged exactly once"
  else
    bad "a re-selected container's exit is logged exactly once (got $ntrans)"
  fi
  nnot=$(grep -c 'NOT copied at exit' "$WORK/fdb-watch.log" 2>/dev/null)
  if [ "$nnot" = 0 ]; then
    ok "no false 'NOT copied at exit' for a container whose trace was captured"
  else
    bad "no false 'NOT copied at exit' for a container whose trace was captured (got $nnot)"
  fi
  # ONE set of background jobs, not two. The launch-site guard is the fix; the
  # launch-scoped NAME is defence in depth behind it. So what this asserts is the
  # guard — exactly one launch id among c1's published generations — and an
  # earlier version of this arm asserting TWO was asserting the mitigation while
  # the cause was still there.
  launchids=$(ls -d "$WORK"/fdb-logs-c1.* 2>/dev/null | sed 's/.*fdb-logs-c1\.\([0-9]*\)-.*/\1/' | sort -u | tr '\n' ' ')
  nl=$(echo "$launchids" | wc -w)
  if [ "$nl" = 1 ]; then
    ok "a re-selected container gets no second copier (launch ids: $launchids)"
  else
    bad "a re-selected container gets no second copier (launch ids seen: '$launchids')"
  fi
  # And the follower and sampler are launched once too — the other two halves of
  # the same defect, which a generation-name check cannot see at all.
  nlog=$(grep -c "watching c1" "$WORK/fdb-watch.log" 2>/dev/null)
  if [ "$nlog" -ge 2 ]; then
    ok "c1 was re-selected, so the guard was actually exercised ($nlog selections)"
  else
    bad "c1 was re-selected, so the guard was actually exercised (only $nlog selections — the case did not reach the path it tests)"
  fi
else
  bad "the watcher never recorded a pid (two-copier case)"
fi
rm -f "$STATE/second" "$STATE/phase-c1" "$STATE/phase-c2"; echo gone > "$STATE/phase"

# `still_there`'s DECISION TABLE, driven as a unit.
#
# The whole design rests on one distinction `docker inspect` cannot make, so the
# function is extracted and driven against a scripted `docker` whose answers are
# a fixed sequence. That reaches states the lifecycle cases cannot: the one that
# matters is a container reading EMPTY and then the daemon going down inside the
# one-second confirmation window, which is not reachable by any phase fixture
# because it is a race between two calls. It shipped once, reading `removed`
# with the outage clock never started — a false `GONE` line reached through the
# re-issue that had been added to prevent a different false reading.
DEC=$(mktemp); DBIN=$(mktemp -d); DST=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$BIN" "$WORK" "$STATE" "$DUMP" "$DWORK" "$ALARM" "$AWORK" "$GUARD" "$GWORK" "$DEC" "$DBIN" "$DST"' EXIT
sed -n '/^still_there() {/,/^}$/p' "$SCRIPT" > "$DEC"
[ -s "$DEC" ] || { echo "FATAL: could not extract still_there"; exit 1; }
grep -q 'gone_reason' "$DEC" || { echo "FATAL: still_there extraction has no reason arm"; exit 1; }
cat > "$DBIN/docker" <<'STUB'
#!/bin/bash
n=$(cat "$DECST/n" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$DECST/n"
case "$(cut -d, -f"$n" < "$DECST/seq")" in
  empty) exit 0 ;;
  id)    echo c1; exit 0 ;;
  down)  exit 1 ;;
esac
exit 0
STUB
chmod +x "$DBIN/docker"

dec_case() {  # $1 name, $2 answer sequence, $3 want-rc, $4 want-reason, $5 want-clock
  printf '%s' "$2" > "$DST/seq"; echo 0 > "$DST/n"
  got=$(DECST="$DST" PATH="$DBIN:$PATH" bash -c '
    maxOutageSeconds=600; outage_start=0; misses=0; gone_reason=
    . "$1"
    still_there c1 "periodic copy" copier; rc=$?
    printf "%s %s %s" "$rc" "${gone_reason:-none}" "$([ "$outage_start" = 0 ] && echo stopped || echo started)"
  ' _ "$DEC" 2>/dev/null | tail -1)
  want="$3 $4 $5"
  if [ "$got" = "$want" ]; then ok "$1"; else bad "$1: got [$got], want [$want]"; fi
}

# A container really gone: two empty answers in a row, and only then `removed`.
dec_case "two empty answers mean REMOVED"                    empty,empty 1 removed stopped
# The daemon answering mid-reload — the shape the re-issue exists for.
dec_case "empty then present is a reload, not a removal"     empty,id    0 none    stopped
# THE ONE THAT SHIPPED WRONG: empty, then the daemon goes down. An outage.
dec_case "empty then UNREACHABLE is an outage, not a removal" empty,down  0 none    started
# And an outright outage from the first call.
dec_case "unreachable from the first call is an outage"      down,down   0 none    started

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

alarm_case() { # $1 name  $2 forensics  $3 inspect ("" = none)  $4 sweep  $5 paging  $6 want-rc  $7 traces? ("t" = a trace dir exists)
  rm -rf "$AWORK"; mkdir -p "$AWORK"
  printf '%s\n' "$2" > "$AWORK/fdb-forensics.txt"
  [ -n "$3" ] && printf '%s\n' "$3" > "$AWORK/fdb-last-inspect-c1.txt"
  # $7 exists because the trace-directory conjunct was unpinned in the GREEN
  # direction: every case wiped $AWORK and created no `fdb-logs-*`, so
  # `have_traces` was 0 in all of them and mutating that conjunct out of the gate
  # reddened nothing. A gate that can fail a night which used to pass needs the
  # arm that says WHEN IT MUST NOT — otherwise only the firing direction is
  # bounded, which is the half that cannot show the blast radius.
  if [ "${7:-}" = t ]; then
    mkdir -p "$AWORK/fdb-logs-c1.1-1"
    echo '<Event Severity="10"/>' > "$AWORK/fdb-logs-c1.1-1/trace.001.xml"
  fi
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
# Evidence present — but the two sources are not interchangeable, and this pair
# is where that is pinned. A LIVE container in the dump was read directly, so it
# is evidence on its own. An inspect ALONE is not: the inspect comes from the
# main loop and the traces from the copier, so an inspect with NO trace
# directory on a failed night means the copier stopped before the cluster did —
# the artifact then cannot say why, and saying nothing is what the alarm exists
# to prevent. It used to pass, on the strength of a file written by the wrong
# author.
alarm_case "a live container in the dump is evidence" '=== inspect c1 ===' '' failure success 0
alarm_case "an inspect with no traces on a failed night is NOT evidence" '=== host ===' 'ts c1 exit=1' failure success 1
# …and the same night WITH a trace directory stays green. This is the arm that
# bounds the blast radius of the conjunct above: without it only the firing
# direction is pinned, and mutating `[ "$have_traces" = 0 ]` out of the gate
# reddens nothing — measured. A gate that can fail a night which used to pass
# needs the arm that says when it must NOT.
alarm_case "an inspect WITH traces on a failed night is evidence" '=== host ===' 'ts c1 exit=1' failure success 0 t

# The TAG GUARD, which sits ABOVE the body each of these steps runs and so falls
# outside every block extracted for them. It exists because the forensics step
# once hardcoded `:7.3.77` in its container filter — a stale ancestor filter
# emptying a loop, which is the exact failure that step exists to report on — so
# a missing pin has to be loud rather than silently matching nothing. That makes
# it the arm here whose absence would reinstate a SHIPPED bug.
#
# There are TWO copies, one per step, and covering one is how a suite reports the
# guard as covered while the other stays free to be deleted. Measured on this
# file as committed, at 50 arms: deleting the forensics copy reddens exactly one
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
