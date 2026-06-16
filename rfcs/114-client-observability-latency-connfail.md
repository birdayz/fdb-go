# RFC-114: Client observability round 2 — latency distributions + connection-failure visibility

**Status:** Proposed. Implements the two R2-CRITICAL punch-list items from RFC-113
(`rfcs/113-client-prod-readiness-round2.md`): #1 latency metrics, #2 connection-failure visibility.
Extends RFC-097 (`ClientMetrics` + `fdbmetrics` Prometheus handler + slog), which deliberately deferred
both. Driven via the `fdb-client-engineer` workflow (FDB-C-dev + Torvalds + codex + @claude).
**Spec:** FoundationDB C++ `libfdb_c` 7.3.75 (vendored at the Bazel `foundationdb+` external).
**Scope:** `pkg/fdbgo/client` + `pkg/fdbgo/fdbmetrics`. **Wire-compat impact: none** — both items are
pure client-local instrumentation (latency sampling + counters + logs); no request/reply bytes change.

## Problem (from RFC-113)

The pure-Go client exposes 17 monotonic counters and **zero latency distribution** — no GRV/read/commit
p50/p90/p99, the first SLI an operator pages on. And connection/dial failures are **invisible**:
`handleConnError` (`client/topology.go:162`) and `handleDialError` (`client/database.go:316`) feed
`failMon.markFailed` but emit **no slog event and no counter** — a flapping proxy or dead storage server
produces nothing in logs or dashboards.

## C++ spec — `DatabaseContext` latency DDSketches

C++ tracks six latency/size distributions on `DatabaseContext` as `DDSketch<double>`
(`DatabaseContext.h:657`), default-constructed (`NativeAPI.actor.cpp:1585`) ⇒
**`errorGuarantee = 0.005`** (`DDSketch.h:220`), `γ = (1+eG)/(1-eG)`. The three this RFC ports
(latency, in **seconds** — C++ `now()` is seconds-as-double):

| C++ member | Sampled at | Go analog site |
|---|---|---|
| `readLatencies` | GetValue reply (`NativeAPI.actor.cpp:3698`) | `getValue` round-trip (`client/readpath.go`) |
| `commitLatencies` | commit success (`:6681`) | commit round-trip (`client/commitpath.go`) |
| `GRVLatencies` | GRV reply (`:7417`) | GRV reply applied (`client/grv.go`) |
| `latencies` (total tx) | commit success (`:6682`, `now()-startTime`) | `now − tx.creationTime` at commit success |

Surfaced in the `TransactionMetrics` TraceEvent (`:661-693`): the aggregate `latencies` emits
mean/median/**p90/p98**/max; the per-category ones emit mean/median/max.

**DDSketch port (`DDSketchBase`, `DDSketch.h:86-168`):** `addSample(v)`: `v ≤ EPS(1e-18)` →
`zeroPopulationSize++`; else bucket `index = ⌈log(v)/log(γ)⌉`, `count[index]++`; track
`populationSize`, `sum`, `min`, `max`. `percentile(p)`: `rank = p·(pop−1)`; if `rank < zeroPop` → 0;
else walk buckets (ascending if `p ≤ 0.5`, descending otherwise) to the bucket holding `rank`, return
the representative value `2·γ^index/(1+γ)`. `mean = sum/pop`, `median = percentile(0.5)`.

**Two deliberate, documented divergences (local metric, zero wire impact):**
1. **Exact `log` instead of C++'s `fastLogger` bit-hack.** C++'s `DDSketch` (fast variant) approximates
   `log` with a bit-trick + `correctingFactor` purely for speed (`DDSketch.h:229-232`); the canonical
   `DDSketchSlow` uses exact `log`. Go uses exact `math.Log` — same γ, same relative-error guarantee,
   strictly *more* accurate. A read/commit/GRV is a network round-trip (ms); a `math.Log` (ns) is noise.
2. **Sparse `map[int]uint64` buckets** instead of C++'s pre-sized `2·offset` vector — negative indices
   (sub-unit-second latencies, the common case) are natural map keys, no offset bookkeeping. percentile
   sorts the keys at snapshot time (snapshot is not the hot path).

## Design

### A — Latency metrics (R2-CRITICAL #1)

- **`ddsketch.go`** — a faithful `DDSketchBase` port: `addSample(float64)`, `percentile(p)`, `mean()`,
  `median()`, `max()`, `count()`, `sum()`; `errorGuarantee = 0.005` to match C++. Mutex-guarded
  (`addSample` is the only writer; `Snapshot` the only reader). Unit-tested against the C++
  relative-error guarantee (every reported quantile within `±0.5%·trueValue`) + fuzzed (no panic, all
  quantiles within `[min,max]`, monotone in `p`).
