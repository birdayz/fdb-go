# RFC-106a: Per-statement resource governance — timeout, scan-limit wiring, SQLSTATE, result-size

**Status:** Accepted — Graefe (ACK, Q1 per-request ruling), Torvalds (NAK→split: this is the
106a half), codex (P2s: Java-option semantics) — addressed on r2 (2026-06-13). The memory byte
budget is split out to **RFC-106b** (Torvalds: it needs *completeness* — every cardinality-growing
buffer charged + a CI lint — not a plausible-looking 4-site list).
**Item:** Client launch-readiness #3 (TODO.md) — TODO-production P1.9. Gate: **query-engine
(Graefe)** + Torvalds + codex. Java (4.11.1.0) is the spec for the parity parts (scan-limit wiring);
the statement timeout + result-size cap are Go-only read-path extensions (Java lacks both) that
never touch the wire.

## Problem (multi-tenant safety)

The Go SQL engine has no per-statement resource ceiling. Scan-level limits exist and are ENFORCED
at leaf cursors (`scan_properties.go`: `ReturnedRowLimit`/`ScannedRecordsLimit`/`ScannedBytesLimit`/
`TimeLimit`) but are **per-PAGE** (fresh cursor per `fetchPage`/transaction — they paginate, never
error). The SQL options meant to drive them (`OptMaxRows`, `OptExecutionScannedRowsLimit`/
`BytesLimit`/`TimeLimit`) are **DEAD** (zero non-generated consumers). Eager-buffer caps
(`MaterializationLimit` 100k, `SortBuffer` 5M) throw Go error structs that are **not mapped to a
SQLSTATE** (`translateExecError`, `cascades_generator.go:1143`) — they surface as a generic internal
error. And there is **no whole-statement wall-clock**: a query consuming many inputs per output can
run unbounded across pages. (The per-query memory byte budget for unbounded buffers — `DISTINCT`
maps, recursive-CTE/DFS/union/DML slices — is RFC-106b.)

`FailOnScanLimitReached` (`scan_properties.go:114`) is a dead field — Java throws
`ScanLimitReachedException` (54F01) when set; Go never does.

## Proposed change (all OFF by default; errors not crashes; no wire change)

1. **Map the existing limit errors → 54F01 (operability; ~10 LOC).** `translateExecError`
   (`cascades_generator.go:1143-1144`, which already maps `RecursiveCTEDepthExceededError`) gains arms
   for `*MaterializationLimitExceededError` and `*SortBufferExceededError` → `ErrCodeExecutionLimitReached`
   (`54F01`, already defined `errcode.go:135`). Today they fall through untranslated.
2. **Wire the dead scan-limit options → `ExecuteProperties`, with JAVA semantics (parity; codex).**
   At the single chokepoint `paginatingRows.fetchPage` (`cascades_generator.go:1096`) build the
   per-page props from the connection `Options`:
   - `OptExecutionScannedRowsLimit → WithScannedRecordsLimit`, `OptExecutionScannedBytesLimit →
     WithScannedBytesLimit`, `OptExecutionTimeLimit → WithTimeLimit` — all PER-PAGE, exactly Java's
     `ExecuteProperties.setScannedRecordsLimit`/`setTimeLimit` (codex P2: `OptExecutionTimeLimit`
     keeps its Java per-page meaning; it is NOT the statement timeout).
   - `OptMaxRows` → a **STATEMENT-WIDE** returned-row cap (codex P2): because `paginatingRows`
     auto-follows continuations, wiring it to per-page `ReturnedRowLimit` would make it a page size.
     Instead track a remaining-row budget across `paginatingRows` pages and stop after `MAX_ROWS`
     rows are emitted — Java's JDBC `setMaxRows` semantics (total, not per-page).
   - **`FailOnScanLimitReached`** (parity): when the connection sets it, a leaf cursor that hits
     `ScanLimitReached`/`ByteLimitReached` returns a `*ScanLimitReachedError` (→ 54F01) instead of
     paginating — Java's `setFailOnScanLimitReached(true)`. Default off (paginate; unchanged).
