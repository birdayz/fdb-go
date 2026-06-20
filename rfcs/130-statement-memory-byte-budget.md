# RFC-130 — Statement-wide memory byte budget (RFC-106b)

**Status:** Draft
**Item:** prod-readiness-audit-2026-06-19.md **P1** — "No Full Statement-Wide Memory Byte Budget Yet"
(the `RFC-106b` follow-up explicitly deferred from RFC-106a).
**Reviewers:** Graefe (executor/Cascades alignment) + Torvalds (code quality) + codex + @claude.
Query-engine change → Graefe ACK required before merge.

---

## 1. Problem (verified)

Cardinality-growing buffers in the executor are bounded by **row count**, not **bytes**:
- `MaterializationLimit` (default **100,000 rows**, `scan_properties.go:165/168`) bounds `CollectAllBounded`
  (`executor.go:2908`, ~10 callers: buffered union branch, NLJ inner side, DELETE/INSERT/UPDATE target
  sets, recursive-CTE initial/recursive levels, recursive-DFS root/children).
- The in-memory **sort** buffer is bounded by its own row limit (`streaming_cursors.go:496`).
- The **distinct** seen-set grows per distinct key (`executor.go:1115`).

A query that buffers 100,000 **large** rows (wide records, big blobs) still creates unbounded memory
pressure — a multi-tenant OOM / noisy-neighbour risk. RFC-106a added a *returned-result* byte cap
(`maxResultBytes`, egress only, `cascades_generator.go:1063-1072`) but **not** an *internal-buffer* byte
budget. The aggregate cursor is **streaming** (one group key + running aggregates, `streaming_cursors.go:46`),
so it does not accumulate unboundedly and needs no accounting.

## 2. Proposed change — a statement-wide byte accountant

### 2.1 The budget (shared, accumulating across operators)

A pointer field on `ExecuteProperties` so the count accumulates **statement-wide** (not per-operator):

```go
type MemoryBudget struct { used, limit int64 }              // limit<=0 == unlimited
func (b *MemoryBudget) Charge(n int64) error {              // nil receiver == no budget == no-op
    if b == nil || b.limit <= 0 { return nil }
    b.used += n
    if b.used > b.limit { return &MemoryLimitExceededError{Used: b.used, Limit: b.limit} }
    return nil
}
```

`ExecuteProperties.MemoryBudget *MemoryBudget` — the pointer is copied by value through `WithX` helpers
and the per-operator `innerProps`, so every buffering operator in one statement shares one counter (it is
**not** cleared on the inner-plan boundary; it is a statement-wide ceiling, unlike the per-page
`ReturnedRowLimit`). Constructed once per statement in `paginatingRows`/`ExecutePlan` setup from the option.

### 2.2 Option + error

- Option `OptMaxStatementMemoryBytes` (`api/options.go`), default `0` = unlimited (matching `MaxRows`/byte-cap
  defaults — opt-in, zero behaviour change by default).
- `MemoryLimitExceededError` → SQLSTATE **`54F01`** (`ErrCodeExecutionLimitReached`), the existing
  resource-limit family (scan/byte/time all map there). Message names the budget + the op for diagnosis.

### 2.3 Charge sites (every cardinality-growing buffer)

- `CollectAllBounded`: after `append`, `Charge(estimateQueryResultBytes(value))`; on error, return it
  (replaces nothing — additive to the existing row-count check).
- Sort cursor buffer (`streaming_cursors.go`): charge each materialized row as it is buffered.
- Distinct seen-set (`executor.go:1097-1120`): charge each newly-inserted key.

### 2.4 Byte estimate

`estimateQueryResultBytes(QueryResult) int64` — approximate payload: `Record.Record.Size()` (proto wire
size) when present, else a `Datum`-based estimate (reuse the `estimateRowBytes` value-size logic,
`cascades_generator.go:1059`), plus the `PrimaryKey` tuple length. Intentionally approximate (a ceiling
signal, not exact heap), consistent with RFC-106a's `estimateRowBytes`.

### 2.5 No-bypass guard

The audit asks that new buffered operators can't silently skip accounting. Two layers:
1. Route all unbounded accumulation through the three accounted helpers (CollectAllBounded + the sort/distinct
   buffers); there is no other raw `append`-in-a-loop accumulation of `QueryResult` in the executor (verified).
2. A test (`memory_budget_bypass_test.go`) that greps the executor package for `append(.*results` /
   slice-accumulation patterns outside the accounted helpers and fails on a new one — a cheap structural
   lint pinned in CI.

## 3. Executable spec (tests)

1. **Byte budget trips before the row-count limit:** a query buffering N wide rows whose total bytes exceed
   `OptMaxStatementMemoryBytes` (but N < `MaterializationLimit`) → `54F01`. Revert-proven (no charge → returns
   the rows).
2. **Statement-wide accumulation:** a query with **two** buffering operators (e.g. a buffered-union of two
   NLJ-inner materializations) charges both against one budget → trips when the *sum* exceeds it, not
   per-operator.
3. **Default unlimited:** no option set → large buffers succeed exactly as today (regression guard).
4. **Estimate sanity:** `estimateQueryResultBytes` is within a sane factor of the real payload for a stored
   record and a computed row.
5. **No-bypass lint** (§2.5).

## 4. Wire/behaviour impact

**None by default** (opt-in option; `0` = current behaviour). No persisted bytes, no plan-shape change. When
the option is set, a previously-OOM-risking query instead fails fast with `54F01` — strictly safer.

## 5. Open question for Graefe

Is the pointer-on-`ExecuteProperties` the right place for a statement-wide accountant (vs threading a context
value or an executor-level field)? Java's `RecordQueryPlan` execution carries `ExecuteProperties` +
`FDBRecordStoreBase` per scan; the byte budget is a statement-scoped concern that must survive the
inner-plan `clearSkipAndLimit`-style resets — a pointer that propagates by default (and is never cleared)
models that. Confirm this is the faithful place, and that charging at the three buffer sites (not per-row at
every cursor) is the right granularity (matches where Java would bound a `RecordCursor.asList()`).