- **`ClientMetrics`** gains `readLatency`, `commitLatency`, `grvLatency`, `totalLatency *ddsketch`
  with `observeReadLatency(d)` / `observeCommitLatency(d)` / `observeGRVLatency(d)` /
  `observeTotalLatency(d)` helpers (seconds).
- **Sample sites** (each measures `now()` at send, samples on the successful reply; a retried/failed op
  does not sample, matching C++ which samples on the reply handler):
  - `getValue` → `readLatency` (C++ readLatencies; getKey/getRange are *not* separately tracked by C++
    either — out of scope, noted).
  - commit success → `commitLatency` (round-trip) **and** `totalLatency` (`now − tx.creationTime`).
  - GRV reply applied → `grvLatency` (the per-batch GRV round-trip; cache hits don't sample, matching
    C++ whose cached path returns before the sample).
- **`ClientMetricsSnapshot`** gains a `LatencyStats{Count, Sum, Mean, Median, P90, P99, Max}` (seconds)
  per category. C++ emits p90/p98 only for the aggregate; Go exposes median/p90/p99 **per category** — a
  documented superset (local metric, operator-standard tail, zero wire impact). p99 is the conventional
  Prometheus tail (vs C++'s trace-only p98).
- **`fdbmetrics`** renders each as a Prometheus **summary**: `fdb_client_{read,commit,grv,transaction}_latency_seconds`
  with `{quantile="0.5"|"0.9"|"0.99"}` + `_sum` + `_count`. (Summaries over all-time, not a decay window —
  honest and matches the all-time DDSketch; documented.)

### B — Connection-failure visibility (R2-CRITICAL #2)

C++ has no `DatabaseContext` *counter* for connection failures, but emits connection-lifecycle
`TraceEvent`s (FlowTransport `ConnectionClosed`/peer-failure) and drives `IFailureMonitor`. The Go analog
is slog events at the unified failure sink + Go-only counters (the precedent is RFC-110's
`recoveredPanics` — a Go-only observability counter with no C++ `CounterCollection` twin).

- **Counters** (Go-only, documented as such): `clientConnectionFailures` (incremented in
  `handleConnError`, `topology.go:162` — the single sink both `handleConnError` and a live-ctx
  `handleDialError` route through) and `coordinatorChanges` (incremented when a coordinator forward is
  followed, `topology.go:127`).
- **slog**: `handleConnError` → `Warn("fdb client connection failed", "address", addr)`;
  `handleDialError` → `Debug` with the dial error detail (it already routes to `handleConnError` for the
  Warn + counter when the ctx is live, so no double-count/double-Warn); coordinator change keeps its
  existing `Info` log (`topology.go:127`) + the new counter. All guarded `db.logger != nil` /
  `Enabled()` like RFC-097.
- **Snapshot + `fdbmetrics`**: `ClientConnectionFailures`, `CoordinatorChanges` →
  `fdb_client_connection_failures_total`, `fdb_client_coordinator_changes_total` (counters).

## Test plan

- **DDSketch**: relative-error guarantee (uniform + lognormal samples, every quantile within ±0.5%),
  zero/empty/single-sample edges, monotonicity in p, `-race`, fuzz (no panic, quantile ∈ [min,max]).
- **Latency (real FDB testcontainer)**: after N commits + N point reads + GRVs, the snapshot reports
  `Count == N`, `Max ≥ Median > 0`, and `Sum ≈ Mean·Count`; read-only txns contribute to `readLatency`
  but not `commitLatency`/`totalLatency` (C++-parity, pinned). Revert-proof: removing a sample site
  zeroes that category's `Count`.
- **Connection failure (deterministic, `faultDialer`)**: a dial against a dropped/closed conn increments
  `ClientConnectionFailures` and emits the Warn event (captured via the per-handle `slog` handler, never
  `SetDefault`); a coordinator forward increments `CoordinatorChanges`. Revert-proof: removing the
  increment reddens the assertion.
- **`fdbmetrics`**: text-exposition output contains the summary quantile lines + `_sum`/`_count` and the
  two new counters; `WriteText` golden updated.
- Full `just test` green; `-race` over `//pkg/fdbgo/client`.

## What this is NOT

`mutationsPerCommit`/`bytesPerCommit` (C++ has them; per-commit *size* distributions, not latency) are a
documented follow-on, out of scope for the latency item. No periodic trace emission (consumer scrape is
the analog, per RFC-097). No per-key conflict attribution. No decaying-window summaries (all-time DDSketch).
