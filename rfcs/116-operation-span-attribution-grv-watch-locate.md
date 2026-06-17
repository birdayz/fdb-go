# RFC-116: Operation-span attribution on the GRV, watch, and locate requests (`pkg/fdbgo`)

**Status:** Accepted (RFC review: **FDB C++ maintainer ACK** — every `file:line` re-verified at
7.3.75, fresh-root batch-span port faithful, "thread a tx span" confirmed as the divergence to avoid;
**Torvalds ACK** — executable spec real + revert-provable, concurrency respected, scope cohesive).
Re-review the implementation at HEAD (gating).

**Closes:** RFC-115 §4's explicitly-documented tracing follow-on — *"GRV (batched across txns),
watch (async long-poll), and locate (shared location cache) get no [operation] span … not cleanly
per-tx in Go's architecture; documented follow-on."* Also closes the **codex P2** raised on PR #303
(the `GetReadVersionRequest` still carries a zero `SpanContext`) and the matching `TODO.md` item.

**Spec:** C++ `libfdb_c` 7.3.75 (`/home/birdy/projects/foundationdb`, `git describe` → `7.3.75`).
All `file:line` cites are `fdbclient/NativeAPI.actor.cpp` / `fdbclient/include/fdbclient/Tracing.h`
at that tag.

---

## Problem (Go, verified `file:line`)

RFC-115 §4 landed wire SpanContext propagation for the **per-tx** request paths: reads stamp a
per-op **child** span (`readpath.go:241,453,562,821`), commit stamps the **tx** span. Three request
paths were **explicitly deferred** because they are not cleanly per-tx in Go's architecture, and
they still send a **zero** or **wrong** span:

1. **GRV** — `buildGetReadVersionRequest` (`grv.go:713`) constructs `GetReadVersionRequest` with no
   `SpanContext` (slot 6 stays zero). Every `GetReadVersion` RPC — including the **first** RPC of a
   traced transaction — is uncorrelated on the server side. (codex P2 on #303.)
2. **watch** — `sendWatch` (`readpath.go:1147`) stamps `SpanContext: span` where `span` is the
   **raw tx span** captured in `WatchSetup` (`readpath.go:1069`). C++ stamps a **child** of the tx
   span, not the tx span itself.
3. **locate** (`GetKeyServerLocationsRequest`) — `buildGetKeyServerLocationsRequest`
   (`locality.go:553,569`) sets no `SpanContext` (slot 6 stays zero); `locate`/`locateRange`
   (`locality.go:100,303`) don't thread a span at all.

In the **default** configuration (`TRACING_SAMPLE_RATE = 0.0`) every span is unsampled and the
server discards it, so this is invisible. It becomes a real fidelity gap the moment a consumer
enables sampling (`WithTracingSampleRate > 0`) or injects a `SPAN_PARENT`: the server-side GRV and
locate spans are **missing** from the trace, and the watch span is parented at the tx spanID instead
of a per-op child. Per CLAUDE.md design principle #2, a Go-vs-C++ behavioral difference on the wire
is a Go bug.

---

## C++ spec — what each path actually stamps

### GRV is batched, and the batch span is a FRESH ROOT — not any tx's span

This is the subtle one. The naive "thread a representative tx span through the batcher" is **wrong**
— C++ does not put any transaction's traceID on the GRV wire. `readVersionBatcher`
(`NativeAPI.actor.cpp:7307`):

```cpp
state Span span("NAPI:readVersionBatcher"_loc);          // :7334  — FRESH ROOT span
loop {
    when(DatabaseContext::VersionRequest req = waitNext(versionStream)) {
        ...
        span.addLink(req.spanContext);                   // :7345  — tx span linked, NOT parented
        requests.push_back(req.reply);
        ...
    }
    if (send_batch) {
        ...
        getConsistentReadVersion(span.context, cx, count, priority, flags, ...);   // :7385
        span = Span("NAPI:readVersionBatcher"_loc);      // :7389  — fresh root for next batch
    }
}
```

- The batcher span is built by `Span(const Location&)` → `Span(location, SpanContext())`
  (`Tracing.h:160`) → `SpanContext(parent.traceID, randomUInt64(), parent.m_Flags)` with a
  default-zero parent (`Tracing.h:147-148`). So the batch span starts
  **`{traceID: 0:0, spanID: random, flags: unsampled}`** — `isValid()` is *false* (zero traceID,
  `Tracing.h:56`).
- `addLink` (`Tracing.h:198-211`) records the tx span as a **link** and *only* mutates the batch
  span when the link is **sampled** and the batch span is not yet sampled: it flips the batch span
  to sampled and, because it is still invalid (zero traceID), assigns a **fresh random
  traceID + spanID**. An unsampled link changes nothing.
- At flush, `getConsistentReadVersion(span.context, …)` makes its own child
  `Span("NAPI:getConsistentReadVersion"_loc, parentSpan)` (`:7238`) =
  `{batch.traceID, fresh random spanID, batch.flags}` and stamps it on the wire request
  `GetReadVersionRequest req(span.context, …)` (`:7245`).

**Net wire SpanContext on the GRV request:**

| batch contents | wire `SpanContext` |
|---|---|
| no sampled tx (the default case) | `{traceID: 0:0, spanID: random, unsampled}` |
| ≥1 sampled tx | `{traceID: fresh-random, spanID: random, sampled}` — a **brand-new root**, *not* any tx's traceID |

The per-tx spans connect to the GRV only through **local span links**, which are not part of the
`GetReadVersionRequest` schema (it has a single `SpanContext`, slot 6) — so links never go on the
wire. The server-side GRV span lives in its own (batch) trace, linked from the transactions'.

> The causal-read-risky `attemptGRVFromOldProxies` path (`:982-986`) stamps a *different* span and
> is **out of scope** — Go has no provisional-proxy GRV path. This RFC is only the mainline
> `getConsistentReadVersion`.

### watch stamps a CHILD of the tx span

`watchValue` (`NativeAPI.actor.cpp:3933`): `state Span span("NAPI:watchValue"_loc,
parameters->spanContext);` — a **child** of the tx span. The `WatchValueRequest(span.context, …)`
(`:3965`) stamps that child. Separately, the watch's own `getKeyLocation(cx, …,
parameters->spanContext, …)` is passed the **tx** span (which makes its own locate child, below).

### locate stamps a CHILD of the (tx) span it is passed

`getKeyLocation_internal` (`NativeAPI.actor.cpp:3011`): `state Span span("NAPI:getKeyLocation"_loc,
spanContext);` → `GetKeyServerLocationsRequest(span.context, …)` (`:3036`). The range variant
`getKeyRangeLocations_internal` (`:3184`) is identical (`:3197`). The `spanContext` argument is
`trState->spanContext` (the tx span), threaded from the read/commit callers (`:3141,:3300,:3406`).

---

## Proposed Go change

Three independent commits, one per path. **Wire-compat impact:** request bytes change from a
zero/raw span to the correct operation span — **compatible** (the field already exists in the schema
and servers already parse it; unsampled spans are still discarded). No data-plane bytes (keys,
records, indexes, continuations) change. The libfdb_c differential compares the **data plane and
ignores wire trace IDs** (per the skill's differential rule + `bench` design), so it is unaffected;
proof is behavioral (below).

### 1. GRV batcher span (faithful `readVersionBatcher` port)

- Add `spanContext types.SpanContext` to `grvRequest` (`grv.go:248`); `getReadVersion`
  (`grv.go:264`) gains a `span types.SpanContext` parameter, threaded from both callers in
  `transaction.go` (`:610,:642`) as `tx.spanContext`. The cache-hit fast path needs no span (no
  RPC).
- A new pure helper folds the batch's tx spans into the wire span, 1:1 with the C++ above:

  ```go
  // batchGRVSpanContext folds a GRV batch's per-tx span contexts into the
  // readVersionBatcher's span and returns the getConsistentReadVersion CHILD to
  // stamp on the wire. 1:1 port of NativeAPI.actor.cpp readVersionBatcher
  // (:7334 fresh root, :7345 addLink) + getConsistentReadVersion child (:7238).
  func batchGRVSpanContext(txSpans []types.SpanContext) types.SpanContext {
      // Span("NAPI:readVersionBatcher") → root {traceID:0, spanID:random,
      // unsampled} (Tracing.h:147-148,:160). isValid()==false (zero traceID).
      batch := types.SpanContext{SpanID: rand.Uint64()}
      for _, s := range txSpans { // addLink (Tracing.h:198-211)
          if !isSampled(batch) && isSampled(s) {
              batch.Flags = traceFlagSampled
              if !spanContextValid(batch) { // still zero traceID → fresh random
                  binary.LittleEndian.PutUint64(batch.TraceID[0:8], rand.Uint64())
                  binary.LittleEndian.PutUint64(batch.TraceID[8:16], rand.Uint64())
                  batch.SpanID = rand.Uint64()
              }
          }
      }
      return childSpanContext(batch) // getConsistentReadVersion child (Tracing.h:147)
  }
  ```

  `spanContextValid` mirrors `SpanContext::isValid` (`Tracing.h:56`): both traceID halves non-zero
  **and** spanID non-zero. (Lives in `span.go` next to its siblings.)
- `flush` (`grv.go:323`) collects `req.spanContext` over the popped batch and passes
  `batchGRVSpanContext(spans)` down through `sendGRVRequest` → `buildGetReadVersionRequest` (both
  gain a `span` parameter). `backgroundRefresher` (`grv.go:549`) has no tx waiters, so it passes
  `batchGRVSpanContext(nil)` = `{0, random, unsampled}` — the no-sampled-link case, matching a
  C++ updater GRV.

### 2. watch child span

- `WatchSetup` already captures `txSpan := tx.spanContext` synchronously (`readpath.go:1069`, the
  data-race fix). `WatchPoll` computes the watch child **once** (stable across the retry loop,
  matching C++'s single `Span("NAPI:watchValue", …)` per actor) and stamps it on the
  `WatchValueRequest` via `sendWatch`; the watch's `locate` call (commit #3) is passed the **tx**
  span (`txSpan`), not the child — exactly as C++ passes `parameters->spanContext` to
  `getKeyLocation` while stamping `span.context` on the watch request.

### 3. locate child span

- `locate`/`locateRange`/`refresh` (`locality.go`) gain a `span types.SpanContext` parameter;
  `buildGetKeyServerLocationsRequest` stamps `childSpanContext(span)` (`Tracing.h:3017` child).
  Thread `tx.spanContext` from the read callers (`readpath.go:165,391,642`), the watch locate
  (`readpath.go:1102`, the captured `txSpan`), the metrics/estimation callers (`metrics.go:45,175`),
  and `transaction.go:751,2010,2027`.

---

## Executable spec (proof)

Behavioral tests in `pkg/fdbgo/client` (real FDB testcontainer; `t.Parallel`, unique prefixes),
each **revert-proven** (back out the stamp → test reddens):

- **GRV — `batchGRVSpanContext` table test** (pure, deterministic):
  - empty / all-unsampled batch → traceID **zero**, **unsampled** (spanID is random → assert only
    "non-... not pinned"); revert: returning a tx span instead makes traceID non-zero → red.
  - ≥1 sampled tx → **sampled** flag set **and** traceID **non-zero** **and** the traceID is **not
    equal** to the sampled tx's traceID (proves the fresh-root model, catching the naive
    "thread the tx span" mistake). Mixed sampled+unsampled order-independence.
- **GRV — wire round-trip**: a batch with a sampled tx produces a `GetReadVersionRequest` whose
  marshalled-then-parsed `SpanContext` is sampled + non-zero (the stamp reaches the wire bytes).
- **watch**: the `WatchValueRequest` carries a span with the **same traceID** and **same flags** as
  the tx span but a **different spanID** (child, not raw tx span). Revert (`span` → keep raw) → red.
- **locate**: a cache-miss `GetKeyServerLocationsRequest` carries a span that is a **child** of the
  issuing tx span (same traceID/flags, fresh spanID); a sampled tx yields a sampled locate span.
  Revert (drop the stamp) → span goes zero → red.
- `-race` over `//pkg/fdbgo/client` for the touched paths; `--runs_per_test=10` on the watch/locate
  tests (async + shared-cache concurrency).

No `cmd/fdb-diff-oracle` change: the `SpanContext` **encoding** is unchanged (RFC-115 §4 already
pinned it); this RFC changes only **which** context is stamped, which the byte oracle doesn't model.

---

## What this is NOT

- Not a Layer-2 (OpenTelemetry) change. RFC-115 §4 gives GRV/watch/locate **no otel child span**
  ("not cleanly per-tx"); that stays. This is **Layer 1 (wire) only** — the wire SpanContext now
  matches C++ for these three paths.
- Not the `attemptGRVFromOldProxies` causal-risky GRV (Go has no provisional-proxy path).
- Not a tx-span change for commit (RFC-115 §4 correctly stamps the tx span there; unchanged).
```
