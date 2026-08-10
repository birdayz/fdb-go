#!/usr/bin/env bash
# Run a `go test` package pattern in a loop, keeping the COMPLETE output of
# every iteration on disk.
#
# This exists because a `go test ./pkg/relational/...` failure was once piped
# through a grep filter, so the only surviving evidence was the summary line
# `FAIL fdb.dev/pkg/relational/sqldriver 271.639s` — the actual failure text,
# the test name, and the FDB error code were all discarded. A one-off failure
# whose message is gone cannot be root-caused; it can only be re-hunted. Never
# filter a run you might need to explain: capture everything, grep the file.
#
#   scripts/capture-go-test.sh [-n ITERATIONS] [-o OUTDIR] [-t PATTERN]
#                              [-j PARALLEL_PACKAGES] [-T TIMEOUT]
#                              [-- extra go-test args...]
#
# Defaults to 5 iterations of ./pkg/relational/... into ./flake-runs/<stamp>/.
# Exits non-zero as soon as an iteration fails, leaving that iteration's log in
# place and printing where it is. Also snapshots `docker ps` and the load
# average alongside each run, because container oversubscription is the first
# hypothesis for any FDB-testcontainer flake and it is unrecoverable after the
# fact.
#
# TWO SETTINGS BELOW ARE NOT COSMETIC, and a run without them manufactures its
# own failures — which is worth stating because such a run reddens in a way that
# looks exactly like a product flake.
#
#   -timeout   `go test` applies a flat 10-minute default to EVERY package. This
#              repo has packages whose Bazel targets declare far more than that
#              because they legitimately need it: //pkg/relational/conformance/
#              factorycorpus/full is `timeout = "eternal"` (3600s via .bazelrc)
#              and //pkg/relational/conformance/rowdiff is `timeout = "long"`
#              (900s). Run under the go default and those two panic with
#              `test timed out after 10m0s` on a busy host — MEASURED here, both
#              in one run at load average 61 on 24 cores, with their subtests
#              still visibly advancing (0-13s each). That is starvation against
#              a wrong deadline, not a hang, and it is an artefact of the lane.
#
#   -p         `go test` runs up to GOMAXPROCS package binaries at once (24 on
#              this host) and each FDB package starts its own container. Bazel
#              caps the equivalent at 4 (`.bazelrc`: --local_test_jobs=4). The
#              go lane has no such cap, so it is applied here explicitly.
#
# Override either by passing your own flag after `--`; the last occurrence wins.
set -uo pipefail

iterations=5
outdir=""
pattern="./pkg/relational/..."
# Above the largest declared Bazel budget in the tree (eternal => 3600s), so the
# per-package deadline is the repo's, never go test's flat default.
timeout_arg="3600s"
# Mirrors .bazelrc's --local_test_jobs=4: at most this many container-starting
# package binaries at a time.
procs="4"
extra=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n) iterations="$2"; shift 2 ;;
        -o) outdir="$2"; shift 2 ;;
        -t) pattern="$2"; shift 2 ;;
        -j) procs="$2"; shift 2 ;;
        -T) timeout_arg="$2"; shift 2 ;;
        --) shift; extra=("$@"); break ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -z "$outdir" ]]; then
    outdir="$repo_root/flake-runs/$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$outdir"

echo "capture-go-test: pattern=$pattern iterations=$iterations outdir=$outdir timeout=$timeout_arg -p=$procs"
echo "capture-go-test: HEAD=$(git rev-parse HEAD) branch=$(git rev-parse --abbrev-ref HEAD)"

status=0
for ((i = 1; i <= iterations; i++)); do
    log="$outdir/run-$(printf '%03d' "$i").log"
    env_log="$outdir/run-$(printf '%03d' "$i").env"
    {
        echo "=== iteration $i ==="
        echo "started_utc: $(date -u +%FT%TZ)"
        echo "loadavg: $(cat /proc/loadavg)"
        echo "nproc: $(nproc)"
        echo "--- docker ps ---"
        docker ps --format '{{.ID}} {{.Image}} {{.Status}} {{.Names}}' 2>&1
        echo "--- free ---"
        free -m 2>&1
    } >"$env_log"

    echo "capture-go-test: iteration $i -> $log"
    # 2>&1 into the file and nothing else: no tee through grep, no filtering.
    go test -count=1 -timeout "$timeout_arg" -p "$procs" "${extra[@]}" "$pattern" >"$log" 2>&1
    rc=$?

    {
        echo "--- finished ---"
        echo "finished_utc: $(date -u +%FT%TZ)"
        echo "exit_code: $rc"
        echo "loadavg_after: $(cat /proc/loadavg)"
        echo "--- docker ps after ---"
        docker ps --format '{{.ID}} {{.Image}} {{.Status}} {{.Names}}' 2>&1
    } >>"$env_log"

    if [[ $rc -ne 0 ]]; then
        echo "capture-go-test: FAILURE on iteration $i (exit $rc)"
        echo "capture-go-test: full output kept at $log"
        echo "capture-go-test: environment snapshot at $env_log"
        echo "--- first failing lines ---"
        grep -n -m 40 -E '^(---|===) FAIL|^FAIL|panic:|DATA RACE' "$log" || true
        status=$rc
        break
    fi
done

echo "capture-go-test: done, status=$status"
exit "$status"
