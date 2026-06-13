# RFC-106: Per-statement resource governance (timeout, memory budget, scan-limit wiring)

**Status:** Draft
**Item:** Client launch-readiness #3 (TODO.md) — TODO-production P1.9 (resource limits /
backpressure). Gate: **query-engine (Graefe)** + Torvalds + codex — this is executor/cascades
work, so Graefe substitutes for the FDB C++ dev. **Java (fdb-record-layer 4.11.1.0) is the spec
for the parity parts; the statement timeout + memory byte-budget + result-size cap are Go-only
read-path extensions Java lacks (allowed per "query reach may exceed Java" — they need deep test
coverage and never touch the wire).**

## Problem (multi-tenant safety — one tenant must not OOM/wedge a shared host)

The Go SQL engine has NO per-statement resource ceiling. Today's governance:

- **Scan-level limits exist and are ENFORCED at leaf cursors** (`scan_properties.go`:
  `ReturnedRowLimit`, `ScannedRecordsLimit`, `ScannedBytesLimit`, `TimeLimit`) — but they are
  **per-PAGE** (a fresh cursor per `fetchPage`/transaction): they PAGINATE, never error, and never
  sum across continuations (`cascades_generator.go:1096` sets only `WithTimeLimit(txPageTimeLimit=4s)`).
- **The SQL options that should drive them are DEAD** — `OptMaxRows`, `OptExecutionScannedRowsLimit`,
  `OptExecutionScannedBytesLimit`, `OptExecutionTimeLimit`, `OptTransactionTimeout`
  (`api/options.go`) have **zero non-generated consumers**. A tenant can't bound a query and the
  server can't be configured to.
- **Eager buffering is capped but un-surfaced:** `MaterializationLimit` (100k rows, NLJ inner /
  buffered union / INSERT source / recursive-CTE / DFS-join — `executor.go:2635`) and
  `DefaultMaxSortBufferRows` (5M, `streaming_cursors.go`) throw Go error structs that are **not
  mapped to a SQLSTATE** (`translateExecError`, `cascades_generator.go:1143`) — they surface as a
  generic internal error.
- **Unbounded buffers with NO cap:** `DISTINCT`'s dedup `seen` map (`executor.go:861`) grows with
  cardinality; the hash-join probe index. A `SELECT DISTINCT` over a high-cardinality column can
  OOM the host.