3. **Statement timeout (Go-only extension).** A wall-clock deadline spanning the WHOLE statement
   (all pages of one `Execute`). **Stored in a Go-LOCAL execution config, NOT a new `api.OptionName`**
   (codex P2: `api.OptionName` mirrors Java's enum exactly; a Go-only name would break parity). It is
   surfaced via a Go-local `SET statement_timeout = …` handled engine-side / a driver/connection
   field — distinct from the Java-backed `Options` map. Implement by wrapping the statement ctx in
   `context.WithTimeout` at `cascadesPlan.Execute` (`cascades_generator.go:798`); every cursor already
   gates on `ctx.Err()` (verified: `CollectAllBounded:2638`, sort `429/523`, hash `780/850`), so the
   deadline bounds the work with zero per-operator plumbing. Map the deadline → 54F01 ("statement
   timeout"). **Q1 (Graefe ruling): PER-REQUEST, not per-logical-statement.** Java's `TimeScanLimiter`
   is per-`ExecuteState`, reset on every continuation resume — there is no cross-continuation
   wall-clock by design (a resumed continuation is a new logical scan with serialized state). One
   `Execute()` is bounded; a continuation resumed by a new request starts fresh. Documented at
   `cascadesPlan.Execute`. The per-tx FDB timeout is unaffected.
4. **Result-size (bytes) cap (Go-only extension).** Also a **Go-LOCAL** config (not `api.Options`):
   `paginatingRows` accumulates the tuple-encoded size of returned rows across the statement and
   errors (54F01) past the cap. Complements the row-count `MAX_ROWS`. The size estimate is the
   cheap encoded length, not exact heap (documented as a non-exact egress ceiling).

## Test plan (yamsql + executor unit + FDB integration)

- **Scan-limit wiring (parity):** a scenario sets `OptExecutionScannedRowsLimit` + `FailOnScanLimitReached`,
  runs a scan that exceeds it → `54F01`; without the flag → pagination/continuation (unchanged).
- **MAX_ROWS statement-wide (codex):** `MAX_ROWS=10` over a >10-row, multi-page result returns
  EXACTLY 10 rows total (not 10/page) — pins the across-page tracking.
- **SQLSTATE mapping:** force `MaterializationLimitExceededError` (buffered union >100k) and
  `SortBufferExceededError`; assert surfaced SQLSTATE `54F01` (was generic). Revert-prove.
- **Statement timeout:** a long query (aggregate over many inputs / injected slow operator) with a
  tiny statement timeout → `54F01` "statement timeout"; same query without the timeout completes.
  Deterministic via an injected slow operator, not wall-clock flakiness.
- **Result-size cap:** a wide-row scan past the byte cap → `54F01`.
- **Default-safety (Torvalds):** a connection with NO options yields `ExecuteProperties` with
  `ScannedRecordsLimit==0` / no page-limit change — a DIRECT assertion (the "full suite unchanged"
  test only proves the no-option path; this proves the wiring's default is inert). Plus the full
  suite green with no limits set.

## What this defers (RFC-106b)

The per-query **memory byte budget** for unbounded buffers. Torvalds NAK'd folding it in here: the
budget must charge EVERY cardinality-growing buffer or it is a silent cap (CLAUDE.md violation) —
`CollectAllBounded` (covers union/NLJ-inner/INSERT/recursive-CTE/DFS via the chokepoint), the two
sort buffers, `executeDistinct`'s `seen` (`executor.go:861`), the union-distinct `seen`
(`executor.go:2338`), and the DELETE/UPDATE result slices (`1856/2027`) — plus a CI lint/test that a
new buffering operator without a charge fails. Graefe's Q2 ruling (a bare `*atomic.Int64` on
`ExecuteProperties`, charged once at the `CollectAllBounded` chokepoint, NOT an `ExecuteState`
object) is the design 106b will use.
