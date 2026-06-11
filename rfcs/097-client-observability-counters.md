# RFC-097: Client observability — transaction counters, export hook, slog events (P1.3)

**Status:** draft — needs FDB-C++-dev + Torvalds ACK (client item; fdb-client-review gate)
**Scope:** TODO-production.md P1.3 + the P1.2 remainder ("emit operational events through
the slog path"). Client (`pkg/fdbgo`) only; record-layer events (online-indexer progress)
are the P1.2 record-layer half and stay a separate item.

## Problem

The pure-Go client is operationally blind. There is no way to answer, on a running
production handle: how many commits succeeded, how many hit conflicts (1020), how many
ended `commit_unknown_result`, how often transactions are retrying, whether the
ratekeeper is throttling. `StoreTimer` (record layer) is in-memory only and carries no
commit-conflict/retry counters; the client has nothing at all. For a SaaS control plane
this is the difference between diagnosing a conflict storm from a dashboard and
diagnosing it from `tcpdump`.

## C++ anchor

C++ counts exactly this on `DatabaseContext` (`DatabaseContext.h:585-635`,
`CounterCollection cc` — logged periodically as `TransactionMetrics`):
the operationally-critical subset is incremented at well-defined sites we already
mirror structurally:

| C++ counter | C++ increment site |
|---|---|
| `transactionsCommitStarted` | `commitMutations` (`NativeAPI.actor.cpp:6808`) — **NOT** tryCommit entry; the empty/read-only fast path returns at `:6800-6806` WITHOUT incrementing either commit counter. Go's read-only `Commit` fast path must mirror (no counting), or read-only `Transact` loops inflate commit counts. `readOnly`-option commits throw `transaction_read_only` AFTER Started (`:6810-6811`) — Started−Completed asymmetry (failed/in-flight) is intentional. |
| `transactionsCommitCompleted` | tryCommit success (`:6673`) |
| `transactionsNotCommitted` (the conflict counter) | `onError` 1020 arm (`:7749`) |
| `transactionsMaybeCommitted` | `onError` 1021 arm (`:7751`) |
| `transactionsResourceConstrained` | `onError` 1042/1078 arm (`:7754`; a second site in `updateBackoff` `:3160` on the location-lookup path — mirrored if/where Go has the corresponding backoff) |
| `transactionsProcessBehind` | `onError` 1037 arm (`:7756`) |
| `transactionsThrottled` | `onError` 1051/1213 (`:7758`), 1223 (`:7760`) |
| `transactionsTooOld` / `transactionsFutureVersions` | `onError` 1007/1009 arms (`:7770/:7772`) |
| `transactionReadVersionsCompleted` (+ per-priority) | `extractReadVersion` (`:7428-7440`) |

Notes pinned by review: C++ retries `database_locked` (1038) and
`blob_granule_request_failed` (1079) with NO counter (`:7743-7747`) — Go's
aggregate `transactionRetries` counts them anyway (documented Go-only aggregate,
not a per-code C++ counter). `commitDummyTransaction` (`:6306-6344`) commits a
real transaction through `commitMutations`/tryCommit, so its commits and onError
retries hit the same counters — Go counts the dummy identically. Counters are
monotonic, so poll deltas ≡ C++'s periodic trace deltas; the only gap vs C++'s
trace emission is no persisted history if nothing polls before process death —
accepted and documented.

## Design

1. **`ClientMetrics` on `database`** — a struct of `atomic.Int64` counters using the
   C++ NAMES (the subset above), incremented at the Go sites that already mirror the
   C++ increment sites 1:1: the `OnError` retry-classification arms
   (`transaction.go:1288/:1302` switch), commit start/success in `Commit`/`commit`,
   and the per-transaction GRV consumption point (the `extractReadVersion` analog).
   One Go-only aggregate, `transactionRetries` (total OnError-sanctioned retries),
   because "retries/sec" is the single most useful operational number and C++ only
   exposes it per-transaction (`trState->numErrors`).
2. **Export = poll, zero core deps:** `Database.Metrics() ClientMetricsSnapshot` — a
   plain value struct of `int64`s read atomically. No export goroutine, no sink
   interface, no new core dependencies (matching the project's interfaces-only P1.2
   decision). Poll IS the export hook: Prometheus collectors and OTel async gauges
   are pull-based consumers of exactly this shape.
   **The Prometheus adapter is an IN-SCOPE deliverable of this RFC** (P1.3 says
   "ship a Prometheus/OTel adapter as a separate optional package + example" — a
   deliverable, not garnish): a separate `pkg/fdbgo/fdbmetrics` optional package
   exposing the counters as an `http.Handler` in the **Prometheus text exposition
   format** (the format every Prometheus server scrapes), plus a runnable example.
   Deliberately NOT `prometheus.Collector`: that would pull
   `github.com/prometheus/client_golang` into the module's go.mod for ~12 monotonic
   counters, against the no-new-deps decision; the text format is ~60 lines, fully
   scrapeable, and a user who wants a `Collector` writes a trivial one over
   `Metrics()` (documented in the package). P1.3 is not checked off until it ships.
   *(Adapter shape changed from `prometheus.Collector` to zero-dep text handler
   after the RFC ACKs — flagged for the implementation review.)*
3. **slog events (P1.2 remainder), same source as the counters:** the `database`
   gains a per-handle logger (`DatabaseOptions.Logger *slog.Logger`, nil →
   `slog.Default()` — still standard-library-only, no new logging API; the global
   default remains the zero-config integration point per the P1.2 decision, the
   per-handle field exists so tests and multi-tenant hosts never mutate process
   globals). A single internal `emit` helper increments the counter and (guarded by
   `logger.Enabled(ctx, level)`) logs the event:
   - `commit_unknown_result` → `Warn` (rare, operationally significant: ambiguous
     write);
   - conflict (1020), throttle, too-old, process-behind, resource-constrained retry
     events → `Debug` (hot-path; a conflict storm at `Warn` would melt the log — the
     counter, not the log, is the storm signal; rates belong on dashboards).
   The `Enabled` guard keeps the disabled-logging hot path at one atomic add + one
   branch.
4. **What this is NOT:** no periodic `TransactionMetrics` trace emission (C++ logs
   counters on a timer into its trace files — Go's analog is the consumer polling
   `Metrics()`); no per-request byte/key counters (C++ has ~40 more counters — out of
   scope until something needs them); no record-layer events.

## Wire-compat statement

No wire bytes change. Counter increments and guarded debug logs on existing paths.
The only hot-path cost: one atomic add per counted event; one `Enabled` check per
loggable event.

## Test plan

- Counter increments live at PER-TRANSACTION sites (the OnError arms, commit
  start/success, the per-txn GRV consumption point in ensureReadVersion) — NOT in
  `applyGRVReply`, which is shared with the background refresher and would pollute
  per-handle numbers with refresh traffic. Counter-delta assertions are safe under
  `t.Parallel()` because `openTestDB` returns a fresh `*Database` (fresh counters)
  per test.
- Unit: a `Transact` loop against real FDB with a forced conflict (two txns with
  overlapping read/write conflict ranges committed concurrently — the existing
  conflict-test pattern) asserts `transactionsNotCommitted` and `transactionRetries`
  advance; a clean commit advances `transactionsCommitStarted/Completed` AND
  `transactionReadVersionsCompleted` (the commit's GRV) — and no error counters;
  `commitDummyTransaction`'s dummy commits are counted as commits (matching C++,
  whose dummy goes through the same tryCommit) — pinned explicitly so the choice is
  deliberate.
- slog: tests inject a capturing handler via the per-handle
  `DatabaseOptions.Logger` (never `slog.SetDefault` — process-global mutation races
  every `t.Parallel()` test in the package): assert the 1021 path emits the Warn
  event with the expected attributes, and that `Debug`-level events do NOT reach a
  handler whose level is `Info` (the Enabled guard works).
- `-race` on the client package (concurrent Transact loops hammering the counters).
- Revert-proof: removing an increment site reddens the corresponding assertion.