- **No whole-statement wall-clock** anywhere (only the per-page 4s + FDB's per-tx 5s). A query that
  consumes many inputs per output (aggregate, deep join) can run unbounded across continuations.

`FailOnScanLimitReached` (`scan_properties.go:114`) is a **dead field** — Java throws
`ScanLimitReachedException` (54F01) when set; Go never does.

## Proposed change

Five pieces, smallest-blast-radius first. All surface as **errors** (SQLSTATE `54F01`
`ErrCodeExecutionLimitReached`, already defined `errcode.go:135`), never crashes.

1. **Map the existing limit errors to 54F01 (operability, do first).** `translateExecError`
   (`cascades_generator.go:1143`) maps `*MaterializationLimitExceededError` and
   `*SortBufferExceededError` (and the new errors below) to `ErrCodeExecutionLimitReached`. Today
   they fall through untranslated. No behavior change beyond the surfaced SQLSTATE.
2. **Wire the dead scan-limit options → `ExecuteProperties` (PARITY — Java maps these).** At the
   single chokepoint `paginatingRows.fetchPage` (`cascades_generator.go:1096`), build the props
   from the connection `Options`: `OptExecutionScannedRowsLimit→WithScannedRecordsLimit`,
   `OptExecutionScannedBytesLimit→WithScannedBytesLimit`, `OptMaxRows→` page `ReturnedRowLimit`.
   Implement `FailOnScanLimitReached`: when set, a leaf cursor that hits `ScanLimitReached`/
   `ByteLimitReached` returns a `*ScanLimitReachedError` (→ 54F01) instead of paginating — Java's
   `ExecuteProperties.setFailOnScanLimitReached(true)` semantics. Default off (paginate, unchanged).
3. **Statement timeout (Go-only extension).** A wall-clock deadline spanning the WHOLE statement
   (all pages/continuations), distinct from the per-page/per-tx limits. Source: `OptExecutionTimeLimit`
   (or a `statement_timeout` SET). Implement by wrapping the statement's execution ctx
   (`cascadesPlan.Execute`, `cascades_generator.go:798`) in `context.WithTimeout`; every cursor
   already honors `ctx.Err()` (`executor.go:2640`, `streaming_cursors.go:430`), so the deadline
   propagates with no per-operator change. Map the deadline error → 54F01
   (distinct message "statement timeout"). The per-tx FDB timeout is unaffected.
4. **Per-query memory byte budget (Go-only extension).** A `MemoryLimitBytes` field on
   `ExecuteProperties` (sibling to `MaterializationLimit`, `scan_properties.go:124`; 0 = unlimited,
   the default — no behavior change unless set). A per-statement atomic accumulator
   (`queryMemoryBudget`) charges the bytes a buffering operator holds and returns
   `*MemoryLimitExceededError` (→ 54F01) when the running total exceeds the budget. Charge sites
   (the operators that can grow unbounded): `CollectAllBounded` (`executor.go:2635`), the sort
   buffers (`streaming_cursors.go:452/548`), the `DISTINCT` `seen` map (`executor.go:861`), the
   hash-join probe (`streaming_cursors.go:663`). Bytes = a cheap row-size estimate (tuple-encoded
   length), not exact heap accounting — the budget is a safety ceiling, not a profiler.
5. **Result-size (bytes) cap (Go-only extension).** Optional `OptMaxResultBytes`: `paginatingRows`
   accumulates the encoded size of returned rows across the statement and errors (54F01) past the
   cap. Caps total egress for a single statement, complementing the row-count `MAX_ROWS`.

### What this does NOT do

- No change to the per-page scan limits' *enforcement* (they stay, paginating, as Java does) —
  only their *wiring* + the opt-in fail-mode.
- No spill-to-disk (Java's `FileSortCursor`). Go caps the buffer and errors; spilling is a future
  RFC. The memory budget makes the existing row-count caps byte-aware, not unbounded.
- No change to wire bytes, key encoding, continuations, or any persisted format. Pure read-path.

## Java spec / parity

Java `ExecuteProperties` carries `ReturnedRowLimit`, `ScannedRecordsLimit`, `ScannedBytesLimit`,
`TimeLimit`, `FailOnScanLimitReached`, surfaced via `RecordScanLimiter`/`ByteScanLimiter`/
`TimeScanLimiter` and `ScanLimitReachedException`. Pieces 1–2 port that 1:1 (wire the options,
implement the fail-mode + the SQLSTATE). Java has **no** cross-page statement timeout, **no**
memory byte budget, and **no** result-size cap (its only memory governor is a sort row-count cap
that spills, never errors) — pieces 3–5 are Go-only read-path extensions, allowed because they
never touch the wire and a Go app can simply *express* tighter bounds than a Java app; each needs
deep test coverage (below).

## Test plan (yamsql + executor unit + FDB integration)

- **Scan-limit wiring (parity):** a yamsql scenario sets `OptExecutionScannedRowsLimit`, runs a
  scan that exceeds it with `FailOnScanLimitReached`, asserts a `54F01` error (and, without the
  fail flag, asserts pagination/continuation — unchanged).
- **SQLSTATE mapping:** force `MaterializationLimitExceededError` (a buffered union past 100k) and
  `SortBufferExceededError`; assert the surfaced SQLSTATE is `54F01` (was generic). Revert-prove.
- **Statement timeout:** a query that loops long (aggregate over many inputs / a `generate_series`
  style) with a 50ms statement timeout → `54F01` "statement timeout"; the same query without the
  timeout completes. Deterministic via an injected slow operator, not wall-clock flakiness.
- **Memory budget:** `SELECT DISTINCT` over a high-cardinality generator with a tiny
  `MemoryLimitBytes` → `54F01`; without the budget it completes (bounded test data). Same for the
  sort buffer + a buffered union. Pin the charge sites individually (unit) so a future operator
  that buffers without charging is caught.
- **Result-size cap:** a wide-row scan past `OptMaxResultBytes` → `54F01`.
- All caps **off by default** → a regression test that the full existing suite is unchanged when no
  limit is set (no behavior/plan change). `-race` on the executor (the memory accumulator is shared
  across operator goroutines — atomic).

## Open questions for Graefe

- **Q1:** Is wrapping the statement ctx in `context.WithTimeout` at `cascadesPlan.Execute` the right
  layer, or should the deadline live in `ExecuteProperties` and be checked at each `fetchPage`
  boundary (so a continuation resumed by a *new* request re-derives the remaining budget)? The ctx
  approach bounds a single `Execute` call; a continuation across HTTP requests starts fresh — is
  that the intended semantics (per-request, like the per-tx limit) or per-logical-statement?
- **Q2:** Memory charge granularity — is a per-operator atomic accumulator threaded via
  `ExecuteProperties` acceptable, or does Cascades expect resource accounting in a specific place
  (an `ExecuteState`-like object, mirroring Java's `ExecuteState.getRecordScanLimiter()`)? Java
  threads limiters through `ExecuteState`; should Go mirror that rather than a bare atomic?
