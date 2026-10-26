# Java 4.14.2.0 migration

Merged: `upgrade/java-4.14.2.0` (PR #786, 2026-10-09). FDB/C++ remains **7.3.77**.

## Execution contract

**Active: WS-E. Finish it before starting another workstream.** Work in the order
below; do not substitute an easier item or reopen the archived backlog as a new
parallel campaign. A completed patch is not a completed workstream.

During coding use **`just test`**, with normal Bazel caching. Implement coherent
batches, not a suite invocation after each edit. At a workstream boundary use
**`just test-full`** and the required Java/FDB acceptance checks. Completion means
implemented behavior, regression coverage that exercises it end to end, truthful
compatibility notes, and a green workstream—not merely types or rules that exist.
The final migration receives the milestone review; no intermediate RFC rounds.

The old backlog and its evidence (`TODO_OLD.md`) are in git history, not the active
execution order. Historical audit/design source (not a current completion claim):

```
git show 13d5a3d1e:rfcs/257-java-4.14.2.0-upgrade.md
git show 13d5a3d1e:rfcs/257-java-upgrade-audit/ws-e-design.md
# Other workstreams: replace ws-e with ws-d, ws-f, etc.
```

Verify remaining audit claims against current Go and Java before changing code.
Never mark a whole workstream complete because one of its subitems passed.

## 0. `pkg/fdbgo` CPU under concurrency — TOP PRIORITY

Set as the top priority by the owner on 2026-10-15, ahead of the active workstream.
Server CPU comes first: every client of the cluster pays for it. Some of the gap may be
configuration (options, flags, request shape); the rest is parity with libfdb_c 7.3.77.

Method (reproducible on one host): `fdbserver` 7.3.77 run natively as three processes, one
each of class `stateless`, `transaction` and `storage`, `configure new single memory`, each
pinned to its own core, disjoint from the client's; `tools/clientbench` with
`GOMAXPROCS=8` on 8 other cores. A single-process server gives every role one shared
connection, which overstates what write coalescing saves. Server cost per transaction is
`tools/clientbench/scripts/servercost.py`: `perf stat -p <all fdbserver PIDs>` cycles,
as the difference between a short and a long run. Server work counts come from the
servers' trace-log metrics (`GrvProxyMetrics`, `ProxyMetrics`, `StorageMetrics`,
`NetworkMetrics`), as deltas around a run with idle time on both sides.

- [x] **RFC-264 results-only decision: Rejected.** The [completed local screen](docs/benchmarks/rfc-264/local-quick-screen.md)
  retained all 72 processes with verified cleanup. P0 and P2t passed throughput/CPU
  noninferiority bounds but failed their intended p50 improvement conditions; neither
  is promoted. The [Rejected RFC](rfcs/264-send-wait-cost-and-ordering.md) retains the
  historical bisect results, common harness and reproduction inputs, not production
  prototype changes. Keep #954's C++-parity 10/20 µs default; the owner accepts its
  latency-versus-CPU/allocation trade-off. Remaining unbuilt ideas are not pursued
  under this RFC; no further prototype or measurement is authorized.
- [ ] **Deferred — zero-delay option comparison; owner rescheduling required.** The
  previously assigned `WithCoalesceDelay(0, 0)` versus 10/20 µs comparison for
  get100/get1000 at 8/64 workers remains deferred, not an active RFC-264 workstream.
  If separately authorized, report latency p50/p99, successful ops/s, errors/retries
  per 1k completed operations with raw counts, and cores busy plus CPU/op. Add other
  metrics only to explain an outlier. The [option godoc](pkg/fdbgo/client/options.go) documents the
  explicit option and unchanged default; update the measured trade-off only if
  warranted by those separately authorized results. No run is currently authorized.

- [x] **Timeout delivery parity (C++ C4).** Done: the eager `checkTimeout` is gone; read,
  commit, watch and metric entry and `OnError` (owned and borrowed) test only the recorded
  cause, as C++ tests `resetPromise.isSet()`. 1031 is recorded only by delivery or an arm
  that is already expired (`ReadYourWrites.actor.cpp:1567-1579, 1506-1517`). An `OnError`
  past an undelivered deadline retries, and its re-arm records 1031 for the next operation,
  as `resetRyow` does. A pipelined Get no longer cuts its reply wait at the deadline and
  resends; delivery ends the wait (RYW:361-391). Pinned pending versus delivered over the
  manual timeout queue: `TestReadIncarnation_ElapsedTimeoutIsNoCauseUntilDelivered` (eight
  entry points, real FDB), `TestSetTimeout_UndeliveredKeepsPipelinedReplyWait`,
  `TestElapsedPendingTimeoutIsNoCause`, `TestExpiredArmIsSynchronous/OnError`,
  `TestOwnedOnErrorElapsedDeadlineIsNoCause` and the borrowed-verdict case; all
  mutation-checked. Tests that slept past a 1 ms timeout now wait for delivery or loop as
  the C unit tests do.

