# Go / CGo client benchmark

A Linux closed-loop measurement executable, **not `testing.B`**. The Go and CGo
executables share fixtures, operation scheduling, exact byte checks, clocks and
raw output, but never load both clients into one process. The Go executable uses
the public `pkg/fdbgo/fdb` facade; CGo uses Apple's Go binding to `libfdb_c`.

The workload/measurement implementation was imported from the frozen V6 VM
harness (source archive SHA256
`3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`).
This repository version uses the main module and Bazel dependency graph,
including its native-binding patch, instead of the historical standalone module.
It is **not a claim that newly built binaries reproduce historical results**.
Record the new source/toolchain/binary/library identities for every campaign.

## Build and verification

Requirements: Linux `/proc`, Python 3.9+, GNU `time`, `timeout`, `taskset`, and
FoundationDB **7.3.77** headers, shared library and server. The API is **730**;
Go **1.26.9** and the native Go binding version are pinned by the root build.
The server must already be running on a dedicated test cluster. This tool does
not provision, configure, restart or destroy servers.

From the repository root, **before any measurement**:

```sh
just test
bazelisk build //tools/clientbench/cmd/clientbench:clientbench_go \
  //tools/clientbench/cmd/clientbench:clientbench_cgo
campaign=$(mktemp -d "$HOME/clientbench-r3.XXXXXXXX")
mkdir "$campaign/bin"
install -m755 bazel-bin/tools/clientbench/cmd/clientbench/clientbench_go_/clientbench_go \
  "$campaign/bin/clientbench-go"
install -m755 bazel-bin/tools/clientbench/cmd/clientbench/clientbench_cgo_/clientbench_cgo \
  "$campaign/bin/clientbench-cgo"
bazelisk shutdown
```

Follow the shared-machine locking/resource rules when developing in a shared
checkout. Both executables are also built as inputs of the bounded real-FDB
smoke test in `just test`: it exercises all eleven workloads, both backends,
raw rows, measured CPU, persisted validation, native linkage and overwrite
refusal. The fast lane also runs the Go fixture/counter/scheduling tests and
Python plan/analyzer/process-cleanup tests. These are harness checks, not a
replacement for candidate-client correctness or full CI validation.

The Go target is `pure = "on"`, the CGo target is `pure = "off"`; their backend
source files **and dependency graphs** are separate. Each process asserts the
absence/presence of a mapped `libfdb_c`, records its actual library hash and
native client version, and hashes its own executable.

## Freeze the inputs

Use a clean, committed checkout. Save the source archives, compiler/build
information, actual runtime library and server identity, configuration and
machine topology. Do not build, profile, install software or run another
workload during timing. A guest CPU affinity is not physical-host isolation.
For example, from the repository root:

```sh
test -z "$(git status --porcelain --untracked-files=normal)"
revision=$(git rev-parse HEAD)
git archive --format=tar HEAD | gzip -n > "$campaign/source.tar.gz"
git archive --format=tar HEAD tools/clientbench | gzip -n > "$campaign/harness.tar.gz"
harness=$(sha256sum "$campaign/harness.tar.gz" | cut -d' ' -f1)
printf '%s\n' "$revision" > "$campaign/source-revision.txt"
go version -m "$campaign/bin/clientbench-go" > "$campaign/go-build.txt"
go version -m "$campaign/bin/clientbench-cgo" > "$campaign/cgo-build.txt"
ldd "$campaign/bin/clientbench-cgo" > "$campaign/cgo-linkage.txt"
lscpu > "$campaign/lscpu.txt"
uname -a > "$campaign/uname.txt"
sha256sum "$campaign"/bin/* /usr/lib/libfdb_c.so /usr/sbin/fdbserver \
  /etc/foundationdb/fdb.cluster /etc/foundationdb/foundationdb.conf \
  > "$campaign/inputs.sha256"
```

The example creates `$campaign` outside the source checkout so generated
artifacts do not defeat the clean-tree check. Archive
any candidate patch separately, with its base commit, and freeze both trees;
a label such as “master” is not an immutable source identity. Rebuilding one
arm with a different harness invalidates the comparison.

## R3-design campaign: exact budget and workload

This reproduces the **R3 experimental design**, not its old source binaries:

