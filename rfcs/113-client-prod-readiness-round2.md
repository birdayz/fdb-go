# RFC-113: Production-readiness round 2 — operational observability & degraded-cluster robustness (`pkg/fdbgo`)

**Status:** Assessment + punch-list (snapshot at HEAD of `feat/client-prod-readiness-punchlist`,
after RFC-097/106/107/111/112). Not a single change — an audit that files the *next* wave of work
items, each to be driven to completion on its own stacked branch via the `fdb-client-engineer` skill
(RFC → FDB-C-dev + Torvalds + codex review → implement → review-clean).
**Companion to** `rfcs/prod-readiness-go-client.md` (round 1). It deliberately does **not** re-file the
items round 1 already closed — RFC-111 (cluster-file re-watch, round-1 P0.1), RFC-112
(`SetTimeout`→RPC-ctx, round-1 P0.2), RFC-097 (client counters + Prometheus handler + slog retry
events, P1.3). This round is the class round 1 *under-weighted*: operational **observability depth**
and **degraded-cluster behavior**, not common-path wire correctness.
**Spec:** FoundationDB C++ `libfdb_c` at tag **7.3.75** (the `foundationdb` pin in `MODULE.bazel`).
C++ is the spec; the differential oracle is the Apple CGo binding over `libfdb_c`.
**Scope:** `pkg/fdbgo` only (`client/`, `transport/`, `fdb/`, `wire/`). NOT the record layer / SQL / planner.

**Wire-compat impact: no schema/format change and no compatibility impact for any item here.** That is
itself the headline finding — round 1 closed the wire-correctness axis (the hard line, and the
strongest part of the client); what remains is operability and availability/latency under a degrading
cluster. One caveat for implementers: the tracing item (R2-MEDIUM #8) *populates* an existing, currently
all-zero `SpanContext` request field — that changes the serialized request bytes (zero → a real trace
id), wire-*compatibly* (the field already exists in the schema and servers already parse it), so its
implementation still goes through wire review even though it is not a format change.

---

## Verdict

**Wire-correctness is launch-grade. Operability is not — and that, not correctness, is what now gates
production.** You can trust this client with your data today: it will not corrupt a shared Java+Go
cluster. You cannot yet *operate* it at scale — the first time the cluster degrades you are flying
blind (no latency metrics, connection failures invisible in logs and counters), and several
degraded-cluster paths are handled measurably worse than `libfdb_c` (a large unbounded scan OOMs the
process instead of erroring; a connection-dead storage server keeps being selected for reads).

> **Adopt now for the data path behind a flag, with the RFC-109 `libfdb_c` escape hatch compiled in;
> not yet ready to be the sole client for a cluster you must keep alive at 3am.** The expensive,
> dangerous-to-get-wrong part — wire/data correctness — is done and well-defended. The remaining work
> is mostly observability and a handful of degraded-path divergences, none requiring an architectural
> change.

This refines round 1's "operational robustness: good, 2 gaps" grade. Both of those gaps (cluster-file
rotation, `SetTimeout`) are now closed. A fresh four-axis survey at HEAD surfaces a *different* class
that round 1 did not enumerate, listed below with `file:line` evidence and the C++ spec anchor.

## Method

