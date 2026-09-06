#!/bin/bash
# Tests the orphan-FDB-container sweep as it is actually shipped: the script is
# EXTRACTED from cloud-init.yaml and executed against stubbed `docker` and `ps`,
# so this cannot drift from what the boxes run and needs no Docker to run.
#
# What it pins is one incident and the two ways the repair for it went wrong.
#
# The sweep removed any foundationdb container older than 1800s, every five
# minutes, on the premise that "no per-test container lives that long". Three
# nightly lanes hold one for hours. So it was not sweeping orphans, it was force
# removing the live container of every long-running test on this fleet, which is
# why those lanes died 30-35 minutes in on every night measured, and why the
# container was always found already GONE rather than exited.
#
# Repair one, a blanket "skip everything while a job runs", traded that leak for
# another: an orphan left by a previous cancelled job is then exempt too, and
# with a five-minute timer against a four-hour lane the box need never be idle
# for a tick. Hence the start-time comparison the cases below drive.
#
# Repair two took the worker's start time from `stat -c %Y /proc/<pid>`, which is
# not process start time — it is when the procfs inode was allocated, i.e. first
# lookup after a cache miss, biased LATE and unbounded. Measured on a dev box:
# /proc/1 read 3s late, a fresh process 1-2s late. Two ways that restores the
# original incident: if the first lookup is the sweep's own tick, the worker
# looks minutes younger than it is and the job's own container reads as older
# than the worker; and a dentry evicted under memory pressure and re-looked-up
# at hour 3 of a lane moves the worker's apparent start to hour 3, at which
# point the live container is swept. `ps -o etimes=` is the real clock, and case
# E pins that an unreadable age fails CLOSED rather than open.
#
# Run: bash infra/orphan_fdb_sweep_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$(mktemp); BIN=$(mktemp -d); STATE=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$BIN" "$STATE"' EXIT

# Same extraction idiom as link_ci_volume_test.sh: cloud-init.yaml is a Terraform
# template, so `$${x}` reaches the box as `${x}` and the test must unescape it or
# it runs a different script from the deployed one.
extract_file() {
  awk -v want="  - path: $1" '
    $0 == want { at = 1; next }
    at && !inb && $0 == "    content: |" { inb = 1; next }
    inb {
      if ($0 != "" && $0 !~ /^      /) exit
      print substr($0, 7)
    }
  ' infra/cloud-init.yaml | sed -e 's/\$\${/${/g' -e 's/%%{/%{/g'
}

extract_file /usr/local/bin/orphan-fdb-sweep.sh > "$SCRIPT"
[ -s "$SCRIPT" ] || { echo "FATAL: extracted an empty script"; exit 1; }
# Sanity-check the EXTRACTION, not the guard: asserting the guard's own text here
# would make a REMOVED guard look like a broken test rather than a failing case.
grep -q 'foundationdb/foundationdb' "$SCRIPT" || { echo "FATAL: extraction has no FDB arm"; exit 1; }

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fail=1; }

# `docker` stub. `ps` lists one FDB container; `inspect` answers its StartedAt from
# the case's fixture; `rm` records the removal instead of performing one.
cat > "$BIN/docker" <<'STUB'
#!/bin/bash
case "$1" in
  ps)
    # `--format '{{.ID}} {{.Image}}'` — one container, id c1.
    echo "c1 foundationdb/foundationdb:7.3.77"
    ;;
  inspect)
    # -f '{{.State.StartedAt}}' c1
    cat "$SWEEPTEST_STATE/started"
    ;;
  rm)
    echo "c1" >> "$SWEEPTEST_STATE/removed"
    ;;
  volume) : ;;
esac
exit 0
STUB
chmod +x "$BIN/docker"

# `ps` stub: prints the worker's elapsed seconds, or nothing when the fixture asks
# for an unreadable age.
cat > "$BIN/ps" <<'STUB'
#!/bin/bash
[ -s "$SWEEPTEST_STATE/etimes" ] && cat "$SWEEPTEST_STATE/etimes"
exit 0
STUB
chmod +x "$BIN/ps"

# `pgrep` stub: a worker exists iff the fixture says so.
cat > "$BIN/pgrep" <<'STUB'
#!/bin/bash
[ -f "$SWEEPTEST_STATE/worker" ] && echo 4242
exit 0
STUB
chmod +x "$BIN/pgrep"

# One case. $1 name, $2 container age in seconds, $3 worker elapsed seconds
# ("" = no worker, "unreadable" = a worker whose age cannot be read), $4 expectation.
run_case() {
  rm -f "$STATE/removed" "$STATE/worker" "$STATE/etimes"
  date -u -d "@$(( $(date -u +%s) - $2 ))" +%Y-%m-%dT%H:%M:%S.000000000Z > "$STATE/started"
  case "$3" in
    "") ;;
    unreadable) touch "$STATE/worker" ;;
    *) touch "$STATE/worker"; echo "$3" > "$STATE/etimes" ;;
  esac
  SWEEPTEST_STATE="$STATE" PATH="$BIN:$PATH" bash "$SCRIPT" >/dev/null 2>&1
  got=removed
  [ -s "$STATE/removed" ] || got=survived
  if [ "$got" = "$4" ]; then ok "$1 ($got)"; else bad "$1: got $got, want $4"; fi
}

echo "orphan-fdb-sweep:"
# A: the job's OWN container. Older than the 1800s threshold — so the age arm
#    WOULD fire — but newer than the worker, so it must not. A fixture where the
#    container is under the threshold would pass without the guard and prove
#    nothing; the first draft of this case had exactly that shape reversed and
#    failed, which is the case earning its keep before it was ever committed.
run_case "live container, started after the worker" 3600 7200 survived
# B: an orphan from a PREVIOUS job, while a job runs. The blanket skip stranded
#    this one for the whole lane; the start-time comparison sweeps it.
run_case "orphan, started before the worker"        3600  600 removed
# C/D: the idle box, where age is the whole test.
run_case "old container, no worker"                 3600   "" removed
run_case "young container, no worker"                600   "" survived
# E: fail CLOSED. A worker exists but its age is unreadable, so nothing is swept
#    rather than everything.
run_case "worker present, age unreadable"           3600 unreadable survived

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