- [ ] **Server CPU per transaction.** At 8–64 workers the Go client makes the servers do
  1.5–2.4× the CPU per transaction of libfdb_c; at 1 worker it is even.
  - 3-node cluster (`hcloud-multinode-vpc-20261014` phase M1, server node CPU per
    transaction, Go / CGo, median of 6 runs each) at 8 / 32 / 64 workers: `get100` 2.09 /
    2.04 / 1.85, `set100` 2.39 / 2.08 / 1.51, `grv` 1.73 / 1.34 / 1.14, `range100` 1.74 /
    1.29 / 1.09. At 1 worker: 0.88–0.99 (`grv` 1.25). Measured before RFC-259 Part A.
  - Both clients request identical work: reads, writes, commits and bytes per transaction
    match. The difference is how many GRV requests, commit batches and packets carry it.
  - [RFC-259](rfcs/259-request-batching-parity.md) Part A (#943) waits the GRV batch
    window on a precise timer, as libfdb_c's `readVersionBatcher` does
    (NativeAPI.actor.cpp:7307-7405); Go timers round sub-millisecond waits up to about
    1 ms when the process is idle, which is why Go had skipped the wait.
  - Local three-process cluster at master `066920ebb` (#943–#948), server cycles per
    transaction Go / libfdb_c, median of 5 runs: `grv` 0.90 at 8 workers, 0.98 at 32;
    `get100` 0.99 at 1, 1.12 at 8, 1.49 at 32; `range100` 1.29 at 8, 1.17 at 32;
    `set100` 0.97 at 1, 1.50 at 8, 1.30 at 32.
  - #948 fixed a leak that inflated every concurrent result: a transaction's TIMEOUT
    timer held the transaction until its deadline, so with clientbench's 30 s timeout
    every finished transaction stayed live (845 MB after 5 s of `get100` at 32 workers)
    and GC cycles grew long enough to split commit convoys.
  - `get100`, 8 workers (before #948): GRV requests and storage queries per transaction are equal;
    the servers process 19% more network events (more, smaller packets). libfdb_c's
    `connectionWriter` waits 9–22 µs and then writes everything queued for a peer
    (FlowTransport.actor.cpp:704-739); Go writes each frame on its sender. RFC-259 Part B
    prototypes on the three-process cluster cut `get100` at 32 workers by 10–16% but
    saved nothing at 8 workers and cost latency, so Part B is not worth its complexity yet.
    Superseded: the write delay below, measured on current master, closes the `get100`
    gap and narrows `range100`.
  - `range100`, 8 workers (before #948): Go sends 1.9× libfdb_c's GRV requests (0.346 vs 0.184 per
    transaction). Both use a window of half the smoothed GRV latency, about 57 µs for Go.
    libfdb_c measures that latency on its network thread, which here is busy deserializing
    range replies, so its window is longer and its batches bigger (5.4 against 2.9
    transactions per request) at 32% more transaction latency (545 vs 413 µs). Matching it
    would mean waiting longer, a latency trade-off rather than a parity fix.
  - `set100` is the largest gap. Workers in a closed loop commit in convoys, and the proxy
    batches a convoy's commits if they arrive within 0.5 ms of the first
    (`COMMIT_TRANSACTION_BATCH_INTERVAL_FROM_IDLE`, CommitProxyServer.actor.cpp:430-436).
    Earlier traced runs linked convoy splits to GC: every observed split began at a cycle that
    started as the convoy's commit replies arrived, and the late goroutines lost 0.5–1 ms
    (stop-the-world, mark assists, and runnable goroutines waiting while Ps run GC mark
    workers). Once split, the two groups alternate on the transaction log and stay apart.
    Earlier `GOGC=off` runs matched libfdb_c; the current-code confirmation below
    finds a partial, not complete, effect at 32 workers. At 8 workers after #948,
    2 of 5 runs stay in one convoy (427k and 450k cycles, 6.6–6.7 ms, as libfdb_c)
    and 3 split (666k–898k, 8.6–10.3 ms). Lever: less allocation and CPU between a
    commit reply and the next commit, tracked under client CPU.
  - [GRV observation change](docs/benchmarks/rfc-259/grv-observation-20261016.md),
    measured against `f377e16d9` on the local three-process cluster: moving adaptive
    sampling after reply publication/yields reduced median server cycles by 16.9% for
    `get100/8`, 3.0% for `get100/32`, and 11.2%/14.3%/7.9% for `grv` at 8/32/64.
    All four 1-worker arithmetic mean latencies changed by less than 1%. An apparent
    five-sample `set100/8` regression did not reproduce in 20 alternating pairs
    (paired cycle ratio 0.887, 95% bootstrap interval 0.757–1.047); writes remain noisy.
    This is not a completed goal: `get100` at 32/64 still costs 1.45×/1.34× libfdb_c,
    `range100` at 8/32/64 costs 1.30×/1.15×/1.16×, and `set100` at 32/64 costs
    1.78×/1.31×. Merged in #952; no multi-node rerun. Raw data retain all
    299 configurations, including the original write regression and its confirmation.
  - [Follow-up screens](docs/benchmarks/rfc-259/followup-screens-20261016.md): skipping
    unused timed successors reliably saves four allocations, but did not materially
    reduce server cycles at 32/64 workers. Native completion yields had an encouraging
    8-worker sample, not a clear 32-worker win. A never-exposed-header reserve was
    active in a profile positive control but also gave no 32-worker benefit
    (prefetch/base 1.017, interval 0.978–1.055). None of these scratch changes is promoted.
    The follow-up report retains all 305 configurations, beyond the main 299.
    A fresh clean-candidate GC comparison at 32 workers gave an off/default paired
    cycle ratio of 0.738 (95% bootstrap interval 0.642–0.852), but even GC-off did not
    consistently match CGo; GC-off is diagnostic only, never a production proposal.
  - Fresh current-code tracing found a 32-reply wave followed by GC initiated in
    `CreateTransaction`, then persistent 10+22 client publication clusters. No captured
    GC start was in FrameReader; the older AfterFunc initiator predates #947. Further
    attribution found separate nine- and 15-request GRV batches already detached before
    their callbacks. The 15-request flush goroutine waited 285.6 µs runnable before
    first execution, then incurred a 63.1 µs assist. This is one onset, not an aggregate
    causal estimate. Investigate timer-expired GRV dispatch placement while preserving
    overlapping RPCs, single timer-reader ownership, and Close's registration barrier;
    a failed successor registration must drain both detached and newly pending work.
    A [timer-worker handoff screen](docs/benchmarks/rfc-259/grv-dispatch-screen-20261017.md)
    against `590aa97e3` did not improve `set100/32`: 10 paired runs gave server-cycle
    ratio 1.016 (95% interval 0.992–1.038) and client-CPU ratio 0.987 (0.970–1.004).
    It is not promoted. Separate traces confirm the inline path was active but still
    find timer/successor scheduling delays and pre-send GC-coordination stalls. Fresh
    baseline flushers at another onset start promptly, then stall in span/request
    construction assists; their long sync waits are inside `gcMarkDone`, not pool
    contention. Investigate the broader pre-GRV-send frontier, not another handoff.
    A [GRV no-ack screen](docs/benchmarks/rfc-259/grv-send-noack-screen-20261017.md)
    also found no benefit: 66 configurations, ten pairs each at `get100/32`, `grv/32`
    and `set100/32`. Client-CPU ratios were 1.006, 1.011 and 1.007; every client/server
    interval crossed 1. The prototype was removed. Do not extend this unqualified
    change to the shared read helper, which would also move timeout/hedge timing.
    Do not arbitrarily widen batch windows, repeat rejected handoff/reserve screens,
    or recycle exposed public transactions or borrowed read data.
  - [ ] **Read gap.** A wire comparison of both clients on current master (48 runs,
    [write-coalesce report](docs/benchmarks/rfc-259/write-coalesce-20261017.md)) found
    the same storage queries and commits per transaction but 1.5–4.3× libfdb_c's TCP
    segments: Go writes
    each frame at once, libfdb_c waits 10–20 µs and writes everything queued for the
    connection (FlowTransport.actor.cpp:704-739). `WithCoalesceDelay` (default libfdb_c's
    knobs) ports that wait. Server cycles / libfdb_c with the delay: `get100` 0.59 at 8
    workers, 0.88 at 32 (1.34 without); `grv` 0.76 at 8; 1-worker 0.93–0.99. The wait
    raises 1-worker `get100` latency 26% (221 → 279 µs; libfdb_c 404 µs). OWNER
    DECISION (2026-10-17): the wait is on by default, as in libfdb_c;
    `WithCoalesceDelay(0, 0)` restores immediate writes.
    A [closed-loop sweep](docs/benchmarks/rfc-259/wire-capture-20261018.md) of all four
    workloads at 1/8/32/64 workers on `e5209b8c6`: server cycles ≤ 1.03× libfdb_c
    everywhere except `range100` at 32–64 (1.12–1.15×, outside the criterion) and
    `set100` at 32 (1.30×, see the write gap), with Go throughput 1.3–2.1× at 32–64
    workers on reads and GRVs and 1-worker latency 0.66–0.98×. `range100`'s range
    requests and reply sizes match; Go sends 46–48% more GRV requests (smaller adaptive
    batches at equal wire latency) and at matched paced load (5,000/s) costs 1.03×.
    Still open: closed-loop `range100` at 32–64 workers and the multi-node rerun.
  - [ ] **Write gap.** Closed-loop `set100` keeps a gap (cycles-only rerun: 1.33×
    libfdb_c at 8 workers, 1.53× at 32). Wire captures
    ([report](docs/benchmarks/rfc-259/wire-capture-20261018.md)) associate it with GC: in a
    closed loop commits mostly form batches of the worker count until a batch splits,
    after which runs stay mostly split, for both clients. Every Go first split followed
    a GC start by 2.6–3.2 ms (GCs every 232–348 ms); with `GOGC=off` all three Go runs
    stayed whole, against one of three libfdb_c runs. Allocation cuts did not move the
    first split. At matched scheduled offered load
    ([paced-load report](docs/benchmarks/rfc-259/paced-load-20261018.md), 64 workers,
    `clientbench -rate`, timerfd pacing) write server cost is similar at the three tested
    rates (0.99–1.00× at 1,000–4,000 operations per second, five pairs each). The closed-loop gap therefore
    depends on how often lockstep rounds stay together; split rates also moved with run
    shape and instrumentation in a small screen. Earlier record:
    `set100` at 8 and 32 workers costs the servers 1.6–1.8× libfdb_c (medians) with or without the write delay whenever libfdb_c's runs stay
    unsplit; in the final campaign libfdb_c split at 32 workers and the ratio there was
    1.17–1.20. Runs are bimodal for both clients: about one commit batch per worker
    count of transactions (0.126 per transaction at 8 workers, 0.032 at 32) or about
    0.2 and 0.06, at roughly twice the server cycles. That suggests a closed-loop round
    reaches the commit proxy whole or split; it is inferred from run averages, not yet
    traced per round. Runs near one batch per round cost the same for both clients. Go
    split in 28 of 30 runs at 8 and 32 workers, libfdb_c in 5 of 18. Commits per
    transaction are equal, but when libfdb_c stays unsplit the commit proxy forms
    1.5–2.0× the commit batches (0.061–0.063 vs 0.032 per transaction at 32 workers:
    about 16 commits per batch against 31), and the transaction role, which
    pays per batch, costs about 2×. At 8 workers Go already sends 1.9× the GRV requests
    (0.28 vs 0.15 per transaction), so a closed-loop round splits before its commits do.
    Go's write latency is also worse: 10–11 ms against 6.5–6.9 ms when libfdb_c stays
    unsplit (10.0 ms when it splits at 32 workers). Next: trace when each
    client sends its GRVs and commits within a round and compare with readVersionBatcher
    and the commit path in NativeAPI.actor.cpp, as for the read gap.
  - Paced reads: `get100` costs the servers 0.94× libfdb_c at 10,000 operations per
    second and 1.04× at 30,000, with lower Go latency (327–340 µs against 424–554 µs).
    A first campaign paced with `time.Sleep` showed 1.18–1.36×, consistent with the CGo
    arm's millisecond wake-ups bunching its arrivals (not isolated). In both rows the CGo
    arm's starts still depart from the schedule (10,000/s: 1.4% over 1 ms late; 30,000/s:
    9.3%), so this is similar cost under the same scheduled load, not equal actual
    arrivals. More workers than rate × p99 latency, or a shared dispatcher, would match them.
  - [ ] **Client CPU on paced writes.** `set100` used 1.51–1.53× libfdb_c's client CPU at
    1,000–4,000 operations per second (reads 1.03–1.04×). A waiting sender now takes the
    connection writer's turn ([writer-turn report](docs/benchmarks/rfc-259/writer-turn-20261018.md)):
    paced `set100` 1.32–1.34× libfdb_c (0.89–0.90× master), paced `get100` 0.98×,
    closed `get100` at 8 workers 0.76×; closed `set100` at 32 workers 1.00× master
    (95% interval 0.954–1.045). Commits keep their non-waiting send. With the delay off
    a paced write costs 86.6 µs (libfdb_c 81–91 µs). See "Client CPU per transaction".
  - [RFC-260](rfcs/260-inline-native-commit.md) (deferred): on a tmpfs cluster, paced
    `set100` costs 1.32× libfdb_c. Measured contributors: the write delay's `timerfd` wait
    and the per-commit goroutine's scheduler wake-ups. Running the commit on the caller cut
    client CPU to 0.873× master paced and 0.937× at 32 closed-loop workers (a mutant
    without abandonment handling). A faithful version needs a resumable GRV fetch and
    send; the requirements are in the RFC.
  - Done when server cycles per transaction are within 10% of libfdb_c for `get100`,
    `set100`, `range100` and `grv` at 8–64 workers, also under matched paced load
    (`clientbench -rate`), locally and on a multi-node rerun,
    with 1-worker mean latency no worse than libfdb_c's (owner decision, 2026-10-17;
    previously within 5% of the Go client's own).
- [ ] **Wire divergences found by the wire goldens.** The Go client's requests differed from
  libfdb_c's in three [wire-golden](tools/clientbench/wiregolden/README.md) scenarios
  (`go-divergences.diff`); fixing one regenerates the goldens. The fast lane checks the
  Go client against `go.golden` (`tools/clientbench/wiregolden/golden_test.go`).
  - [x] `getkey_*` (RFC-262 phase 2): getKey runs the ported walk; the six sections
    equal libfdb_c's. New phase-2 sections (`getkey_across_clears`,
    `getkey_across_write_conflict_ranges`, `getkey_enclosed_write_conflict_ranges`,
    `getkey_adjacent_clears{,_reversed,_no_conflict}`, `snapshot_ryw_disabled_getkey`)
    were captured from libfdb_c and match.
  - [ ] `set_then_range` (RFC-262 phase 3; RYW storage reads): libfdb_c sizes each storage read
    of a key selector or an RYW range read in `getRangeValue`/`getRangeValueBack`
    (limits 1, 2, -1, -3 in the scenarios; 80000 rows for an iterator-mode RYW read; a
    read past a pending write to the end of the unknown stretch); Go reads 256 rows (or
    10000) and stops at the pending key. Port the request sizing: `requestCount`,
    `clearLimit`, `skipUncached`/`skipUncachedBack`, `countUncached`, `additionalRows`
    (`ReadYourWrites.actor.cpp:505-590, 592-800, 897-1053`). Was: a key selector with
    offset 2 is resolved by a storage range read of limit 2 in libfdb_c and of limit 256
    in Go. C++ resolves it through
    `getRangeValue` with `GetRangeLimits(1)` (`ReadYourWrites.actor.cpp:140-160`); the
    offset beyond the first row becomes `additionalRows` (`:731-749`), which
    `setRequestLimits` adds to the one-row limit (`:569-575`).
  - [x] `clear_range_then_set`: libfdb_c sends a clear split around a later set in it,
    as `writeRangeToNativeTransaction` emits one clear per cleared stretch of the write map
    (`ReadYourWrites.actor.cpp:2001-2018`); Go sends the whole clear. The stored data is the
    same, but the mutation stream (and so mutation logs) differs.
  - [x] `add_then_get`: Go shipped duplicate read conflict ranges in read order; C++
    ships its `CoalescedKeyRefRangeMap` ranges, sorted and merged (`ReadYourWrites.h:237`,
    `KeyRangeMap.h:304-350`).
  - [x] `estimated_range_size`: without a tenant the `WaitMetricsRequest` asks for
    `latestVersion` (`NativeAPI.actor.cpp:7824`); with one it reads at the transaction's
    read version (`ReadYourWrites.actor.cpp:1860-1863`, `NativeAPI.actor.cpp:7820-7843`).
  - [ ] Tenant location requests ask for `MinTenantVersion: latestVersion`
    (`pkg/fdbgo/client/locality.go`); C++ passes the transaction's version into the
    location lookup of the tenant metrics path (`NativeAPI.actor.cpp:3197-3198,
    7825-7834`).
  - [x] Read tag sampling: about 1% of libfdb_c read requests carry the read tag set,
    even an empty one (`sampleReadTags`, `NativeAPI.actor.cpp:2080-2083`). C++ draws once
    per native request (`:3681,3806,3970,4303,4684`) and `loadBalance` reuses it across
    alternatives and hedges (`LoadBalance.actor.h:446,662,689`). A watch's first request
    uses the read tags as of the commit (`setupWatches`, `:6418-6432`); the set captured at
    registration (`:5697-5703`) only reaches `restartWatch`. Go draws in each read loop
    iteration, keeps the draw across reply-timeout, maybe-delivered and dead-endpoint
    retries (including a pipelined get's fallback read), and draws again after
    wrong_shard_server or all_alternatives_failed. The async watch sends the commit-time
    set, the synchronous Go-only `Watch` the setup capture. Pinned by
    `read_tags_unit_test`, `TestReadTagSamplingRequestKinds` (storage_plumbing_fdb_test)
    and `TestReadTagSamplingOneDrawPerRequest` (client_test). The goldens leave these
    fields out of the read requests.
  - [ ] `Reset` or `OnError` after a commit cancels that commit's watches: Go's
    `cancelWatches` cancels every registered watch context, while C++ `setupWatches`
    empties `watches` once the commit hands them off (`NativeAPI.actor.cpp:6435`), so
    `Transaction::reset`'s `cancelWatches` (`:6173`) no longer reaches them. Observed:
    WatchSetup, commit, WatchPoll, then `Reset` ends the poll with context canceled.
  - [ ] The sample rate can be overridden through the cluster's global configuration
    (`transactionTagSampleRate`); Go has no global configuration (libfdb_c refreshes it at
    startup with `GlobalConfigRefreshRequest`).
  - [x] `clear_key`: libfdb_c serializes a single-key clear `[k, k\x00)` as `(k\x00, '')`
    (`MutationRef::serialize`, `CommitTransaction.h:318-334`); Go sends `(k, k\x00)`.
  - [ ] Mapped range continuation selector: inside one native call a forward mapped read
    continues at `FGE(keyAfter(last))` (`pkg/fdbgo/client/mappedrange_selectors.go:179`);
    NativeAPI `getRange` sends `firstGreaterThan(last)` (`NativeAPI.actor.cpp:4850`). Same
    rows, different `Begin` bytes. RFC-262 D4 leaves mapped reads on their sender; port the
    selector-capable sender for them and add a mapped multi-request golden scenario.
- [x] **Explicit write-conflict-range errors are immediate.** `AddWriteConflictRange`
  returns `key_outside_legal_range` (2004) from the call, while the C binding runs RYW
  `addWriteConflictRange` on the network thread and records the error as the
  transaction's deferred error (`ThreadSafeTransaction.cpp:659-663`, check at
  `ReadYourWrites.actor.cpp:2466-2468`), surfacing it on the next operation.
  `AddWriteConflictKey` already defers it. Align the range form (the facade's return
  value then only carries synchronous errors such as inverted ranges) with a test.
  Fixed in RFC-267: the range form defers 2004 through the key form's path and returns
  only inverted_range; pinned by fast-lane tests and a libfdb_c differential.
- [x] **Explicit read-conflict-range errors are immediate or missing.** libfdb_c runs RYW
  `addReadConflictRange` on the network thread too (`ThreadSafeTransaction.cpp:589-594`):
  an endpoint past `getMaxReadKey()`, other than the exact metadataVersionKey range
  (`ReadYourWrites.actor.cpp:1950-1955`), becomes the deferred error (2004) and surfaces
  on the next operation. Go's `AddReadConflictRange` returns 2004 from the call
  (`stateAddReadConflictRange` in `pkg/fdbgo/client/transaction.go`), and
  `AddReadConflictKey` (`stateAddReadConflictKey`) has no legal-range check, so a system-key read conflict is recorded without error; the
  Apple binding's key form calls the same C function as the range form. Defer both as
  RFC-267 does for the write form, pinned by a libfdb_c differential.
  Fixed in RFC-268: both forms defer 2004 through RFC-267's deferred-error path; the
  range form returns only inverted_range, and the key form also drops an oversized key as
  the range clamp does. Pinned by fast-lane tests and a libfdb_c differential.
- [ ] **Panic-safe locks outside the GetKey walk.** Scoped recovery now unwinds GetKey
  walk locks, but older paths still pair bare Lock/Unlock around fallible work:
  `ryw.go:620-632` (`get` / `localPointLocked`), `ryw.go:643-645` (`getSnapshot`),
  and `transaction.go:3220-3222` (`addReadConflictRangeRYW` / `conflictRanges`).
  Scope deferred unlocks there and pin a panic inside each locked operation; do not
  treat a panic after a reply outside the lock as proof. Also audit transport
  `writer_turn.go:52-99`: `NotifySuspend` and the coalescing wait run with `writeMu`
  held and no panic rollback for the lock, counted frame or `turnCollecting`; pin a
  suspension-observer panic and prove connection reuse after recovery. A fallback
  startup handoff at `writer_turn.go:67` also retains `writeMu` until the other
  resolver parks, stalling unrelated frames on that connection. Startup completion
  or cancellation releases this bounded handoff; it is not a permanent deadlock,
  but notification should not serialize unrelated writes behind that startup.
- [ ] **Panic-safe QueueModel accounting in read senders.** Reply registration
  cleanup does not release a delta when a recovered panic occurs after
  `startRequest`: `readpath.go:313` (getKey), `:561` (getValue), `:636` (single-server
  getValue), and `:1077` (range family). RFC-281 (#1017) removes watch accounting.
  Add a separately owned
  accounting guard through sender-to-RPC ownership transfer and pin a panic after
  each increment. C++ `ModelHolder::~ModelHolder` removes the delta without a
  latency sample (`LoadBalance.actor.h:53-67`); `replyGuard` only owns the endpoint.
- [x] **Join monitor reply waiters on connection close.** `sendPingWithReply`
  launches its reply-wait goroutine outside `Conn.loopWG` (`transport/conn.go`).
  Connection cancellation wakes it, but `Close` can return before it releases
  its reply handle. Track that owner through shutdown and pin zero live waiters
  on return; the cold-control outbox worker is tracked separately.
  Fixed in RFC-269: the waiter is gone. `connectionMonitor` owns the PING reply
  handle, as C++ holds `state PingRequest` in the monitor actor, and releases it
  on every exit before `loopWG.Done` and before any teardown it causes. Five tests,
  each failing on the reverted code, pin the reply, Close, cancellation, frozen-ping
  and panic exits; a unit test pins pooling a teardown reply published after the wait.
- [ ] **Control packets must not be dropped behind FIFO gaps or full queues.**
  Transport `tryEnqueue` drops monitor pings and ping replies when the ordinary
  or held sender FIFO is pending, even if its channel has a free slot; it also
  drops them when the ordinary, held or cold-control inbox is full. C++ queues
  these packets as ordinary entries in `Peer::unsent`
  (`FlowTransport.actor.cpp:1967-1998,2078`). Cold-prefix controls join the
  ordered outbox, but these admission failures remain. Connection teardown is
  a separate lifecycle outcome, not another instance of this enqueue gap.
- [ ] **Measure multicore send-admission contention.** Compare the merged
  RFC-262 sender with follow-ups under concurrent same-peer and independent-peer
  traffic, reporting throughput, client CPU, allocations and mutex/block profiles.
  Single-thread loopback RPC measurements do not establish contention cost.
- [ ] **Reads after an illegal Set precede its deferred verdict.** `stateSet`
  (`transaction.go:1268-1284`) buffers illegal keys/values until commit validation;
  C++ RYW set throws before modifying WriteMap (`ReadYourWrites.actor.cpp:2344-2353`)
  and the thread-safe wrapper defers that error. A later GetKey/conflict filter can
  therefore observe a Go write that C++ never installed. Align mutation admission
  and first-error ordering, with an illegal-Set-then-read/conflict regression.
- [ ] **Transaction options during in-flight operations.** Option setters (`SetLockAware`,
  `SetReadLockAware`, GRV cache, tags, priority, `SetAutoThrottleTag`) write plain
  `Transaction` fields under a shared state lease, while in-flight operations read them:
  GRV completion reads lock awareness for a locked reply (C++ checks the live option there,
  NativeAPI.actor.cpp:7405, 7425-7426), read builders read it per request, and a read's
  tag sample encodes `tx.readTags` (read_tags.go), on its own goroutine for a range read.
  C++ runs these on the network thread. Give the options one synchronization (atomics or a
  mutex) and add a test that overlaps a setter with GRV completion and reads under -race.
- [ ] **Client CPU per transaction.** At 8–64 workers pure-Go uses 1.29–2.16× libfdb_c's
  process CPU on reads and commits (`docs/benchmarks/hcloud-multinode-vpc-20261014/phase-m1-client.md`,
  `hcloud-rebench-20261014/phase-a2-client-8w.md`), while it uses less at 1 worker
  (0.570–0.748×) and on multi-worker `grv` (0.736–0.904×).
  - Local three-process cluster at `066920ebb`, client CPU per transaction Go / libfdb_c
    (median of 5): `get100` 0.91 at 8 workers, 1.00 at 32; `grv` 0.74 at 8, 0.64 at 32;
    `range100` 1.07 at 8, 1.02 at 32; `set100` 1.23 at 8, 1.32 at 32.
  - 3-node cluster at `fa8725f1d` ([fdbmn-20261020](docs/benchmarks/fdbmn-20261020/phase-m1-client.md)),
    lifecycle client CPU per transaction Go / libfdb_c at 8 and 64 workers: `grv` 0.57–0.73,
    `get` 1.04–1.16, `set` 1.15–1.25, `range` 1.28–1.35; at 1 worker 0.78–0.91.
  - Go 1.27.3 costs 1.0–2.8% more client CPU per operation than Go 1.26.10 in every
    local cell (worst `get100` at 32 workers: 1.028, 95% CI 1.020–1.038), with throughput
    within 1.1% ([#981](https://github.com/birdayz/fdb-go/pull/981)). Cause not established.
  - Done: vtable offsets without a map lookup (#944); no derived context for an operation
    whose caller cannot cancel (#945, 8 → 2 allocations per operation); a blind write's
    native GRV without derived contexts and the first native commit inline (#947, 89 →
    67 allocations per `set100` transaction); the TIMEOUT timer no longer retains
    finished transactions (#948).
  - [x] **RFC-263 B1 / generation-scoped timeout merge gates.** Revision 6 merged in
    #973 (`ee05cf5b6`), with canonical resurrection/C7 regressions and compiled mutation
    proofs ([evidence](docs/benchmarks/rfc-263/rev6-production-evidence.md));
    all B1 merge gates passed.
    The later non-inferiority confirmation cleared only the 8w p999 blocker: 97.5% upper
    bounds 1.005 (get) and 1.041 (set), both <1.05
    ([confirmation](docs/benchmarks/rfc-263/rev6-confirm-results.md)). The original screen's
    get p999 +9.30% (4/6 pairs adverse), set p999 +8.94% (5/6) and set mean +0.104%
    (6/6), and its adverse production range trace remain recorded, not erased by clearance
    ([screen and traces](docs/benchmarks/rfc-263/rev6-production-tail-results.md)).
    The separate [A1/A2 CPU qualification](docs/benchmarks/rfc-263/rev6-cpu-results.md)
    now passes the owner-approved 2% rule: A1 remains historically unresolved solely
    on both grv-w8 endpoints; A2 replaces only those estimates without pooling, and
    all sixteen final upper 95% bounds are ≤1.02. Both endpoints improve strictly for
    grv-w8/grv-w32/set100-w8/set100-w32. All eight production queue-wait cells are <1%
    (maximum 0.008781790%); mutex/block views stay separate. Earlier range 8w +1.66%
    and confirmation get 8w +1.69% remain ABOVE baseline historical findings, not
    retrospectively neutralized by the later NI rule. P999 clearance did not waive
    either CPU criterion. Get A/A log SD
    0.25–0.27 exceeded planned 0.15, so planned power was not established despite clearance.
    Set epoch ratios 0.965 → 1.054 and order/serial effects remain disclosed. The owner's
    final race passed 157/157 targets and full passed 210/210, including `client_test`;
    all five official final-HEAD reviews and CI (7/7) passed before merge.
    B3a/B3b are separate work below.
  - [x] **Clientbench lifecycle validator: allow cleanup queued during a completed drain.**
    `tools/clientbench/scripts/run.py:validate_lifecycle` now drops only the entry/exit
    cleanup-queued equality, matching the producer's fixpoint and RFC-263 CPU amendment A1.
    The real e1-r000 lifecycle-only fixture passes; pending exit cleanup, incomplete
    drains and >16 rounds fail. Complete=true, 2–16 rounds, executed=queued at exit,
    counter/GC and every other lifecycle/resource check remain. The standalone A1
    helper stays byte-identical for frozen replay; the original epoch remains invalid.
    See [A1 and census evidence](docs/benchmarks/rfc-263/rev6-cpu-plan.md#amendment-a1-lifecycle-fixpoint-2026-10-20).
  - [ ] **RFC-263 B3a/B3b follow-up.** Deferred lazy-incarnation/turnover work is not part
    of B1. The broader Client CPU per transaction parent item remains open.
  - Remaining hot spots on `set100` (67 allocations, 7.8 KB per transaction): each
    transaction's first `Set` builds its `rywCache` map (about 6 allocations); every
    commit starts a goroutine (`startNativeCommit`: a weak pointer, a cleanup, two
    channels, a context); each incarnation creates its own
    cancellable context, and turnover after every commit, including one with no writes,
    builds the next incarnation (libfdb_c does not reset after commit at API ≥ 410).
  - [Commit allocations](docs/benchmarks/rfc-259/commit-alloc-20261018.md): the
    commit's owned conflict vector and compact GRV state together cut `set100`
    allocations 12% and closed-loop client CPU 6.5% (0.935, 0.902–0.971); neither
    moved client CPU measurably alone.
  - [Commit dispatch without a write acknowledgment](docs/benchmarks/rfc-259/commit-send-noack-20261017.md),
    measured against `57ddc9a77`: 154 completed write configurations, with 10/20/10/30
    alternating Go pairs at 1/8/32/64 workers. Client CPU paired ratios are 0.927
    (95% interval 0.877–0.965) at 32 workers and 0.930 (0.918–0.942) at 64; no material
    server-cost improvement is established. One-worker arithmetic mean latency falls
    3.0%. The initial 64-worker mean-latency increase of 10.0% is retained; 20 follow-up
    pairs give −0.5%, and all 30 pairs give +4.1%, with no strict parallel-latency
    equivalence claim. These are local-only results; client and server parity remain open.

- [ ] **Multi-node throughput and saturation campaign.** Measure Go against libfdb_c on
  `grv`, `get100`, `set100` and `range100` over a worker ladder of 1, 8, 32, 64, 128 and
  256, four alternating blocks per cell, reporting latency, throughput, errors and
  saturation. Throughput counts successful operations in a fixed post-warmup window,
  checked against workers ÷ mean latency and, for writes, the servers' commit counts.
  Needs those checks in `tools/clientbench` and owner approval of the cloud budget.

- [x] **Read hedging second-request budget.** RFC-281 (#1017) ports C++ 7.3.77's
  database-wide budget (initial 0, +0.05 per lone reply, cap 100, cost 1 per hedge),
  adaptive multiplier (initial/floor 1, +0.01 per spend, −0.00025 per lone reply)
  and second-alternative delay/instant arm (`LoadBalance.actor.h:554-563,694-716`,
  `QueueModel.h:121`, `flow/Knobs.cpp:278-283`). Deterministic timer tests pin each
  accrual/spend arm; real-FDB transcripts cover value, selector, range and mapped
  requests, disabled/re-enabled hedging, the unhedged backup fallback and watches.
  Existing hedge tests seed the budget they need.

- [ ] **Connection monitor has no `resetPing` arm.** C++ `connectionMonitor` also
  ends a PING wait on `peer->resetPing.onTrigger()` (`FlowTransport.actor.cpp:696-698`),
  triggered when packets arrive from an incompatible peer (`:1596`) and when the last
  peer reference drops with nothing outstanding (`:1892-1896`). Go's `monitorPing`
  waits only on the reply, the timeout and cancellation. The second trigger depends on
  the peer-reference count Go does not track (see the idle-connection divergence in
  `connectionMonitor`); port both with the unreferenced-close knobs, pinned by tests.

- [x] **A commit reply that does not decode is not `commit_unknown_result`.** Fixed in #1004
  (RFC-276): malformed post-send replies route through `unknownResult`, returning 1021
  only after the dummy-transaction fence succeeds. A corrupted real-FDB commit reply
  pins the applied mutation, the synchronization fence and the unknown-result error.

- [ ] **Endpoints ignore their secondary address.** C++ `Endpoint::serialize` calls
  `choosePrimaryAddress` on deserialization (`FlowTransport.h:62-68,97-103`): a client,
  whose local address list is plaintext with no secondary, swaps to the secondary when
  the primary's TLS flag differs. Go dials `NetworkAddressList.Address` as written
  (`client/endpoint.go`); RFC-276 decodes `SecondaryAddress` as a `NetworkAddress`
  (vector `GetKeyServerLocationsReply_secondary_address`), so the choice can be
  ported and pinned against that vector.
- [ ] **Presence-only Optional tables still encode as byte strings.** RFC-276 decodes
  `ClientDBInfo.EncryptKeyProxy`, `CommitTransactionRequest.CommitCostEstimation` and
  `TenantInfo.Token` as presence only (`optTable` in `cmd/fdb-schema-extract`), but their
  encoders write `VisitDynamicSize` behind the union offset. Give the types Go
  equivalents, or make the encoder refuse a presence-only field, before the client sets one.
  The registered table encoders for `NetworkAddressList.SecondaryAddress` and
  `CommitTransactionRef.spanContext` are pinned byte-exactly by C++ request fixtures
  in #1004.

- [ ] **Read replica selection is best-of-two random.** C++ `loadBalance` sends the
  first request to the alternative with the lowest `smoothOutstanding` (ties to the
  lowest index) and the second to the next-lowest (`LoadBalance.actor.h:469-552`).
  Go's `chooseTopTwo` picks the primary by two random choices and the backup by the
  lowest metric among the rest (`client/loadbalance.go`). Port the C++ selection,
  including its local-DC `countBest` cut-off, with a unit test per arm.

- [ ] **A hedge race ends on the first reply.** Go's `raceReplies` returns whichever
  reply arrives first, even a failure C++ retries in `loadBalance` (`process_behind`,
  `server_overloaded`, a transport failure). C++ discards it, keeps waiting for the
  other request, and sends further second requests to the remaining alternatives
  while the first is outstanding (`LoadBalance.actor.h:648-686`,
  `checkAndProcessResultImpl` `:325-378`). Lone inline retryable errors also bypass
  `primaryReplied` because decoding happens after the hedge helper returns; the
  transport-error unit cases do not cover server error bodies. Port both reply
  paths with encoded-response integration regressions and a synctest per arm.

## 1. WS-E — scalar/SQL semantics and statement execution — ACTIVE

- [x] Invalid UTF-8 rejection on SQL, nested bindings and record writes; copied
  byte/VECTOR bindings (`7875cb7b7`).
- [x] Parameter typing order: known types before Valuer, typed pointers/null
  wrappers, fixed/named byte arrays and slices, empty-array typing, nested-array
  refusal (`564c52f48`).
- [x] Temporal values (`bd59131f7` and its completion): one parser; a bound
  `time.Time` is UTC TIMESTAMP text (22008 outside 0000–9999, arrays included,
  sub-second dropped); DATE→TIMESTAMP and DATE/TIMESTAMP→STRING promotions with
  NULL arms, injected for comparisons, both IN forks, CASE, COALESCE, IFNULL,
  GREATEST and LEAST (IF/IIF are not SQL-reachable: "Unsupported operator IF").
  Pins: `yamsql/testdata/temporal_promotion.yaml` (index and residual, IN forks,
  merging operators, bound times, column types incl. DATE/TIMESTAMP-spelled
  columns as STRING, assignment 22000), `expr/temporal_promotion_property_test.go`
  (producers × operators × NOT × commute, GREATEST/LEAST; mutation-checked),
  binder and promotion unit tests. The explain-differ harness now binds a
  scenario's `args`. OWNER RULING (2026-10-05): only current behavior is
  supported; no legacy mixed-text pins or repair procedure. Full-lane copies
  (`sqldriver` param-rendering and temporal-comparand pins) are updated but
  were not run.
- [x] Bit-exact bindings and floating NaN bits: CAST(string AS DOUBLE/FLOAT) is
  `javanum` (Java's grammar, single binary32 rounding, canonical NaN; replaces
  recordlayer's private parser); MIN/MAX are `Math.min`/`Math.max` line for
  line (NaN operand bits); the plan-cache key renders bound values exactly
  (TRUE/FALSE shared a plan: wrong rows). Pins: `javanum_test` (+30 s fuzz),
  `TestCastValue_StringToFloatingIsJavas`, `TestAggMinMax_ReturnsTheNaNOperandsBits`,
  `param_cache_key_test.go` (red with the old key), `bound_parameter_bits.yaml`.
  The Java-oracle pins (`ws_e_probe_conformance_test.go` v11/v12/cross) are
  flipped to the target's outcomes but not yet run (full lane).
- [x] Terminal NaN equality over a value index or primary key: the binder reads
  both NaN key blocks (`nanBlockTails`), the planner binds a NaN equality as the
  terminal component and prices two seeks, and a NaN never pins ordering.
  Aggregate-index and vector-partition keep the refusal; DIVERGENCES.md "A NaN
  equality over an index returns every stored NaN". Pins:
  `nan_block_binding_test.go`, `nan_index_equality.yaml` (ORDER BY row
  mutation-checked).
- [x] Non-terminal NaN equality: the two NaN blocks plus a key filter on later
  components (`scanKeyFilter`, `filterScanKeys`), below the continuation, for
  value-index and primary-key scans; filter comparands in the fingerprint (NaN
  and ±0 as classes). Pins: `TestNaNEqualityKeyFilterSelectsExactly` (equality,
  zero, range tail, second NaN, reverse, NULL), `TestKeyFilterCursorForwardsContinuations`
  (limit expiring on a rejected entry), and the FDB test
  `TestFDB_DynamicNaNCompositeIndexKeyFilter` (was the refusal pin; DOUBLE and
  FLOAT, forward/reverse, scan limits 0/1/2) — the last is in `sqldriver_test`
  (full lane) and has NOT run yet. The primary-key filter's record-type-prefix
  offset has no end-to-end pin: SQL plans for a dynamic NaN PK prefix with a
  bound suffix pick a scan on the corpus tables.
- [x] Conflict-free index-state reads (design 6.4): planning and per-page
  revalidation read `PeekIndexStates` (no conflict; Java's PlanContext), scans
  keep their per-index key, `GetAllIndexStates` takes one range over the
  index-state subspace (Java's getAllIndexStates). Pins: Ginkgo
  `index-state read conflicts` (fast lane) and `TestFDB_IndexStateReadScope`
  (sqldriver, full lane, not run: scanned index conflicts; unused index and
  record scan commit).
- [x] Connection options (design 6.2): `EmbeddedConnection.SetOption` merges
  one option checked by Java's contracts (`api.ValidateOption`, 22023);
  `SetOptions` replaces and is what `ResetSession` restores; DSN `dry_run` and
  `isolation_level_snapshot`; an unknown DSN parameter is 22023 listing the
  accepted names; a result set reads the options captured at execution on
  every page. Connection DRY_RUN/SNAPSHOT were already merged per statement.
  Pins (fast lane): `option_contracts_test.go`, `connection_option_scope_test.go`,
  `dsn_test` (new fast-lane target). FDB pins in `dml_dry_run_fdb_test.go`
  (Raw DRY_RUN lasts one borrow, DSN dry_run persists, DDL ignores DRY_RUN) —
  full lane, not run.
- [x] IN semantics: rewrite/partition/cost behavior, covering unions, multi-binding
  product limit, and constant-IN evaluation timing; coordinate shared machinery
  with WS-F without losing either acceptance obligation.
  Closed (2026-10-07): evaluation timing, the product limit, comparand
  sources and covering in-unions (`Fetch(InUnion(COVERING))`, below under
  WS-F) are done; the one-value collapse is declared. What remains is WS-F's
  item 10 (the two-source in-union), tracked in the IN-union item below.
  In-union comparand sources done (2026-10-07): a row-independent source
  planning cannot evaluate is carried as `ArrayDistinctValue(comparand)`
  (`RecordQueryInUnionPlan.WithInComparands`, folded into the plan's
  identity as a Value) and evaluated when the plan opens, before the size
  check and the one-combination fast path, which now read the evaluated
  sizes; it used to be dropped at planning and fail at execution with "no
  planning-time values" (`TestExecuteInUnion_ComparandSourceEvaluatedAtOpen`,
  `TestInUnionPlan_ComparandIdentity`, mutation-checked). A list item that
  fails to evaluate (`1 / 0`) no longer fails translation (0AF00): it takes
  Java's `__internal_array` path and the explode carries
  `arrayDistinct(comparand)` for any evaluation failure, so it raises when
  the plan opens, even over an empty table, as Java does
  (`InComparandSourceJavaProbe`, 18 shapes; mutation-checked). Not done:
  carrying EVERY comparand source unevaluated (the design's full conversion;
  today a comparand that does evaluate at planning is still extracted there,
  as the in-join does). Measured with the same probe: literal, duplicated,
  computed and CAST items under in-union and in-join, ordered both ways,
  answer Java's rows, so the remaining conversion is not observable in rows.
  The product limit is done (F-7b item 5, after item 2 removed its blocker).
  The executor checks first (Java RecordQueryInUnionPlan.java:151-153, with a
  saturating product declared in DIVERGENCES.md), the relational maximum is
  24, and both rule arms carry it. Pins: `TestInUnionValuesSize`,
  `TestExecuteInUnion_MaxSizeBoundsProduct` and `in_union_max_size.yaml`
  (24 runs, 25 and 5 x 5 fail, 4 x 6 runs).
  The single-element collapse deletion of step (5) was measured on F-7b's tree
  and reverted. Fifteen corpus plans move. Two of them,
  `category IN ('food') ORDER BY id` and `status IN ('active') ORDER BY id`,
  go from a streaming index scan to `InMemorySort(InJoin(...))`. The design
  measured these as IN-unions, but F-7b item 2 now builds no in-union ordered
  by an id past the record-type coordinate. Java plans a scan with a filter.
  A one-value `IN (?)` ordered by the primary key would buffer its whole
  result. Before the collapse goes, either F-7c's cost model has to choose
  the streaming plan, or a one-literal in-join has to bind its value FIXED, a
  Go extension that needs an owner decision. Decided with F-7c (2026-10-06):
  the cost model keeps the sorted probes for a multi-value IN (DIVERGENCES.md
  "an IN ordered by a key no probe provides"), so the collapse stays; its
  deletion would turn the one-value form into the same sorted probe. It is
  declared (DIVERGENCES.md "a one-value IN is an equality"), and the WS-F
  row `w8_rl_default_in1_order_by_price` is `DIFF-PATH single-element-in`
  (the oracle's acceptance now reads DIFF-PATH for record-layer rows).
- [x] Semantics/pins: scalar variadic promoted-child types, Value nullability
  census, target simplification regime, adjacent/decorated literals and lexer
  boundaries, FROM-less metadata, LOG_QUERY. Decimal normalization and structured
  variadic promotion have prior implementations; check current coverage first.
  Simplification regime (design 5.4), in progress.
  - (b): the generic fold (`DefaultFolder`, `ExpressionFolder`,
    `SimplifyAll`) is deleted; it had no non-test caller.
  - (a): 17 of the 18 translator `SimplifyPredicateValues` sites are gone, so
    a SQL predicate reaches Cascades unfolded. No corpus plan moved: the
    planner's simplification rule folds the same constants.
    - The sparse-index DDL path now folds its stored predicate itself, which
      keeps the enum and UUID refusal messages.
    - The EXISTS bound-query fold is gone too (2026-10-07), with the
      scope-ambiguity decline it fed: a subquery's legs already have their
      own bindings, so an inner source reusing an outer name reads the inner
      one, Java's inner shadow. The seven declined rows answer Java's rows
      (`ExistsInnerShadowJavaProbe`, mutation-checked by restoring the
      guard). The positive-only constraint on a middle subquery with an
      outer-only conjunct beside a nested EXISTS is gone as well: the
      conjunct stays inside the existential, so NOT EXISTS and projected
      EXISTS over it answer Java's rows. The JVM probe checks seven such
      shapes over data where the conjunct holds for one outer row and not
      another; mutation-checked by restoring the constraint. The
      UNNEST-frame admission boundary and the "JOIN ON references an alias
      reused as a later inner join source" decline are gone too: an early
      ON resolves left-to-current, so a later same-name inner leg does not
      capture it (probe row `on_before_later_same_name`, Java [2]).
      A correlated conjunct in an OUTER join's ON, or in an inner ON before
      a later RIGHT join, is answered (it stays in its ON; was 0A000; nine
      probe rows, mutation-checked by lifting it). Still refused: a nested
      EXISTS inside such an ON (DIVERGENCES.md, pinned). A correlated UNION
      ALL body is answered too (each branch keeps its correlation; five
      probe rows). A parenthesised statement is the query it encloses, so the
      Java corpus files `union-empty-tables.yamsql` and `union.yamsql` pass
      (corpus 139 files, 8478 queries). Several projected EXISTS, or one
      beside a WHERE/ON EXISTS, plan: PartitionSelectRule no longer bails on
      a select holding more than one existential when one is projected, as
      Java partitions every subset; `exists-in-select.yamsql` passes (140
      files, 8486 queries; mutation-checked). Stars: a star over an
      enclosing source expands to correlated columns, and a mixed star
      under GROUP BY is expanded in place (`StarScopeJavaProbe`, 7 rows);
      a correlated star in a grouped list reads its columns as the outer
      references written by name (`StarScopeJavaProbe`
      exists_outer_star_group*); `select-a-star.yamsql` passes (150 files,
      9344 queries). A correlated HAVING inside EXISTS keeps its grouped
      body whole (`CorrelatedHavingExistsJavaProbe`, 6 rows). QUALIFY in an
      aggregated block is a HAVING conjunct over the aggregate's output, as
      Java conjoins it, and a correlated QUALIFY inside EXISTS keeps its body
      whole (same probe, 17 rows; an aggregate in QUALIFY fails in Java and
      is recorded in DIVERGENCES.md). An EXISTS in HAVING is Java's 42803 after
      the HAVING resolves (old RFC-180 Y4 "HAVING-EXISTS error-surface
      alignment"; same probe, mutation-checked); an EXISTS in an aggregated
      block's QUALIFY, which Java answers, stays 0AF00 (DIVERGENCES.md). A non-grouping
      column in an aggregated block's QUALIFY, with or without HAVING, is
      Java's 42703 (validateQualifyOverAggregate; the final-review Medium
      finding, whose premise of a 42803 check was disproved by the probe).
      IN-list typing follows Java's order (literal array identical types,
      then __internal_array promotion, then the IN's probe promotion,
      22000); a comparison is an IN item. `InComparandSourceJavaProbe` has
      42 rows, all equal; `in-predicate.yamsql` now runs 324 queries and stops
      at its run-once block (Java's cache-dependent XX000, issue #3583).
      A comma source's own JOINs follow it in the FROM's left-deep order
      (`CommaJoinJavaProbe`, 4 rows equal; a LEFT JOIN over a lateral unnest
      is a declared Go refusal); `right-deep-plan-tests.yamsql` passes
      (141 files, 8808 queries). A WITH nested in a recursive CTE body is
      declared before the seed and wraps each union leg;
      `recursive-cte.yamsql` passes (142 files, 8899 queries). The
      documentation file's nested example stops at a subquery reading a
      FROM alias as a table (`engine-gap:exists-from-alias`).
      The catalog schema is queryable: its template is built from table
      definitions as Java's (SCHEMAS/DATABASES/TEMPLATES, RecordTypeUnion);
      the CRUD metadata's wire layout is the same (version 4, keys 0/1/2, the
      three indexes at added versions 2-4). `catalog.yamsql` and
      `create-drop.yamsql` pass; so does `valid-identifiers.yamsql`, once a
      loaded template's table answers to its decoded name (GetRecordType's
      last fallback; 145 files, 9132 queries).
      An enclosing FROM alias used as a table re-reads its table or CTE
      (`StarScopeJavaProbe` from_outer_alias_*, mutation-checked). A
      recursive CTE's self-reference read inside an EXISTS of its own
      recursive leg is typed by the seed and lowered inside the recursive
      translation (`RecursiveReferenceInSubqueryJavaProbe`, 3 rows);
      with-documentation-queries.yamsql passes (149 files, 9206 queries).
      CASE_SENSITIVE_IDENTIFIERS is honoured (unquoted uids are quoted as
      written before planning, Java's normalizeString branch;
      `TestCaseSensitiveIdentifiers`); `setup-with-connection-options.yamsql`
      passes (147 files, 9139 queries). Names differing only in case are
      distinct (2026-10-07): INSERT no longer folds "COLUMN"/"column" onto one
      field, the planner's table lookups no longer re-fold a normalized
      `Table1`, a user scalar function call name is case-preserved, and the
      CREATE-time refusal of case-colliding columns is gone;
      keyword-case-insensitivity.yamsql passes (148 files, 9205 queries).
      case-sensitivity.yamsql is booked go-accepts at its case-off block
      (Go's relaxed case pass, DIVERGENCES.md).
      The translator's `existsInnerScopeCollidesOuter` guard is no longer
      reached from SQL (measured 2026-10-07: a panic on its true arm, zero
      hits over sqldriver, core, conformance corpora and probes); it stays
      as a net, with no effect on answers.
  - (d): Java's COALESCE rule. Only a NULL head (NullValue, a nil constant)
    or a BOOLEAN literal head folds; an INT/STRING literal head stays, so
    `COALESCE(1, 1/0) = 1` in a WHERE raises 22012 as in Java.
    `simplification_regime.yaml` holds the 30 Java-measured rows. With an INT
    head a colliding EXISTS reference no longer folds away; it reads the
    inner source (see (a)).
    - Known gap, part of (e): `NOT CAST(NULL AS BOOLEAN)` inside a predicate
      value is not folded. Go keeps `NOT x` as a predicate value, which the
      simplifier does not enter (`coalesce_not_cast_null_head_where` is
      excluded from the yaml with that note).
  - (c), first half: Java's ConstantFoldingRuleSet is ported as
    `constantFoldingRules` (`rule_constant_folding.go`): the default predicate
    rules, `ValuePredicateSimplificationRule` (the leaves' PREDICATE value set,
    with the ranges and multi-constraint folds) and
    `ConstantFoldingValuePredicateRule` over effective constants
    (`predicates.FoldComparisonMaybe`, Java's `foldComparisonMaybe`; Go's
    boolean ValuePredicate folds as `value = TRUE`). Both of Java's consumers
    run it: QueryPredicateSimplificationRule over the whole conjunction, and
    `foldPredicateAtNull`, which no longer runs the Go-only
    `DefaultSimplifyRules` (EvaluateConstant comparison folds, NOT over a
    constant). No corpus plan moved; sqldriver, factorycorpus and javacorpus
    pass.
  - (c), second half: the Go-only driver set is deleted: `DefaultSimplifyRules`,
    `NormalizationRules`, AndFlatten, OrFlatten, AndDedup, OrDedup,
    ComparisonConstantSimplify and ValuePredicateConstantFold, with their
    tests. The tests that used the set as an oracle (`expr` walk, resolver
    and fullstack tests, the simplify fuzzers, `simplifier_test.go`, the De
    Morgan tests) run Java's set and are re-pinned to its answers: `5 = 5`
    and `(1 + 2) > 0` stay, NOT over a constant stays, NOT over OR
    distributes. `NotConstantSimplifyRule` stays, in
    `TranslatorConstantPredicateRules`, for the translator's EXISTS fold.
  - (e): Java's CollapseNullStrictValueOverNullValueRule is in both value sets
    (`values.collapseNullStrict`, the six classes of `IsNullStrictValue`, which
    the null-on-empty substitution shares; Go's boolean NULL literal
    `BooleanValue{nil}` counts as a NULL). `CAST(NULL AS T)` is the typed
    NullValue (`ResolveCast`, Java's `CastValue.inject`), and an IN item is
    refused at plan time only when NULL-TYPED, a typed NULL when the list is
    evaluated, as before. NOT over a bare boolean value is Java's `NotValue`
    (EXPLAIN `NOT _.B`), so `COALESCE(NOT CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1)`
    answers every row as Java does (`coalesce_not_cast_null_head_where`, now in
    `simplification_regime.yaml`). NOT over a literal no longer folds
    (`NotValue` left `isFoldableComposite`): Java keeps `NOT 'false'`. The WS-E
    oracle: one row fixed, none moved the other way. The substitution keeps
    its collapse because Go cannot rebuild a FieldValue over a NullValue.
  - (j), the oracle half: the WS-E oracle asserts Go's side of the 89
    simplification rows of its v4, v5, v6 and v8 rounds (`wsE5GoPins` to
    `wsE8GoPins`, `checkGo`): every pin probed and answered exactly, and every
    row answering as the target does (`wseOutcome`: outcome class, SQLSTATE,
    rows) unless `wseGoDivergences` declares it. Eight are declared: the
    target's VerifyException and CASE-branch defects (5.4(h)) and two
    REWRITING hash ties Go's prune decides for the fold (5.4(g)). A moved pin
    was checked to redden it (full lane). Go's EXPLAIN shows `[n preds]`, so the
    surviving member's predicate (`[FALSE]` against `n > 0`) is not asserted
    there; that needs a predicate-showing EXPLAIN.
  - The `EvaluateConstant` arms of `SimplifyValue` are gone: neither value
    set evaluates a constant composite (`1 + 2`, `CAST(3 AS STRING)`,
    `UPPER('x')`, a CASE or a promotion stays for the plan to compute), and
    NOT over a literal no longer folds. The one evaluation left is
    `values.EvaluateConstantComparand` / `predicates.EvaluatePredicateComparands`,
    for the sparse-index predicate, which Java stores with
    `comparison.getComparand(null, null)` (IndexComparison.java:166); it keeps
    the enum and UUID refusal messages. Measured: no corpus plan or EXPLAIN
    moved with the arms removed. The remaining `EvaluateConstant` callers read
    comparands for plan properties (equality shapes, ordering, intermediate
    matching) or translator checks and rewrite nothing.
  - (f) closed: `effectiveConstant` already has Java's three shapes, and the
    Object overload has no Go caller (Go comparands are Values).
  - (g), the conjunct rung: `residualConjuncts` drops tautologies as Java's
    NormalizedResidualPredicateProperty does (java:81-121), so `[A, TRUE]`
    beats `[A, B]` on the count instead of tying into the hash
    (`TestRewritingCostModel_TautologyCountsNoConjunct`). Corpus golden
    unchanged; the WS-E oracle passes with no Go pin moved. The dense
    predicate-level map is WS-F's (Finding 6-followup), and the hash rung stays
    Go's `deepHash` (no `semanticHashCode` port).
  - (g)/(j) tie pins: `TestFoldTiePins_DecidingRung` (embedded, fast lane)
    plans each tie row 20 times cold over the v5 and v4 schemas and asserts
    the plan, the kept member's predicates and the deciding rung. Mutation
    runs: inverting REWRITING's `deepHash` flips the six rewriting-hash rows
    (the five div0 rows keep `NULL = 1`, the NOT row keeps `NOT (FALSE)`) and
    nothing else; inverting PLANNING's `costExprHash` flips only
    `in_cast_null_join_inner_empty` (NLJ sides swap). Both redden the test.
    `null_strict_div0_cast_null_is_null_where` and the COALESCE-under-AND row
    are decided on the conjunct count.
    Go-only: the walker collapses arithmetic over a typed NULL and folds IS
    [NOT] NULL over a NOT NULL constant (`expr.ResolveArithmetic`,
    `ResolveIsNull`). So the div0 pair is `NULL = 1` vs `UNKNOWN`, both
    answering no rows, and the hash picks the plan, not the answer; and
    `COALESCE(1 / 0, 5) IS NULL` reaches the memo already `FALSE`. Measured
    with both folds removed: the pairs become Java's, Go's hash then keeps
    the unfolded `COALESCE(1 / 0, 5) IS NULL` (22012 where the target
    answers `[]`), and six projections lose their NULL collapse (arithmetic
    yaml #14-17 and #19, simplification_regime #25), because Go has no
    DEFAULT-set simplify of result values at pull-up (Expression.java:
    243-245). The walk-time folds stay until that pull-up simplify is
    ported.
  - (j) prune scenarios: `fold_prune_regime.yaml` (round 8: T with T_N) and
    `fold_prune_unique_regime.yaml` (round 9: UNIQUE T_UN, the union leg)
    carry every round-8/9 fold row at Java's measured answer: literal
    annulments 22012 (`1 = 2` is no effective constant), the IN-list row
    22012 as declared, reductions, type annulments, type reductions, the tie
    folds and the deduplicated variants of `COALESCE(1 / 0, 5) IS NULL`
    (Go keeps the duplicates; the fold wins on the count, added to the tie
    pins). Plan-only difference: Java scans T_N `<,>` under `FILTER false`,
    Go scans T.
  - (j) negative control: `TestFoldPrune_NegativeControl` (cascades) ranks
    the original and the fold directly. REWRITING keeps the fold; PLANNING
    over the implemented plans prefers the unfolded probe for the
    primary-key, primary-key IN, UNIQUE-index (provable cardinality 1) and
    union-leg groups. So the REWRITING prune decides these folds.
  - (f)/(j) the rest: `effectiveConstant` is now exactly Java's
    `from(Value)` (a non-boolean literal takes the type arm; only an
    untyped constant changes, from NOT_NULL to UNKNOWN; corpus unchanged),
    pinned shape by shape (`TestEffectiveConstant_JavaShapes`). The COALESCE
    head classes not yet pinned (a `NOT FALSE` head never folds at a
    fixpoint, an arithmetic head is not evaluated, a `CAST(NULL)` head
    collapses and is skipped) are pinned per set
    (`TestSimplifyCoalesce_HeadClassesPerSet`). `rejectsNull` is pinned over
    13 rejecting and accepting shapes through the ported set
    (`TestRejectsNull_ShapesThroughThePortedSet`). `NOT (k = 5)` is not
    proven in either engine: children fold first, and the set has no NOT
    over a constant.
  - (h): DIVERGENCES.md "Constant-fold defects and fold ties" records the
    target's VerifyException (COALESCE under AND/NOT, beside an IN list) and
    CASE-branch defects with Go's answers, and the schema-dependent hash ties.
  - (i): no set evaluates a LIKE pattern (`TestPatternForLikeValue_NeverEvaluated`).
  - (b) census: `//pkg/recordlayer/query/plan/constfoldcensus` parses every
    non-test Go file under pkg/ (1113; the target follows MODULE.bazel's
    runfiles symlink to the real tree and is tagged `external`, never
    cached). It counts every reference to the simplification and
    constant-evaluation entry points, plus every `Evaluate(nil)` /
    `Eval(nil)`, keyed by file, function and callee. All 41 keys are
    assigned a reason (simplify, comparand, analysis, IN, LIKE, coercion,
    walk-time NULL, Go-only, harness), with a vacuity floor, a positive
    control and a matcher arm test. Found by it: the CAST arm evaluated
    `tryCastConstant` before checking the evaluate mode (result discarded);
    it now checks first.
  Done: LOG_QUERY (statement and connection) sets `PlanGenerationInfo.LogQuery`
  (`TestPlanLogging_LogQueryFlag`). Literal decoding and the decorated-literal
  refusals were already implemented and now have a fast-lane pin
  (`string_literal_tokens.yaml`); lexer comment boundaries are pinned in
  `parser/parser_test.go` `TestParse_Comments`. COALESCE/GREATEST/LEAST and CAST
  nullability already follow Java (`ScalarFunctionValue.Type`, `CastValue.Type`).
  A CAST never yields NULL from a non-NULL operand: an operand no arm converts
  is a cast error, and a RECORD cast passes an equal type through
  (`TestCastValue_NonNullOperandNeverCastsToNull`, every admitted pair).
  The per-class nullability table is `valueNullabilityCensus`
  (`values/value_nullability_census_test.go`). It enumerates every Type()
  class from the embedded sources and checks each fixed type against its entry.
  Every class Go typed NOT NULL where Java types it nullable is loosened, and no
  `censusGoStricter` entry remains. This covers AndOr, Not, Exists,
  EvaluatesTo, ToOrderedBytes, Rank, RowNumber, RecordType, Incarnation,
  ConditionSelector, Collate, Distance, the *DistanceRowNumber values and Empty.
  Java's oracle measured `not_false_select` and `true_and_true_select` as
  nullable.
  FROM-less metadata: `TestFromlessSelect_ResultMetadata` (fast lane) pins
  result types and nullability to the oracle's measured Java values for the
  same expressions. `FromlessSelectJavaProbe` compares nullability between
  the engines (ran green 2026-10-07).
  Variadic promoted-child types: COALESCE/GREATEST/LEAST promote every argument
  to the common type with its own nullability, scalars included
  (`variadic_promotion.yaml`).
  Simplification regime (design 5.4): (a)-(j) implemented and pinned (the
  sub-bullets above). Remaining, each owned elsewhere:
  - the dense predicate-level map is WS-F's (Finding 6-followup);
  - the walk-time typed-NULL arithmetic collapse and the IS NULL fold over a
    NOT NULL constant are KEPT as Go extensions (DIVERGENCES "Constant-fold
    defects and fold ties"): without them the answer of a fold tie depends on
    the hash, as Java's does per schema. Removing them would also need the
    pull-up DEFAULT-set result simplify (Expression.java:243-245), which Go
    lacks.
  Closed (2026-10-07): nothing of this item remains that is not owned by
  WS-F or kept as a declared Go extension.

**Done:** every remaining WS-E design obligation is reconciled to implementation
and an executable pin; temporal compatibility/repair documentation is shipped;
fast and full lanes plus Java/FDB acceptance pass. Then move to WS-F.

## 2. WS-F — planner scheduling, query blocks and properties

- [x] Conditional decorrelate→simplify and merge→pushdown rule chains with
  progress-driven fallback; partition-based select merge; multi-leg pushdown;
  physical REWRITING prune; full comparator configuration; per-partition yields.
  W6 step 1 rule-call observer done (2026-10-07): `Planner.SetRuleCallObserver`
  reports every rule call (phase, the inner rule's Java name, expression,
  progress); `embedded.PlanQueryForTestObservingRules` plans through it.
  Through it, Java's wrapper-name rule is ported: disabling
  `ConditionalCascadesRule` turns every conditional chain off, as Java's
  isRuleEnabled does for the wrapper (Go ignored the name)
  (`TestRuleCallObserver_SelectMergeAndPushDownChain`, mutation-checked).
  Measured with the observer: a WHERE over a UNION ALL derived table is a
  LogicalFilterExpression in Go (cascades_translator.go `exactFilter`), so
  PredicatePushDownRule never fires on it, where Java's pushdown yields the
  pushed form. The PLAN is the same in both engines: Java's REWRITING model
  counts normalized conjuncts over a LOGICAL union as an AND of its legs
  (NormalizedResidualPredicateProperty ORs only the physical
  RecordQueryUnionOnValuesPlan), so the pushed form (one conjunct per leg)
  loses to the unpushed one, which is Go's plan
  (`PredicatesFilter(UnorderedUnion(...))`). A Go filter-through-union push
  was tried and loses the same way. So §2.7's "pushdown pushes in one
  firing over UNION ALL" is a rule-call difference only, not a plan one.
  The physical REWRITING prune (F-5) is done.
  - Measured on the whole plan corpus before the change: every group crossed into
    PLANNING with exactly one final, and every child the REWRITING comparator
    descended had exactly one. The designated final was degenerate.
  - The crossing now requires exactly one final (`RewritingCrossingError`, Java's
    advancePlannerStage Verify) and no longer carries members.
  - The comparator reads each child's one final (`rewritingComparator`,
    `RewritingPruneError`).
  - `designated_final.go`, its coherence instrument, the finals generation and
    the DIVERGENCES entry are deleted.
  - Two gaps the invariant exposed are fixed. First, `ImplementDistinctFinalRule`
    and `ImplementSortRule` minted plans as exploratory members of
    canonical-stage groups; they now use Java's memoizePlan. Second, a memo
    merge into a never-explored group inherited the loser's exploration
    progress, so no rule ran on the survivor's members (the asymmetric-union
    no-plan shape). `Reference.Absorb` now keeps constraints but not progress.
  - No corpus plan moved. The full lane (sqldriver and conformance) has not run
    against the invariant.
  The conditional chains, finalization partitions and pruned-input rules were
  already in place (`d5a5132a1`). F-7a is done: the planner's and the rule
  calls' comparators rank with the whole planner context (configuration and
  metadata) without statistics too; `costModelDiagnosticsOnlyContext` is
  deleted. Two corpus plans moved, both a self-join's inner leg, from a primary
  key range with a filter to the index equality probe, as PREFER_INDEX ranks
  them (`join_optimization_probes.yaml#4`, `multi_feature_integer.yaml#3`).
  D2 (rule dependencies) is done. The 23 Java rules that declare a constraint
  (`ImmutableSet.of(REQUESTED_ORDERING | REFERENCED_FIELDS)`) declare it in Go
  (`ConstraintDependencies`), as do the three Go-only readers
  (AggregateDataAccess, ImplementLimit, PushRequestedOrderingThroughFilter).
  An undeclared rule now has Java's empty set, so a re-exploration re-queues
  only rules whose declared key changed since the group's last committed
  exploration (`shouldPushRule`).
  `TestRuleConstraintDependencies_DeclareWhatTheyRead` fails any rule method
  that reads a constraint its type does not declare. No corpus plan moved;
  tasks fall 4-24% (chains 3/4/5: 564→546, 2367→2219, 13968→12985; star
  2559→2379; right-deep hub+6 9308→7071), pins re-baselined. D1 (progress)
  and D3/D4 (conditional chains on progress) were already in place.
  D5's re-arm conversion is done (2026-10-07): the group-wide
  `lastRearmTick` is gone. A member that arrives after its group's
  exploration began (a merge's folded members and finals, an out-of-band
  `InsertReExploring`) is marked on the Reference and its next exploration
  runs every rule (Java's ExploreExpression with forceExploration); the
  group's other members re-run only rules whose declared constraints
  changed (ReExploreExpression). No corpus plan or task-count pin moved;
  sqldriver, conformance corpora and yamsql pass. Pins re-stated:
  `TestReExplorationSkipsConstraintIndependentMatching`,
  `TestConstraintIndependentExplorationRules`, and the `pending_child` mode
  of `TestUnorderedUnionFetchSchedulingRetainsFutureFetchLeg`
  (mutation-checked: no forced exploration fails them). The `outerJoinCount` review
  is closed (2026-10-06): the design kept it omitted with a PLANNING
  re-derivation of `RewriteOuterJoinRule`, and WS-J's v36 fold superseded
  that. `outerJoinCount` is the REWRITING comparator's first criterion, as
  in Java (`rewritingComparator.compare`, LEFT OUTER selects), and
  `RewriteOuterJoinRule` runs only in REWRITING (`RewritingRules`); no
  PLANNING registration remains (DIVERGENCES.md, the REWRITING cost model).
  F-8 is done (2026-10-06). ImplementTypeFilterRule is
  Java's per-partition rule. Over each stored-record partition of the inner,
  a plan the filter already covers is yielded bare and the others are grouped
  by kept types into a TypeFilterPlan over `MemoizeMemberPlansFromOther`; no
  winner is pre-selected. No corpus plan moved; tasks fell (chains 3/4/5
  546→526, 2219→2155, 12985→12775; star 2379→2355). Insert and temp-table
  insert yield per plan partition; intersection legs range over every
  ordering-satisfying stored-record member (spines pinned) in one fresh
  reference; the level union's legs range over their rolled-up partitions;
  the DFS join yields one plan per initial plan under the insert, its
  recursive leg over the rolled-up partition below the insert. None moved a
  corpus plan. Kept, in DIVERGENCES.md "Implementation rules that still
  choose a child": ImplementLimit (approved Go extension) and the NLJ, whose
  preserve-winner pick is the local choice Java's rolled-up inner partition
  makes. Per-inner-member yields were measured and reverted: 3 golden and 15
  factory plans moved, several to a fetch before the residual; per outer
  member too, the fixed-factor harness hits the task cap.
  D5 measured and reverted (2026-10-06; superseded by the forced-set
  conversion above, 2026-10-07, which marks members at their arrival
  rather than by a member count: FK-chain rule calls are unchanged, 3/4/5
  tables 411/1971/11782 before and after): forcing only the
  members past the last round's count, with a re-arm no longer re-queuing
  every rule, moved no corpus plan but RAISED tasks (4-table chain 4500→4818,
  5-table 20557→21836) and broke `TestUnorderedUnionFetchSchedulingRetainsFutureFetchLeg`
  (the union's fetch lift is lost) and the member-invalidation pins of
  `rule_error_propagation_test.go`. Go's re-arm also stands in for a CHILD's
  membership change (a parent rule over a member whose child gained a
  member must re-fire), which Java gets from bottom-up task order, not from
  forcing. The conversion needs that ordering first. The large join is not cured by D2: a 6-table FK chain takes
  134894 tasks (5 tables: 20k), at Java's own count (122839; see the
  large-join item below).
- [x] Reconcile query-block acceptance with the current translator: top-level
  Sort(Select), ORDER BY resolution against projected Values, DISTINCT ordering,
  index-DDL root handling and ordered IN. Old blocker prose in `TODO_OLD.md`
  predates later fixes; do not reimplement the already-landed single-Select port.
  Reconciled (2026-10-06), each against the current tree:
  - Top-level Sort(Select): `topLevelSort` states every top as Java's
    `generateSelect` does, an unsorted sort over the block, and now a bare
    table read as a block Select under it (`TestTranslateScan` and the
    `queryBody` helper); an ORDER BY is `sortedBlock`'s Sort over the block's
    Select with the keys pulled up, and an unprojected key adds the Select
    above the sort (`orderBySorts`).
  - ORDER BY against projected Values (old blocker b): over a join `ORDER BY
    a.id` is sort-free with the ordered outer, projected or not
    (`ambiguous_column.yaml`, golden `Map(FlatMap(outer=Scan(A), ...))`).
  - DISTINCT ordering (old blocker a): `SELECT DISTINCT category FROM t ORDER
    BY category` keeps its order (`distinct_order_by.yaml`, golden).
  - Index-DDL root: `ddl.checkTop` is `DdlVisitor.java:274`'s `viewPlan
    instanceof LogicalSortExpression` (`index_ddl_resolver_arms_test.go`).
  - Ordered IN (old blocker c): the ordered in-join is DIVERGENCES.md's
    RFC-191 entry, and IN ordered by an unprobed key is the in-memory-sort
    entry; both declared in the WS-F oracle.
- [x] Explicit raw KEY/VALUE readers, ordered-bytes evaluation, extraction trie,
  covering reader/Value plan, aggregate cardinality/distinctness/entry readers
  (`c1ad5a87d` through `d819eb25e`). Plan transport remains outside this closure.
- [x] IN-union product limit/size, null-safe singleton candidates, zero-based
  EXPLODE ordinality/distinctness, subscript typing/errors, display-only EXPLAIN
  decoding, ordered Value folding, vector-preference applicability pins.
  Vector-preference criterion pins done: Java's PlanningCostModelVectorEngineTest
  cases against `compareVectorIndexEnginePreference`
  (`TestVectorEnginePreference_JavaCases`, mutation-checked: counting several
  accesses as one fails it). The yamsql fixtures are done too:
  `TestFDB_VectorIndexEnginePreference` runs Java's
  vector-engine-preference.yamsql fixture set (both-engine tables, union
  legs switching engine, GUARDIANNONLY, HNSWONLY, MIXEDMETRICS per metric,
  the non-vector covering scan) and asserts Java's EXPLAIN index names and
  rows (mutation-checked: the criterion disabled fails three cases).
  Null-safe singleton candidates (W9, design section 5) are done.
  NOT_DISTINCT_FROM binds index and PK scans: a NULL operand reads the null
  key, and the literal may be on either side. IS DISTINCT FROM stays residual,
  and a sparse IS NOT NULL index serves only a non-NULL literal. EXPLAIN shows
  the bound as `≡`. Pins: `null_safe_equality_scan.yaml`, which includes the
  #4598 keyset shape with bound parameters. The seven w9 oracle rows' Go pins
  are predicted and moved out of `wsfOpenUntil` (full lane, not run). W9 item 4,
  the range builder's `isCompileTime`, is done: `comparisonIsCompileTime` /
  `rangeMatchableValue` port RangeConstraints.isCompileTime and
  IndexComparison.isSupported, pinned over every comparison type and
  comparand kind (`TestComparisonIsCompileTime_JavaTable`, mutation-checked). The table-function form of the keyset
  query needs WS-E's simplification regime.
  Zero-based EXPLODE ordinality and distinctness (W12, F-2) are done. Both
  Explode classes take Java's zero-based flag through checked constructors and
  every rebuild; it is in equality, and in the hash only when set. The executor
  numbers the whole list from 0 or 1 before resume and skip/limit, and an
  ordinality explode reports distinct records (one- or zero-based). Pins:
  `TestExecuteExplode_ZeroBasedOrdinality`, `TestExplodePlan_ZeroBasedOrdinality`,
  `TestExplode_ZeroBasedOrdinality`, `TestExplodePlan_DistinctRecordsIffWithOrdinality`
  and `unnest_at_distinct.yaml`. No corpus plan moved, since Go's FlatMap claims
  no distinctness, so a DISTINCT above an `AT` unnest stays. Subscript
  typing/errors were already ported, and the oracle pins them (`w13_subscript_*`).
  F-1 leftovers. `w10_enum_not_distinct_explain` reaches its target with W9; its
  Go pin is updated. `w10_enum_distinct_explain` is the IS DISTINCT FROM covering
  scan Java picks under PREFER_INDEX, so it is reassigned to F-7c, like
  `w9_distinct_explain`. `w13_display_scan_explain` (a dotted escaped table got
  no PK scan) is at SAME-PATH with RFC-238 §7c, ACKed on 2026-10-05 and done:
  the scan leaf and the INSERT, UPDATE and DELETE targets carry the storage
  name, UPDATE carries its correlation separately (`WithTargetAlias`),
  `AddGeneratedIndex` matches by storage name, and EXPLAIN decodes the
  record-type name as Java 4.14 does (#4437). Escaped tables now get PK ranges,
  secondary indexes and aggregate indexes. The same pass fixed a regression
  from WS-E 5.4(a): with the translator no longer folding `CAST(3 AS BIGINT)`,
  range enclosure saw a CastValue and refused the sparse index
  (`w9_sparse_not_distinct_cast_value_explain`). `compileTimeComparand` now
  evaluates a row-free, binding-free comparand, as Java's
  `CompilableRange.compile` does.
  F-7b (design 4.3), the IN-list plans. Item 2 is done.
  - An index plan's rich ordering marks the primary-key keys it reaches only past
    the record-type coordinate (`WithPastRecordTypeHorizon`).
  - Only the in-union rule reads the mark. It builds no merge for a request
    naming a marked key, but a marked key may still be a free suffix.
  - Sort elision is unchanged, and so is the `col1 = ? ORDER BY id` index read
    (DIVERGENCES.md "an index read past the record-type coordinate").
  - Ten corpus plans moved off `InUnion`, nine to `InMemorySort(InJoin)` and
    one to a sorted range scan: every IN ordered by the primary key over an
    index without `id`. The simfdb golden rows are unchanged. Four FDB tests in
    `sqldriver_test` now name `id` in the index key to keep their merge; they
    are full lane and have not run.
  - The oracle's `w8_in_union`, `w8_in25_order_by_id` and `w8_tie_in_order_by_id`
    explain rows have new Go pins and move to F-7c, whose cost model decides
    between Go's sorted InJoin and Java's `SCAN | FILTER`. The sorted InJoin
    buffers the whole result where the in-union streamed, so F-7c and the
    stress re-measure must check large results (the stress IN-list query
    returns 46 rows).
  Item 5, the size check, is done (see WS-E "IN semantics"). The record-layer
  oracle row `w8_rl_default_in2_order_by_pk` now fails at size 0 as Java's
  does, but it stays open on item 3's fetch placement. The SQL in-union
  benchmarks stop at N=24. Items 4 and 6 are done. The unordered in-union
  arm is deleted (one corpus plan, an unordered IN, became Java's in-join).
  The size-less constructors are gone: `NewRecordQueryInUnionPlan` takes the
  size, and the rule fails a call without a planner context.
  Item 3 is done.
  - The rule memoizes each ordering partition whole
    (`MemoizeFinalExpressionsFromOther`, which carries the inner reference's
    requested orderings). The single-member pin is gone.
  - Extraction checks every in-union's child against its comparison keys
    (`InUnionChildOrderingError` as a planner invariant violation;
    `TestCheckInUnionChildOrdering`).
  - The set-operation push through a fetch translated comparison keys over a
    fresh placeholder alias, so a keyed set operation never pushed. It now
    uses the keys' own `_current` root, as Java rebases them.
  - Result: `w8_explicit_id_in_order_by_id_explain` is Java's
    `Fetch(InUnion(COVERING))` and leaves `wsfOpenUntil`. With the in-join
    disabled, `ORDER BY col1` is the same shape
    (`TestInUnion_MemoizesThePartitionWhole`). No corpus plan moved.
  - The record-layer `w8_rl_default_in2_order_by_pk` is now
    `Fetch(InUnion(COVERING))` too, and it leaves `wsfOpenUntil`. The oracle's
    Go index definition now states the primary key's entry positions, as
    embedded's metadataIndexDef does; without them the covering record
    omitted `order_id`. Predicted from the planner alone; the full lane has
    not run.
  Item 9's matcher is already Java's: `InComparisonToExplodeRule.onSelect`
  rewrites a select holding the IN, and the `w8_rl` in-joins plan. The
  one-value row `w8_rl_default_in1_order_by_price` is Go's single-element
  collapse, declared `DIFF-PATH single-element-in` (WS-E section 4).
  Item 10 (the two-source in-union, WS-E section 4) is in place: measured
  2026-10-07, `a IN (7, 8) AND d IN (9, 3) ORDER BY v` over an (a, d, v)
  index plans `InUnion(IndexScan(IAD, [=, =, *] COVERING), bindings=2)`, as
  Java's two-source in-union. Ordered by the IN columns themselves Go keeps
  the sorted in-joins (`w8_in5x5`/`w8_in4x6`, DIFF-PATH rfc-191, declared).
  The DESC
  tie row is resolved: `w8_tie_in_order_by_col1_desc_explain` and its ASC
  twin are SAME-PATH (the sorted IN-join over I5, as Java).
  Item 10 measured (2026-10-06), both variants reverted. Go's WHERE carrier
  (the filter arm of InComparisonToExplodeRule) explodes one IN per firing,
  so two lists nest two one-source in-unions; ImplementInUnionRule itself
  already takes every explode of a select. (1) Exploding every IN into one
  predicate-free select builds Java's two-source in-union (`InUnion(...,
  bindings=2)` with the in-join disabled) and moves no golden plan, but loses
  Go's in-join over one list with the other as a residual: `a IN (7, 8) AND
  d IN (9, 3)` falls from `InJoin(PredicatesFilter(IndexScan(IDX_A, [=])))`
  to a full IDX_A read with both INs residual (a factory scenario drifted).
  The split rule skips a predicate-free select, and PartitionSelectRule's
  partitions are not the in-join rule's shape. (2) Java's form, the
  equalities in the select itself, plans `InJoin(FlatMap(IndexScan,
  Map(Filter(Explode))))` at five times the tasks. Java plans that query as
  nested FLATMAPs over both explodes with an intersection of the two index
  probes. The nested in-unions are reachable only with the in-join disabled
  (the 5x5 and 4x6 rows are DIFF-PATH rfc-191), so the port waits on Go's
  in-join rules accepting a select with several explodes.
  F-7c LANDED (2026-10-06). The Go-only pruning in `abstract_data_access_rule.go`
  (a full index scan with no search argument and no requested ordering was
  dropped) is deleted: a PRESERVE request is satisfied by every scan, as in
  Java, and PREFER_INDEX ranks the full index scan against the primary scan.
  - 64 corpus plans move, none regressed. Classified against Java's plan for
    the same SQL (scratch oracle over every moved query): every query Java
    plans with a data access is a full index read in Java (`ISCAN`/`COVERING
    <,>`), none `SCAN`, and Go now reads the same index (26 rows); 33 rows Java
    cannot run (DATE, LIMIT, UnableToPlan); 5 are pre-existing sargability gaps
    (NaN IN probes, the DESC index) that move to the same index as Java. The
    WS-F rows `w8_covering_all`, `_neq`, `_id_neq`, `w8_prefer_index_neq`,
    `w9_distinct` and `w10_enum_distinct` reach SAME-PATH and leave
    `wsfOpenUntil`; the WS-E v8 type-annulment EXPLAINs reach Java's `COVERING(T_N
    <,>) | FILTER false`; Java's corpus file `versions-tests.yamsql` now passes
    (javacorpus 130 pass).
  - With it: the PLANNING residual rung is Java's NormalizedResidualPredicate-
    Property (an ordered union on values ORs its legs' residuals, tautologies
    dropped), so `(a=1 OR b=2) AND (c=10 OR c=20)` plans Java's union of the two
    index probes (`TestPlanHarness_UnionWithFixedUnindexedFactor` re-pinned to
    Java's measured plan; the old pin's "one access over two" claim was wrong).
  - Two latent defects it reached, fixed: PartitionSelectRule's positional
    merge typed a de-null-on-emptied leg's slot NOT NULL under a group typed
    nullable (memo admission error 64, planning failed), and the executor
    refused a NOT NULL row under the leg's nullable carrier. The slot keeps the
    stated nullable row and the layout attach admits exactly that widening
    (`TestLeftJoinLegUnderInPlans`,
    `TestAttachOrdinalLayout_NotNullRowTakesNullableCarrier`). The factory full
    corpus (8147 scenarios) passes: it failed 21 at the parent commit.
  - Plan pins re-pinned to the new (Java) shapes; the DISTINCT-elision
    (R2/R3) and proof-stamp FDB tests now pin a primary-key range so their
    base-record-scan premise holds (bare, the unique index is read whole in
    key order and dedups streaming). `TestPlanHarness_FixedFactorUnionJavaComparable`
    unordered now stops at the task cap: Java does not plan that statement
    either (StackOverflowError, `FixedFactorUnionScalarJava`).
  - Still open: Java skips a match that satisfies none of the requested
    orderings; Go does not (alone it moves 336 plans, a join's inner probe
    among them: Go's request sets differ from Java's). Re-measured after the
    leaf climb (2026-10-06), reverted: under ORDER BY a join leg is asked for
    a concrete ordering no probe provides (Java cannot plan those queries),
    and the skip drops the leg's probes, so yamsql join scenarios degrade to
    scans. It needs Go's in-memory sort to request PRESERVE below it too.
    Third measurement (2026-10-08), reverted: the skip plus PRESERVE beside
    the keys in all three sort pushes and the group-by push (skip only when
    a request exists; Go's correlated join-inner probes are planned with
    none) keeps every sorted query's probes: 2 corpus plans move, both OR
    unions becoming MergeSortUnion (one regains COVERING legs). But the
    distinct-union rule then enumerates an ordered union per request, and
    {keys, PRESERVE} under a sort exhausts the 150000-task budget (54F02) on
    three factory OR scenarios (fc_0001884206) and
    TestPlanHarness_FixedFactorUnionJavaComparable/ordered; the metamorphic
    ordering test and factory determinism fail too. DONE (2026-10-08) by that
    route: a RequestedOrdering carries a Go-only `sortable` mark, set by the
    three sort pushes and the group-by push and carried through push-down,
    rebase, pull-up and Exhaustive (Combine keeps a sortable request beside
    an unsortable one). PrepareMatchesAndCompensations skips a match that
    satisfies no request unless a request is sortable or there is none
    (Go's correlated join-inner probes are planned without one). No corpus
    plan moves; without the mark 182 do. Pin:
    `TestPrepareMatchesAndCompensations_SkipsAMatchSatisfyingNoRequest`
    (both arms mutation-checked). Rows still open in
    `wsfOpenUntil` as F-7c follow-ups: none. The IN-join versus filtered scan
    rows (`w8_in25`, `w8_in_union`, `w8_tie_in`) are declared `DIFF-PATH
    in-memory-sort` (DIVERGENCES.md "an IN ordered by a key no probe
    provides"): Java's only plan for them is the ordered scan, because it has
    no in-memory sort. The 1M stress comparison the design requires with
    F-7c ran on 2026-10-06 (section 2's last item): equal rows, no regression.
  - Follow-ups closed or re-diagnosed (2026-10-06):
    - `w8_no_predicate` was no rank question: a bare `SELECT * FROM T1`
      reached the planner as `Sort(Scan)`, with no Select for the data-access
      rules to match, so no index read was ever built. `topLevelSort` now
      states it as Java's generateSelect does, a block Select returning the
      read's row, and Go reads `IndexScan(I1, [*])`, Java's `ISCAN(I1 <,>)`.
      No corpus plan moved; the row leaves `wsfOpenUntil`.
    - `w6_left_join_indexed` DONE (2026-10-06), and the diagnosis above was
      wrong: measured on the JVM (`planRuleTrace`), Java's preserved leg is a
      bare LogicalTypeFilter with no Select, and WithPrimaryKeyDataAccessRule
      gives it `ISCAN(I1 <,>)` because the leaf match climbs through the
      candidate's Select to its MatchableSort (`SelectExpression.adjustMatch`).
      Go's AdjustMatchRule refused the climb (it required zero node-local
      correlations); it now runs Java's subset check (node-local correlations
      minus own quantifiers and placeholder parameters within the child's).
      The row is SAME-PATH: `FlatMap(outer=IndexScan(I1, [*]),
      inner=DefaultOnEmpty(IndexScan(I6, [=])))`
      (`TestAdjustMatches_LeafMatchClimbsToTheCandidateRoot`). Bare join legs
      and aggregation inputs now read the full index scan Java reads (oracle-
      checked: MAX/SUM/COUNT and GROUP BY stream off `ISCAN(I <,>)`, DELETE
      reads `ISCAN | DELETE`); a GROUP BY over an indexed column streams with
      no sort. With it, an existential FlatMap plans one outer per ordering
      requested of the outer group (Java's per-ordering roll-up), else
      `EXISTS ... ORDER BY id` sorted a cheaper full index scan instead of
      using the PK-ordered outer. javacorpus `array-agg-documentation-
      queries.yamsql` now runs its scan-choice row (class retired); 3125
      factory headers re-blessed, 6 duplicate points retired (RETIREMENT_
      LEDGER.md; census: 0 equality-probe losses). Left: `secondary_index_
      pushdown.yaml#69` (`v NOT BETWEEN .. ORDER BY id`) lost COVERING on its
      union legs, a fetch per entry where the legs were covering; Java plans
      the ordered SCAN | FILTER there (in-memory-sort divergence). Follow-up:
      with the climb working, the Go-only OrderedIndexScanRule /
      OrderedPrimaryScanRule can be measured for retirement. DONE
      (2026-10-08): both are deleted, with the nested-loop join's private
      re-fire of them on a bare source group (orderedFullScanAlternatives;
      the join's own data-access realization over the leg's partial matches
      already yields those scans). No corpus plan moved; fast lane,
      sqldriver, conformance, factory full corpus and RFC-257 oracle/parity
      pass.
    - `w8_or_two_indexes` is done: Go built the ordered union only over two
      fetching index scans, because the merge-distinct identity proof refused
      a `Fetch(COVERING)` leg on the stale premise that Go's Fetch passes its
      child through (it loads the stored record by primary key since
      RFC-220). A covering scan now proves its index's identity under a
      primary-key Fetch (through row-selecting operators only), so the merge
      pushes below the fetch: `Fetch(MergeSortUnion(COVERING, COVERING))`,
      Java's `COVERING ∪ COVERING COMPARE BY (_.ID) | FETCH`
      (`TestMergeDistinctStoredRecordIdentity_CoveringOnlyUnderAFetch`). Five
      corpus plans move to covering merges that fetch nothing
      (`index_range_*`, `null_safe_equality_scan#15/16`), two factory
      scenarios filter on index entries before fetching; the OR-union FDB
      tests accept the merge as the primary-key dedup below the fetch.
    - The depth rungs now read an absent operator as Java's
      `ExpressionDepthProperty` does (Integer.MAX_VALUE, deepest): Go skipped
      the rung whenever one side lacked the operator. Only the distinct rung
      can differ in practice. One factory scenario moved (3 projections, re-
      blessed), to the plan Java measures for it: `(NOT e >= 4.0 AND s IS
      NOT NULL) OR b IN (5, 5)` merges two IDX_S reads by (S, ID) instead of
      deduplicating an IDX_S range and an IDX_B probe by primary key
      (`TestPlanningCostModel_DistinctDepthRanksAbsentAsDeepest`, mutation-
      checked). ImplementDistinctUnionRule's legs now range over every
      member that can feed the merge, spines pinned, not the cheapest one
      (F-8; no corpus plan moved;
      `TestImplementDistinctUnionRule_LegKeepsEveryMergeableMember`).
  - Measured after F-7c (2026-10-06), both reverted: deleting the
    single-element IN collapse (WS-E step 5) moves 13 plans, and the ORDER BY
    rows still go from a streaming probe to `InMemorySort(InJoin(...))`;
    ranking the in-memory-sort count FIRST (emulating Java, which has no
    in-memory sort and so plans an order-providing plan wherever one exists)
    flips 93 plans, several clearly worse (`cte.yaml#20` runs a streaming
    aggregate per row of a full scan; `comma_join_exists.yaml#2` reverses the
    driving table). The IN-join-versus-filtered-scan follow-up needs a narrower
    rule than "sort-free first": Java's plans for these rows are order-
    providing scans because Java cannot sort, not because its cost model
    prefers them.
  History, from the measurement before it landed:
  The fast lane with the pruning off failed 22 tests and exposed two defects
  the pruning had masked, both of which blocked F-7c:
  - Wrong rows, FIXED. A unique index filtered by a Go closure (opaque
    filter) was served as a full index scan, as if it held every record:
    `TestClosureSparseUniqueIndex_IsNeverAnEliminationProof` planned
    `Distinct(Map(IndexScan(CS_EMAIL, [*] COVERING)))`. The defect was live
    without F-7c: an ORDER BY, a range or an equality probe read the index.
    The candidate now refuses to produce any scan when it has an opaque
    filter (`canProduceScanPlan`). Pin:
    `TestClosureSparseIndex_ServesNoQuery`, with a full-index control.
  - The task budget. `TestPlanHarness_FixedFactorUnionJavaComparable/#00`
    (ORDER BY b, id) hits MaxTasks (150,000 tasks, 8.3 s). Its distinct-union
    group grows to 2998 members, 1980 of them MergeSortUnion plans. This is
    the OR-union budget the pruning comment named. Measured cause:
    `ImplementDistinctUnionRule` fires once per union alternative, of which the
    DNF expansion holds about 500. It fires under PRESERVE over legs of 3 to 11
    disjuncts with 19 to 67 ordering partitions in all, and every firing
    reaches 4 merge states. With the pruning on, a leg whose disjunct binds no
    index has only the primary scan, and fewer states result. Each state yields
    a pinned MergeSortUnion, so about 500 x 4 merges reach the group. Java has
    no task cap here. A fix has to bound the merges built per union group, not
    per alternative.
  - `TestPlanHarness_UnionWithFixedUnindexedFactor` is a plan pin: the
    two-residual tie now breaks toward a full covering scan, not the primary
    scan.
  The rest are plan pins encoding the pruning: like-prefix and unindexed-IN
  full scans, ORDER BY elimination, the rfc202 generated index plans,
  order_by_nulls, the self-join probe, and the javacorpus run.
- [x] Reconcile F-6/F-7b with RFC-191's existing `Fetch(InJoin)` ruling; see
  `DIVERGENCES.md` “Plan choice: an ordered IN over a non-covering index”.
  Go's covering emission is already Java's gate: `ToScanPlan` is
  `toEquivalentPlan`. The `w8_in_*` rows differ only by RFC-191's push of a
  comparand in-join through its fetch, so they are declared `DIFF-PATH rfc-191`.
  The 25-value and 5 x 5 rows are declared `DIFF`, because Java's IN-union
  fails and Go's in-join answers. They leave `wsfOpenUntil` (full lane, not
  run).
- [x] RANK-index match-candidate gap: verified, no reach. Java plans a RANK
  index only for a record-layer `RecordQuery` with a rank comparison
  (`QueryRecordFunctionWithComparison` builds the `RankValue` that
  `WindowedIndexExpansionVisitor`'s candidate matches); fdb-relational SQL
  can neither declare a RANK index nor build a `RankValue`, and Go has no
  declarative RecordQuery API (its record layer exposes BY_RANK scans and the
  `rank` record function directly). The candidate is ported with a RecordQuery
  API, if one is ever ported: it then also needs Java's windowed expansion
  (Go's expands the columns as a value index) and a BY_RANK scan type on
  `RecordQueryIndexPlan`, which today always scans BY_VALUE. Both dotted
  identifier gaps of `embedded/dotted_identifier_gap_test.go` are closed: the
  primary-key ORDER BY (the escaped-column item below) and the aliased GROUP BY
  (`t."foo.tableA.A2"` was 42703: the GROUP BY check peeled the alias and asked
  only whether the root segment is a field; `dotted_column_group_by.yaml`).
- [x] Escaped COLUMN names (`"c$1"`, stored `c__1`) plan as their unescaped
  twins. Key expressions carry stored names and the planner's row layouts
  decoded ones (`FieldNameForProtoField`), so names are decoded once where
  metadata enters the planner, as Java's KeyExpressionExpansionVisitor and
  ScalarTranslationVisitor read `toUserIdentifier`: `embedded.layoutNames`
  (index columns, value columns, vector and aggregate names and paths),
  `coveredPrimaryKeyColumns`, `TranslatePrimaryKeyToValues` (was
  `strings.ToUpper`), and in cascades `resolveKeyFieldPath`/`layoutFieldPath`
  and the key-expression name comparisons. `storedFieldName` re-encodes where a
  root is spelled back from names. ON-source DDL resolves the SQL name against
  the decoded layout. Thirteen shapes measured against their twins (index and
  PK ordering, IN, DISTINCT, SUM index, intersection, ON-source) now match;
  `escaped_column_index.yaml` pins them with rows.
- [x] Close the large-join memo planning-cost regression introduced by
  `54fcf78f0`. Preserve Java's block-Select architecture. Investigate a cheap
  negative filter or fewer sibling alternatives using Java's PartitionSelectRule
  and Reference.insert. Cross-batch reference-comparison caching was rejected for
  excessive live heap, not left as a recommended fix. Exact historical measurements
  and SHAs are in the archive's RFC-257 completion ledger.
  Closed as Java parity (2026-10-06). Java avoids neither cost: its
  Reference.insert scans every member with `isMemoizedExpression`
  (Reference.java:996-1018, deep `findMatches` + `containsAllInMemo`), and
  Go's relational switches (right-deep, deferred cross products) are Java's.
  Measured with the target's `planRuleTraceOutcome` TASK-COUNT (a scratch
  conformance probe, not kept) against Go's `plan-trace`, `SELECT t1.id FROM
  t1..tn WHERE ti.next_id = ti+1.id`:
  4 tables Java 6731 / Go 3652 tasks; 5 tables 26722 / 20092; 6 tables
  122839 tasks in 77.7 s / 134894 in 23.5 s; a 6-way self-join `o_i.id =
  o_i+1.id` Java 118219 in 54 s; at 7 tables both shapes take Java over two
  minutes. Go's 7-way self-join stops at its 150k cap in ~45 s
  (`TestPlannerCapHit_ProductionSelectPathSQLSTATE`). Two filters were
  measured on it and reverted, neither moving the time: Java's same-reference
  shortcut restricted to the group's free aliases, and pruning the quantifier
  bijection by the aliases' result-value/predicate components (3.76M
  node-equal pairs, 3.73M negative; the cost is the pair count, ~10 us each,
  ~40% GC).
  Launch finding "6-table join plans in 5.6 s with no error" (2026-10-10). On
  the probe's 6-table `ti.next_id = ti+1.id` chain: Java 4.14.2.0 122839 tasks
  / 77.7 s, Go then 134894 / 23.5 s, Go now 134903 / ~8 s (`plan-trace`, load
  ~32). On planner_budget_test's 6-table star-schema chain Go plans 110701
  tasks in ~6 s. Go explores a subset of Java's space: all of Java's
  PartitionSelectRule prunes plus the unconnected-lower-side prune
  (rule_partition_select.go:531-539); task granularity differs, so the counts
  compare only roughly. No error is right: Java's budgets default to unbounded
  and its SQL layer sets none; Go matches, with opt-in MAX_TOTAL_TASK_COUNT /
  MAX_TASK_QUEUE_SIZE / MAX_NUM_MATCHES_PER_RULE_CALL caps raising 54F02.
  On the star chain the target takes 541965 tasks / 12.5 min (17 under load);
  `//conformance:conformance_planning_cost_test` ("Six-table chain planning
  cost", manual) pins the target's counts for both chains.
- [x] Re-measure planner/executor stress against the actual merge-base with
  explicit SHAs and equal row populations; resolve regressions, not just timeouts.
  2026-10-06, `c116de63b` against the recorded merge-base `e48f5b49` (2 samples
  each, the baseline's command: `TestFDB_Stress_1M$`, `--nocache_test_results`).
  Both samples pass (167.5 s, 167.9 s); every one of the 22 queries returns the
  merge-base's row count; median ratio cur/base 0.25-1.29, the largest
  increases 2-3 ms absolute on single-digit-ms lookups (needle, ORDER BY PK +
  index filter), within run noise. Faster: GROUP BY customer HAVING 641 -> 209
  ms, ORDER BY PK (full) 5.8 -> 4.3 s, full scan filter 831 -> 590 ms. Load
  average 41.9 falling to 3.6 across the runs (the baseline's was 5.6-5.8).

## 3. WS-D — vector engines and maintenance

- [x] GuardiANN safety: zero-candidate admission; n<k/peel/unsplittable split
  fallbacks; empty-core repair; primary-preferred cleanup; underreplication
  deltas; committed negative-count disable. Checked decoding, task poisoning,
  KMeans preconditions and merge/drain target checks have prior fixes.
  Committed negative-count disable is done: the disable is Java's keyed commit
  check, and no heartbeat check shares the merge transaction (Java's core
  merges run no pre-commit callback), so it commits, and counts
  `vector_index_disabled_on_negative_task_count`
  (`guardiann_negative_count_test.go`). Zero-candidate admission is done:
  `insertMaxCandidateClusters < 1` refuses the insert (engine and queue
  enqueue) with a poisoning `VectorCapabilityError`, declared (b)
  (`guardiann_insert_admission_test.go`). Knob consumers are done: the
  declared (c) livelocks (split/merge/reassign neighbour fetch of width or
  pipeline below 1) raise `VectorCapabilityError` where Java writes the
  empty-list task back; split-merge/reassign/collapse/bounce/delete
  concurrency below 1 fails with forEach's IllegalArgumentException at Java's
  statement (`guardiann_knob_consumers_test.go`). The KMeans knobs had prior
  fixes. Split fallbacks and empty-core repair are done, declared (h)
  (DIVERGENCES "GuardiANN splits what Java cannot",
  `guardiann_unsplittable_test.go`, each fixture mutation-checked). They cover
  the n<k INVALID rule, the admitted outlier peel with its geometric floor and
  admission bound, the terminal reconcile with `ClusterUnsplittableError`
  above the hard cap, the zero-primary new-child drop, and the empty merge
  core. Timing and acceptance fixtures done (2026-10-07):
  `guardiann_peel_timing_test` (full lane) times, in process CPU, the whole
  peel and its worst case (every k-means iteration of every refit, each
  round's sort, assignment and score on all n) at W = B, n = 1001 d = 2175 and
  the fixture shapes, and at W = B under cosine, with RaBitQ and at
  I = 1, R = 31: max 1.25 s against the 2.5 s margin (2026-10-10). The k-means
  early stop had hidden a 2.66 s Euclidean worst case; cosine, RaBitQ (14 s)
  and the knobs were untimed, and the admission's knob factor now counts each
  restart's seeding and final passes (I = 1, R = 31 admits d = 551 at
  n = 2000, not 980) and prices n and d at no less than 128 each (at I = 1,
  n = 2000 admits at most R = 136 and n = 128 R = 3587).
  `guardiann_peel_fixtures_test`
  (full lane, real FDB, HALF, no RaBitQ) splits the d = 768 n = 2000 cluster
  by a deferred drain and by an inline insert and the d = 2048 n = 1001
  cluster by a drain, every transaction in one attempt (attempt observer),
  and refuses the peel at I = 16 (cluster reconciled whole). Mutation-checked:
  admission off fails the three splits; dropping the knob factor fails I = 16.
  Not built: the d = 4096 reconcile fixtures (16 MB of references per read),
  covered by the admission unit table.
  Primary-preferred cleanup and underreplication deltas are done, declared (h)
  (DIVERGENCES "GuardiANN keeps primaries and underreplication counts exact",
  `guardiann_counts_test.go`, mutation-checked). Declared (d) is done:
  `consumerOutcome` (CONSUMED / RUNS / REFUSED / REFUSED_UNLESS_ALL_NK) over
  each kind's prologue, and an inline delete skips a refused head task
  (DIVERGENCES "GuardiANN inline deletes skip a head task Go would refuse",
  `guardiann_consumer_outcome_test.go`, mutation-checked). The (d) race
  fixtures are done (2026-10-07, `guardiann_consumer_outcome_test.go`): a
  skipping delete commits over a concurrent drain of the task it skipped; a
  false-alarm clear fails with 1020 after a concurrent insert into its
  cluster; a non-skipping delete fails with 1020 after a concurrent
  higher-priority enqueue. Each is mutation-checked (a serializable skip read,
  a snapshot prologue, a snapshot queue read). The bounce JVM row is done
  ("bounce re-enqueue and follow-up ids over a split among a grid of clusters",
  `rfc257_guardiann_test`): bounces with up to six outstanding dependents
  re-enqueue byte-equal. It found an HNSW divergence, now fixed: a delete
  repair that re-inserts a neighbour already in a list now moves it to the
  end and rewrites the node, as Java's InsertNeighborsChangeSet.merge does.
  Go had kept it in place (mutation-checked). Bits-9 quantizer refusal done
  (2026-10-07): the codec defers the RaBitQ construction error
  (`requireQuantizer`) to Java's construction points (an insert of a new key,
  a search, a task write but a bounce's, a split/merge/reassign/collapse body
  past its no-op exits, every vector-reference write), so a delete that
  enqueues nothing and an empty drain run, as in Java. The JVM-row fixture
  ("RaBitQ with 9 extra bits", `rfc257_guardiann_test`) compares steps and
  bytes over a refused task body and a refused merge write; mutation-checked
  (an unchecked task write fails it). The search refusal is pinned too: the
  script probe has a search op, and the bits-9 script searches once
  untrained (both engines answer the same keys) and once trained (both
  refuse); an unchecked search fails it.
- [ ] `guardiann_peel_timing_test` exceeded its CPU margin under load: 2.59 s against 2.5 s
  in a `just test-full` run at load average 58 on the 24-thread dev box (2026-10-24, PR #998);
  the rerun at load 27 measured 1.67 s. Process CPU time should not grow with load, so find
  what does (GC or scheduler contention counted as CPU) before touching the margin.
- [x] HNSW/engine: general fetch/cardinality/layer scans, ordered retrieval,
  covering/rank results, search-free continuation replay, operation-local caches,
  partition locks, cosine zero/clamp, sample-UUID closure, option catalog/identity.
  efSearch defaults and bounded-beam use already have fixes. Cosine done
  (2026-10-06): the HNSW/GuardiANN cosine is Java's CosineMetric (zero vector
  +Inf, non-finite NaN, no clamp) and HNSW orders distances by Double.compare
  (`hnsw_cosine_java_test.go`, mutation-checked); SPFresh keeps its own
  clamped cosine (`spfreshVectorDistance`), as the design requires.
  Operation-local caches done (2026-10-06): the maintainer no longer caches
  HNSW storage across operations; each insert/delete/search gets its own
  parsed-node cache, as Java's. The shared cache let a snapshot search's
  nodes serve a later insert in the same transaction, which then committed
  without the read conflicts it owed
  (`hnsw_operation_local_cache_fdb_test.go`, mutation-checked). Partition
  locks done: the executor's scan read-locked nothing and SearchKNN the whole
  index subspace, so neither excluded a partition writer; both now read-lock
  the partition subspace for the search (`vector_partition_lock_fdb_test.go`,
  each path mutation-checked; hold span declared in DIVERGENCES). The option
  catalog (aliases, alias conflicts, Java parsing) was already ported
  (`hnsw_options.go`); a plan carries only Java's two SQL vector options
  (RowNumberValue.SUPPORTED_OPTIONS: ef_search, return_vectors), both in the
  continuation salt. Sample-UUID closure done: HNSW SAMPLES keys are
  `(count, tuple.UUID v4)`, Java's StorageHelpers.appendSampledVector (was a
  16-byte string element); a byte-string entry is still consumed
  (vector_index_test.go "writes a SAMPLES key", mutation-checked). GuardiANN
  already wrote v4 tuple UUIDs. Search-free continuation replay done: a
  resumed single-partition scan searched (and now would have locked) before
  replaying its continuation's page; it replays with no search and no lock,
  as Java's scanSinglePartition (the multi-partition cursor already did;
  vector_partition_lock_fdb_test.go, mutation-checked). Fetch, cardinality
  and orderByDistance exist on GuardiANN's centroid HNSW
  (guardiann_centroids.go); its comparator now orders by Double.compare then
  key (`TestCentroidNodeOrder_IsDoubleCompareThenKey`, mutation-checked).
  Covering/rank results and ordered retrieval checked against 4.14.2.0, no
  gap: layer-0 nodes decode Java's 3- and 4-field tuples and a rewrite keeps
  the fourth (`parseNodeValue`); an index entry is (prefix + PK, vector raw
  bytes or null), Java's toIndexEntry, with no covering columns or rank; the
  record layer's HNSW search is kNearestNeighborsSearch only (orderByDistance
  is used by GuardiANN's centroid walk alone, ported), and cardinality reads
  at most two layer-0 keys.
- [x] Distinguishing pins for codecs, evaluator, collapse, bounce, reassignment,
  task counts and merge locks.
  Persisted-byte differential added (2026-10-06,
  `conformance/guardiann_byte_conformance_test.go`): Java's Guardiann and Go's
  engine (`recordlayer.NewGuardiannEngine`) run one deterministic insert /
  split / delete scenario and every key and value under both subspaces must
  match after the inserts, after EACH build task, after the deletes and after
  the final drain. It found a real divergence: the KMeans input order. Java
  collects a split's references in a HashMap<VectorId, ...> filled by
  compute, whose values() order KMeans sums and the statistics fold in;
  Go used encounter order, so centroids and M2 differed in the last bit. Go
  now ports that traversal (`javaHashMapOrder`: bucket by spread hash at the
  capacity compute leaves, head-of-bin insertion), pinned against a real
  compute-filled HashMap at sizes either side of the resize points. Covers
  identities, cluster metadata and statistics, references, the centroid
  HNSW and the task queue for split. Scripted scenarios added (same
  differential, step by step): merge, collapse, and split-with-reassign; the
  last found a second divergence, in HNSW neighbour selection: Go skipped
  the diversity heuristic when every candidate fit under M (Java's
  selectCandidates always runs it) and broke distance ties arbitrarily (Java:
  then primary key). Fixed for every HNSW graph
  (`TestSelectNeighbors_IsJavasSelectCandidates`, mutation-checked; the
  removed early return also hid an out-of-range slice in the no-triangle
  branch). Bounce is covered by the collapse scenario (every collapse
  enqueues a bounce), and a RaBitQ scenario (sampling, statistics, training,
  encoded references; asserted to end trained) is byte-identical. Through
  the record layer too: Java and Go stores save and delete the same Orders
  into a GUARDIANN index, deferred and with autoMergeDuringCommit, and the
  index subspace AND the secondary subspace (vector task counts) match after
  every step (mutation-checked: dropping Go's enqueue count fails at the
  first split). The merge lock across engines is pinned both ways: one
  engine's merge session claims the partition, the other engine's merge
  (another session) neither drains nor re-claims it, and the holder's
  merges drain the queue (`GuardiANN merge lock across engines`,
  mutation-checked: a Go merge that ignores a live lease fails).
- [x] Runner: unified bounded attempts, per-owner retries, commit ownership and
  deactivation, client proxy wait/body-chain causes, SPFresh stall bound and
  instrumentation. Apply the SPFresh paper review to affected algorithms.
  Scope is ws-d-design.md section 5, "Attempt bounds of the transaction
  owners" (phase D-0). Commit ownership and deactivation done (2026-10-06):
  `RecordContextNotActiveError` (Java's RecordContextNotActiveException, a
  RecordCoreStorageError); a context is deactivated by its first commit
  whatever the outcome; a route's context (Run, RunWithWeakReads,
  RunWithVersionstamp, RunWithRetry) refuses its body's commit before
  anything commits; the two self-committing bodies (`runDDL`,
  `ensureCatalogInit`) no longer commit inside Run
  (`record_context_active_fdb_test.go`, mutation-checked; DIVERGENCES).
  Retry predicates and delay done: `isRetriableAnyCause` (the runner's
  rule, walking error trees) for RunWithRetry and the heartbeat cleanup,
  `isRetriableFirstCause` (FDBExceptions.isRetriable) for the
  auto-continuing cursor, `RecordCoreRetriableTransactionError`, Java's
  ExponentialDelay (uniform [0, current), doubling, 2 ms floor);
  runner.go's own code list (with 1235/1242) is gone
  (`TestRetryPredicates_AnyCauseAndFirstCause`, mutation-checked).
  Attempt loop done: `attemptLoop` (attempt_loop.go) carries Run,
  RunWithWeakReads (weak reads on the first execution only),
  RunWithVersionstamp, RunRead, RunWithRetry and the heartbeat cleanup;
  maxAttempts 10 (`FDBDatabase.SetMaxAttempts`/`SetRetryDelays`), backend
  retry limit 0 per attempt, the body error recorder (reset per execution),
  `AttemptCall`/`AttemptTransactor`, an attempt observer,
  `SPFreshSplitWindowError` retried uncounted and `SPFreshStalledSealError`
  after 100 under simulation; SPFresh lifecycles stay on `runClientLoop`;
  the record hunt reconciles exhausted operations against the predicted
  side (`exhaustion_test.go`). DIVERGENCES entry added.
  Owner policies done: OnlineIndexer `SetMaxAttempts` (its 14 Run sites and
  the heartbeat cleanup), DDL statements one attempt (`RunWithMaxAttempts`);
  the concurrent chaos harness counts exhausted operations under a 5%
  ceiling; the page-retry harness keys faults on `AttemptCall`.
  The catalog bootstrap already writes nothing over an initialized catalog
  (every step checks first; SaveSchema ERROR_IF_DIFFERENT over the same row
  is a no-op), so a one-attempt DDL racing another session's first `Ping`
  commits (`TestFDB_Initialize_OverAnInitializedCatalogDoesNotConflictWithADDL`,
  with an uninitialized control that conflicts). The chaos transactor's
  faults commit and re-run inside one call and never surface an error to
  the loop, so its scenario model cannot see an exhausted call and needs no
  reconcile; keying its arms on `AttemptCall` is left until an arm needs it.
  SPFresh fixtures done over a real seal (spfresh_split_test.go): a stalled
  seal fails with `SPFreshStalledSealError` after 100 uncounted retries, and
  a takeover that publishes the split between attempts lets the write
  complete with no attempt spent (both mutation-checked).
  Instrumentation: each retry delay records `EventRetryDelay` (Java's
  Events.RETRY_DELAY; the drawn delay under simulation).
  Pure-Go wrapper done: `TransactCtx`, `ReadTransactCtx` and the tenant
  forms keep the last execution's body error when OnError re-raises its
  code (the Apple binding's `retryable`), panics included
  (`TestFDB_*KeepsBodyErrorChain`, mutation-checked).
  Proxy wait done: a commit with no known commit proxy now waits for a
  proxy change or its context, then reports 1021 through the
  maybe-delivered fence (C++ LoadBalance over the empty set raced against
  onProxiesChanged); the Go-internal 1200 is gone from the client, both
  onError sets and the wire name map
  (`TestCommit_NoProxies_WaitsForContext`/`_WakesOnProxyChange`).
- [ ] vectorindex peel deferred drain: phase-two split transaction is bounded
  by work, not time. Under race, call 12's callback_including_precommit was
  4.17–4.41 s and its commit 93–118 ms over ten runs (callback plus commit at
  most 4.52 s of the 5 s window); master failed once under heavy load without
  a captured code. Decide time-bounding
  vs Java parity (Java shares atomic phase two). The peel fixtures now record
  each attempt's durations and FDB error codes, so the next failure is
  diagnosable.

## 4. WS-H — stored-query runtime

- [x] CallSiteArguments, typed options and row-number encapsulation (`781468430`);
  macro catalog bytes/named calls and metadata getters have existing Java pins.
- [x] Engine-wide plan cache keyed by schema template, matching Java's
  RelationalPlanCache; stored-query startup warming, invalidation, timing and
  counters. The existing per-connection cache does not satisfy this obligation.
  Done: `RelationalPlanCache`, one per driver connector, shared by every
  connection, with Java's three stages (template / query / equivalence),
  sizes, TTLs (primary expire-after-access, the others after-write) and the
  PLAN_CACHE_* hit/miss/LRU-eviction counts (`relational_plan_cache_test.go`,
  `TestFDB_PlanCacheIsEngineWide`). Declared (DIVERGENCES "Engine-wide plan
  cache"): the query key keeps database and schema and Go's planned-in
  literals, so plans are shared per schema, not per template. Also done:
  UPDATE/DELETE plans are cached (INSERT never, Java's shouldNotCache); the
  transaction's temporary functions are part of the key, so their DDL no
  longer invalidates (`TestFDB_PlanCacheKeysTemporaryFunctions`); a session
  reset keeps the shared cache; PLAN_CACHE_* sizes/TTLs are DSN parameters;
  the corpus `check_cache` pass runs (Java's EmbeddedConfig one-hour TTLs,
  checks shuffled after the executions, +1 tertiary hit required; the
  check-cache skip class is emptied). Per-template keys and the stored-query
  warm-up DONE (2026-10-06): the query key drops database and schema (kept
  only for a PLANNER_STATISTICS plan), so a template's schemas share plans
  as in Java; `WarmStoredQueries` ports OfflineStoredQueriesProcessor (catalog
  read, offline planning with every index readable, DECLAREd functions
  declared first, failures logged and counted) and runs at the connector's
  start (`TestFDB_StoredQueriesWarmThePlanCache`: a stored query's first
  execution hits, an unstored one misses). Cache timing: Go reports a
  plan's cache outcome and planning duration per statement
  (PlanGenerationInfo.Cache / PlanningDuration); Java's per-phase
  RelationalEvent timers (LEX_PARSE, CACHE_LOOKUP, OPTIMIZE_PLAN, ...) have
  no Go registry to land in, declared with the warm-up's counts (DIVERGENCES
  "Engine-wide plan cache").

## 5. WS-I — shared APIs and lifecycle

- [x] Lock-registry cleanup; serializer retry diagnostics; typed client knobs
  (read C++ 7.3.77); typed session/index-update sets and write-only key collisions.
  - Lock registry (#4545): an entry counts its holders and waiters and is
    removed with the last (`TestLockRegistryRemovesCompletedLocks`).
  - Serializer retries (#4290): the read retries were ported; a failure now
    carries Java's RETRY_COUNT / RESULT (and META_DATA_VERSION on a refused
    reattempt) as `RecordSerializationError` fields.
  - Client knobs (#4488): `FDBClientKnob` (Java's 19, typed), `SetKnob` /
    `SetKnobByName` / `Knobs` / `ClearKnobs` with Java's validation (decode
    without '#', `Double.parseDouble`'s grammar, boolean or int). They are set
    before the factory's first open, through `fdbclient.SetKnob`: the `knob`
    network option on libfdb_c, a refused open on the pure-Go client
    (DIVERGENCES "Client knobs").
  - Session sets (#4289): `ContextSessionKey`, `GetInSession` /
    `PutInSession`, and the readable / write-only / queued index-update sets
    filled per index update. Java's two write-only keys share the name
    "writeOnlyIndexesUpdated" and so one set; Go keeps that.
- [x] Client range/HNSW/GuardiANN/vector-task/queue timer instrumentation;
  online-indexer configuration limits; ICU byte baseline.
  - Timers: HNSW node reads/writes by layer and bytes with the generic
    index key/value counters (HnswVectorIndexEngine OnRead/OnWrite); GuardiANN
    `vector_vector_reads`, task enqueued/executed, disabled-on-negative-count;
    `wait_delete_store` around DROP SCHEMA's deletion. The pending-queue
    counters were already in. Pinned by `vector_counters_test.go`.
  - Client range instrumentation: Java's 4.14 change is EventKeeperTranslator's
    count/event dispatch over fdb-java's EventKeeper; neither Go client has an
    EventKeeper, so there is nothing to translate (pre-existing, no analogue).
  - Online-indexer limits: initial limit, increaseLimitAfter (Java's default
    -1 re-increases after every success, as its code does, not as its doc
    says), the 900,000-byte write limit and 4,000 ms transaction time limit,
    which end a range early (`hadTransactionReachedLimits`). Pinned by the
    throttle tests and `online_indexer_txn_limits_test.go`.
  - ICU: declared in DIVERGENCES ("Record Layer collation — explicit Go-only keys"); no byte
    baseline, since matching ICU 78.3 sort keys means porting ICU collation.
- [x] Lucene backend: out of scope for this migration PR (#786), owner decision;
  tracked as a separate follow-up PR (section 7).

## 6. WS-K — harness and relational entry points

- [x] Direct-API Struct inserts: UUID scalar/nested/array and nested unique index.
  Go's `api.DirectAccessStatement` had no implementation; it is now ported
  (`embedded/direct_access.go`: insert, get, scan, delete, delete-range over
  Java's KeyBuilder and `toDynamicMessage` with the #4243 UUID case;
  `rowstruct.StructBuilder`/`ArrayBuilder`). Pinned by
  `TestFDB_DirectAccessNestedUUIDUniqueIndex` (Java's
  `insertToArrayNestedUuidFieldMarkedUnique`) and the round-trip test.
  Limitations: the connection's own schema only, no INDEX_HINT, scans are
  materialized and do not resume from a continuation. A bare UUID array is
  written (Java's repeated-field path cannot write it; see DIVERGENCES, Go-only
  extensions).
- [x] Setup-block version gating (`e510c3d2a`: a setup block's
  `supported_version` is checked before its connection options, as
  `SetupBlock.java:95`). Typed INDEX_FETCH_METHOD: `api.OptionFromString` ports
  `Options.parseStringOption` (enum valueOf, comma-split collections,
  `Boolean.parseBoolean`), and the corpus runner routes a string option through
  it, as Java's `TestBlockOptions.parseConnectionOptions`.
- [x] Recursive result metadata in the corpus runner (CQ-74 closed). A result
  column carries its planned type (`resultset.ColumnDef.DataType`, from
  `rowstruct.DataTypeOf`); `api.WithResultSetMetaDataObserver` hands a query's
  metadata to the caller, and the runner ports `extractDescriptors` over it.
  An array column's `DatabaseTypeName` is Java's `ARRAY` (was the element
  name). `unsupported:result-metadata-nested` emptied (190 inner, 6 file
  skips): the check-result-metadata positives pass and the six struct/array
  negatives fail on their metadata mismatch.
- [x] JSON descriptor FieldOptions import (#4540). Harness-only in Java
  (`CommandUtil.loadRecordMetaDataFromJson` for the yaml `load schema
  template` command). Done: the javacorpus runner runs `load schema
  template` and `set schema state` (schemacommand.go) through
  `EmbeddedConnection.ApplyMetadataOperation`; the yaml-tests protos and
  metadata JSON are vendored verbatim and the protos built by Bazel into
  descriptor sets under Java's file names; the JSON loader ports
  `restoreFieldOptionExtensions`; `ddl.SetStoreStateConstantAction` ports
  RecordLayerSetStoreStateConstantAction; the schema template refuses
  duplicated generation options as Java's `addGeneration` does; an unnamed
  records file builds. Corpus 130 → 137 passing; the 9
  `unsupported:schema-command` files now run (7 pass, `disabled-index-tests`
  reaches plan assertions, `deprecated-fields-tests-proto` is Java-@Disabled
  and fails as Java does, booked `conformance:java-disabled`).
  Mutation-checked: without the extension recovery, field-options-extension
  fails with Java's "Duplicated options for different generations".
- [x] Relational queued-state plumbing; SQL vector-option and preference-cache pins.
  Relational planning reads only READABLE indexes and the fleet build treats
  WRITE_ONLY_WITH_QUEUE as write-only; `TestFDB_QueuedVectorIndexIsWriteOnlyToSQL`
  pins a format-15 store with a queued GuardiANN index through SQL (format and
  state kept, INSERT queued, the index-only KNN unplannable with 0AF00). Java's
  only other relational reach is `RecordLayerSetStoreStateConstantAction`, the
  yaml `set schema state` command (see the JSON item). Every
  `SUPPORTED_VECTOR_OPTIONS` entry is pinned to its canonical option key and
  serialized value (`TestVectorIndexSQLOptionsStoreJavasOptions`), and each
  engine preference keys its own plans (`TestPlannerOptions_CacheKeyPart`).

## 7. Migration-wide acceptance and decisions

- [x] Reconcile implemented WS-A/B/C/J against the historical designs and current
  regression tests. The archive's completion ledger did not list new obligations
  for them; that is not a fresh completeness proof. Include recursive promotion,
  RaBitQ/HNSW legacy compatibility, storage lifecycle, pending writes/format 15,
  catalog version/rebind/carry rules and index-definition fidelity.
  Audit (2026-10-07, against the designs at 13d5a3d1e). PINNED: WS-A ~14,
  WS-B ~20, WS-C ~40, WS-J ~60. Open, each to be closed:
  - [x] DIVERGENCES.md lost entries restored (2026-10-07). Each of the 41
    lost sections was checked against the current tree: 33 restored (12 of
    them edited to the current code and tests) and 8 dropped as obsolete.
    The dropped ones: superseded; the virtual prune deleted; NaN bits fixed;
    spanning predicate now Java's; RFC-209 withdrawn. New sections were
    written for WS-J §9 (a) (record_types order and anonymous names), (c)
    and (l) (rebind validator, inline messages) and (f) (22003 vs XXXXX).
    The "VECTOR index metadata validation" section is replaced by its
    current text. The 35 `ws-j-design.md` citations in Go files point at
    DIVERGENCES sections or Java source instead. (q), a Go message
    argument's log name, is not declared; it is log-only.
    Upstream report drafts are written (`docs/upstream/record-layer-4.14.2.0-reports.md`):
    the DeleteStore retirement callback and the overflow check keyed by
    name. They are not filed; filing them is the owner's call.
  - Lost bookings, rechecked (2026-10-07). USING over an unnest leg is fixed
    (DIVERGENCES "A FROM item's correlated path"). A lateral chain's last
    element and EXISTS over derived legs are now compared equal (same
    section). Table qualifier = template name is declared (DIVERGENCES "A
    table's qualifier is its schema template's name"). Open:
    - [x] select-list-vs-WHERE fault precedence (§9(t), Java
      QueryVisitor.java:272-274 vs :283-322). Done (2026-10-07): when a
      SELECT build fails, both builders re-resolve the WHERE
      (`whereFaultFirst`) and report the WHERE's fault. Three of the four JVM
      probe shapes moved to Java's answer (`select_where_precedence_java_probe_test.go`);
      mutation-checked. Since extended (2026-10-07): a block's join ONs are
      resolved first, in join order, then the WHERE, and the unknown-function
      check waits for the scope, so its 0AF00 also yields to a WHERE fault.
      Eight more probe rows (ON, LEFT JOIN, USING, comma join, function),
      all equal to Java; both changes mutation-checked.
    - [x] 42703 message text (2026-10-07): every Go unknown-column site
      says Java's "Attempting to query non existing column <identifier>"
      (SemanticAnalyzer.java:446). The fromless JVM probe's `unknown` and
      `quoted_hidden_bool` rows are now identical to Java's. The reference
      renders as written (qualified, unknown qualifier, UPDATE SET), and a
      qualified star or a JOIN USING miss says "Unknown reference X":
      `unknown_reference_text_java_probe_test.go` compares 31 shapes, all
      identical to Java;
    - [x] a nested-loop join's orientation over a derived aggregate
      (2026-10-07, `nlj_orientation_java_probe_test.go`). Where Java can
      plan the query, both engines drive from the aggregate and probe the
      index under either conjunct order. The tied, conjunct-order-dependent
      shape (a materialized join under an in-memory sort) is one Java cannot
      plan at all, so there is no Java orientation to match.
  - [x] WS-A: higher-layer HNSW entry replacement after the transform exists.
    Done (2026-10-07): "Same writes, same index bytes"
    (`conformance_test`) has both engines write 60 vectors and then 16 deletes
    (the entry first), each into its own tenant, and compares every index key
    after each write. It runs four configurations: cosine RaBitQ;
    euclidean with small M; small M with extended candidates and kept pruned
    connections; and trained euclidean RaBitQ. The access-info entry
    replacement is asserted. The comparison found six HNSW divergences, all
    fixed and each mutation-checked:
    - a prune keeps the surviving neighbours in list order
      (resolveChangeSetFromNewNeighbors);
    - a list at exactly mMax is pruned too;
    - a prune never extends candidates;
    - a delete repair prunes every candidate and counts the deleted node and
      only existing primaries in its sampling rate;
    - a layer search refetches its entry's stored vector (beamSearchLayer);
    - pairwise distances use the estimator when exactly one vector is
      encoded.
  - [x] WS-B: rank entry too short was already pinned
    (`rank_scan_test.go`, "validates dispatch and state"). Sliding-window
    replay with tied ordering values done: the replay matrix has a `tied` axis
    (64 rows); comparing only the window value reddens 16 tied rows.
  - [x] WS-C. Done (2026-10-07):
    - DELETE_WHERE duplicate / last-known-wins envelope rows
      (`TestPendingQueueOperationLastKnownWins`);
    - overflow commit check across two stores with one index name. Go
      disables both; Java keys the check by index name only. New DIVERGENCES
      entry "Pending-queue overflow disables every overflowing store's index";
      mutation-checked;
    - DeleteStore cancelling the deleted store's heartbeat:/merge: checks but
      not a sibling store's, then delete-then-recreate in one transaction
      (`index_queued_dispatch_test.go`; mutation-checked). Chunked entries
      are not exercised: the queued vector payloads fit in one chunk.
    Also done (2026-10-07), each mutation-checked:
    - state-4 consumers (`state4_consumers_test.go`): write-only, not
      scannable, not readable, refused rank scan; a save into a queued unique
      or rank index fails in serializePendingWriteQueue as in Java; a queued
      vector index enqueues. Bitmap has no row, since it uses the same
      unsupported serializer;
    - markReadable continuing past a failed target
      (`online_indexer_test.go`, "publishes the other targets");
    - publication before a state-4 writer (`index_queued_dispatch_test.go`,
      the 6-row "writer after checked publication" matrix).
    Also done (2026-10-07):
    - rebuild to WRITE_ONLY_WITH_QUEUE and its format-14 refusal
      (`rebuild_write_only_with_queue_test.go`), with a new DIVERGENCES entry
      "Queued index states require format 15 and a queue-capable index";
    - drain defaults (`TestThrottledIteratorDefaults`);
    - `SetStoreState` to WRITE_ONLY_WITH_QUEUE, through the metadata action
      at format 14 (refused) and at 15 (`TestFDB_QueuedVectorIndexIsWriteOnlyToSQL`).
    Each is mutation-checked.
    MISSING: Lucene pending-queue consumption (keep visible; L). Java's vector
    merge calls no heartbeat callback, so Go's needs none.
  - [x] WS-J. Done (2026-10-07):
    - restore one-history rows for swapped record-type keys and a
      former-index key reused by a live index (`TestFDB_Restore_OneHistory`);
    - a concurrent second RestoreTemplateVersion of the same or other bytes:
      the first conflicts, and its retry is refused 42F62 "it is stored"
      (`TestFDB_Restore_ConcurrentWrites`);
    - "Could not delete unknown schema template" asserted, both deletes, with
      the no-op when not thrown (`TestFDB_VersionGuard_DeleteBoundVersionRefused`);
    - unnest- and derived-sourced indexes in the production-path template pin
      (`TestFDB_ExecutedTemplateIsTheToolingPathsTemplate`);
    - grouped_count and split_point outside [0, columns], negative and
      too-large (`TestKeyExpressionFromProtoErrors`).
- [x] WS-G implementation: Java aggregate continuation state, legacy reads,
  grouping-output simplification/ARRAY_AGG cap/resume and plan-schema tags have
  committed pins. Whole-upgrade acceptance remains open.
- [x] **SUM index: DECIDED 2026-10-05, exactly as in Java; done.** A SUM or
  COUNT aggregate index answers alone: a vacated group reads 0, an all-NULL
  group has no SUM / COUNT(col) row, a SUM residue reads 0 (Java 4.12.11.0
  measured `[[10 0] [11 0] [13 7]]`, `conformance/probe_zerokey_allnull_java_test.go`;
  4.14.2.0's `AggregateIndexMatchCandidate` and maintainer have no change here,
  checked by source diff). RFC-209 is withdrawn: the `__GROUP_COUNT` companions,
  the `GroupExistenceMerge` outer merge, the COUNT(*) zero drop, the
  `NeedsNonNullCompanion` gate (5459c90a2) and the reserved-name guard are
  deleted, and an ungrouped SUM / COUNT index is a candidate. The tests pin both
  answers through `mmAggregateIndexRowsAgree` / `WantAggregateIndex`
  (sqldriver), which admits exactly Java's three differences from the records.
  The 43 WSJ `both-accept-companion` runs are now `both-accept-equal` (their Go
  digests equal Java's, recomputed offline). The metamorphic sweeps no longer
  declare SUM / COUNT aggregate indexes.
- [x] Full-lane failures found running the sqldriver and factory suites locally
  (2026-10-05). `TestFDB_InListSignedZeroKeepsPrimaryKeyOrder`'s twins index
  `(e, id)` (F-7b item 2 builds no IN-union past the record-type coordinate);
  `TestFDB_DynamicNaNCompositeIndexKeyFilter` reads the id after the record-type
  key. The factory corpus drift (53 scenarios) was censused against `d5a5132a1`
  before re-blessing: the 12 lost equality probes are 6 IN-unions F-7b item 2
  removes (Java builds none) and 6 join outers F-7a moved from `IDX_AB [=,*]` to
  a fully matched range index, Java's `unmatchedFieldsCount` rung.
- [x] **Aggregate roll-up port.** `yieldAggregateGroupingSubsumption`
  (`rule_aggregate_roll_up.go`) ports the rest of Java's
  `GroupByExpression.groupingSubsumedBy`: query grouping keys match the
  candidate's columns as a set, an equality on a grouping column matches it
  implicitly, an exact match is the index scan with a projecting Map, and
  otherwise the candidate rolls up to its longest matched prefix (explicit keys
  inside it, a rollable index type, no residual) as a streaming SUM / MAX / MIN
  over the scan. `aggregate_index_roll_up.yaml` is Java's
  aggregate-empty-table.yamsql T2 blocks: every plan shape and answer, the
  commented-out residue reads included. Not ported: roll-up inside the
  multi-aggregate intersection.
- [x] **Lucene: DE-SCOPED from PR #786 by the owner; a separate follow-up PR.**
  It is still to be done (the 2026-10-05 in-scope decision stands for the
  port), but it does not gate this migration's completion or parity claim;
  the Lucene queue/heartbeat/quota/spell-check/state contracts go with it.
  The notes below are the follow-up's starting point. Java runs Apache
  Lucene 8.11.1 inside the JVM (`fdb-record-layer-lucene`, 28.7k lines of main
  Java, 7 protos). It stores the segment files in FDB through `FDBDirectory`,
  with `LuceneOptimizedCodec` over Lucene87 parts (Lucene84 postings, Lucene80
  doc values, Lucene86 points and segment info, Lucene50 compound). Java's
  relational layer does not reach it. The engine is not chosen yet:
  - Zinc (now `vcaesar/riot` + `ice` segments), bluge and bleve write their
    own segment formats. Java could not read a Go-written Lucene index, and Go
    could not maintain a Java-written one, which breaks the wire hard line.
  - `geange/lucene-go` (Apache-2.0, Lucene 8.11.2 base, experimental) has
    ports of the same Lucene87 codec parts plus BlockTree and FST. It is a
    candidate reference or vendored start for a format-compatible port.
  The engine choice is made in the follow-up PR.
- [x] **Catalog/keyspace: DECIDED 2026-10-05, exactly Java's layout, no
  compatibility with the old Go layout.** Go's SQL driver used the string-key
  catalog/schema layout `(__SYS, __SYS, CATALOG)` / `(dbPath, schemaName)`.
  Replaced it with Java's `RelationalKeyspaceProvider` layout: the typed system
  path `(NULL, NULL, int64(0))` and directory-layer domain -> database ->
  schema levels. No migration from the Go layout: data written by an earlier Go
  build is recreated. A template created through Go must load in Java and the
  reverse (`TODO_OLD.md`, “Go SQL driver uses Java's relational catalog and schema keyspace”).
  Measured scope (4.14.2.0 source):
  - Catalog store at `KeySpaceDirectory(__SYS, NULL) / (__SYS, NULL) /
    (CATALOG, LONG, 0)`, so the tuple `(nil, nil, 0)`.
  - A database path is `/DOMAIN/DB`. A domain must be registered
    (`registerDomainIfNotExists`; Java's server registers `FRL`, its tests
    `TEST`); the domain is a `DirectoryLayerDirectory` resolved by the GLOBAL
    `ScopedDirectoryLayer` (FDB directory layer `createOrOpen([domain])`, the
    prefix's tuple long, plus an `FDBReverseDirectoryCache` entry). `dbName`
    and `schema` are `DirectoryLayerDirectory`s resolved by a
    `ScopedInterningLayer` at `domain/__internedStrings` ("IL");
    `defaultSchema` is a NULL directory. A one-segment path is INVALID_PATH.
  - Go has the FDB directory layer (`pkg/fdbgo/fdb/directory`) but no
    `ScopedDirectoryLayer`, `FDBReverseDirectoryCache`, `StringInterningLayer`,
    record-layer `HighContentionAllocator` or `ScopedInterningLayer`; its
    `keyspace.FDBResolver` is a Go-only format. These are byte-exact ports
    (about 2,800 lines of Java with `LocatableResolver`), then the keyspace tree,
    `sqldriver`, `catalog`, `ddl`, `fleet`, the `frl` CLI.
  - Domains: DONE (owner decision 2026-10-06, exactly Java). A process-wide
    registry (`keyspace.RegisterDomainIfNotExists`, re-exported by
    `sqldriver`), Java's path matcher (`keyspace.ToDatabasePath`, a port of
    KeySpaceUtils.toKeySpacePath over RelationalKeyspaceProvider's tree) at
    Java's sites (CREATE DATABASE, a schema's store resolution, connect), and
    SemanticAnalyzer.validateDatabaseUri for parsed paths. The driver
    registers nothing; `frl`, the factory/stress tools and the javacorpus
    runner register FRL; test packages register FRL and TEST. About 3,700 test,
    doc and help paths were renamed to `/FRL/...` by script.
  - Byte-exact layout: DONE (2026-10-06). The catalog store is `(NULL, NULL,
    0)`; a schema store is `(domain, database, schema)`, the domain resolved by
    the global FDB directory layer (ScopedDirectoryLayer.global, plus the
    `recdb_rd_cache` reverse directory cache) and the names interned by the
    domain's ScopedInterningLayer at `(domain, "IL")` (StringInterningLayer
    mapping/reverse/counter at 2/1/0, state at -10, the record-layer
    HighContentionAllocator). `pkg/recordlayer/locatable_resolver.go` ports
    the resolution path: resolveWithMetadata in a committed child transaction
    with the database's directory cache, readInTransaction (no create,
    used by the frl CLI so a mistyped address interns nothing),
    reverseLookup, the UNLOCKED state check. `pkg/fdbgo/fdb/directory` now
    runs over any `fdb.WritableTransaction` (libfdb_c and the simulator too)
    and takes an injected random source (the DST seam). Pinned by the Java
    interop spec "Relational key space shared with Java" (each engine resolves
    the other's schema at the same prefix and reads its rows; the interning
    mapping moved off 2 reddens it). Not ported: resolver locking/migration/
    setMapping/setWindow administration, the global root interning layer
    (0xFC), and the in-memory reverse-directory cache's retriable mismatch.
    `pkg/recordlayer/keyspace`'s Go-only `FDBResolver` had no users and is
    removed (2026-10-07); the package keeps the `LocatableResolver` interface
    and `ResolverDirectory`.
- [ ] Reconcile living compatibility claims/CHANGELOG, run `just test-full` and
  required interop/performance checks, then final migration review and PR CI.
  Fix all Medium-or-higher findings before declaring completion.

## Follow-ups after the 4.14.2.0 migration (not blocking)

- [ ] **Decide which nightly nets to bring back (all deleted 2026-10-09).** The seven nightly
  workflows (coverage, factory, fuzz, oracles, reconcile, rowdiff, stress), `cmd/verify-pr-checks`
  (used only by reconcile) and the `census_test` target were removed: a late nightly could hold
  both CI runners, and nobody could say what they still caught. The libfdb_c cross-client
  differential moved into ci.yml as its own per-PR job. Each one is in git history
  (`git show 8d3f8c4ad:.github/workflows/<file>`). Bring one back only with a stated catch rate.
  What no longer runs anywhere:
  - the `-race` runs of `//pkg/relational/conformance/...` and of the client, record-layer and
    chaos container suites;
  - active fuzzing;
  - the 10K/100K/1M stress suite;
  - coverage reporting;
  - the self-checking oracles and the nightly rowdiff and factory batches.

- [ ] Two IN lists ordered by their own columns: Java plans nested in-unions
  over both explodes; Go keeps sorted in-joins (`w8_in5x5`/`w8_in4x6`, declared
  DIFF-PATH rfc-191). Needs Go's in-join rules to accept a select with several
  explodes (WS-F §2, item 10).
- [ ] CI/dev speed, next hills (measured 2026-10-08): `distinctelision` (~86 s,
  bound by one FDB container's write rate over ~620k fixture rows); planner
  allocation (GC is ~39% of planner CPU); split 2, vector indexes out of
  `pkg/recordlayer` behind a maintainer registry (58 -> ~38 test targets per
  vector change); a Bazel cache shared by both CI runners.
- [ ] `pkg/fdbgo`: bound GetRange selector resolution by the range end. The facade
  resolves a non-trivial selector with an unbounded `GetKey` first
  (`pkg/fdbgo/fdb/range_result.go:413-470`, `pkg/fdbgo/client/ryw_getkey.go`), so an
  overshooting index-scan Skip reads, and in serializable mode conflicts on, keys past the
  range. libfdb_c resolves within the read (ReadYourWrites.actor.cpp:725-760) and conflicts
  on the requested range (:246-282). Client team.
- [ ] `pkg/fdbgo`: make `GetAddressesForKey` transactional. Go answers from the location
  cache (`pkg/fdbgo/client/transaction.go:2698`); libfdb_c 7.3.77 starts the transaction and
  reads `\xff/serverTag`, the key's `\xff/keyServers` entry and the server interfaces at the
  read version (NativeAPI.actor.cpp:5707-5753). Found by the multi-node placement probe
  review; the probe does not rely on the Go path. Client team; held while feature work is
  paused.
- [ ] Run the remaining `libfdbc`-tagged tests under Bazel. CI's `libfdbc-differential`
  job runs them with a standalone `go test -tags libfdbc` over `pkg/fdbgo/libfdbc/...`
  and `pkg/internal/fdbclient/...`; only `network_lifecycle_test` and
  `mapped_skip_regression_test` have Bazel targets, so `just test-full` skips
  `differential_test.go` and the other cross-client differentials. Blocker (2026-10-24):
  every remaining file uses `startCluster` from `differential_test.go`, which imports
  `pkg/recordlayer`; under the `libfdbc` gotag Bazel compiles `pkg/internal/fdbclient`
  without its only source (`open_purego.go` is `!libfdbc`), and `open_libfdbc.go` needs
  `libfdbc_native`, which shares the stub's import path. The fix is a tag-aware select
  across both libraries.
- [ ] Java/Go raw key-value parity for shared TEXT indexes: plain, grouped and
  no-position variants over the Unicode-delta inputs, comparing sorted relative keys,
  values and positions; force bunch splitting with 25 common postings; cross-engine
  update and delete, obsolete-term removal and final emptiness. Exclude the documented
  tokenizer divergences and Go-only collation.
- [ ] Java proto rules: fail the build if a `java_proto_library` regenerates protobuf
  well-known types other than `any` and `descriptor`, and test that the failing fallback
  toolchain is selected on a non-Linux platform.
- [x] `tools/lanes/run-verified-lane.sh`: a cancelled lane leaves its Bazel server holding
  the lane lock until the server is shut down. Shut the server down on exit, or keep the
  lock descriptor out of the server. Done: `flock -o` holds the lock for the
  lane's lifetime and keeps it out of the server (`TestVerifiedLaneLock` in `//infra:infra_test`).
- [ ] Update Buf from 1.67.0 to 1.74 (`BUF_VERSION` in `justfile`).
- [ ] SQL: the ANSI forms `POSITION(x IN s)` and `EXTRACT(<field> FROM x)` fail with 0AF00
  "projection slot 0 has no resolved Value", and `YEAR` over a BIGINT answers NULL rather than
  refusing the type. `DocClaimsJavaProbe` pins them as they are (`position_in`, `extract_year`,
  `date_part_of_integer`); Java answers the operand unchanged for both ANSI forms and refuses
  `YEAR` (0AF00).
- [ ] `SQL_ANSI_CONFORMANCE.md`'s `Java?` column is still pinned against 4.12.11.0
  (`pkg/relational/conformance/yamsql/ansi_roster.go`, the notice in `ansiledger.go`). Re-verify
  it on 4.14.2.0, as RFC-278 did for SQL_CONFORMANCE.md.
- [ ] **TLS settings without a network option.** RFC-277 reads `FDB_TLS_*` only. libfdb_c
  also takes the `TLS_*` network options (`fdb.options:70-105`) and
  `FDB_NETWORK_OPTION_TLS_*` variables (`MultiVersionTransaction.actor.cpp:3484-3536`). It
  re-reads certificate files on change every 12 h (`Knobs.cpp:128`), and its handshake
  timeout is 2 s (`Net2.actor.cpp:1101-1106`) where Go's is 10 s. The `fdb` facade first
  needs network options.
- [ ] **TLS differential.** RFC-277's verify-peers cases are pinned against the C++ source,
  not measured: run libfdb_c and the Go client with the same `FDB_TLS_*` against one TLS
  listener per `TestVerifyPolicyMatching` case (OpenSSL's Latin-1 widening, SAN
  fall-throughs, `ssl_server` purpose), and open a TLS testcontainer fdbserver (test-full).
- [ ] **One PBES2 decryptor.** `pkg/fdbgo/client/tls_key.go` (RFC-277) and
  `pkg/recordlayer/keystore_key_manager.go` each decrypt PBES2/PBKDF2/CBC; move both onto
  one package under `pkg/fdbgo` so a bound or a new PRF lands once.

## Test lanes and test-speed work (committed with the temporal change)

- [x] Fast/full lanes (`just test` / `just test-full`, `infra/test_lanes_test.go`,
  the fast pre-commit hook). CLAUDE.md restored (its deletion broke 126
  references) and aligned with AGENTS.md; stale "every `just test`" claims fixed
  in the workflows, DIVERGENCES.md and the dst skill. PR CI is unchanged: it
  runs `//... -stress`, so `test-full` targets still gate every PR.
- [x] Every test target now builds: `million_record_test` (never compiled
  standalone) gets the Ginkgo suite file and deps, and its env-gated Skip is
  removed; the FDB C++ genrules cap ninja by available memory (`-j$(nproc)`
  took 36 GB on a 24-thread host).
- [x] FDB readiness (`WaitAvailable`, `DatabaseAvailable`: "unavailable" no longer
  matches), rowdiff shared planning (held by reference, released by the last
  holder, pinned by `sweep_helper_test.go`), private parser ATNs per prediction
  lease (removes ATN lock sharing; no measured speed gain).
- [x] Run `just test-full` once end to end. 2026-10-06 at `a15d1ec70`, 111
  targets (manual ones included): 109 pass, 2 failed and were fixed in
  `c116de63b` (a typed NULL CASE branch lost its type in result metadata;
  stale CQ-74 oracle pins), both re-run green.
- [x] Split `sqldriver_test` — done on split/sqltest: the end-to-end suite is
  `pkg/relational/sqltest/<area>` packages + the manual whole-corpus census target;
  `sqldriver_test` keeps the driver's own tests and is the race lane's SQL target.
  Original item: Split `sqldriver_test` (1748 tests, ~7.5 min, 77% of its time in ~40
  sweep/probe tests) so its cheap regression pins return to the fast lane.
  A fast-lane `sqldriver_fast_test` (same binary, 45 heavy tests skipped) was
  tried and reverted (2026-10-07): ~170 s on every commit hook is too slow for
  the fast lane. The test-suite speed investigation owns this now.
  - The race lane's timeout (1h0m16s, run 37669344266) is fixed by the split:
    it runs `sqldriver` (the driver's own tests) in about 11 min, green.
- [x] Flake: `TestNativeReadTagSampling/fallback-two-nontrivial-selectors/hedge=true`.
  RFC-281 (#1017) funds both concurrent selectors, then seeds a separate budget
  for the exact read after both selectors complete. An incidental selector hedge
  cannot consume the exact read's planned hedge; live replies keep generous timeouts.

## Row-limit lookahead at an exact page boundary

- [x] Index cursors (ordinary VALUE/VERSION plus COUNT/SUM, MIN/MAX_EVER_TUPLE,
  MAX_EVER_VERSION and BITMAP_VALUE) pass down the finite row budget, report
  `RETURN_LIMIT_REACHED` with a last-key token at exact exhaustion, and charge
  the terminal attempt. `conformance/index_scan_limit_conformance_test.go` and
  `conformance/index_continuation_range_conformance_test.go` compare every
  index boundary with Java, including empty final-page resumes.
- [x] Record scans (key_value_cursor.go) follow Java's scanTypedRecords: per
  key-value scan charges (inline versions included in record sizes), limits that
  finish the crossing record, and `RETURN_LIMIT_REACHED` at an exact row limit.
  CountRecords follows Java's countRecords (key-values for unsplit stores).
  `conformance/record_scan_limit_conformance_test.go` compares every layout.

## Issued-record load byte accounting

- [x] Successful raw record loads charge bytes at completion, even if a failed
  index cursor discards the queued result (#912). Raw failures do not charge;
  later decode/version failures retain successfully read bytes. Strict counters
  are compared after already-issued loads quiesce, not at Java's racy
  failure-instant snapshot. Real-FDB gated Java and Go regressions pin this
  without additional reads or tolerance.

## RFC-266: Full JDK 21 parity and text-index migration

- [ ] **RFC-266 — full JDK 21 parity, including migration of Go-written TEXT indexes.**
  The owner selected full parity after the pin-only RFC-265 lands; no RFC-266
  implementation is part of that pin. Match BreakIterator segmentation,
  NFKD/final-sigma context and the JDK Cased set (`x:y`, `ĿΣ`, `ΑΣ\u037A`,
  `\U00010780Σ`); full English keystore-alias lowercase (`İ`, final sigma);
  and column-label `equalsIgnoreCase` (`İ` versus `i`). Go-written v0 TEXT
  postings for affected inputs already differ from Java's. Define how to detect
  potentially affected/unknown-writer indexes, rebuild them, handle tokenizer
  versions and exclude old divergent writers during rollout; prove no orphaned
  terms with Java differentials. Widen the direct-caller Unicode guard to
  identifier/name callers and helper indirection; route or explicitly exclude
  the remaining `strings.EqualFold`/`strings.ToUpper` uses in
  `pkg/internal/querycensus` and `cmd/frl`, which are outside the RFC-265 pin.
  Evidence: [RFC-265](rfcs/265-unicode15-compatibility-pin.md#java-fidelity-and-deliberately-retained-divergences),
  `conformance/testdata/unicode15_java.json`, immutable
  `tools/unicode15baseline/testdata/go1.26.10.json` and the raw write/update/delete
  fixtures plus mixed-compiler harness in `tools/unicode15store/`.

## Static analysis coverage

- [ ] Restore the dereference-then-later-nil-check coverage when an upstream-supported
  SA5011 replacement is available. Staticcheck 2026.2 retired SA5011 in `5161aaa18`;
  nogo's enabled x/tools `nilness` checks proven-nil uses but not that pattern.
  See CHANGELOG.md, Unreleased.

## GROUP BY duplicate-key check: follow-ups found by RFC-272

- [ ] Found while removing the `GetText` fallback (RFC-272 item 6); each predates it and needs a
  JVM probe before Go changes. (1) The 42702 check runs at aggregate build time, before WHERE and
  SELECT-list resolution and before 42803: `WHERE nosuch = 1 GROUP BY amount+1, amount+1` and
  `SELECT nosuch, COUNT(*) … GROUP BY amount+1, amount+1` are 42702 (Java 42703), and
  `SELECT category, COUNT(*) … GROUP BY amount+1, amount+1` is 42702 (Java 42803)
  (`QueryVisitor.java:274-305`, `LogicalOperator.java:436-455`). (2) An unknown function or a
  scalar subquery as a GROUP BY key returns a raw `*expr.UnsupportedExpressionShapeError`, not an
  `api.Error` (Java 0AF00 / 42601). (3) `category || 'a'` as a key is 42804; Java is 0AF00
  "Unsupported operator ||". Parameter keys (`amount = ?, amount = ?`) are value-dependent; the
  probe runner binds no parameters, so Java's answer is unmeasured (DIVERGENCES.md).

## Java-corpus plan comparison (quality review 2026-10-24, RFC-273)

The javacorpus runner books every plan directive as a counted skip
(`pkg/relational/conformance/javacorpus/testblock.go:436-437`, RFC-201 ruling 2:
no Java-format plan renderer, so no verbatim compare). Measured on the vendored
4.14.2.0 corpus (255 `*.yamsql`, YAML-parsed `test_block.tests` query configs;
grep positive control agrees): 1,266 directives, 1,214 `explain` and 52
`explainContains`, on 1,266 queries in 141 test blocks of 87 files; `planHash`
appears in no file. 8 are runner meta-tests, 11 are UPDATE/DELETE (Go's EXPLAIN
renders logical DML). Go's EXPLAIN is a different format (nested calls,
predicates as `[N preds]`, no AISCAN bounds, folded MAP layers), but every Java
operator has a Go counterpart. `wsfAccessPath`
(`conformance/ws_f_access_path_test.go`) already reduces both formats to an
access-path tree for 775 of the 1,211 non-meta `explain` strings; the other 436
need AISCAN (178), AGG (161), TFILTER (39), DEFAULT (33), union/intersection
(32), RUNION/TEMP SCAN (22), TF (14), DISTINCT BY PK (9) — each count is the
strings containing that operator, and a string can need several, so the counts
sum to 488. On 28 queries whose
Go schema matches Java's and whose Go plan is in `plan_shape.golden`: 0 exact
string matches, 28/28 same access leaves, 26/28 same operator skeleton (a
biased sample: those Go scenarios were ported from Java's; WS-F already books
12 DIFF-PATH plans).

1. [ ] **Capture.** At the skip site, run `EXPLAIN` for the same query on the
   same connection and record `{file, line, kind, java, go | go-error}`; the
   directive stays a skip. Exit: census rows == reached directives, per-kind
   totals pinned in `pinned_ledger_test.go`, the existing ledger line unchanged.
2. [ ] **Shared normalizer.** Move `wsfAccessPath` into a library package
   (e.g. `pkg/relational/conformance/accesspath`) with the WS-F probe as its
   first consumer. Exit: WS-F verdicts byte-identical; per-operator table tests
   for both syntaxes.
3. [ ] **Operator coverage.** Add the operators above and vector BY_DISTANCE.
   Compare leaf kind, index or table, comparison kinds, covering/fetch and the
   filter/sort/agg/on-empty/join/union/IN operators; drop MAP, predicate text,
   `@cN` literals and quantifier names. Exit: no unknown node over the 1,211
   strings; the 47 non-meta `explainContains` reduce to leaf assertions.
4. [ ] **Classify and pin.** One verdict per row: SAME-PATH, SAME-CLASS,
   DIFF-PATH(reason), GO-ERROR or NOT-COMPARABLE (DML, meta-tests). Exit:
   per-verdict counts and a per-row digest pinned, so a plan flip fails a test.
5. [ ] **Triage DIFF-PATH.** Each distinct reason becomes a DIVERGENCES.md entry
   with reversal criteria or a planner bug item (first suspect: an ungrouped
   StreamingAgg without DefaultOnEmpty, `select sum(col1) from T2 where col1 =
   0`). Exit: no undeclared DIFF-PATH row; SAME-PATH only ratchets up.
6. [ ] **Fidelity gaps.** `AggregateIndex.Explain` omits scan bounds (93 AISCAN
   directives carry them) and filters render as `[N preds]` (450): render the
   comparison kinds or compare those rows at SAME-CLASS. Exit: the decision
   recorded as an RFC-201 amendment, the explaindiff golden regenerated if any
   `Explain()` changes.

Out of scope: `planHash` (no corpus use; Go's `PlanHash` is FNV-64a, Java's an
int, and RFC-024 makes hash compatibility a non-goal).

## Quality review 2026-10-24

A six-area read-only review; line numbers are from master `12e2a8fe2`. ✔ marks a finding
re-verified by reading the code; the rest are reviewer reports to verify against current Go
and Java (or C++ 7.3.77) before changing anything. Owner scope: work every item except the
multi-page auto-commit SELECT, which stays tracked only.

### Record layer

- [x] **High ✔ Unique index reports false violations.** `checkUniqueness`
  (`pkg/recordlayer/index_maintainer.go:623`) scans `fdb.PrefixRange(Pack(value))`; Java
  scans `indexSubspace.range(valueKey)`, `[pack+00, pack+ff)` (`StandardIndexMaintainer.java:490`).
  A value whose packing extends another's (bytes or strings with NUL, nested tuples:
  `b"\x01"` vs `b"\x01\x00\x05"`) collides; WRITE_ONLY indexes persist violation entries
  Java would not write. Fixed in #995 (RFC-270).
- [x] **High ✔ RANK remove keeps orphaned scores.** The non-duplicate-counting remove
  (`pkg/recordlayer/rank_index_maintainer.go:279`) checks for other holders of a score with
  `fdb.PrefixRange(Pack(score))` instead of Java's tuple range, so a remaining score
  `X ++ \x00…` keeps `X` in the ranked set. Fixed in #995 (RFC-270), with six more sites of the same range bug.
- [x] **Medium ✔ SUM index cannot remove `math.MinInt64`.** `pkg/recordlayer/atomic_mutation.go:285`
  returns an error; Java adds `-lval`, which wraps to MinInt64 and is correct mod 2^64. The
  test that introduced the guard (`byte_limit_bug_test.go:89-111`) only asserts Go arithmetic. Fixed in #995 (RFC-270).
- [ ] **Medium:** mutual-indexing fragment edges are raw shard keys
  (`pkg/recordlayer/indexing_mutual.go:105-118`), not Java's `getPrimaryKeyBoundaries` (first
  record key at or after the boundary, split suffix dropped); a non-tuple boundary fails the
  `Unpack` at `:368` or `:377`.
- [ ] **Medium:** the mutual build loop (`buildFragmentRange`, `pkg/recordlayer/indexing_mutual.go:359-425`)
  never calls `transactionReachedLimits`; `buildRange` does (`online_indexer.go:2106`).
- [ ] **Medium:** the mutual FULL phase builds one chunk and stays on the fragment
  (`pkg/recordlayer/indexing_mutual.go:283-294`), then skips any partly built fragment
  (`:344-350`), so every peer falls into ANY: partitioning is lost and 1020s follow.
- [ ] **Medium:** no backoff between lessen-work retries (`buildRangeWithRetries`,
  `pkg/recordlayer/online_indexer.go:1605-1631`; Java waits in `IndexingThrottle.java:394`).
- [ ] **Medium:** no explicit-version save or `VersionstampSaveBehavior`
  (`pkg/recordlayer/store.go:682`, "Phase 2"); not listed in DIVERGENCES.md or
  docs/compatibility.md.
- [ ] **Low:** `TransactionSizeWarningError` comes back through the error result after the
  write (`pkg/recordlayer/database.go:925`, via `CheckTransactionSize` at `store.go:644,1032`).
- [ ] **Low:** keyspace accepts sibling directories with the same key type
  (`pkg/recordlayer/keyspace/keyspace.go:166`; Java `KeySpaceDirectory.java:356-368`) and
  `PathFromTuple` (`:338`) picks the first match; the resolver uses `context.Background()`
  (`keyspace/resolver.go:111`).
- [ ] **Low:** `%v` drops error chains (`pkg/recordlayer/store_builder.go:1037`,
  `store_state_cache.go:305`); `buildRange` adds a record read conflict
  (`online_indexer.go:2085`) before the per-index record-type filter (`:2093`).
- [ ] **Low:** rank and text edges: `GetNth` returns a key where Java returns null; a
  non-int64 rank bound becomes an unbounded scan (`extractRankValue`); TEXT silently skips
  non-string columns.

### Go FDB client

- [x] **High ✔ `handleConnError` closes the pooled connection, not the failed one**
  (`pkg/fdbgo/client/topology.go:239`). Its 35 non-test call sites pass only an address (4
  direct in `commitpath.go` and `pending_get.go`, 20 through `handleReadConnError`, 11 through
  `handleDialError`), so a late handler can close a fresh connection `getOrDialConnFor` pooled
  after the failure and fail its RPCs (a commit becomes 1021). Only the dial-panic path
  compares identity (`pkg/fdbgo/client/database.go:582`). Fixed in #997 (RFC-271).
- [x] **Medium:** the location cache keeps overlapping stale ranges. `insertSorted` replaces
  only an equal `begin` (`pkg/fdbgo/client/locality.go:64-80`) where C++
  `CoalescedKeyRangeMap::insert` overwrites the range, and `lookupLocked` (`:139`) checks
  only `idx-1`. Fixed in #997 (RFC-271).
- [x] **Medium:** decoding can allocate quadratically: vector element offsets may alias one
  inner vector (`pkg/fdbgo/wire/reader.go:415`, reachable through `GetMappedKeyValuesReply`).
  Add a per-decode element budget in the generator and a raw-bytes `UnmarshalFDB` fuzzer.
  Fixed in #1004 (RFC-276): a `len/4` element budget per decode, `FuzzUnmarshalFDB`, and
  decoders that fail malformed input.
- [x] **Medium:** TLS sets `ServerName` to the dialed host (`pkg/fdbgo/transport/conn.go:1533-1535`),
  a hostname check libfdb_c does not make; `FDB_TLS_VERIFY_PEERS` and `FDB_TLS_PASSWORD`
  are never read. Fixed in #1006 (RFC-277): the `FDB_TLS_*` path verifies as libfdb_c does;
  `WithTLSConfig` keeps Go's check, and SNI is still sent (DIVERGENCES.md).
- [x] **Low:** malformed location replies are absorbed: a missing range covers the whole
  keyspace (`parseGetKeyServerLocationsReply`, `pkg/fdbgo/client/locality.go:665`);
  `ReadVectorCount` truncates an oversized count instead of failing
  (`pkg/fdbgo/wire/reader.go:435-437`). Fixed in #997 (RFC-271); generated decoders that read a vector-count error as an absent vector are #1004's.
- [ ] **Low:** `handleConnError` closes the connection under `connMu`
  (`pkg/fdbgo/client/topology.go:242`), and a TLS socket close in `failConnection`
  (`pkg/fdbgo/transport/conn.go:1007`) can block for up to 5 s.
- [ ] **Medium:** `GetEstimatedRangeSizeBytes` and `GetRangeSplitPoints` time out each
  replica after 5 s and end with a non-retryable `all_alternatives_failed` (1006)
  (`pkg/fdbgo/client/metrics.go:96,170,275,322`); C++ retries 1006 and wrong-shard without a
  cap (`NativeAPI.actor.cpp:8079,8205`).
- [ ] **Low:** a mapped-range read with a positive skip re-sends on reply timeout without the
  10-re-send cap the other reads have (`pkg/fdbgo/client/mappedrange_selectors.go:154,265`).
- [ ] **Low:** a `TransactCtx` deadline does not bound a dispatched commit
  (`pkg/fdbgo/client/transaction.go:1882`); documented at `pkg/fdbgo/fdb/database.go:409`.
  Decide whether to keep it.

### SQL

- [ ] **Tracked only (owner excluded it from this work).** An auto-commit SELECT runs each
  page in its own FDB transaction (`pkg/relational/core/embedded/cascades_generator.go`,
  `runInCapturedTx` with `r.tx == nil`), so one result set spans read versions; Java holds
  one transaction per result set. User docs do not say so.
- [x] **Medium:** inside an explicit transaction the statement context does not reach FDB
  reads (the "KNOWN GAP" at `pkg/relational/core/embedded/connection.go:939`;
  `pkg/recordlayer/database.go:795` sets `context.Background()`), and `TxOptions.ReadOnly`
  is ignored. Fixed in #999 (RFC-272); `ReadOnly` is now refused with 0A000.
- [x] **Medium:** panics are recovered only in Exec, Query and Rows.Next
  (`pkg/relational/core/embedded/connection.go:805,841`, `cascades_generator.go`); not in
  `embeddedTx.Commit`, BeginTx, SetSchema, PlanExplain, CollectStatistics, DirectAccess or
  `paginatingRows.Close`. Fixed in #999 (RFC-272).
- [x] **Medium:** the connector's `initialize` runs in a `sync.Once`
  (`pkg/relational/sqldriver/driver.go:251`), so a transient open failure breaks that
  `*sql.DB` for good; it runs under the first caller's context. Fixed in #999 (RFC-272).
- [x] **Medium:** `ResetSession` (`pkg/relational/core/embedded/connection.go:1022`) keeps the
  statement timeout, max result bytes, fail-on-scan-limit, plan and stats loggers and
  slow-query threshold of the previous borrower. Fixed in #999 (RFC-272).
- [x] **Low:** the nesting guard counts parentheses inside string literals
  (`pkg/relational/core/parser/parser.go:45,70`) and has no test. Fixed in #999 (RFC-272).
- [x] **Low:** the GROUP BY ambiguity fallback compares `GetText()` with `EqualFold`
  (`pkg/relational/core/embedded/plan_visitor.go:1767`). Fixed in #999 (RFC-272); three older 42702 mismatches are a separate section above.
- [x] **Low:** a stale `Tx` handle's Commit/Rollback clears a newer `activeTx`
  (`pkg/relational/core/embedded/connection.go:432,439`). Fixed in #999 (RFC-272).
- [x] **Low:** a self-returning `driver.Valuer` recurses without bound
  (`pkg/relational/core/embedded/param_binding.go:204`). Fixed in #999 (RFC-272).
- [x] **Low:** dead exported helpers (`functions.ApplyMathOp`, `IsTruthy`,
  `LiteralMatchesPKKind`, `ToIntegerArg`); `pkg/relational/sqldriver/txctl_statements_probe_test.go:10-12`
  contradicts the code. Fixed in #999 (RFC-272).
### Planner

- [x] **Medium:** `joinOptimizationProbesScenario` is excluded from the cross-engine gate
  (`conformance/yamsql_cross_engine_conformance_test.go:400-406`) on a historical claim that
  a 3-way or arithmetic join returns varying row counts (TODO-production.md P2.3). Its six
  queries are two-table joins, one arithmetic, and the same queries pass in the fast-lane
  `yamsql_test` (`pkg/relational/conformance/yamsql/testdata/join_optimization_probes.yaml`). Reproduce the claim or re-include
  the scenario; its arithmetic pin includes a row the yaml says does not qualify. This item
  supersedes the row-count bullet of TODO-production.md P2.3. Not reproduced; the wrong pin is fixed and the scenario is back in the gate (#1001, RFC-273).
- [x] **Medium:** `pkg/recordlayer/query/plan/cascades/max_match_map.go:472,521` reads child
  match results from a Go map and pruning keeps the first on `maxDepth` ties, so map order
  can pick the winning match (Java uses insertion-ordered maps). Fixed in #1001 (RFC-273).
- [x] **Medium:** no corpus-wide plan comparison with Java: the javacorpus runner skips every
  `explain`/`explainContains`/`planHash` assertion
  (`pkg/relational/conformance/javacorpus/testblock.go:436-437`); only a few hand-written
  conformance probes and tests compare EXPLAIN output for chosen queries. Assessed in #1001 (RFC-273); the work is the "Java-corpus plan comparison" section above.
- [x] **Medium:** the determinism fuzzer compares only `len(refA.Members())`
  (`pkg/recordlayer/query/plan/cascades/planner_fuzz_test.go:104`); check plan, EXPLAIN and
  rows across repeated plannings. Fixed in #1001 (RFC-273).
- [x] **Medium:** the context is checked only between tasks, and the fan-out inside one rule
  is not counted against `MaxNumMatchesPerRuleCall`
  (`pkg/recordlayer/query/plan/cascades/physical_wrapper.go:250-265`, `planner.go:490-530`).
  Fixed in #1002 (RFC-274): the context was already checked per binding; a cancelled call's
  memoize no longer grows the memo, and 14 rules count Java's bindings (the rest are listed in
  DIVERGENCES.md).
- [x] **Low:** quantifier order comes from map iteration
  (`pkg/recordlayer/query/plan/cascades/rule_decorrelate_values.go:320,398`). Fixed in #1001 (RFC-273).
- [x] **Low:** `pkg/recordlayer/query/executor` imports `pkg/relational/api` (`executor.go`,
  `executor_new_plans.go`, `resultset.go`, `scalar_subquery.go`). Fixed in #1002 (RFC-274); the
  `recordlayerdeps` nogo analyzer keeps non-test record-layer code off `pkg/relational`.
- [ ] **Low (planner gap):** in `FROM emp a, emp b, dept d WHERE a.did = d.did AND b.did = d.did`,
  any extra predicate tying b to a (`a.eid < b.eid`, `a.salary - b.salary > d.did * 5`) makes Go
  read the innermost leg whole: `inner=PredicatesFilter(IndexScan(IDX_EMP_DID, [*]), [2 preds])`
  with `b.did = d.did` as a residual. Without it, or with a b–d predicate, Go probes `[=]`. Java
  4.14.2.0 drives from `SCAN(DEPT)` and probes both emp legs with `IDX_EMP_DID [EQUALS q0.DID]`
  in all four shapes. Pinned by `conformance/three_way_join_sarg_java_probe_test.go` and
  `join_optimization_probes.yaml#6-#10`; find why the b leg loses the `[=]` match.

### Tests and CI

- [x] **High:** Bazel testcontainers sessions collide across PID namespaces, and Ryuk can
  exit between a suite's last terminated container and later parallel tests. Fixed in #1018:
  per-process Bazel session IDs and a process-lifetime reaper connection, pinned by
  sibling-process isolation and real-Docker last-container lifetime regressions.
- [x] **High ✔ SQL suites pass when the FDB container fails to start.**
  `pkg/relational/sqltest/testkit/embedded_fdb.go:47-56` treats every `Run` error other than
  `TerminationError` (timeout, pull, OOM) as "no Docker" and runs with an empty cluster file:
  30 packages, about 1,168 skip sites. Fail unless Docker is genuinely absent. Fixed in #998.
- [x] **High ✔ Stacked PRs get no pre-merge CI.** `.github/workflows/ci.yml`
  `pull_request.branches: [master, main]` filters on the base branch, and AGENTS.md requires
  `gh stack`. Fixed in #998.
- [x] **Medium:** docs still describe the deleted nightlies: AGENTS.md:32, `justfile:524`,
  `infra/README.md:64,74-76,228`, `docs/operations.md:82` (the differential now runs in
  ci.yml's `libfdbc-differential`), `pkg/relational/sqldriver/stress/README.md:60-80`,
  `pkg/recordlayer/metadata_user_identifier_lookup_test.go:242-244`,
  `cmd/fuzzrun/main.go:3-8,81`, `.bazelrc:73` and `.claude/skills/query-grind/SKILL.md:80`.
  Sweep `git grep -n -i nightl` for the rest. See the open nightly item in the follow-ups
  above. Fixed in #998.
- [x] **Medium:** five of the seven `.github/workflows/ci.yml` jobs have no `timeout-minutes`. Fixed in #998.
- [x] **Medium:** the Java oracle's Maven dependencies are unpinned: `maven.install` has no
  `lock_file` (`MODULE.bazel:130,275`). Fixed in #1008 (`maven_install.json`, `just maven-repin`).
- [x] **Low:** tests in no lane: `pkg/relational/core/parser/yamsql_corpus_test.go` (`yamsql`
  tag, run only by a `go test` recipe), `pkg/relational/sqldriver/stress/stress_10m_test.go`
  (`realcluster`) and `tools/bazelscaleset` (no BUILD file, no workflow). The libfdbc tests
  have their own item. Fixed in #1008: the corpus test joins `parser_test`, the 10M scales get a
  real-cluster-only target checked by `tenmguard` in test-full, and the retired
  `tools/bazelscaleset` is deleted (source at 86f05ccf8).
- [x] **Low:** `.bazelrc:40-42` sets a no-op `--test_timeout` with a wrong comment; comments
  still say `.bazelrc` sets `--local_test_jobs=4` (it sets 0, `.bazelrc:63`):
  `.github/workflows/ci.yml:619`, `infra/main.tf:226`,
  `pkg/testcontainers/foundationdb/BUILD.bazel:30`,
  `pkg/relational/sqltest/agg/distinctelision/distinct_unique_elision_cost_probe_test.go:866`;
  five hard-coded `go-version`s in ci.yml; unused `pkg/linters/gofmt`; the `golden-update`
  and `smoke-yamsql` recipes call `go test`; non-standard skips in
  `cmd/fdb-diff-oracle/fuzz_test.go` and `pkg/relational/core/ddl/fdb_ddl_test.go:100-196`. Partly done in #998 (`.bazelrc` comment, the `--local_test_jobs=4` claims, `pkg/linters/gofmt` removed; the hard-coded `go-version`s stay, since `TestWorkflowGoMatchesGoMod` blocks drift and setup-go ignores `toolchain`). The recipes and skips are fixed in #1008 (both recipes go through Bazel; the oracle skips are failures or deleted dead branches, the DDL skips use the no-Docker form).
### Docs, API and hygiene

- [x] **Medium:** the `pkg/fdbgo/fdb` package doc (`key.go:1-37`) names itself as the Apple
  binding, gives the wrong `Error()` format and says there is no per-transaction context. Fixed in #996.
- [x] **Medium:** "drop-in compatible" fails at the type level (`Transact` takes
  `func(WritableTransaction)`, Apple's takes `func(Transaction)`) but is claimed in
  `pkg/fdbgo/fdb/API_PARITY.md:3`, `pkg/fdbgo/fdb/database.go:395`, `pkg/fdbgo/doc.go:64`,
  `pkg/fdbgo/README.md:73` and, as "API-compatible", `pkg/fdbgo/fdb/key.go:3`. Fixed in #996.
- [x] **Medium:** `pkg/recordlayer` has no package doc; the `sqldriver` package example
  (`pkg/relational/sqldriver/driver.go:10-19`) does not compile (a blank import, then
  `sqldriver.RegisterDomainIfNotExists`). Fixed in #996.
- [x] **Medium:** STATUS.md's 5 s per-RPC gap omits location lookups
  (`pkg/fdbgo/client/locality.go:572`; C++ waits for the reply or a proxy change) and that a
  read fails with 1007 after 10 timed-out re-sends (`readpath.go:39`). Dials are not a gap:
  C++ also times out a connect (`CONNECTION_MONITOR_TIMEOUT`). Fixed in #996.
- [x] **Medium:** DIVERGENCES.md:973 and SQL_CONFORMANCE.md:3 are classified against Java
  4.12.11; SQL_CONFORMANCE's pass count omits the skipped files and plan assertions. Fixed in
  #1007 (RFC-278).
- [x] **Low:** stale: TODO.md:3 (PR #786 merged), `tools_cli.go:5-7` (no `cmd/frl/go.mod`),
  `pkg/fdbgo/client/CRASH_BUG.md` (resolved; title says otherwise). Fixed in #996.
- [x] **Low:** comments against CLAUDE.md's rules: shift tags
  (`git grep -n -i -E 'this (same )?shift|follow-up shift|shift investigation' -- '*.go'`),
  review rounds (`git grep -n -E 'round [0-9]+\)' -- '*.go'`: 3 of its 5 hits, e.g.
  `pkg/fdbgo/client/readpath.go:1452`, `cascades/values/values.go:2112`; also "PR #446 round 3"
  at `values.go:2051`) and "(codex)" tags in `.github/workflows/ci.yml` and `infra/main.tf:129`. Partly done in #996 and #998; left: the round-18 notes in `pkg/fdbgo/client/transaction_unit_test.go:806-817`, a shift tag at `cascades/benchmark_test.go:302`, "follow-up shift" at `query/plan/plans/insert.go:34-35` and "Codex-caught" at `pkg/relational/conformance/yamsql/testdata/uuid_column.yaml:36`.
  Done in docs/review-leftovers: every shift tag in non-test Go (several also stated stale facts),
  every "codex" tag and the listed round notes. Kept: about 30 test files and functions named
  for "RFC-141 R4 round-N" and similar review rounds (`git grep -n -E 'round-[0-9]+' --
  '*_test.go'`); renaming them is churn with no behavior change.
- [ ] **Deferred:** the public API surface: about 16K exported declarations outside
  `internal/`, including test hooks such as `values.SetCensusHooks`. Deferred by the owner on
  2026-10-25: another session's client optimizations may change the surface; do not start it.
- [ ] **Owner decision:** test and tool dependencies in the library `go.mod` (docker,
  testcontainers, ginkgo, staticcheck, cobra); nogo without `unused`; nine reused RFC numbers
  and no RFC index.

### Follow-ups from the fix PRs' reviews

- [x] **Low:** probe two seed-corpus Go extensions against live Java 4.14.2.0 and record each in
  DIVERGENCES.md's Go-extension table or drop the note
  (`pkg/relational/conformance/plandiff/seedcorpus/corpus.go`, the NOTE comments before the
  LIMIT note): bare-paren `WHERE (boolean_expr)` (4.12.11 rejected it) and a `WITH RECURSIVE`
  body with no self-reference (4.12.11 rejected it; 4.14.2.0's `QueryVisitor.java:212-214`
  routes it to `visitNamedQuery`, so Java may now accept). `plandiff/README.md:65` also cites
  CLAUDE.md "gotcha" entries that do not exist. Fixed in #1007: the parenthesized WHERE is a Go
  extension; both engines refuse the recursive form.

- [x] **Low:** when a statement's context ends inside an explicit transaction, the transaction
  is cancelled from a background goroutine with no panic recovery; `sqlpage.go` still calls
  `SetOptions` where the DST skill now recommends `SetOption` (#999's final review). Fixed in
  #1013: the abort recovers and closes the connection; sqlpage and rowdiff
  use `SetOption`.
- [ ] **Low:** STATUS.md's timeout paragraph: the facade's pipelined Get makes one more timed-out
  send than the others (#1011 owns it). Done in docs/review-leftovers: `readpath.go:415-418` and
  the two `Database` superset rows (#996's final review).
- [x] **Low:** the header of `pkg/relational/sqldriver/sql_integration_test.go` still describes a
  blank import (#996's final review). Fixed in #1013.
- [x] **Low:** enum-assignment and `range()` bound errors render their message twice since #1002;
  the `range()` test checks only "in range"; the rule-match trip's error context is unasserted
  (#1002's final review). Fixed in #1013, pinned end to end.
- [ ] **Low:** `translateFDBError`'s deserialization, already-exists, invalid-UTF-8,
  MetaDataException (42000), RecordCoreException and QueryInvalidExpression arms
  (`pkg/relational/core/embedded/connection.go`) build the message from the typed error and pass
  the chain holding it as the cause, so the message renders twice. `api.WrapStatedError` (#1013)
  renders it once, but `Error()` then stops showing the chains' `fmt.Errorf` context; decide per
  arm against Java's message (#1013 decided so for the schema-template build arm, whose name
  context Java's message lacks) (#1013's review).
- [ ] **Low:** a schema template's index refusal renders as `index "IX": XXXXX: …`: the DDL index
  paths (`pkg/relational/core/embedded/ddl.go`, `index_onsource.go`) wrap the `api.Error` with
  `fmt.Errorf("index %q: %w")`, a prefix Java's message lacks (`sqltest/core`
  `TestFDB_DDLRefusalRendersMessageOnce`, index predicate case). Decide whether to drop it or record
  it in DIVERGENCES.md (#1013's review).
- [x] **Low (#1001):** `matchResults`' structural keying was pinned only through put/get.
  `TestRecurseQueryResultValue_FoldedVariantSharesAnEntry` drives a real frame in which q.a over
  q's expansion folds to a distinct copy of q.a. Fixed in #1016.
- [x] **Low (#1001):** the fuzzer's plan comparison never faced a choice: no seed (nor 3,000
  generated inputs) put two plans in one group. It now plans over an index on `T.g`, and
  `TestPlannerDeterminismSeedsFaceAChoice` requires every seed to plan and rank alternatives.
  Fixed in #1016.
- [x] **Low (#1001):** the three-way probe goldens read the innermost `IDX_EMP_DID` leg whole;
  compared with Java's EXPLAIN (it probes the leg) and tracked as the Planner "planner gap"
  entry above. Fixed in #1016.
- [x] **Nit (#1001):** TODO-production.md P2.3 no longer cites `TestRepeatedPlanningIsStable` as
  row evidence; RFC-273 lists every field the memo rendering prints. Fixed in #1016.
- [x] **Nit (#1001):** the Java-corpus operator counts say they overlap (sum 488). Fixed in #1016.
- [x] **Nit (#1001):** the comments the review listed are two lines or fewer. Fixed in #1016.
- [x] **Nit (#1001):** `recurseQueryResultValue`'s doc no longer says the expansion is unported.
  Fixed in #1016.
- [x] **Nit (#1001):** the `corpusOnce` comment names the tests that plan the corpus themselves.
  Fixed in #1016.
- [x] **Low (#1002):** `physical_wrapper.go` distinguishes DML's partition match cap from
  per-member fan-out, which still needs the coarse task backstop. Fixed in #1016.
- [x] **Low (#1002):** the `MaxNumMatchesPerRuleCall` doc says Java's count applies only where a
  rule reports one. Fixed in #1016.
- [x] **Low (#1002):** `TestJavaBindingsSortRollsUpStoredRecord` pins the sort roll-up with plans
  that differ only in storedRecord. Fixed in #1016.
- [x] **Low (#1002):** `TestMaxNumMatchesPerRuleCallTripsTheCountingRule` asserts which rule trips
  one below its Java count (partition select, binary select, NLJ, ordering push, simple select,
  delete, update) and that the statement plans at the count. Fixed in #1016.
- [x] **Nit (#1002):** the comments the review listed are two lines or fewer. Fixed in #1016.
