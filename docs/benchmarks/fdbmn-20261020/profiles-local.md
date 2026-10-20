# fdbmn-20261020: local diagnostic profiles

The profiling batches (base, E1 and the quiet-slot confirmation profiles) are diagnostics, not timings. They come from a shared local machine and a single-host FDB cluster. No latency or throughput claim is drawn from them, and every old-vs-new statement based on them is evidence of where time is spent, not a cause of the Hetzner results. The one exception is the A2 add-on, which **is** a timing comparison: old vs new Go planskip rows10, 6 rotated blocks of 3 testing.B repeats per process, run in a gated quiet slot. Its limits: one machine with a baseline load of about 5 (1-minute load 6.4–9.3 during the runs), one single-storage loopback cluster, one cell and limit, and no confidence intervals (per-block minimum and maximum only).

## Key findings

- **USE_REMOTE_FETCH slowdown (A2 quiet-slot timing).** This is a local timing on a non-exclusive host (baseline load about 5, 1-minute load 6.4–9.3 during the runs), against a single-storage loopback cluster, for one cell (planskip rows10). Within those limits:
  - New Go's USE_REMOTE_FETCH is about 17% slower per op than old Go's: 1.167× reverse and 1.164× forward planskip rows10, geometric mean over 6 rotated blocks.
  - SCAN_AND_FETCH new/old ratios are near one: 0.988 reverse and 1.014 forward (per-block ranges [0.964, 1.012] and [0.987, 1.068]).
  - The remote/ordinary ratio moves from 0.70 to 0.83 (reverse) and from 0.73 to 0.84 (forward).
  - The USE_REMOTE_FETCH slowdown warrants code-path investigation; it is not attributed to any change. The single-storage topology removes cross-storage placement variation. It does not rule out every environmental explanation, or a shared path that the remote method exercises more heavily. Details: [Quiet-slot confirmation](#quiet-slot-confirmation).
- **8-worker reads wait more in new Go** (confirmed in the quiet slot):
  - GRV wait 1.14–1.16×, read-reply wait 1.25–1.29×.
  - Scheduler latency 1.27–1.35× in total, and 1.48–1.51× for goroutines woken by readLoop.
  - On Hetzner, new/CGo p50 at 8 workers is 0.977–1.013. get1000 and set100 are unresolved around one, which is not evidence of equivalence; see RESULTS.md.
- **set100 wait ratios are at or below about one in new Go** (commit wait 0.88–0.95×, commit-reply wait 1.01–1.02×). The local profiles do not reproduce the Hetzner 64-worker set regression in wait composition.
- **Less GC and fewer allocations in new Go.** New Go spends a much smaller CPU share in GC and allocates 0.56× the mallocs per set100 tx, matching Hetzner. It spends a larger share in the scheduler (`findRunnable`).

## What was profiled

- **Arms** (campaign binaries, hashes rechecked against the campaign receipts, Go 1.26.9; all three were built from the same frozen harness, d94b4c613 + #948 lifecycle drain fix):
  - old Go `267a41434` (`c70e4d27…`)
  - new Go `fa8725f1d` (`187a282a…`)
  - Apple Go binding over libfdb_c 7.3.77 (`9032ee8f…`, library `e4ad7fed…`)
- **M1 cases:** grv, get100, get1000, range100, set100, batchset50, batchset50x1000 at 1, 64 and 8 workers. Iterations and warmup are copied from the sealed schedules. M1w32 was not profiled.
- **M2 cases:** `BenchmarkIndexFetch`, 12 cases, and `BenchmarkPlanSkip`, 4 cases. Each runs SCAN_AND_FETCH and USE_REMOTE_FETCH on new Go only, with the sealed command lines.
- **Profile types:** one profile type per process. Block and mutex never run in the same process as CPU.

| Profile | Rate | Window |
|---|---|---|
| CPU | 100 Hz | M1: measured window only (harness `--profile`), 3 processes merged. M2: whole process. |
| heap/allocs | `MemProfileRate` 524288 (set explicitly) | `alloc_*` counts from process start; `inuse_*` is an end-of-window snapshot |
| block | rate 1 | measured window |
| mutex | fraction 1 | measured window |
| runtime/trace | – | measured window. Summarised with `go tool trace -pprof=sched\|sync\|net\|syscall`. M2 trace runs use count=1 and benchtime=1s (approved deviation). |
| perf | `perf record -F 499 -g` (frame pointers) | whole process, all three arms |

- **perf symbols:** the binaries and libfdb_c are stripped, so perf attribution is per thread and per DSO only.
- **Per-tx allocation** is measured from runtime counters over the measured window, as the median of the 3 CPU-profile runs.
- **E1 runs:** for set100, get100 and get1000 at 64 and 8 workers, extra CPU and perf runs used iterations multiplied by f (per-case factors in `e1-plan.json`), so every merged CPU profile has at least 2,000 samples. These E1 profiles are primary for those cases.
- **Base CPU profiles at 1 worker are thin:** about 35–215 merged samples per arm×case.
- **Raw data:** not in Git. It lives under `${FDB_PERF_HOME}/profiles/fdbmn-20261020-local/` and is identified by its `MANIFEST.sha256`: 8129 files, manifest SHA-256 `1cdbb7c1f4c499e4605944d741070055ba702d3e5ce3f39c726ccffc191e42e5`. Small summary JSON files are published in [`profiles-local/`](profiles-local/).
  - `MANIFEST.sha256` covers every file.
  - `provenance/DEVIATIONS.md` lists every deviation, including the wrong-lock incident (item 10).
  - `analysis/` in the raw data directory holds the extracted top-N tables for all arm×cases.

### Environment caveats

- **Cluster:** the fdb4 cluster is single redundancy, memory engine, one storage process, on loopback and tmpfs.
- **CPU pinning:** client on CPUs 2-5 and 14-17 with `GOMAXPROCS=8`; servers pinned to 8-10 and 20-22.
- **The machine was not exclusive.** Other agents' builds and tests, and Docker FDB testcontainers, ran during the runs. The containers were not pinned, so they could run on the client CPUs. That matters most for the scheduler-latency evidence.
- **Load flag:** a run is flagged when its 1-minute load exceeded 8 at start or end. That covers 327 of 736 profile runs: M1w1 24/168, M1w8 64/168, M1w64 13/168, all 72 E1 runs, M2 154/160. The maximum recorded load was 33.5.
- **Failures:** one run failed after its profile window (M1w64 batchset50 old cpu1: `process_behind` in persisted validation).
- **A2 timing add-on:** the first attempt ran under contention and is marked INVALID; no numbers from it appear here. The rerun in the quiet slot is reported below.
- **Wrong-lock incident:** the invalid A2 batch flocked `${FDB_PERF_HOME}/fast.lock` rather than the shared `${FDB_PERF_HOME}/validation/fast.lock`. The `fast_lock` fields in every `done.json` written before 15:10Z describe that wrong file and carry no information (`provenance/DEVIATIONS.md` item 10).

## Old vs new Go: where the waiting is

The table compares stable anchor functions (cumulative), in µs per measured transaction.

- **Each cell** shows old → new (new/old). It comes from one block-profile process and one trace process per arm and case.
- **Block and sync values** are summed over all goroutines, so a caller-path wait scales with worker count times time spent waiting.
- **Block rate 1 inflates absolute values.** Read the rows as wait composition, not latency.
- **Renamed functions** are matched to their old equivalents:
  - GRV fan-out: old `admit.func1` vs new `publishReply`.
  - Sender flush wait: old `writeContext` vs new `writeAdmittedContext`.

| wait anchor (µs per measured tx, summed over goroutines) | w64 set100 | w64 get100 | w64 get1000 | w8 set100 ⚑ | w8 get100 | w8 get1000 |
|---|---:|---:|---:|---:|---:|---:|
| block: op total (Database.Transact) | 1249.0 → 1098.1 (0.88×) | 351.7 → 360.6 (1.03×) | 351.9 → 373.2 (1.06×) | 1038.6 → 991.0 (0.95×) | 232.6 → 281.6 (1.21×) | 237.4 → 284.0 (1.20×) |
| block: GRV wait (grvBatcher.getReadVersion) | 193.9 → 178.5 (0.92×) | 235.5 → 235.3 (1.00×) | 215.6 → 234.3 (1.09×) | 142.0 → 125.3 (0.88×) | 179.2 → 210.1 (1.17×) | 182.0 → 210.6 (1.16×) |
| block: read reply wait (PendingGet.resolve) | – | 114.7 → 124.3 (1.08×) | 135.7 → 138.2 (1.02×) | – | 53.4 → 71.5 (1.34×) | 55.4 → 73.4 (1.33×) |
| block: commit wait (commitAdmitted) | 1249.0 → 1098.0 (0.88×) | – | – | 1038.6 → 991.0 (0.95×) | – | – |
| block: commit reply wait (waitReplyOrProxiesChanged) | 738.8 → 751.0 (1.02×) | – | – | 806.5 → 812.2 (1.01×) | – | – |
| block: sender flush wait (old writeContext / new writeAdmittedContext) | 30.1 → 0.8 (0.03×) | 50.1 → 51.6 (1.03×) | 51.9 → 50.1 (0.96×) | 14.6 → 0.1 (0.01×) | 14.0 → 32.1 (2.29×) | 14.2 → 31.8 (2.25×) |
| block: writer goroutine idle (writeLoop) | 15.6 → 14.1 (0.90×) | 11.2 → 10.1 (0.91×) | 11.3 → 10.8 (0.95×) | 127.4 → 118.5 (0.93×) | 31.0 → 60.4 (1.95×) | 31.8 → 67.4 (2.12×) |
| mutex: total | 3.1 → 1.8 (0.57×) | 6.8 → 1.1 (0.16×) | 7.0 → 1.6 (0.23×) | 1.7 → 0.4 (0.22×) | 0.8 → 0.5 (0.54×) | 1.0 → 0.4 (0.42×) |
| trace sched: total | 248.3 → 160.0 (0.64×) | 152.4 → 100.1 (0.66×) | 177.6 → 101.3 (0.57×) | 48.7 → 42.1 (0.86×) | 26.9 → 36.9 (1.37×) | 28.0 → 38.0 (1.36×) |
| trace sched: woken by readLoop (replies) | 159.6 → 113.4 (0.71×) | 59.2 → 47.6 (0.80×) | 60.2 → 47.2 (0.78×) | 27.7 → 22.6 (0.82×) | 10.1 → 15.9 (1.58×) | 10.5 → 16.2 (1.54×) |
| trace sched: woken by GRV reply fan-out | 52.0 → 33.8 (0.65×) | 51.9 → 33.9 (0.65×) | 60.4 → 34.4 (0.57×) | 14.4 → 16.1 (1.12×) | 12.4 → 14.1 (1.14×) | 12.9 → 14.6 (1.13×) |
| trace sched: woken by write completion | 24.6 → 0.1 | 38.7 → 14.4 (0.37×) | 53.8 → 15.2 (0.28×) | 3.9 → 0.0 | 2.6 → 5.4 (2.09×) | 2.7 → 5.6 (2.08×) |
| trace sync: total | 2236.3 → 2104.3 (0.94×) | 367.6 → 406.1 (1.10×) | 389.6 → 404.4 (1.04×) | 2455.5 → 2394.2 (0.98×) | 361.1 → 431.2 (1.19×) | 358.4 → 433.6 (1.21×) |
| trace net: total | 16.6 → 35.9 (2.16×) | 12.2 → 22.7 (1.87×) | 13.0 → 22.0 (1.69×) | 125.8 → 254.2 (2.02×) | 63.6 → 125.7 (1.98×) | 63.6 → 128.1 (2.02×) |
| trace net: GRV window timer goroutine (grvBatcher.runWindow, timerfd; old Go uses a runtime timer that this profile cannot see) | 0 → 17.3 (new) | 0 → 7.3 (new) | 0 → 7.3 (new) | 0 → 125.0 (new) | 0 → 40.7 (new) | 0 → 41.6 (new) |
| trace net: sender turn coalescing (Conn.coalesceWait) | 0 → 2.2 (new) | 0 → 2.5 (new) | 0 → 2.6 (new) | 0 → 5.0 (new) | 0 → 4.5 (new) | 0 → 4.6 (new) |
| trace syscall: total | 7.3 → 2.5 (0.35×) | 6.4 → 3.4 (0.53×) | 7.1 → 3.9 (0.55×) | 14.2 → 6.1 (0.43×) | 15.6 → 5.3 (0.34×) | 15.2 → 5.3 (0.35×) |
| trace syscall: sender-written turn (Conn.takeTurn) | 0 → 0.6 (new) | 0 → 0.7 (new) | 0 → 0.8 (new) | 0 → 2.0 (new) | 0 → 1.9 (new) | 0 → 1.8 (new) |

⚑ The w8 set100 wait profiles are load-flagged: 1-minute load 8.9–9.3 old, 12.3 new. All other columns started at load 6.2–8.0.

### Named mechanisms

- **GRV.**
  - The caller's GRV wait is larger in new Go for reads at 8 workers (1.16–1.17×). At 64 workers and for set100 its new/old ratio is 0.88–1.09.
  - Both Go versions batch GRVs behind a time window (C++ `readVersionBatcher`). Old Go arms it with `time.AfterFunc` (`grvBatcher.admit`), a runtime timer that the block, sync, net and syscall profiles cannot see. New Go waits on a timerfd in a dedicated goroutine (`grvBatcher.runWindow` → `hrtimer.(*Timer).Wait`), which appears as net blocking: 7–125 µs/tx, largest at 8 workers. That row therefore shows the window becoming visible, not a new wait. These profiles cannot compare the window lengths; the caller-side GRV wait above includes the window in both versions.
  - Scheduler wait for goroutines woken by the GRV reply fan-out (`publishReply`, with `yieldReady`/`Gosched`) is smaller at 64 workers (0.57–0.65×) and slightly larger at 8 workers (1.12–1.14×).
- **Write gate / outbox / held outbox.**
  - The sender's wait for its frame to be flushed (`writeAdmittedContext`, after `enqueueAdmitted`) roughly doubles at 8 workers for get100 and get1000 (14 → 32 µs/tx). At 64 workers its new/old ratio is 0.96–1.03.
  - In set100 this wait almost disappears in new Go (30 → 0.8 µs/tx at 64 workers).
  - The writer goroutine's idle wait (`writeLoop`/`nextWrite`) about doubles for the 8-worker gets.
  - `enqueueHeld` (held outbox) does not appear in any focus-case block, sync or sched profile.
- **Same-peer FIFO / ordered send.**
  - The new-only frames `SendFrameContextOrdered`, `enqueueOrdered`, `takeTurn`/`writeTurn` and `coalesceWait` are present but small as waits: 0.6–2 µs/tx in syscalls and 2–5 µs/tx in coalescing.
  - In CPU, `takeTurn` accounts for 1.4–6.1% cumulative of new-Go CPU in the E1 profiles, highest at 8 workers.
- **Startup handoff.**
  - No frame containing handoff, suspend, startup, prefix or cold appears in the focus-case block, mutex or trace profiles.
  - These profiles therefore give no evidence either way. The handoff may be inlined, or below the sampling of a single process.
- **Commit path.**
  - The commit wait (`commitAdmitted`) is smaller in new Go (0.88× at 64 workers, 0.95× at 8). The commit-reply wait ratio is 1.01–1.02×.
  - Scheduler wakeups from write completion vanish for set100.
  - Locally, then, the set100 wait composition does not show the Hetzner 64-worker set regression. The one-host loopback cluster may simply not reproduce it.
- **Pattern.** At 8 workers, for get100 and get1000, almost every wait anchor is larger in new Go: GRV, read reply, sender flush, writer idle, and readLoop-woken scheduler latency. At 64 workers most anchors are equal or smaller. New-only frames at both worker counts are the ordered-send turn and coalescing waits, plus the now-visible GRV window goroutine.

### Against the C client

The Hetzner results put new Go at or below libfdb_c latency in the cases where it regressed against old Go. Old Go was well below both at 64 workers. The p50 ratios below come from [`analysis/M1-comparisons.csv`](analysis/M1-comparisons.csv); the bracketed range is the 95% interval.

| Hetzner p50 ratio | new/old | new/C | old/C | measured-window process CPU per op, new/C |
|---|---:|---:|---:|---:|
| w64 set100 | 1.283 [1.273, 1.294] | 0.893 [0.887, 0.899] | 0.696 | 1.294 |
| w64 get100 | 1.348 [1.308, 1.395] | 0.934 [0.911, 0.966] | 0.693 | 1.184 |
| w64 get1000 | 1.298 [1.254, 1.331] | 0.770 [0.730, 0.821] | 0.593 | 1.183 |
| w8 set100 | 1.137 [1.099, 1.168] | 1.013 [0.978, 1.047] | 0.891 | 1.185 |
| w8 get100 | 1.113 [1.097, 1.128] | 0.977 [0.961, 0.992] | 0.878 | 1.057 |
| w8 get1000 | 1.154 [1.139, 1.172] | 1.003 [0.986, 1.021] | 0.869 | 1.149 |

The last column is measured-window CPU (`process_cpu_ns_per_op`), not the lifecycle E−A CPU that RESULTS.md reports. RESULTS.md explicitly does not substitute window CPU for lifecycle evidence. The E−A new/C ratios from the same CSV (`shutdown_process_cpu_ns_per_op`), in table order, are 1.249, 1.135, 1.156, 1.148, 1.037 and 1.133.

What the local profiles can say about the C client:

- **Go-side waits only.** In the CGo arm, the shared profiling anchor for Go-side waiting is `fdb.(*future).BlockUntilReady`, which takes a `sync.Mutex` that the future's callback unlocks.
  - Not every future waits there: a ready future returns at once (`bindings/go/src/fdb/futures.go:98-118`). A callback set on a ready future can fire synchronously (`flow/include/flow/ThreadHelper.actor.h:385-399`). A transaction can await several futures, for example its reads and then `Commit().Get()` (`bindings/go/src/fdb/database.go:225-228`). Citations are to 7.3.77; the binding module the CGo arm uses has byte-identical `futures.go` and `database.go`.
  - GRV, read and commit waits cannot be told apart at this anchor.
  - Timers inside libfdb_c, including its GRV batch window (`readVersionBatcher`, which the Go batcher ports), are invisible to pprof. perf sees only the network thread's CPU, not its timer waits.
  - So no local profile shows whether libfdb_c has an equivalent GRV batching delay. The C++ client runs the same mechanism, so a batch window by itself is not a Go-only cost.
- **Summed per-op wait, three arms** (µs per measured tx from the same block and trace profile runs, rate 1; a wait composition, not latency):

| case | block op wait old / new / CGo | trace sched old / new / CGo | trace sync old / new / CGo | load1 old / new / CGo |
|---|---|---|---|---|
| w64 set100 | 1249 / 1098 / 1742 | 248 / 160 / 32 | 2236 / 2104 / 1901 | 7.3 / 7.9 / 7.5 |
| w64 get100 | 352 / 361 / 1164 | 152 / 100 / 66 | 368 / 406 / 1465 | 6.2 / 6.2 / 7.0 |
| w64 get1000 | 352 / 373 / 1404 | 178 / 101 / 66 | 390 / 404 / 1751 | 6.9 / 7.4 / 7.5 |
| w8 set100 ⚑ | 1039 / 991 / 978 | 49 / 42 / 21 | 2456 / 2394 / 1305 | 8.9 / 12.3 / 11.9 |
| w8 get100 | 233 / 282 / 293 | 27 / 37 / 74 | 361 / 431 / 405 | 6.6 / 6.9 / 7.0 |
| w8 get1000 | 237 / 284 / 305 | 28 / 38 / 78 | 358 / 434 / 450 | 8.0 / 8.0 / 7.9 |

- **At 8 workers the local ordering of the gets matches Hetzner's direction.** The old-Go op wait is lowest; new Go and CGo are close (282 vs 293, 284 vs 305). In these profiles, the extra waits found in new Go at 8 workers (GRV, read reply, sender flush) bring its summed wait close to the CGo arm's, not above it.
- **At 64 workers the local CGo arm waits 3–4× more per get than either Go client**, unlike Hetzner, where C was 7–30% slower than new Go. One untested hypothesis is that the C client's single network thread is the constraint on one loopback host. Its share of 45% of all process samples in perf at w64 set100 does not establish saturation. With libfdb_c stripped, attribution stays at the thread and DSO level. No inference is drawn from the 64-worker CGo column.
- **Scheduler latency in CGo** (goroutines woken by the cgo callback) is lower than both Go clients at 64 workers and about twice new Go's at 8 workers. In CGo it measures callback wakeups, not wire handling, so it is not comparable stage by stage.

### CPU and scheduler composition (new vs old, cumulative % of merged CPU)

| case | GC worker old → new | findRunnable old → new | Syscall6 old → new | takeTurn new |
|---|---|---|---|---|
| w64 set100 (E1 ⚑) | 21.5 → 5.5 | 7.1 → 9.9 | 10.5 → 11.9 | 1.4 |
| w64 set100 (base) | 21.6 → 7.6 | 5.8 → 5.5 | 7.9 → 9.2 | 0.8 |
| w64 get100 (E1 ⚑) | 13.7 → 3.3 | 4.8 → 9.0 | 13.0 → 11.6 | 2.3 |
| w64 get100 (base) | 10.1 → 3.0 | 6.8 → 9.8 | 17.9 → 11.2 | 2.4 |
| w8 get100 (E1 ⚑) | 12.2 → 3.2 | 16.7 → 22.4 | 27.1 → 16.2 | 6.1 |
| w8 get100 (base) | 12.0 → 4.5 | 16.3 → 21.7 | 32.1 → 15.9 | 3.2 |
| w8 get1000 (E1 ⚑) | 13.4 → 4.9 | 15.8 → 20.6 | 27.4 → 15.6 | 5.0 |
| w8 get1000 (base) | 12.1 → 5.8 | 16.3 → 24.3 | 25.3 → 13.9 | 4.0 |

- New Go spends a much smaller share of CPU in GC.
- At 8 workers, a larger share goes to the scheduler's `findRunnable`: goroutines parking and waking, and spinning.
- The share in syscalls is smaller, consistent with turn-based sender writes coalescing frames.
- Base and E1 agree on direction except for w64 set100 `findRunnable`.

### Allocation per measured tx (Go heap; native libfdb_c heap excluded)

| case | old B/tx | new B/tx | CGo B/tx | old mallocs/tx | new mallocs/tx | CGo mallocs/tx |
|---|---:|---:|---:|---:|---:|---:|
| w64 set100 | 10499 | 6466 | 441 | 99.0 | 55.6 | 10.0 |
| w64 get100 | 6241 | 5368 | 666 | 66.7 | 51.5 | 16.0 |
| w64 get1000 | 8099 | 7240 | 1578 | 66.7 | 51.6 | 16.0 |
| w8 set100 ⚑ | 10402 | 6376 | 445 | 99.9 | 55.9 | 10.0 |
| w8 get100 | 6216 | 5360 | 669 | 68.0 | 51.9 | 16.0 |
| w8 get1000 | 8074 | 7230 | 1580 | 68.0 | 51.9 | 16.0 |

- The local set100 mallocs ratio at 64 workers (new/old 0.56×) matches the Hetzner 0.561×.
- batchset50x1000 allocates more bytes per tx in new Go at every worker count (w1: 207 KB → 269 KB). batchset50 allocates more at 1 and 8 workers and less at 64. Both have fewer mallocs per tx in new Go.
- The full table for all 21 cases is in [`profiles-local/alloc-per-tx.json`](profiles-local/alloc-per-tx.json).

## Go vs C client

- **The CGo arm's pprof does not isolate Go CPU.** Its denominator is all pprof CPU samples of the process (merged E1 profiles). That includes native execution on Go threads, attributed to `runtime.cgocall`, and any foreign-thread samples, attributed to `runtime._ExternalCode`.
  - **Native time attributed but unresolved:** `runtime.cgocall` flat is 61–71% of all samples in the focus cases (cumulative 64–73%). This includes the libfdb_c network thread, which runs `fdb_run_network` from a goroutine. `runtime._ExternalCode` is at most 0.04%.
  - **Native time pprof doesn't see at all:** CPU on threads the Go runtime doesn't sample is absent from these profiles. Only the whole-process perf data below covers it.
  - **Shares:** GC (`gcBgMarkWorker`, cumulative) is 0.6–1.2% of all samples, or 1.9–3.3% of the samples left after removing `cgocall` and `_ExternalCode` flat ("resolved Go samples"). `findRunnable` is 5.0–8.3% of all samples, or 15.8–25.5% of resolved Go samples (17.5% at w64 set100, 25.3% at w8 get100). The Go-only arms' percentages elsewhere in this document are unaffected.
  - Go allocation is about 440–1580 B/tx and 10–16 mallocs/tx, against 5–10 KB and 50–100 mallocs/tx for the Go client.
- **perf, whole process (E1 runs):**
  - In the CGo arm one thread dominates, at 45% of process samples for w64 set100 and 53% for w8 get100. It is the busiest libfdb_c thread, consistent with the FDB network thread.
  - About 25% of all samples are libfdb_c code on that thread; most of the rest of that thread is kernel.
  - By DSO, CGo samples are about 37–38% kernel, 28–30% libfdb_c, 23–26% Go binary and 6% libc.
  - Both Go clients spread samples evenly over about 10 threads (top thread about 11–13%).
  - Kernel share in the Go clients is 14–16% at w64 set100 and about 38% at w8 get100, with no single network thread.
- The C client puts the wire work on one serial thread. The Go clients spread it across goroutines, at the cost of more scheduler and GC work: GC in old Go, `findRunnable` in new Go.

## M2 (new Go, local single storage server)

- **Placement doesn't apply locally.** With one storage server, mapped reads are never cross-server. Client profiles omit server stacks, but their waits still include server-side work: mapped-read execution and its secondary queries run inside the storage server (`fdbserver/storageserver.actor.cpp:6364-6368`, `:5923-5930`, `:4771-4784` in 7.3.77).
- **Skip rows10, USE_REMOTE_FETCH vs SCAN_AND_FETCH:**
  - Remote allocates less in both directions. Reverse: 127 KB and 885 allocs/op vs 141.5 KB and 1043 allocs/op. Forward: 127 KB and 886 vs 141 KB and 1047.
  - The ordinary path blocks in `PendingRange.Get`/`execute` (index scan then fetch).
  - The remote path blocks in `PendingMappedRange.Get`, and allocates in `parseGetMappedKeyValuesReply` (11.6% of alloc_space, cumulative).
- 154 of 160 M2 runs are load-flagged, and 22 of 32 M2 CPU profiles have fewer than 2,000 samples. E1 was not run for M2, so M2 CPU tops are indicative only.

## Quiet-slot confirmation

Slot B, 16:05–16:20Z.

- **Gate:** passed at 16:06:57Z: no Bazel activity, no testcontainers, 60 s settle, 1-minute load 5.99. The baseline load of about 5 comes from agent infrastructure (kind cluster, firefox); 1-minute load during the runs was 6.0–9.3, partly from the batch itself.
- **Lock:** the batch held the shared `${FDB_PERF_HOME}/validation/fast.lock`. The earlier, invalid A2 batch had flocked the wrong file, `${FDB_PERF_HOME}/fast.lock`. The `fast_lock` fields in every `done.json` written before 15:10Z describe that wrong file and carry no information (`provenance/DEVIATIONS.md` item 10).
- **Failures:** none; 96 processes ran.

**Wait profiles, 8 workers, old → new.** Median of 3 alternating repeats at campaign iterations, with one E1-scaled repeat in brackets; µs per measured tx summed over goroutines.

| case (load1 at start) | block total | GRV wait | read reply wait | trace sched total | sched woken by readLoop | trace net total |
|---|---|---|---|---|---|---|
| get100 (6.0–6.8; scaled 6.6–7.7) | 323 → 398, 1.23× [1.17×] | 178 → 206, 1.16× [1.14×] | 55 → 69, 1.25× [1.27×] | 29 → 37, 1.27× [1.23×] | 11 → 16, 1.48× [1.41×] | 67 → 128, 1.90× [1.92×] |
| get1000 (6.1–6.7; scaled 7.4–8.0) | 339 → 411, 1.21× [1.12×] | 184 → 209, 1.14× [1.14×] | 57 → 73, 1.29× [1.26×] | 29 → 39, 1.35× [1.26×] | 11 → 17, 1.51× [1.39×] | 65 → 131, 2.02× [1.99×] |
| set100 (6.1–6.7; scaled 6.6–7.6) | 2249 → 2178, 0.97× [0.97×] | 134 → 124, 0.93× [0.90×] | – | 48 → 41, 0.85× [0.82×] | 27 → 22, 0.81× [0.74×] | 126 → 253, 2.01× [2.02×] |

- **8-worker gets:** the quiet runs reproduce the direction and roughly the size of the earlier increases. Scheduler latency is about 1.3× in total and 1.4–1.5× for goroutines woken by readLoop.
- **8-worker set100:** the earlier column was load-flagged; the quiet runs give the same picture. Waits are flat or lower in new Go, and the doubled net total is the GRV window goroutine becoming visible.

**A2 timing (planskip rows10, skip 3; old vs new Go; 6 rotated blocks, median of 3 testing.B repeats per process).**

| direction | remote/ordinary, old Go | remote/ordinary, new Go | new/old SCAN_AND_FETCH | new/old USE_REMOTE_FETCH | load1 at start (min / median / max) |
|---|---|---|---|---|---|
| reverse (target) | 0.700 [0.689, 0.718] | 0.826 [0.816, 0.831] | 0.988 [0.964, 1.012] | 1.167 [1.150, 1.184] | 6.4 / 7.5 / 8.7 |
| forward (control) | 0.730 [0.703, 0.767] | 0.838 [0.829, 0.845] | 1.014 [0.987, 1.068] | 1.164 [1.148, 1.187] | 6.6 / 7.5 / 9.3 |

Each value is the geometric mean over 6 blocks, with the per-block minimum and maximum in brackets; these are not confidence intervals.

- **Local cluster:** it has one storage server. That removes cross-storage placement variation, not every environmental or shared-path explanation.
- **Remote fetch is about 17% slower per op in new Go than in old Go, in both directions** (1.167 reverse, 1.164 forward; per-block ranges [1.150, 1.184] and [1.148, 1.187]). SCAN_AND_FETCH new/old ratios are near one (0.988 and 1.014; per-block ranges [0.964, 1.012] and [0.987, 1.068]). Remote/ordinary rises from about 0.70–0.73 (old) to about 0.83–0.84 (new).
- **The USE_REMOTE_FETCH slowdown warrants code-path investigation.** It appears in both scan directions. The single-storage topology removes cross-storage placement variation, not every environmental or shared-path explanation. No change is identified as the cause. The local ratios stay below 1, unlike the Hetzner 1.169.
- Per-block ns/op values and the load for each process are in [`profiles-local/a2-quiet-summary.json`](profiles-local/a2-quiet-summary.json).

## Do any conclusions depend on load-flagged runs?

- **Old-vs-new wait findings:** the 64-worker cases and the 8-worker get100/get1000 cases come from unflagged runs (1-minute load 6.2–8.0 at start). The 8-worker set100 column is flagged; the only conclusions drawn from it are that a wait is present or absent (sender flush wait about zero; GRV window goroutine visible), not magnitudes.
- **CPU composition:** the E1 profiles are all flagged. The unflagged base profiles give the same direction for GC, `findRunnable` and syscall shares, except w64 set100 `findRunnable`.
- **A2 timing:** some A2 processes started above load 8: 7 of 24 reverse (maximum 8.67) and 9 of 24 forward (maximum 9.26), per `profiles-local/a2-quiet-summary.json`. The conclusion does not depend on them. Restricted to blocks where all four processes started at load ≤ 8 (reverse blocks 0–2, forward blocks 3–5), new/old USE_REMOTE_FETCH is 1.169 and 1.167 and SCAN_AND_FETCH 0.992 and 0.996. Remote/ordinary goes from 0.70 to 0.83 (reverse) and from 0.72 to 0.84 (forward).
- **Scheduler latency is the most exposed to contention.** Load below 8 doesn't rule out Docker containers sharing the client CPUs. The 8-worker increases in sched-total and readLoop-woken latency were confirmed in the quiet slot at load 6.0–6.8 (see below).

## Appendix: CPU top-5 per arm×case

- Each M1 row lists the top-5 flat functions and the top-5 fdbgo cumulative frames.
- `e1` marks the merged E1 profiles; `base` marks the merged base cpu1–cpu3.
- Full top-30 flat and top-40 cumulative tables, plus heap, block, mutex and trace tops for every M1 and M2 arm×case, are under `analysis/` in the raw data directory.

| phase/case | arm | src | top-5 flat (flat%) | top-5 fdbgo cum (cum%) |
|---|---|---|---|---|
| M1w1 grv | before | base | runtime.futex 22.9; Syscall6 20.0; client.(*grvBatcher).admit 2.9; transport.(*Conn).readLoop 2.9; wire.(*Reader).fieldOffset 2.9 | fdb.runFuture[go.shape.int64].func1 37.1; client.(*grvBatcher).getReadVersion 31.4; client.(*grvBatcher).flushWithin 22.9; client.(*grvBatcher).sendGRVRequest 20.0; client.sendReadFrame 11.4 |
| M1w1 grv | final | base | Syscall6 31.1; runtime.(*mspan).writeHeapBitsSmall 4.4; runtime.futex 4.4; runtime.nanotime 4.4; client.(*grvBatcher).flushWithin 2.2 | fdb.runFuture[go.shape.int64].func1 55.6; client.(*grvBatcher).flushWithin 51.1; client.(*grvBatcher).getReadVersion 51.1; client.(*readVersionState).cachedOrFetchReadVers 51.1; client.readVersionRequest.fetchReadVersion 51.1 |
| M1w1 grv | cgo | base | runtime.cgocall 83.9; runtime.futex 9.7; internal/runtime/atomic.(*Int32).Load 1.6; runtime._ExternalCode 1.6; runtime.stealWork 1.6 |  |
| M1w1 get100 | before | base | Syscall6 44.4; runtime.futex 9.7; context.(*cancelCtx).Done 1.4; context.(*cancelCtx).cancel 1.4; context.WithCancelCause.func1 1.4 | fdb.(*bodyRun).run 55.6; transport.(*Conn).tryWriteDirect 38.9; transport.(*rawWriter).write 38.9; transport.newRawWriter.func1 36.1; transport.(*Conn).SendFrameEagerContext 22.2 |
| M1w1 get100 | final | base | Syscall6 44.3; runtime.futex 11.4; internal/sync.(*Mutex).Lock 2.3; internal/sync.(*Mutex).Unlock 2.3; runtime.findRunnable 2.3 | fdb.(*bodyRun).run 31.8; transport.(*Conn).writeLoop 23.9; client.(*readVersionState).cachedOrFetchReadVers 15.9; client.readVersionRequest.fetchReadVersion 14.8; client.(*grvBatcher).flushWithin 13.6 |
| M1w1 get100 | cgo | base | runtime.cgocall 71.6; runtime.futex 13.8; runtime.addspecial 1.8; internal/runtime/atomic.(*UnsafePointer).StoreNo 0.9; runtime.(*mspan).specialFindSplicePoint 0.9 |  |
| M1w1 get1000 | before | base | Syscall6 37.8; runtime.futex 12.2; runtime.memclrNoHeapPointers 2.7; runtime.nextFreeFast 2.7; time.runtimeNow 2.7 | fdb.(*bodyRun).run 51.4; transport.(*Conn).tryWriteDirect 29.7; transport.(*rawWriter).write 28.4; transport.newRawWriter.func1 28.4; client.(*grvBatcher).getReadVersion 21.6 |
| M1w1 get1000 | final | base | Syscall6 43.2; runtime.futex 14.8; runtime.(*mspan).writeHeapBitsSmall 2.3; runtime.(*waitq).dequeue 2.3; bufio.(*Reader).Read 1.1 | fdb.(*bodyRun).run 35.2; client.(*grvBatcher).getReadVersion 21.6; client.(*readVersionState).cachedOrFetchReadVers 21.6; client.readVersionRequest.fetchReadVersion 21.6; transport.(*Conn).writeLoop 21.6 |
| M1w1 get1000 | cgo | base | runtime.cgocall 75.4; runtime.futex 7.9; runtime.execute 1.8; runtime.nanotime 1.8; runtime.procyieldAsm 1.8 |  |
| M1w1 range100 | before | base | Syscall6 35.5; runtime.futex 6.4; runtime.tryDeferToSpanScan 4.5; runtime.(*randomOrder).start 1.8; runtime.checkRunqsNoP 1.8 | fdb.(*bodyRun).run 34.5; fdb.goRangeResult.GetSliceWithError 30.9; client.(*PendingRange).Get 30.0; client.(*PendingRange).execute 28.2; client.(*rangeRead).run 28.2 |
| M1w1 range100 | final | base | Syscall6 37.0; runtime.futex 9.3; runtime.nanotime 2.8; client.cloneReadKVs 1.9; main.transaction.Range 1.9 | fdb.(*bodyRun).run 48.1; client.(*PendingRange).Get 40.7; fdb.goRangeResult.GetSliceWithError 40.7; client.(*PendingRange).execute 37.0; client.(*panicBackstop).run 37.0 |
| M1w1 range100 | cgo | base | runtime.cgocall 68.7; runtime.futex 5.2; runtime.procyieldAsm 3.7; runtime.typePointers.next 3.0; runtime.(*mspan).specialFindSplicePoint 1.5 |  |
| M1w1 set100 | before | base | Syscall6 32.5; runtime.futex 10.0; runtime.scanObjectsSmall 3.8; internal/poll.(*FD).RawWrite 2.5; runtime.(*randomOrder).start 2.5 | client.(*nativeCommit).run 42.5; client.(*nativeCommit).call 40.0; transport.(*Conn).SendFrameContext 28.8; transport.(*Conn).tryWriteDirect 28.8; transport.(*Conn).writeContext 28.8 |
| M1w1 set100 | final | base | Syscall6 37.1; runtime.futex 13.5; runtime.findRunnable 4.5; runtime.scanblock 2.2; runtime.stealWork 2.2 | client.(*nativeCommit).run 33.7; client.(*nativeCommit).call 29.2; transport.(*Conn).writeLoop 23.6; transport.stampedWriter.Write 18.0; client.(*commitInput).commitNative 14.6 |
| M1w1 set100 | cgo | base | runtime.cgocall 83.7; runtime.futex 4.7; bench.Dataset.Operation.func1 1.2; internal/runtime/atomic.(*Int32).Add 1.2; internal/runtime/atomic.(*UnsafePointer).StoreNo 1.2 |  |
| M1w1 batchset50 | before | base | Syscall6 23.6; runtime.futex 6.2; runtime.scanObject 6.2; runtime.tryDeferToSpanScan 6.2; runtime.findObject 4.2 | client.(*nativeCommit).run 34.7; client.(*nativeCommit).call 33.3; transport.(*Conn).SendFrameContext 21.5; transport.(*Conn).tryWriteDirect 21.5; transport.(*Conn).writeContext 21.5 |
| M1w1 batchset50 | final | base | Syscall6 31.9; runtime.futex 5.9; runtime.memmove 3.7; runtime.findRunnable 2.2; runtime.pcvalue 2.2 | client.(*nativeCommit).run 31.9; client.(*nativeCommit).call 30.4; transport.(*Conn).writeLoop 23.0; client.(*nativeReadState).readVersionForCommit 17.8; client.(*readVersionState).cachedOrFetchReadVers 17.8 |
| M1w1 batchset50 | cgo | base | runtime.cgocall 90.9; runtime.futex 4.2; runtime.procyieldAsm 1.4; callRet 0.7; github.com/apple/foundationdb/bindings/go/src/fd 0.7 |  |
| M1w1 batchset50x1000 | before | base | Syscall6 24.3; runtime.memmove 11.8; runtime.futex 9.5; runtime.scanObjectsSmall 2.4; memeqbody 1.8 | client.(*nativeCommit).run 35.5; client.(*nativeCommit).call 33.1; transport.(*Conn).SendFrameContext 17.8; transport.(*Conn).writeContext 17.8; transport.(*Conn).tryWriteDirect 17.2 |
| M1w1 batchset50x1000 | final | base | Syscall6 32.5; runtime.memmove 17.8; runtime.memclrNoHeapPointers 4.3; runtime.futex 3.7; wire/types.keyRangeEqualsKeyAfter 1.8 | client.(*nativeCommit).call 34.4; client.(*nativeCommit).run 34.4; transport.(*Conn).writeLoop 26.4; client.(*commitInput).commitNative 23.3; transport.stampedWriter.Write 18.4 |
| M1w1 batchset50x1000 | cgo | base | runtime.cgocall 93.0; runtime.futex 0.9; runtime.procyieldAsm 0.9; runtime.stealWork 0.9; runtime.futexsleep 0.5 |  |
| M1w8 grv | before | base | Syscall6 20.2; runtime.futex 7.5; runtime.nanotime 5.3; runtime.nextFreeFast 4.3; runtime.scanObject 4.3 | client.(*grvBatcher).admit.func1 29.8; client.(*grvBatcher).flush 29.8; client.(*grvBatcher).flushWithin 29.8; client.(*grvBatcher).getReadVersion.func2 29.8; client.(*grvBatcher).sendGRVRequest 24.5 |
| M1w8 grv | final | base | Syscall6 20.3; runtime.futex 9.5; runtime.lock2 5.4; runtime.chanrecv 2.7; runtime.mallocgc 2.7 | fdb.(*bodyRun).run 21.6; client.(*grvBatcher).flush 18.9; client.(*grvBatcher).flushWithin 18.9; client.(*grvBatcher).sendGRVRequest 17.6; client.sendReadFrame 14.9 |
| M1w8 grv | cgo | base | runtime.cgocall 59.8; runtime.futex 12.5; runtime.procyieldAsm 6.2; runtime.(*mspan).specialFindSplicePoint 4.5; runtime.markrootSpans 2.7 |  |
| M1w8 get100 | before | e1 | Syscall6 27.1; runtime.futex 6.8; runtime.spanClass.sizeclass 2.5; runtime.scanObjectsSmall 2.4; runtime.scanObject 1.9 | fdb.(*bodyRun).run 33.2; transport.(*Conn).tryWriteDirect 15.2; transport.(*rawWriter).write 14.7; transport.newRawWriter.func1 14.2; transport.(*Conn).SendFrameEagerContext 10.2 |
| M1w8 get100 | final | e1 | Syscall6 16.2; runtime.futex 10.6; runtime.nextFreeFast 2.0; runtime.lock2 1.8; time.runtimeNow 1.7 | fdb.(*bodyRun).run 34.3; fdb.(*pendingFutureByteSlice).Get 12.2; fdb.(*pendingFutureByteSlice).resolve 12.1; client.(*PendingGet).ResolveOrAbort 11.5; client.(*PendingGet).resolve 8.9 |
| M1w8 get100 | cgo | e1 | runtime.cgocall 67.3; runtime.futex 7.0; runtime.procyieldAsm 6.8; runtime.(*mspan).specialFindSplicePoint 2.9; runtime.stealWork 1.1 |  |
| M1w8 get1000 | before | e1 | Syscall6 27.4; runtime.futex 5.6; runtime.scanObject 2.3; runtime.scanObjectsSmall 2.3; runtime.spanClass.sizeclass 2.1 | fdb.(*bodyRun).run 32.9; transport.(*Conn).tryWriteDirect 14.9; transport.(*rawWriter).write 14.4; transport.newRawWriter.func1 14.1; transport.(*Conn).readLoop 10.4 |
| M1w8 get1000 | final | e1 | Syscall6 15.6; runtime.futex 9.5; time.runtimeNow 1.9; runtime.selectgo 1.6; runtime.tryDeferToSpanScan 1.6 | fdb.(*bodyRun).run 33.3; fdb.(*pendingFutureByteSlice).Get 11.3; fdb.(*pendingFutureByteSlice).resolve 11.3; client.(*PendingGet).ResolveOrAbort 10.9; client.(*PendingGet).resolve 8.1 |
| M1w8 get1000 | cgo | e1 | runtime.cgocall 63.2; runtime.procyieldAsm 7.6; runtime.futex 6.9; runtime.(*mspan).specialFindSplicePoint 3.5; runtime.runqgrab 0.8 |  |
| M1w8 range100 | before | base | Syscall6 18.7; runtime.scanObjectsSmall 6.0; runtime.scanObject 5.7; runtime.futex 4.7; runtime.tryDeferToSpanScan 4.7 | fdb.(*bodyRun).run 34.6; fdb.goRangeResult.GetSliceWithError 26.6; client.(*PendingRange).Get 23.9; client.(*PendingRange).execute 20.4; client.(*rangeRead).run 20.4 |
| M1w8 range100 | final | base | Syscall6 17.9; runtime.futex 7.9; wire/types.ParseKeyValueRefStringVector 2.8; runtime.tryDeferToSpanScan 2.5; runtime.typePointers.next 2.3 | fdb.(*bodyRun).run 42.5; client.(*PendingRange).Get 32.6; fdb.goRangeResult.GetSliceWithError 32.6; client.(*PendingRange).execute 30.3; client.(*rangeRead).run 30.3 |
| M1w8 range100 | cgo | base | runtime.cgocall 49.7; runtime.futex 4.9; runtime.procyieldAsm 3.6; runtime.mallocgc 2.6; runtime.memclrNoHeapPointers 2.3 |  |
| M1w8 set100 | before | e1 | Syscall6 21.1; runtime.futex 7.3; runtime.spanClass.sizeclass 4.1; runtime.scanObjectsSmall 2.8; runtime.tryDeferToSpanScan 2.2 | client.(*nativeCommit).run 36.7; client.(*nativeCommit).call 32.8; client.(*commitInput).commitNative 24.5; transport.(*Conn).SendFrameContext 13.5; transport.(*Conn).writeContext 13.5 |
| M1w8 set100 | final | e1 | Syscall6 22.2; runtime.futex 7.5; runtime.pcvalue 1.5; runtime.stealWork 1.5; runtime.step 1.4 | client.(*nativeCommit).run 30.8; client.(*nativeCommit).call 26.0; client.(*commitInput).commitNative 22.5; client.buildCommitInputRequest 16.4; transport.(*Conn).writeLoop 15.4 |
| M1w8 set100 | cgo | e1 | runtime.cgocall 71.3; runtime.futex 6.0; runtime.procyieldAsm 4.3; runtime.(*mspan).specialFindSplicePoint 2.7; runtime.stealWork 1.0 |  |
| M1w8 batchset50 | before | base | Syscall6 10.7; runtime.futex 4.5; runtime.scanObject 4.3; runtime.memmove 4.0; runtime.memclrNoHeapPointers 3.3 | client.(*nativeCommit).run 28.5; client.(*nativeCommit).call 25.2; fdb.(*bodyRun).run 21.5; client.(*commitInput).commitNative 18.3; client.(*rywCache).set 9.9 |
| M1w8 batchset50 | final | base | Syscall6 10.7; runtime.futex 5.8; runtime.memclrNoHeapPointers 4.0; runtime.memmove 4.0; runtime.nextFreeFast 2.9 | client.(*nativeCommit).run 26.4; fdb.(*bodyRun).run 24.7; client.(*nativeCommit).call 21.6; client.(*commitInput).commitNative 18.0; client.(*rywCache).setWith 12.2 |
| M1w8 batchset50 | cgo | base | runtime.cgocall 90.5; runtime.futex 1.6; runtime.procyieldAsm 1.4; runtime.(*mspan).specialFindSplicePoint 0.5; runtime.stealWork 0.4 |  |
| M1w8 batchset50x1000 | before | base | runtime.memmove 29.5; Syscall6 13.2; runtime.memclrNoHeapPointers 6.5; runtime.futex 3.7; runtime.heapSetTypeSmallHeader 2.6 | fdb.(*bodyRun).run 35.8; client.(*nativeCommit).run 28.8; client.(*nativeCommit).call 25.6; client.(*commitInput).commitNative 21.3; transport.(*Conn).SendFrameContext 13.9 |
| M1w8 batchset50x1000 | final | base | runtime.memmove 19.2; Syscall6 16.9; runtime.memclrNoHeapPointers 9.2; runtime.futex 3.7; runtime.(*mspan).writeHeapBitsSmall 1.5 | fdb.(*bodyRun).run 36.3; client.(*nativeCommit).run 22.5; client.(*nativeCommit).call 18.6; client.(*commitInput).commitNative 17.4; transport.(*Conn).writeLoop 14.4 |
| M1w8 batchset50x1000 | cgo | base | runtime.cgocall 92.9; runtime.futex 1.1; runtime.procyieldAsm 0.8; runtime.(*mspan).specialFindSplicePoint 0.6; bench.Dataset.Operation 0.3 |  |
| M1w64 grv | before | base | Syscall6 8.0; runtime.scanObject 5.8; runtime.scanObjectsSmall 5.1; time.runtimeNow 5.1; internal/sync.(*Mutex).Lock 4.4 | fdb.runFuture[go.shape.int64].func1 38.7; client.(*grvBatcher).getReadVersion 11.7; fdb.(*bodyRun).run 10.9; client.(*grvBatcher).flushWithin 8.0; client.(*grvBatcher).sendGRVRequest 7.3 |
| M1w64 grv | final | base | Syscall6 10.9; bench.Dataset.Operation 4.7; runtime.unlock2 4.7; runtime.futex 3.9; runtime.selectgo 3.1 | fdb.(*bodyRun).run 25.8; fdb.runFuture[go.shape.int64].func1 21.1; fdb.Database.applyTxDefaults 15.6; client.(*readVersionState).cachedOrFetchReadVers 14.1; client.readVersionRequest.fetchReadVersion 10.2 |
| M1w64 grv | cgo | base | runtime.cgocall 64.7; runtime.procyieldAsm 6.5; runtime.futex 5.6; runtime.(*mspan).specialFindSplicePoint 3.3; runtime.(*mspan).init 0.9 |  |
| M1w64 get100 | before | e1 | Syscall6 13.0; runtime.spanClass.sizeclass 3.5; runtime.tryDeferToSpanScan 2.8; runtime.memclrNoHeapPointers 2.6; runtime.futex 2.4 | fdb.(*bodyRun).run 42.3; fdb.(*pendingFutureByteSlice).Get 15.5; fdb.(*pendingFutureByteSlice).resolve 15.5; client.(*PendingGet).ResolveOrAbort 15.1; client.(*PendingGet).resolve 10.4 |
| M1w64 get100 | final | e1 | Syscall6 11.6; runtime.futex 4.6; runtime.selectgo 3.0; time.runtimeNow 2.3; runtime.memclrNoHeapPointers 2.0 | fdb.(*bodyRun).run 47.4; fdb.(*pendingFutureByteSlice).Get 17.2; fdb.(*pendingFutureByteSlice).resolve 17.2; client.(*PendingGet).ResolveOrAbort 16.5; client.(*PendingGet).resolve 13.7 |
| M1w64 get100 | cgo | e1 | runtime.cgocall 62.5; runtime.procyieldAsm 6.9; runtime.futex 4.6; runtime.(*mspan).specialFindSplicePoint 3.5; runtime.lock2 0.9 |  |
| M1w64 get1000 | before | e1 | Syscall6 13.5; runtime.spanClass.sizeclass 3.2; runtime.scanObjectsSmall 3.2; runtime.memclrNoHeapPointers 3.0; runtime.tryDeferToSpanScan 2.8 | fdb.(*bodyRun).run 41.6; fdb.(*pendingFutureByteSlice).Get 16.2; fdb.(*pendingFutureByteSlice).resolve 16.2; client.(*PendingGet).ResolveOrAbort 15.5; client.(*PendingGet).resolve 10.4 |
| M1w64 get1000 | final | e1 | Syscall6 12.4; runtime.futex 4.5; time.runtimeNow 2.4; runtime.selectgo 2.2; runtime.lock2 2.1 | fdb.(*bodyRun).run 47.1; fdb.(*pendingFutureByteSlice).Get 18.0; fdb.(*pendingFutureByteSlice).resolve 18.0; client.(*PendingGet).ResolveOrAbort 17.5; client.(*PendingGet).resolve 13.1 |
| M1w64 get1000 | cgo | e1 | runtime.cgocall 60.7; runtime.procyieldAsm 7.1; runtime.(*mspan).specialFindSplicePoint 4.0; runtime.futex 4.0; runtime.runqgrab 1.1 |  |
| M1w64 range100 | before | base | Syscall6 12.5; runtime.scanObjectsSmall 6.6; runtime.scanObject 5.8; runtime.tryDeferToSpanScan 5.4; runtime.spanClass.sizeclass 4.7 | fdb.(*bodyRun).run 38.1; fdb.goRangeResult.GetSliceWithError 28.3; client.(*PendingRange).Get 26.2; client.(*PendingRange).execute 21.2; client.(*rangeRead).run 21.1 |
| M1w64 range100 | final | base | Syscall6 12.1; runtime.futex 5.0; runtime.memclrNoHeapPointers 3.7; runtime.tryDeferToSpanScan 3.6; client.cloneReadKVs 2.7 | fdb.(*bodyRun).run 48.5; fdb.goRangeResult.GetSliceWithError 36.8; client.(*PendingRange).Get 36.6; client.(*PendingRange).execute 28.1; client.(*rangeRead).run 27.9 |
| M1w64 range100 | cgo | base | runtime.cgocall 43.3; runtime.procyieldAsm 4.3; runtime.futex 4.1; runtime.tryDeferToSpanScan 2.9; runtime.memclrNoHeapPointers 2.0 |  |
| M1w64 set100 | before | e1 | Syscall6 10.5; runtime.spanClass.sizeclass 5.0; runtime.scanObjectsSmall 4.3; runtime.futex 3.2; runtime.tryDeferToSpanScan 3.2 | client.(*nativeCommit).run 39.3; client.(*nativeCommit).call 32.5; client.(*commitInput).commitNative 23.4; client.buildCommitInputRequest 11.8; transport.(*Conn).SendFrameContext 9.1 |
| M1w64 set100 | final | e1 | Syscall6 11.9; runtime.futex 4.0; runtime.tryDeferToSpanScan 2.9; runtime.pcvalue 2.6; runtime.memmove 2.3 | client.(*nativeCommit).run 43.0; client.(*nativeCommit).call 34.7; client.(*commitInput).commitNative 28.9; client.buildCommitInputRequest 17.1; wire/types.(*KeyRangeRef).writeToBuffer 11.4 |
| M1w64 set100 | cgo | e1 | runtime.cgocall 71.1; runtime.procyieldAsm 5.2; runtime.futex 3.1; runtime.(*mspan).specialFindSplicePoint 2.8; runtime.lock2 0.7 |  |
| M1w64 batchset50 | before | base | Syscall6 7.1; runtime.memmove 5.7; runtime.findObject 3.4; runtime.heapSetTypeSmallHeader 3.1; runtime.memclrNoHeapPointers 2.7 | client.(*nativeCommit).run 26.7; fdb.(*bodyRun).run 25.2; client.(*nativeCommit).call 21.8; client.(*commitInput).commitNative 17.5; client.(*rywCache).set 11.8 |
| M1w64 batchset50 | final | base | Syscall6 7.7; runtime.memmove 6.5; runtime.memclrNoHeapPointers 4.7; runtime.futex 3.7; runtime.nextFreeFast 2.5 | fdb.(*bodyRun).run 33.6; client.(*nativeCommit).run 26.7; client.(*nativeCommit).call 22.3; client.(*commitInput).commitNative 19.5; client.buildCommitInputRequest 15.5 |
| M1w64 batchset50 | cgo | base | runtime.cgocall 90.8; runtime.futex 1.6; runtime.procyieldAsm 1.2; runtime.(*mspan).specialFindSplicePoint 0.6; bench.Dataset.Operation.func1 0.4 |  |
| M1w64 batchset50x1000 | before | base | runtime.memmove 30.4; Syscall6 11.7; runtime.memclrNoHeapPointers 5.5; wire/types.keyRangeEqualsKeyAfter 2.6; runtime.futex 2.6 | fdb.(*bodyRun).run 33.2; client.(*nativeCommit).run 31.4; client.(*nativeCommit).call 29.0; client.(*commitInput).commitNative 26.4; transport.(*Conn).SendFrameContext 13.1 |
| M1w64 batchset50x1000 | final | base | runtime.memmove 20.7; Syscall6 14.9; runtime.memclrNoHeapPointers 7.2; runtime.futex 3.2; wire/types.keyRangeEqualsKeyAfter 2.5 | fdb.(*bodyRun).run 31.4; client.(*nativeCommit).run 28.5; client.(*nativeCommit).call 24.7; client.(*commitInput).commitNative 22.8; client.buildCommitInputRequest 14.4 |
| M1w64 batchset50x1000 | cgo | base | runtime.cgocall 92.9; runtime.futex 1.4; runtime.(*mspan).specialFindSplicePoint 0.6; bench.Dataset.Operation.func1 0.5; runtime.mallocgc 0.1 |  |
