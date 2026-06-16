# RFC-115: Production-readiness round 2, wave 2 — degraded-cluster behavior, tracing, verification depth (`pkg/fdbgo`)

**Status:** Accepted (RFC). Reviewed clean: **FDB C++ maintainer ACK** (validated every C++ claim against
`/tmp/fdbsrc` 7.3.75; ACK'd the §1 *timed re-admission* recovery design — full `connectionKeeper` port NOT
required — and the §3 quorum non-bug; required citation fixes folded: `LoadBalance.actor.h:607`→`:790`,
`SPAN_PARENT` 33-byte = `8+16+8+1`, `Optional<Error>` is a union not a bare table). **Torvalds ACK**
(conditions met: §1 structural fork resolved to one path; §1 proof gate is the clock-free *selection*
assertion, latency is corroboration only). `/code-review` clean (conventions + consistency). The gating
§1/§4 ACKs re-apply to each impl HEAD, not just this RFC.
Closes the remaining RFC-113 punch-list after RFC-114 (R2-CRITICAL #1 latency + #2
connection-failure visibility) merged in #302. This wave covers the SERIOUS degraded-cluster items
(#3 bounded `GetRange`, #4 dead-server LB exclusion, #5 coordinator quorum) and the MEDIUM
verification-depth + tracing items (#6 wire-oracle/fuzz per-PR, #7 inline-error verification, #8
distributed tracing). Per the user's scoping decision this is **one RFC for the whole remaining
punch-list**; implementation still lands as **one logical change per commit** (each item independently
revert-provable) on a single stacked branch, driven via the `fdb-client-engineer` workflow
(FDB-C-dev + Torvalds + `/code-review` on the RFC, then again on each impl delta; codex + @claude on
the PR).
**Spec:** FoundationDB C++ `libfdb_c` at tag **7.3.75** (the `foundationdb` pin in `MODULE.bazel`);
the differential oracle is the Apple CGo binding over `libfdb_c`. C++ is the spec.
**Scope:** `pkg/fdbgo` only (`client/`, `transport/`, `fdb/`, `wire/`, `cmd/fdb-schema-extract`,
`cmd/fdb-diff-oracle`) + `.github/workflows`. NOT the record layer / SQL / planner.

**Wire-compat impact summary.** Five of six items are **zero-wire** (LB selection, `GetRange`
materialization/docs, coordinator-quorum non-bug, CI gating). **Two touch bytes:** (a) the tracing item
(#8) *populates* the currently all-zero `SpanContext` request field → the serialized request bytes change
(zero → a real, default-**unsampled** trace id), wire-**compatibly** (the field already exists in every
request schema and servers already parse it); (b) the inline-error marshal fix (#7) corrects a
known-mis-encoded `Optional<Error>` writer in the **generated** wire types so a reply error the client
*emits* is byte-identical to C++. Both go through full wire review even though neither is a *format*
change.

---

## Where round 2 stands after #302

RFC-114 (#302, now HEAD on master) closed both R2-CRITICAL operability gaps: latency DDSketches
(read/commit/GRV/total, Prometheus summaries) and connection-failure visibility (counters + a single
edge-triggered `Warn` sink at `recordConnFailure`, `topology.go:181`). **Wire-correctness was already
launch-grade; observability is now adequate to operate the client.** What remains is exactly the class
RFC-113 tiered as SERIOUS/MEDIUM: how the client *behaves when the cluster degrades*, plus tracing and
verification depth. None requires an architectural change; the sharpest one (#4) is a true `libfdb_c`
divergence whose fix is gated on getting the *recovery* mechanism right (§1).

This wave does **not** re-open anything #302 closed and explicitly **retires #5** as a verified
non-bug (§3) — adding a quorum there would make Go *stricter* than `libfdb_c`, a conformance
violation, not a fix.

---

## §1 (R2-SERIOUS #4) — dead storage servers are not excluded from read load-balancing  *(true C++ divergence)*

### Problem (Go, verified `file:line`)

`chooseServer` (`client/loadbalance.go:95`) and `chooseTopTwo` (`:171`) build their candidate set
**purely** from the QueueModel's timed backoff — `now > d.failedUntil` (`:109`, `:188`). And
`failedUntil` is written in **exactly one place**: the `futureVersion` branch of `endRequestFull`
(`:274-279`, error codes 1009/1037). It is **never** set from the connection failure monitor.
`failMon.isFailed` (`client/failure_monitor.go:59`) is documented *"Used by tests only."* — the load
balancer never consults it.

Consequence: a storage server whose TCP connection just died (so `recordConnFailure` →
`failMon.markFailed`, `topology.go:181-182`) **keeps being selected as the hedge primary and the hedge
target** on the very next read to its shard. The read pays a full dial timeout (`DefaultRPCTimeout`,
the `getOrDial` ctx at `database.go:418`) before the sequential fallback (`readpath.go:512-527`)
rescues it from a live replica. Correctness is preserved (the fallback finds a live server); it is
**wasted tail latency on every read to a dead shard**, and a divergence from the C++ load balancer.
The failure monitor wired for RFC-114's observability is exactly the signal the LB should consult —
so the plumbing already exists.

### C++ spec (`fdbrpc/include/fdbrpc/LoadBalance.actor.h`, 7.3.75)

The failure-monitor gate is a **separate, earlier** filter than the QueueModel backoff:

```cpp
// loadBalance(), :499 — failure-monitor gate FIRST
if (!IFailureMonitor::failureMonitor().getState(thisStream->getEndpoint()).failed) {
    auto const& qd = model->getMeasurement(thisStream->getEndpoint().token.first());
    if (now() > qd.failedUntil) {                       // :501 — QueueModel backoff SECOND
        double thisMetric = qd.smoothOutstanding.smoothTotal();
        ...
} else {
    ++badServers;                                       // failed alternative skipped
}
```

`basicLoadBalance` applies the same gate (`:790`: `if (!IFailureMonitor::...getState(...).failed) break;`;
its own all-down branch is at `:804`).
So C++ layers **two** independent gates: (1) `IFailureMonitor.failed` (connection health, binary) and
(2) `QueueData.failedUntil` (per-error timed backoff). Go currently implements **only (2)**.

The **all-down** branch (`:617-637`) blocks until a server recovers, rather than picking a known-dead one:

```cpp
if (!stream && !firstRequestData.isValid()) {            // Everything is down!
    std::vector<Future<Void>> ok(alternatives->size());
    for (int i = 0; i < ok.size(); i++)
        ok[i] = IFailureMonitor::failureMonitor().onStateEqual(
            alternatives->get(i, channel).getEndpoint(), FailureStatus(false));
    Future<Void> okFuture = quorum(ok, 1);
    if (!alternatives->alwaysFresh()) wait(allAlternativesFailedDelay(okFuture));
    else wait(okFuture);
}
```

### The recovery question (the crux — what re-admits an excluded server)

C++ can afford a *binary* failure gate because **FlowTransport self-heals failed peers in the
background, independent of any LB traffic**:

- `connectionKeeper` (`fdbrpc/FlowTransport.actor.cpp:804-819`) keeps reconnecting a peer **even when
  it has zero outstanding/unsent messages** — the `retryConnect` path breaks the idle wait after
  `FAILURE_DETECTION_DELAY`/`SERVER_REQUEST_INTERVAL` and re-dials (`:840`).
- On a successful (re)connect it calls `IFailureMonitor::...setStatus(dest, FailureStatus(false))`
  (`:847`), and on failure `setStatus(..., true)` after `FAILURE_DETECTION_DELAY` (`:944`).
- `connectionMonitor` (`:641-722`) actively pings idle connections (`CONNECTION_MONITOR_LOOP_TIME` /
  `CONNECTION_MONITOR_TIMEOUT`), throwing `connection_failed()` on ping timeout.
- A Peer is only evicted from the `peers` map when fully dereferenced (`:1025-1035`) — there is **no**
  time-based GC; until then it silently reconnects.

**Go has no equivalent.** The pure-Go client is **dial-on-demand**: `handleConnError` *evicts* the
dead conn from the pool and marks it failed (`topology.go:163-171`), and `markAlive` fires **only** on
a subsequent successful `getOrDial` (`database.go:438-445`). `transport/conn.go`'s `connectionMonitor`
(`:864`) pings *existing pooled* conns but there is no background reconnect of an *evicted* peer. So if
the LB simply skips `failMon.isFailed` servers, a server that recovers is **never re-dialed and stays
permanently excluded** from any shard where a live replica exists — a worse bug than the one we're
fixing. **The fix is the gate *plus* a recovery path; the gate alone is incorrect.**

### Proposed Go change

**(a) Add the failure-monitor gate, 1:1 with `:499`.** In `chooseServer`/`chooseTopTwo` candidate
construction, skip `failMon.isFailed(addr)` endpoints **before** the existing `now > failedUntil` /
QueueModel scoring. (Drop the "tests only" comment on `isFailed`.) This is the binary connection-health
gate (1), layered above the existing timed backoff (2) — matching C++'s two-gate structure exactly.

**(b) All-candidates-failed → fall back to "try anyway", NOT a blocking quorum.** C++ blocks on
`quorum(ok,1)` of `onStateEqual(FailureStatus(false))` because its background keeper will flip a peer
healthy without any request. Go's dial-on-demand model makes the *dial attempt itself the probe*: when
every candidate is `isFailed`, keep the existing all-failed branch (`loadbalance.go:114-126`,
soonest-`failedUntil`) so a real read re-dials a candidate — success → `markAlive`, failure → fast
re-fail. A read therefore never stalls forever waiting on recovery (it either recovers via the dial or
fails fast and the tx retry loop / `ctx` deadline governs), which is the Go-faithful equivalent of the
C++ block. **Documented divergence** (Go has no `connectionKeeper`); DIVERGENCES.md gets the entry.

**(c) Recovery via a timed re-admission probe (RECOMMENDED) — the piece Go must add in lieu of the
keeper.** Give `failureMonitor` a per-endpoint `failedSince` + bounded backoff so the LB *re-admits* an
excluded endpoint as a probe candidate after a window, even while no live replica forces a dial there.
Concretely: the candidate filter skips an endpoint iff `failMon.isFailed(addr) && now <
failedSince+backoff`; past the window it is re-admitted (one probe), a real read dials it, and
`markAlive` (already wired, `database.go:445`) clears the flag on success while another failure re-stamps
`failedSince` with grown backoff (reuse the `futureVersionBackoff` growth/cap constants,
`loadbalance.go:56-58`). This reproduces the **observable** C++ end-state — excluded while down,
self-healed on recovery — using Go's existing dial-on-demand + `markAlive` machinery rather than a
persistent-peer reconnect loop.

> **FDB-C-dev decision point — RESOLVED (ACK'd).** The structural choice is settled: implement **(c)**,
> the timed re-admission probe. The FDB C++ maintainer ACK'd (c) as a *faithful observable substitute* —
> excluded-while-down, self-heals when a real read re-dials past the window and `markAlive` clears the
> flag, never permanently strands — and explicitly confirmed the **full `connectionKeeper` port is NOT
> required**. Critically, there is **no correctness hole on the all-down path**: C++'s `quorum(ok,1)` is
> *not* an unconditional block — it is wrapped in `allAlternativesFailedDelay` (`LoadBalance.actor.h:634`)
> and rides the same 5 s / `ctx` transaction ceiling that Go's "try-anyway" fallback honors, so
> Go-re-dialing-as-probe and C++-waiting-on-keeper-flip reach the same end-state and neither surfaces an
> error the other wouldn't. The discarded 1:1 alternative (a background `connectionKeeper` analog) is
> recorded here only as the rejected option; do not implement it. (The gating ACK still applies to the
> impl HEAD, not just this RFC.)

### Executable spec (proof)

- **Deterministic fault test** (`client/fault_test.go`, extend `dropReplyConn`/`faultDialer`): a 3-replica
  shard where one replica's conn is dead. Assert (1) — **the hard gate, asserted with no clock** — while
  ≥1 live replica exists, the dead server is **never** returned by `chooseServer`/`chooseTopTwo` as
  primary or hedge target (a pure selection assertion). The fact that the read then completes well under
  `DefaultRPCTimeout` (no dial-timeout paid) is a latency *observable* used only as corroboration, **never**
  the pass/fail condition — a timing measurement must not be the gate (§5 discipline / the #288 lesson);
  (2) after the backoff window the dead endpoint is
  re-admitted as a probe and, once its conn is restored, a subsequent read selects it again (recovery, no
  permanent exclusion); (3) all-failed → the read still attempts (no deadlock), governed by `ctx`.
  **Make the race structurally impossible to lose** (drop the reply / hold the dial), never a timing
  gamble — per the §5 discipline (the #288 lesson). **Revert-prove:** remove the `isFailed` skip → the
  "dead server not chosen" assertion reddens; remove the timed re-admission → the recovery assertion
  reddens.
- `-race` over `//pkg/fdbgo/client`, `--runs_per_test=10` to prove the recovery determinism.

**Wire-compat impact: none** (server selection only; identical bytes on the wire).

---

## §2 (R2-SERIOUS #3) — unbounded `GetRange` materializes the whole result → OOM  *(shared hazard, NOT a divergence)*

### Problem (Go, verified `file:line`)

`getRangeImpl` (`client/readpath.go:594`) sets `remaining = math.MaxInt` when `limit<=0` (`:601-602`)
and accumulates every shard into one `allKVs` slice (`:689`) with no total byte/row ceiling. The common
facade path `GetSliceWithError` (`fdb/range_result.go:102`) uses `effectiveLimit` → `math.MaxInt32`
(`:64-68`, `:107`) and **ignores `StreamingMode`**. A large unbounded scan materializes the entire
result (≈×2 with the return copy at `:112-115`) and OOMs the process. The 80 KB per-reply limit bounds
each round-trip, not the total.

The `RangeOptions.Mode` doc is **factually wrong** (`fdb/range.go:125`): *"Ignored by the pure Go client
(all reads use exact mode internally)."* In fact `Iterator().Advance()` **does** honor `Mode` via
`batchSize(...)` (`fdb/range_result.go:152`); `Mode` is ignored **only** by `GetSliceWithError`. So the
doc both (a) claims a no-op that isn't and (b) steers users toward the unbounded `GetSliceWithError` and
**away** from the mode-respecting `Iterator()`.

### Why this is NOT a divergence to "fix" by default

The Apple Go binding over `libfdb_c` *also* implements `GetSliceWithError` by appending batches until the
range is exhausted — the C API bounds each *batch*, never the *total*, and never returns a clean "too
big" error. A **default-on** total-byte ceiling that errors would make Go's facade **diverge from the
cgo oracle**. The default must stay oracle-matching; the fix is OOM-*safety* + honest docs **without**
changing default behavior.

### Proposed Go change

1. **Correct the `Mode` doc** (`fdb/range.go:125`): state that `Mode` is honored by `Iterator()` (the
   streaming path) and ignored by `GetSliceWithError` (which always fetches all), and point users at
   `Iterator()` for large/unbounded ranges. Keep the existing `GetSliceWithError` `WARNING` godoc
   (`range_result.go:99-101`).
2. **Opt-in total ceiling, off by default.** Add an opt-in option (e.g. a `RangeOptions` field or a
   per-transaction/database knob — exact surface decided in review) bounding total rows and/or bytes
   materialized by `GetSliceWithError`/`getRangeImpl`; when exceeded, return a **structured error**
   (`errors.As`-matchable, carrying the cap and the count reached). **Default unset → behavior is exactly
   today's** (oracle-matching unbounded append). Never a default clean-error.

### Executable spec (proof)

- **FDB integration test** (real testcontainer): with the opt-in cap set low, a scan that exceeds it
  returns the structured error (not OOM, not a silent truncation); with the cap unset, the same scan
  returns the full result unchanged (oracle-matching). **Revert-prove:** removing the cap check makes the
  "errors above cap" assertion red.
- A `godoc`/doc test (or a `// Mode is honored by Iterator()` assertion in an existing range test)
  pinning the corrected behavior so the doc can't silently rot back.

**Wire-compat impact: none.**

---

## §3 (R2-SERIOUS #5) — coordinator quorum  *(VERIFIED NON-BUG — close, document, no code)*

RFC-113 flagged this **"verify before acting, do not make Go stricter than `libfdb_c`."** Verified
against the 7.3.75 client source — **Go already matches C++; there is no divergence.**

`tryAllCoordinators` (`client/database.go:539`) is first-reply-wins. The C++ **client** is too:

- `getLeader()` computes `majority = bestCount >= nominees.size()/2 + 1`
  (`fdbclient/MonitorLeader.actor.cpp:578`) — but that bool is **server-side leader-election metadata**;
  the function returns the most-voted leader **regardless** of `majority`.
- `monitorLeaderOneGeneration` (`:583-636`) aggregates whatever nominee replies have arrived and calls
  `getLeader(nominees)` **without** any `quorum(...)` wait — it acts on the current best, even if only
  one coordinator has answered.
- `monitorProxiesOneGeneration` (`:840-982`) contacts coordinators **round-robin, one at a time**, and
  adopts the **first successful** `OpenDatabaseCoordRequest` reply (`:919-937`): `repFuture =
  clientLeaderServer.openDatabase.tryGetReply(req, ...)` → `if (rep.present()) { successIndex = index;
  allConnectionsFailed = false; }`. There is **no** `quorum(...)`, no majority gate on client topology
  adoption.

So **adding a coordinator quorum to Go would make it *stricter* than `libfdb_c`** — a conformance
violation, not a fix.

Also reconfirmed (RFC-113 #6b): cluster-file re-read is **failure-gated** in C++ too — only adopted
under `allConnectionsFailed && storedConnectionString.getNumberOfCoordinators() > 0`
(`MonitorLeader.actor.cpp:888-900`), where `allConnectionsFailed` is set after cycling all coordinators
with no success (`:975-979`, gated by `COORDINATOR_RECONNECTION_DELAY`). RFC-111's forward-following +
failure-gated re-read is therefore C++-faithful; **do not add a periodic timer.**

### Action

No code change. **Close as a verified non-bug.** Add a code comment at `tryAllCoordinators`
(`database.go:539`) documenting that first-reply-wins is **deliberate C++-faithful** behavior (cite
`MonitorLeader.actor.cpp:919-937` + `:578` for the majority-is-server-side distinction), so the next
auditor doesn't re-file it. Update DIVERGENCES.md / the TODO mark accordingly.

**Wire-compat impact: none** (no change).

---

## §4 (R2-MEDIUM #8) — distributed tracing: populate `SpanContext`, honor `SPAN_PARENT`

### Problem (Go, verified `file:line`)

Every request type carries a `SpanContext` slot (`GetValueRequest` slot 5, `GetReadVersionRequest` slot 6,
`CommitTransactionRequest` slot 9, `GetKeyServerLocationsRequest` slot 6, plus `GetKeyValuesRequest`,
`GetKeyRequest`, `WatchValueRequest` — confirmed across `wire/types/*request*_generated.go`) but it is
**always zero-valued** (no assignment to `.TraceID`/`.SpanID` outside generated/test code). The tracing
transaction options — `SetDebugTransactionIdentifier`, `SetLogTransaction`, `SetTransactionLoggingEnable`,
`SetServerRequestTracing` (`fdb/options.go:116/175/179/290`) — are accepted-but-ignored no-ops, and the
`SetSpanParent` option that exists (`fdb/options.go:340`) is an accepted-but-ignored no-op stub that
**discards** its bytes (so distributed-trace-parent injection silently does nothing).

### C++ spec — a default client does NOT send a zero `SpanContext`

This reframes RFC-113's "lower urgency, just populate it": C++ generates a **random, default-unsampled**
span **per transaction** and stamps it on every request. Go sending all-zero is therefore a *behavioral*
divergence (minor — both wire-parse; servers ignore unsampled spans — but not byte-for-byte what C++
emits).

- `SpanContext` = `{ UID traceID /*2×uint64*/, uint64 spanID, TraceFlags m_Flags /*uint8*/ }`,
  default `unsampled` (`fdbclient/include/fdbclient/Tracing.h:46-61`).
- `generateSpanID()` (`fdbclient/NativeAPI.actor.cpp:3458-3471`) **always** generates a random
  `traceID`+`spanID`; the **sampled** flag is set iff `deterministicRandom()->random01() <=
  FLOW_KNOBS->TRACING_SAMPLE_RATE`, else `unsampled`.
- `TRACING_SAMPLE_RATE` default = **`0.0`** (`flow/Knobs.cpp:88`) → a default client emits a random
  **unsampled** span on every request.
- Stamped on every outgoing request: GRV (`:985-986`), GetValue (`:3677-3678`), commit (`:6169`).
- **`SPAN_PARENT`** option (`:7126-7133`): a **33-byte** serialized parent `SpanContext` — the size
  check at `:7128` is exact. The 33 bytes are **8 (the `IncludeVersion` protocol-version header) + 16
  (`traceID`, 2×uint64) + 8 (`spanID`) + 1 (`flags`)**; Go MUST emit/parse the 8-byte version prefix, not
  just the 25-byte struct body. `span.setParent(...)` copies the parent's `traceID`+flags and assigns a
  fresh random `spanID` (`Tracing.h:237-242`) — the distributed-tracing injection hook.
- Default tracer is `NoopTracer` (`fdbclient/Tracing.actor.cpp:323`); `LogfileTracer` /
  `FastUDPTracer` are opt-in export backends (`openTracer`, `:329-350`).

### Proposed Go change (scope split: wire-faithful core vs. export extension)

**Core (wire-faithful, this RFC):**
1. **Per-transaction `SpanContext`:** generate a random `traceID` (2×uint64) + `spanID` (uint64) at
   transaction start, flag **unsampled** by default (sampled iff a configurable sample rate fires;
   default rate `0.0` → unsampled, matching `TRACING_SAMPLE_RATE`). Stamp it into every request type that
   carries the slot. Regenerate per transaction / on `Reset()` (C++ `cloneAndReset` →
   `generateSpanID`).
2. **`SPAN_PARENT` option:** accept the 33-byte serialized parent context, deserialize, and use it as the
   transaction's parent (copy `traceID`+flags, fresh `spanID`) — the cross-process correlation hook.
3. Wire the existing trace no-op options to their real local effect where cheap (`DEBUG_TRANSACTION_IDENTIFIER`
   sets a client-side debug id; `SERVER_REQUEST_TRACING` marks server-side tracing) — matching C++
   `setOption` semantics (`NativeAPI.actor.cpp:6998-7059`). (Trace-*log* emission is the export concern,
   below.)

**Export (Go extension, follow-on — NOT required for wire fidelity):** an otel/`slog`-style hook to
*emit* spans (the `LogfileTracer`/`FastUDPTracer` analog). Out of scope for the wire-faithful core;
filed as a follow-on so the core ships without waiting on a collector integration.

### Executable spec (proof)

- **Fixed-context differential** (the differential can't match C++'s *random* ids, so pin the
  *encoding*): serialize a **fixed** `SpanContext` through the Go wire type and assert byte-identical to
  the C++/cgo encoding of the same fixed context (extend `cmd/fdb-diff-oracle` with a `SpanContext`
  fixture, or a hand-pinned golden against `libfdb_c`). Round-trip `SPAN_PARENT`'s 33-byte format
  byte-exact.
- **Behavioral test:** a transaction's requests now carry a **non-zero, unsampled** span; two txns get
  **distinct** trace ids; `Reset()` re-anchors a fresh id; `SPAN_PARENT` makes the child's `traceID`
  equal the injected parent's. Revert-prove (drop the stamp → span goes zero).
- Fuzz the `SpanContext` (de)serializer in `cmd/fdb-diff-oracle` (it is already in the
  discover-all-`Fuzz*` set, §5).

**Wire-compat impact: YES (bytes change, compatibly).** Request bytes go from a zero `SpanContext` to a
real (default-unsampled) one — the field already exists in the schema and servers already parse it, so
this is wire-**compatible**, but because it changes serialized bytes it goes through full wire review and
the fixed-context differential above.

---

## §5 (R2-MEDIUM #6) — promote the wire-type oracle + a fuzz smoke to per-PR

### Problem / current state (verified)

The cross-client **data-plane** differential is **already per-PR** (`nightly-libfdbc.yml` runs on
`push` + `pull_request`, RFC P2.8 — header lines 11-13). What remains **nightly-only** is the
`cmd/fdb-diff-oracle` **wire-type** oracle + the deep fuzz, in `nightly-fuzz.yml` (`schedule: cron '17 3
* * *'` — **not** referenced in `ci.yml`):

- *"Run deterministic tests"* (`nightly-fuzz.yml:37-40`): `go test -run=TestDiff` against the C++ oracle
  binary — **fast** (seconds), corpus + seed replay across all oracle-compared types.
- *"Fuzz ALL oracle-compared types"* (`:42-69`): every discovered `Fuzz*` at **6 min each** (~1h45m) —
  **too slow for per-PR**.

So a wire regression in a less-traveled reply type (e.g. one of the 8 reply-parse types — `GetValueReply`,
`GetKeyReply`, `GetKeyValuesReply`, `GetReadVersionReply`, `Error`, `Endpoint`, `ClientDBInfo`,
`OpenDatabaseCoordRequest`) can merge green and live ~a day until the nightly.

### Proposed change

- **Promote the fast `TestDiff*` deterministic wire-oracle tests to per-PR** (`ci.yml`): build
  `//cmd/fdb-diff-oracle:diff_oracle_bin` and run `go test -run=TestDiff ./cmd/fdb-diff-oracle/` on the
  self-hosted box (it has the C++ toolchain). Seconds-scale; catches a deterministic wire regression in
  any oracle-compared type before merge.
- **Add a short fuzz *smoke* per-PR:** seed-corpus replay (`go test -run=^$ -fuzz=… -fuzztime=15s` per
  target, or a fixed corpus replay) — a fast tail that surfaces an obvious marshal break without the
  1h45m nightly cost. **Keep the deep 17×6min fuzz nightly** (it stays the exhaustive net).
- Keep the empty-discovery no-op guard (`nightly-fuzz.yml:52-55`) on the per-PR variant so the gate can't
  silently become a no-op.

### Executable spec (proof)

Add the `ci.yml` step(s); deliberately break a reply marshal on a branch and confirm the **per-PR** job
reddens (revert-prove the gate, not just its presence).

**Wire-compat impact: none** (CI gating only).

---

## §6 (R2-MEDIUM #7) — close the inline-error verification gap

### Problem (verified, from RFC-113 #8 + the skill wire-truths)

Real FDB delivers read-path wrong-shard / future-version / process-behind via the **inline**
`LoadBalancedReply.error` (`Optional<Error>`) field — `storageserver.actor.cpp` `sendErrorWithPenalty`
— **not** the root `ErrorOr` union. On the read side Go parses it correctly via `wire.ReadInlineReplyError`
(the generated `.Error` field mis-decodes it — a documented schema-extractor bug, since `Optional<Error>`
serializes as a flatbuffers **union**: a **1-byte type tag** + a **4-byte `RelativeOffset`** to a nested
Error table — *not* a bare table and *not* a length-prefixed string; `LoadBalancedReply.error` is the
`Optional<Error>` at `LoadBalance.actor.h:72-76`). But:

1. the **generated writer** still mis-encodes `Optional<Error>` (schema-extractor bug), and
2. the fault harness (`client/fault_test.go`) can only inject a **root** `ErrorOr`, so the inline-error
   arm of `parseGetKeyReply`/`parseGetKeyValuesReply` is exercised **only by hand-pinned fixtures, never
   on a real reply** — the one place "byte-identical on the wire" is asserted but unproven on the read
   error path.

### Proposed change

1. **Fix the `Optional<Error>` marshal in the schema extractor** so the **writer emits the union type
   tag + RelativeOffset** (the missing piece today), regenerating byte-identical to C++ (`cmd/fdb-schema-extract` — fix the
   *extractor*, regenerate; **never** hand-edit generated code, per the wire-types rule), so a reply error
   the client emits encodes byte-identical to C++.
2. **Build the inline-error fault path** (extend `fault_test.go`): an `inlineErrorConn` that replaces the
   next reply frame with a *successful* reply carrying a **non-empty** `LoadBalancedReply.error` (the
   deferred `SimTransport` sliver, scoped to exactly this path), driving `parseGetKeyReply` /
   `parseGetKeyValuesReply`'s inline arm end-to-end.

### Executable spec (proof)

- **Anti-self-confirming:** inject the **canonical literal** `1001` (wrong_shard_server), **never** the
  code-under-test's own `ErrWrongShardServer` constant (the §"Testing discipline" P6 rule — injecting the
  constant passes for any value it holds, exactly how the 1062 bug stayed green).
- Assert the client surfaces the right code from an **inline** reply (and that a `1006`/all-alternatives
  inline error is absorbed, never surfaced). Round-trip the fixed `Optional<Error>` encoding against the
  C++ oracle (a `cmd/fdb-diff-oracle` fixture). Revert-prove: back out the marshal fix → the byte-equality
  assertion reddens.

**Wire-compat impact: the marshal fix is a wire-correctness fix** (the emitted `Optional<Error>` bytes
must match C++); the fault-injection plumbing is test-only.

---

## Priority & sequencing (one PR, one logical change per commit)

| # | Item | Tier | Divergence? | Wire bytes? | Rough size |
|---|------|------|-------------|-------------|-----------|
| §1 | Dead-server LB exclusion + recovery | SERIOUS | **Yes (true)** | none | medium (recovery design) |
| §2 | Bounded `GetRange` (opt-in cap + `Mode` doc) | SERIOUS | no (shared hazard) | none | small |
| §3 | Coordinator quorum | SERIOUS | **no — verified non-bug** | none | doc-only |
| §4 | Distributed tracing (`SpanContext` + `SPAN_PARENT`) | MEDIUM | behavioral | **yes (compatible)** | medium |
| §5 | Wire-oracle + fuzz-smoke per-PR | MEDIUM | n/a (CI) | none | small |
| §6 | Inline-error verification (`Optional<Error>`) | MEDIUM | wire-correctness | **yes** | small–medium |

**Recommended commit order:** §3 (doc-only, retires an item) → §2 (small, no wire) → §5 (CI gate, makes
§6 cheaper to verify) → §6 (wire-correctness fix the oracle now covers) → §1 (the real divergence; needs
the recovery-design ACK) → §4 (wire-byte change; lands last so the differential/oracle are already
per-PR). §1 and §4 are the two that **must** carry an FDB-C-dev ACK on the implementation HEAD, not just
the RFC.

## Recommendation / grade

With #302 merged, the client sits at **~8/10** (wire-correct + observable). This wave targets
**unqualified solo-production operability**: §1 removes the per-read dial-timeout penalty to dead shards
(the last SERIOUS *latency-under-degradation* divergence), §2 removes the OOM foot-gun, §3 confirms the
coordinator path is already correct, §4 brings tracing to C++ behavioral parity + distributed-trace
injection, and §5/§6 close the verification depth on the read error path. **Top two to land first for
operability impact: §1 (dead-server exclusion) and §4 (tracing) — both gated on an FDB-C-dev ACK of the
design before code.**

## What this is NOT

- Not a periodic coordinator/cluster-file timer (§3 — would diverge from C++).
- Not a default-on `GetRange` ceiling (§2 — would diverge from the cgo oracle).
- Not a trace *export* backend (§4 core — that's a follow-on Go extension).
- Not a full `connectionKeeper` port (§1 — recommended recovery reuses dial-on-demand + `markAlive`;
  the keeper analog is the documented heavier alternative pending FDB-C-dev's structural ACK).