Four independent dimension surveys against HEAD (API completeness, resilience/failure-handling, test
maturity, observability/ops), each reading the actual source + the 7.3.75 C++ spec, not the docs. Every
load-bearing claim is cited `file:line`; the two sharpest new findings (#4 OOM, #5 LB) were
hand-verified against the code, not taken from a survey. Findings already tracked or already closed are
called out as such rather than re-filed.

## Already closed since round 1 — do NOT re-file

- **RFC-111** — cluster-file re-watch / coordinator-set rotation follow (round-1 P0.1). The
  "unrecoverable strand on `coordinators auto`" gap is closed for the *forward-following* path.
- **RFC-112** — `SetTimeout` threaded into every read RPC wait ctx + GRV + locality (round-1 P0.2). A
  hung-but-alive read now aborts at the tx deadline, not after `10×5s`.
- **RFC-097** — `ClientMetrics` (17 counters: 13 C++-named + 4 Go-only — `grvCacheHits`,
  `transactionRetries`, `recoveredPanics`, `recoveredPanicsConsecutiveMax`), `fdbmetrics` Prometheus
  text handler, slog retry events. RFC-097 explicitly **deferred** latency/histograms and per-request byte counters
  ("C++ has ~40 more counters — out of scope until something needs them"). Finding #1 below is exactly
  that deferral coming due.
- **RFC-106/106b** — statement resource limits; the per-query MEMORY budget (106b) is a `pkg/relational`
  SQL-layer concern, **distinct** from finding #4 (a `pkg/fdbgo` client-side range-materialization OOM).

---

## Findings (tiered)

### CRITICAL — operability: you cannot see the client work or fail

**1. No latency metrics anywhere.** `ClientMetrics` is 17 monotonic counters and zero histograms —
no GRV / read / commit latency distribution (`grep -ic 'histogram\|latency\|percentile\|observe'
client/clientmetrics.go` → `0`). Tail latency is the first SLI an operator pages on, and it is
invisible. **C++ anchor (verified, 7.3.75):** `DatabaseContext` holds six latency distributions as
**`DDSketch<double>`** — `latencies, readLatencies, commitLatencies, GRVLatencies, mutationsPerCommit,
bytesPerCommit` (`DatabaseContext.h:657`) — fed via `.addSample()` at the read/commit/GRV sites and
surfaced as p90/p98/median/max in the **`TransactionMetrics` TraceEvent** (`NativeAPI.actor.cpp:661`).
(Note: 7.3 uses `DDSketch`, not the pre-7.x `ContinuousSample`; these surface in the trace event, *not*
the client status JSON.) RFC-097 deliberately scoped these out. This is the single biggest operability gap.

**2. Connection / dial failures are invisible.** `handleDialError` (`client/database.go:316`) and
`handleConnError` feed `failMon.markFailed` (`client/failure_monitor.go:22`) but emit **no slog event
and increment no counter**. The entire `client/`+`transport/` layer has ~8 log sites total — cluster-file
persist warnings (`clusterfile.go:223/245`), coordinator-forward warnings (`topology.go:114/121/127`),
the RFC-097 retry event (`clientmetrics.go:196`), and one transport panic-backstop `Error`
(`transport/conn.go:632`). A flapping proxy or a dead storage server produces **no log and no metric** —
you debug it from latency symptoms alone. There is no connection-failure / coordinator-change /
dial-failure counter at all (the only failure-adjacent counter is RFC-110's `recoveredPanics`, which is
orthogonal — goroutine-panic recovery, not connection health).

**3. No distributed tracing.** The `SpanContext` wire field is serialized into every request but is
always zero-valued (no assignment to `.TraceID`/`.SpanID` outside generated/test code); all tracing
transaction options (`SetDebugTransactionIdentifier`, `SetLogTransaction`, `SetTransactionLoggingEnable`,
`SetServerRequestTracing`) are accepted-but-ignored no-ops (`fdb/options.go`). No otel. You cannot
correlate a slow Go transaction into FDB's own trace events. (The RFC-109 `libfdb_c` backend *does*
forward these — the escape hatch restores tracing the pure-Go client lacks.)

### SERIOUS — degraded-cluster behavior (availability/latency, not corruption)