- two arms, Go and CGo; **six paired blocks**, three in each arm order;
- fixed order-shuffle seed **641001**;
- **3,000 measured + 200 warmup transactions per fresh process**, one worker;
- `batchset50`: **50 distinct 100-byte Sets + one commit** per transaction;
- fresh, equal-length **68-byte prefixes**, hence **88-byte batch keys**;
- transaction timeout **30,000 ms**, retry limit **10**, no GRV cache option;
- `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG`;
- 2-second cooldown, then a 1-second CPU/steal quiescence gate before each arm;
- 1 Hz telemetry, **180-second campaign wall cap**. Retain aborted/adverse
  outcomes; do not shorten or rerun a campaign until it fits or looks favorable.

The original 16-logical-CPU guest used housekeeping/runner CPUs 0–1, server
2–7, clients 8–15. Set the server affinity **before** the campaign and inspect
it (`taskset -pc <server-pid>`); the tool never changes it. If your machine has
different topology, explicitly select disjoint CPU groups and report that as a
different environment. Do not reuse those CPU numbers blindly.

On that layout, with the frozen executables and an existing dedicated cluster:

```sh
scripts=tools/clientbench/scripts
python3 "$scripts/r3_plan.py" \
  --go-binary "$campaign/bin/clientbench-go" \
  --cgo-binary "$campaign/bin/clientbench-cgo" \
  --cluster-file /etc/foundationdb/fdb.cluster \
  --native-library /usr/lib/libfdb_c.so \
  --source-revision "$revision" --harness-id "$harness" \
  --pin "$campaign/source.tar.gz" --pin "$campaign/harness.tar.gz" \
  --pin /etc/foundationdb/foundationdb.conf > "$campaign/plan.json"

# Inspect the schedule without opening FDB or executing an arm.
python3 "$scripts/run.py" --plan "$campaign/plan.json" \
  --output "$campaign/schedule-preview" --schedule-only

sha256sum -c "$campaign/inputs.sha256"
timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 \
  python3 "$scripts/run.py" --plan "$campaign/plan.json" --output "$campaign/timed"
sha256sum -c "$campaign/inputs.sha256"
python3 "$scripts/analyze.py" "$campaign/timed" --denominator cgo
```

The plan generator chooses a fresh random campaign namespace; its fixed width
preserves the key lengths. It hashes every pinned input at registration. The
runner requires complete preregistered hashes, rejects changes before execution, rechecks them afterward, kills
only its own process group on timeout/failure, and refuses an existing output
directory. Its own campaign cap is also enforced; the outer `timeout` bounds
cleanup. A plan contains executable argv lists: **do not run untrusted plans**.

`--housekeeping-cpus`, `--server-cpus` and `--client-cpus` configure the plan
builder's CPU groups. The runner affinity must be changed correspondingly in
the shell command. Go/CGo options and workload shape must remain identical.
For more arms/workloads, explicitly freeze an edited plan: block count must be
a multiple of `factorial(arms)`, include CGo, use unique equal-length prefixes,
and preserve declared source identities and hashes. All arm commands must have
identical affinity/options, except the pinned binary, prefix and declared source
revision. The harness ID must match an explicitly pinned archive. Source revision
is an operator declaration, recorded and crosschecked but **not independently
verified against the binary**; Bazel does not embed a VCS stamp here. Preserve the
clean-tree source archives and build procedure to audit that relationship.
The R3 budget is only for its specified two-arm workload; do not apply it silently
to a larger campaign. Telemetry follows the launched process's descendants and
retains accessible server statistics even when owner-only `/proc/PID/io` is denied.

## Results and analysis

`TABLES.md` and `absolute.csv` contain **medians of run-level statistics**, not
pooled percentiles: mean, p50, p90, p99, p99.9, process CPU and its user/system
components, plus Go-runtime allocation and GC counts. `paired.csv` contains
**geometric means of within-block ratios** and nominal 95% percentile
block-bootstrap intervals; `--denominator` selects the reference arm. These
ratios are not ratios of the displayed medians. `analysis.json` additionally
records raw-file hashes, order effects, split-half estimates and a moving-block
sensitivity diagnostic. `per-run.csv` retains every arm's statistics.

CPU is the **measured-phase `getrusage(RUSAGE_SELF)` user + system delta**, divided
by completed transactions. Resource snapshots bracket measurement: MemStats then
getrusage before, getrusage then MemStats after, excluding the MemStats collector's
CPU but including minimal clock/barrier bookkeeping. GNU `time -v` is saved
separately and covers the **whole process** (setup, warmup and validation included),
as do its context-switch counts; do not relabel them phase-only. Neither GNU
time nor `perf task-clock` is interchangeable with this measured-phase metric.