*(Divergence status differs per item: #5 is a true `libfdb_c` divergence; #4 is a hazard **shared with**
the cgo oracle, not a divergence; #6a is unconfirmed.)*

**4. Unbounded `GetRange` materializes the whole result → OOM (a hazard shared with `libfdb_c`, not a
divergence).** *(verified)*
`getRangeImpl` (`client/readpath.go:584`) sets `remaining = math.MaxInt` when `limit<=0`
(`readpath.go:592`) and accumulates every shard into one `allKVs` slice (`readpath.go:679`) with no
total byte/row ceiling; the common facade path `GetSliceWithError` ignores `StreamingMode` and uses
`effectiveLimit → math.MaxInt32` (`fdb/range_result.go:64,102`). A large unbounded scan materializes
the entire result (≈×2 with the return copy) and OOMs the process instead of returning a clean error.
The bounded path (`Iterator()`) exists, but the `RangeOptions.Mode` doc — *"Ignored by the pure Go
client (all reads use exact mode internally)"* (`fdb/range.go:125`) — actively steers users **away**
from it. The 80 KB per-reply limit bounds each round-trip, not the total. **Caveat (codex catch):** the
Apple Go binding over `libfdb_c` *also* implements `GetSliceWithError` by appending batches until the
range is exhausted — the C API bounds each *batch*, never the total, and never returns a clean "too big"
error. So a *default-on* total-byte ceiling that errors would make Go's facade **diverge from the cgo
oracle**, not match it. The fix must be OOM-safety + honest docs **without** changing default behavior:
correct the misleading `Mode` doc + point users at the bounded `Iterator()`, and offer any hard ceiling
as an **opt-in** option (off by default), not a default clean-error. Not a wire change.

**5. Dead servers are not excluded from read load-balancing.** *(verified)* `chooseServer`/`chooseTopTwo`
(`client/loadbalance.go:95-228`) build the candidate set purely from `now > d.failedUntil`
(`loadbalance.go:109`) — and `failedUntil` is set **only** by the `future_version` backoff branch, never
from the connection failure monitor (`failMon.isFailed`, which `handleConnError`/`markFailed` populate).
So a connection-dead storage server keeps being selected as primary (and as the hedge target), costing a
full dial-timeout per read before the sequential fallback (`readpath.go`) rescues it. **C++ anchor:**
`loadBalance` (`fdbrpc/LoadBalance.actor.h`) filters alternatives through `IFailureMonitor` *before* the
QueueModel. Correctness is preserved (fallback finds a live server); it is wasted latency on every read
to a dead shard, and a divergence from the spec.

**6. Coordinator robustness — one real residual, one confirmed non-bug.** Round 1 called the parallel
first-reply-wins coordinator probe (`client/database.go`) "benign … same outcome, faster"; a fresh
pessimistic read, validated against the C++ 7.3.75 source, splits it:
- *(a)* **No coordinator quorum — divergence UNCONFIRMED, verify before acting (codex catch).**
  `tryAllCoordinators` (`database.go:539`) is first-reply-wins. C++ `getLeader()` *does* compute
  `majority = bestCount >= nominees.size()/2 + 1` (`MonitorLeader.actor.cpp:578`) — but that majority bool
  governs leader *election* on the coordination record; it is **not yet confirmed** that the libfdb_c
  *client*'s coordinator-contact path (`OpenDatabaseCoordRequest` / `monitorProxies`) gates topology
  *adoption* on it rather than also taking the first successful reply. If the C++ client also takes
  first-success, then Go matches it and **adding a quorum would make Go *stricter* than libfdb_c** — a
  conformance violation, not a fix. Confirm the client-side gating first; file only if a true divergence
  is proven.
- *(b)* **Cluster-file re-read is failure-triggered — and this MATCHES C++ (confirmed non-bug).** My
  initial read suspected Go lacked a "healthy-timer" re-read that C++ had. **It checked out the other
  way:** C++ has no such timer either. `ClusterConnectionFile.actor.cpp` exposes only on-demand
  `upToDate()`; the actual adoption of a rewritten on-disk cluster string happens in
  `monitorProxiesOneGeneration` **only under `allConnectionsFailed`** (`MonitorLeader.actor.cpp:888`,
  gated by `COORDINATOR_RECONNECTION_DELAY`). So an operator rewriting `fdb.cluster` while the old
  coordinators still answer is not picked up *in C++ either*. RFC-111's forward-following +
  failure-gated re-read is therefore C++-faithful — **close this as a non-bug / document the behavior**,
  do NOT add a periodic timer (it would diverge from C++).

### MEDIUM — verification depth on the error/fault paths

**7. The deepest checks are nightly, not per-PR.** The full 17-type C++ wire oracle
(`cmd/fdb-diff-oracle`) and the 4-min/target fuzz run nightly; per-PR you get the data-plane bench
differential + the record-layer gold gate (promoted per RFC P2.8). A wire regression in a rarely
exercised reply type can merge green and live ~a day. (Round 1 P2.8 / this branch already moved the
record-layer gold gate per-PR — this is the residual for the wire-type oracle + high-value fuzz.)

**8. Inline-error reply parsing is known-mis-marshaled and never verified on a real reply.** Real FDB
delivers read-path wrong-shard via the **inline** `LoadBalancedReply.error` field; the generated writer
mis-encodes `Optional<Error>` (a documented schema-extractor bug), and the fault harness can only inject
*root* `ErrorOr`, so `parseGetKeyReply`/`parseGetKeyValuesReply`'s inline-error arm is exercised only by
hand-pinned fixtures. The one place "byte-identical on the wire" is asserted but unproven on a real
reply — and it sits on the read error path. The planned `SimTransport` that would close it was deferred
as YAGNI.

**9. No model-based / invariant oracle for the client.** `pkg/recordlayer/chaos` is record-layer-specific
(shadows records/index aggregates, injects at the record-layer tx boundary); the client gets only
*transitive* coverage. Client-internal paths (RYW snapshot logic, conflict ranges, pipelined reads under
fault) are not modeled. (TODO C3 — port FDB's `workloads/*.actor.cpp` Cycle/AtomicOps/Serializability to
the client — remains open.)

---

## Punch-list (prioritized, actionable — each → its own impl RFC)

**R2-CRITICAL — close before "operate it solo in production":**
1. **Latency observability.** Add GRV/read/commit latency sampling to `ClientMetrics` (the C++
   `ContinuousSample`/latency-band analog) and expose distributions through the `fdbmetrics` Prometheus
   handler (histogram or summary). Highest leverage; the #1 missing SLI. *(extends RFC-097.)*
2. **Connection-failure visibility.** Emit slog events + counters at `handleDialError`/`handleConnError`
   and on coordinator change, so a flapping proxy/storage server is visible in logs and dashboards. Cheap,
   high-value, pairs with #1.

**R2-SERIOUS — degraded-cluster correctness of behavior:**
3. **Bound `GetRange` against OOM — without changing default behavior.** This is a hazard shared with
   the cgo oracle (whose `GetSliceWithError` also materializes unbounded), **not** a divergence to
   "fix" — so the default must stay oracle-matching. Deliverables: (a) fix the misleading
   `RangeOptions.Mode` doc and point users at the bounded `Iterator()`; (b) offer any total-byte/row
   ceiling as an **opt-in** option (off by default) that errors above the cap. Do **not** make the
   default `GetSliceWithError` return a clean "too big" error — that would diverge from the cgo oracle.
4. **Consult the failure monitor in load-balancing.** Filter `chooseServer`/`chooseTopTwo` candidates
   through `failMon.isFailed` before the QueueModel, matching C++ `loadBalance`.
5. **Coordinator quorum — verify, don't pre-commit (finding #6a).** First confirm whether the libfdb_c
   *client* coordinator-contact path gates topology adoption on `getLeader()`'s majority or also takes
   first-success. **Only if** a true divergence is proven, decide between porting the quorum and
   documenting first-reply-wins as deliberate — but do **not** add a quorum if C++ takes first-success
   (that would make Go stricter than libfdb_c). Finding #6b (failure-gated cluster-file re-read) is
   **already C++-faithful — close as a non-bug**; do not add a periodic timer.

**R2-MEDIUM — verification depth:**
6. **Promote the wire-type oracle + high-value fuzz to per-PR** (or a fast subset), so a wire regression
   in a less-traveled type is caught before merge, not nightly.
7. **Close the inline-error verification gap** (finding #8): fix the `Optional<Error>` extractor marshal
   and build the fault path that injects an inline reply error end-to-end (the deferred `SimTransport`
   sliver, scoped to this one path).
8. **Distributed tracing** (finding #3): populate `SpanContext` and/or wire an otel integration — lower
   urgency than #1/#2; the escape hatch covers it in the interim.

## Recommendation

- **Use it now** for the read/write/atomic/versionstamp/tenant/range data plane — that surface is
  well-proven against both `libfdb_c` and Java and shares a cluster safely.
- **Operate it with** a bounded `ctx` on every `Transact`, **bounded `GetRange` limits** (never an
  unbounded scan until R2-SERIOUS #3 lands), and the RFC-109 `libfdb_c` escape hatch compiled in for the
  critical path.
- **Top 2 to close** for unqualified solo-production operability: latency metrics (R2-CRITICAL #1) and
  connection-failure visibility (R2-CRITICAL #2). Neither is on the prior launch stack; both are small.