Window CPU is **not complete-lifecycle per-transaction CPU**. The Apple binding
releases native transactions/futures through Go GC finalizers (7.3.77 binding
`database.go:102-106`, `futures.go:93`), then transaction teardown is queued on the
network thread (`fdb_c.cpp:921-926`, `ThreadSafeTransaction.cpp:402-406`). Warmup
cleanup can enter the window; late measured-transaction cleanup can fall after it.
The unequal deferred-cleanup effect is unmeasured. Paired tables include both
arms' Go GC cycles beside CPU ratios; these counts do not prove native cleanup
completed. Do not present window CPU as native full-cost or capacity evidence.

The imported master590145 baseline also omits C++'s causal-read-risky flag on
write-only commit GRVs (`NativeAPI.actor.cpp:6577-6578`, versus Go
`client/transaction.go:830,1980-1985,3398-3411`). Without it, the server waits for
`updateLastCommit` (`GrvProxyServer.actor.cpp:647-648`). Check whether each tested
revision retains this pre-existing difference before attributing `set100` or
`batchset50*` results; the harness measures actual implementations, not identical
algorithms. This tool does not silently correct client semantics.

All raw phases, sample indices/counts/errors/checks, persistence success,
declared source consistency, pinned harness/binary identities, native linkage and Go GRV evidence must validate
before the analyzer emits a successful report. `ABORTED.json`, incomplete counts
or missing `complete.json` mean diagnostic data, not a clean performance result.
Keep all artifacts, including failures. Once finished, seal them, for example:

```sh
(cd "$campaign"; find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$campaign.files.sha256"
tar -C "$(dirname "$campaign")" -czf "$campaign.tar.gz" \
  "$(basename "$campaign")" "$(basename "$campaign").files.sha256"
sha256sum "$campaign.tar.gz" > "$campaign.tar.gz.sha256"
```

Six blocks are exploratory evidence on one server, **not** a noninferiority,
global-native-speed or capacity claim. The bootstrap resamples paired run blocks,
not individual operations; independence and confidence-interval coverage are not
established on a continuously running server. No multiplicity correction is made.
A CI containing 1 is not equivalence. p99.9 is especially unstable at these sample
counts. Go MemStats and heap profiles exclude the native C++ heap; native client
internal counters are **unobserved**, never synthetic zeros. Process RSS high
water includes setup, not just the measured window.

## Workloads and safety

`--allow-writes` is required for **every** workload: setup overwrites fixture keys
under the reserved `clientbench/…/` prefix. Use a dedicated test cluster and a
unique prefix, one benchmark process at a time. There are no database-wide clears.

| Workload | Measured transaction operation |
|---|---|
| `get100`, `get1000`, `get10000` | Fresh `Transact`, exact Get, read-only commit fast path |
| `read1000` | Fresh `ReadTransact`, exact Get, no commit |
| `grv` | Fresh `ReadTransact`, positive read version, no data read |
| `set100` | Changing 100-byte overwrite and commit |
| `batchset50` | 50 distinct changing 100-byte Sets and commit |
| `batchset50x1000` | Same 50 keys/transaction, 1,000-byte values |
| `range10`, `range100` | Fresh `Transact`, WantAll, exact ordered keys and 8-byte little-endian row-index values |
| `ryw` | Local Set/Get exact 100-byte value, Cancel without commit; GRV-free control |

Batch payloads are generated before timing. One worker seeds 154 keys outside
timing. Each batch operation writes its 50 existing slots; every final persisted
slot is checked byte-for-byte in a **fresh read-only transaction** afterward.
This validates final state, **not an independent read-back of every intermediate
commit**. The range uses WantAll and limit N+1 to detect extras; it is not the
historical `RangeOptions{}` benchmark shape. Different workload/value sizes
change wire traffic and allocation as well as copying: compare within a workload.

Counts are total across workers; operation `i` belongs to worker `i % workers`.
Each worker is sequential/closed-loop, not a controlled open-loop offered load.
Payloads, worker goroutines and sample storage are prepared before timing;
CSV/JSON serialization happens afterward. Latency includes retries,
materialization and exact checks. Callback attempts are observed for both arms;
internal native RPC retries are not.

`samples.csv` and `warmup.csv` contain
`phase,index,worker,start_offset_ns,latency_ns,attempts,error,check_status`.
On a terminal error, workers stop before their next operation, in-flight work
finishes, unattempted rows are omitted and exit status is nonzero.

`--profile cpu|heap|mutex|block|trace` is available for **separate attribution
runs only**. Profiling perturbs measurement; campaign validation requires
`--profile none`. Heap profiles/Go allocation counters do not measure C++ memory.
