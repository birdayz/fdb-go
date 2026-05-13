# RFC: Cross-Transaction Query Pagination

## Problem

Every SQL query executes inside a single FDB transaction (`DB.Run()`). FDB transactions have a hard 5-second lifetime. At ~50K rows the scan exceeds 5 seconds, and:

- GROUP BY returns partial groups (2/4 status values at 100K)
- ORDER BY returns partial rows (47K/100K)
- JOINs return 0 rows
- No error is raised — the cursor treats transaction death as end-of-data

This is an architectural mismatch, not a bug to patch. Java's relational layer never wraps an entire query in one transaction.

## How Java does it

### Cursor-level limits

Every `RecordCursor` has a `CursorLimitManager` that checks three limits **before each row read**:

| Limit | Purpose |
|---|---|
| `timeLimit` (ms) | Stop before FDB's 5s hard wall |
| `scannedRecordsLimit` | Cap rows scanned per transaction |
| `scannedBytesLimit` | Cap bytes read per transaction |

These are set via `ExecuteProperties` at query start. When any limit fires, the cursor returns `NoNextReason.TIME_LIMIT_REACHED` (or `SCAN_LIMIT_REACHED`, `BYTE_LIMIT_REACHED`) instead of `SOURCE_EXHAUSTED`. The cursor's continuation token captures exactly where it stopped.

### Client-driven continuation loop

The transaction boundary is **outside** the executor. The client (JDBC driver, gRPC service) runs:

```
continuation = BEGIN
while !continuation.atEnd():
    txn = openTransaction()
    cursor = plan.executePlan(store, evalCtx, continuation, executeProperties)
    while cursor.hasNext():
        emit(cursor.next())
    continuation = cursor.getContinuation()
    txn.commit()
```

Each iteration is a fresh FDB transaction. The continuation token resumes the scan from where the previous transaction stopped. The plan itself is stateless — reconstructed from the continuation blob (which carries a serialized `CompiledStatement`).

### What this means for composite plans

Plan nodes don't know about transaction boundaries. They just obey cursors:

- **Leaf scans**: Resume from continuation key. Already implemented in Go's `keyValueCursor`.
- **Filters**: Wrap child cursor. When child hits limit, filter propagates continuation up.
- **Sorts/Aggregations**: Must materialize their input. If the input cursor hits a limit mid-materialization, the partially materialized result is discarded, the continuation is returned, and the next transaction restarts the materialization from the continuation point. With proper scan limits (e.g., 2000 records per transaction), materialization fits in memory and completes within the time window.
- **Joins (NLJ/FlatMap)**: Outer cursor resumes from outer continuation. Inner is re-executed per outer row (or per outer page). Both continuations are nested in the top-level continuation blob.

## Current Go architecture

```
cascadesPlan.Execute()
  └─ DB.Run(func(tx) {
       cursor = executor.ExecutePlan(plan, store, ...)
       rows = wrapCursorLazy(cursor)   // cursor consumed OUTSIDE tx
       return nil
     })
  └─ return rows   // reads happen after tx committed
```

The cursor is created inside `DB.Run` but consumed outside it. FDB reads happen against a dead transaction. At small scale this works because FDB buffers early results. At scale the transaction expires mid-scan.

## Proposed architecture

```
cascadesPlan.Execute()
  └─ return &paginatingResultSet{
       plan:  physicalPlan,
       store: storeBuilder,  // creates fresh store per tx
       props: executeProperties(timeLimit=4s, scanLimit=5000),
     }

paginatingResultSet.Next()
  └─ if currentCursor has rows:
       return currentCursor.Next()
     if currentCursor stopped with SOURCE_EXHAUSTED:
       return false  // done
     // Limit reached — open new transaction, resume
     continuation = currentCursor.getContinuation()
     DB.Run(func(tx) {
       store = openStore(tx)
       currentCursor = executor.ExecutePlan(plan, store, evalCtx, continuation, props)
       // Eagerly read one page into buffer
       page = collectUpToLimit(currentCursor)
       nextContinuation = currentCursor.getContinuation()
     })
     return page[0]
```

### Key changes

1. **`paginatingResultSet`** replaces `cascadesRows`. Owns the pagination loop. Each call to `Next()` either returns a buffered row or opens a new transaction to fetch the next page.

2. **`ExecuteProperties` with time limit**: Set `timeLimit = 4000ms` (1 second safety margin below FDB's 5s wall). The existing `keyValueCursor` already checks `executeProps.TimeLimit` — it returns `TimeLimitReached` with a continuation. This machinery is built and tested; we just never set a time limit.

3. **Eager materialization per transaction**: Each transaction reads a full page (up to the scan limit) into a buffer, then commits. `Next()` drains the buffer before opening the next transaction. This avoids the current bug where the cursor is consumed after the transaction dies.

4. **Store builder**: Each transaction needs a fresh `FDBRecordStore` since the record context is tied to the transaction. Pass a store-builder function (subspace + metadata) that creates a new store per transaction.

5. **Plan reuse**: The physical plan is stateless (no transaction-bound state). Reuse the same `RecordQueryPlan` across transactions — only the continuation token changes.

## What already works

The cursor-level limit infrastructure is **already implemented** in Go:

- `keyValueCursor.OnNext()` checks `TimeLimit`, `ScannedRecordsLimit`, `ScannedBytesLimit` before each read (lines 122-149 of `key_value_cursor.go`)
- Returns `TimeLimitReached` / `ScanLimitReached` / `ByteLimitReached` with a continuation token
- `wrapContinuation()` / `unwrapContinuation()` serialize continuation tokens
- `initIterator()` resumes from continuation via `EndpointTypeContinuation`

We just never set a time limit. `DefaultExecuteProperties()` has all limits at 0 (unlimited).

## What needs building

### Phase 1: Basic pagination (fixes Bug 1 — silent truncation)

1. **`paginatingResultSet`**: The continuation loop that replaces `cascadesRows`. Opens a new `DB.Run` per page.
2. **Set `TimeLimit = 4000ms`** on `ExecuteProperties` passed to `ExecutePlan`.
3. **Eager page buffer**: `CollectAll` within the transaction, store in `[]QueryResult`, serve from buffer.

This alone fixes: full table scans at 100K+, GROUP BY truncation, ORDER BY truncation.

### Phase 2: Join predicate pushdown (fixes JOIN O(N*M))

Separate from pagination but equally critical. The Cascades planner produces `Filter(preds, NLJ(scan, scan))` — the NLJ has zero predicates and does a full cross-product. Fix: `ImplementNestedLoopJoinRule` must absorb predicates from the parent `LogicalFilterExpression` into the NLJ plan.

With predicates inside the NLJ, the existing hash join infrastructure activates (equi-join key extraction, hash index build, O(N+M) probe).

### Phase 3: EXECUTE CONTINUATION (optional, matches Java fully)

Expose continuations to the SQL driver layer so callers can explicitly paginate:
- `Continuation` type with `Reason` enum (`TRANSACTION_LIMIT_REACHED`, `CURSOR_AFTER_LAST`)
- `EXECUTE CONTINUATION ?` SQL syntax
- Serialized continuation carries plan hash + cursor state

Phase 3 is optional because Phase 1's automatic pagination handles the common case transparently. Phase 3 adds explicit control for applications that want cursor-based pagination.

## Scope of changes

| Component | Change | Size |
|---|---|---|
| `cascades_generator.go` | Replace `cascadesRows` with `paginatingResultSet` | ~100 LOC |
| `executor.go` | No change (already accepts continuation + props) | 0 |
| `key_value_cursor.go` | No change (already has limit checks) | 0 |
| `ExecuteProperties` | Set `TimeLimit = 4s` | 1 line |
| Stress test | Remove >10K warnings, add 100K/1M assertions | ~20 LOC |

Phase 1 is ~120 LOC of new code, zero changes to the cursor/executor layer.

## Risks

- **Aggregations across transactions**: GROUP BY / SUM / COUNT materialize all input. If the input exceeds one transaction's scan limit, the materialization restarts from the continuation on the next transaction. The aggregate state is lost and rebuilt. This is correct (same rows, same result) but wastes work proportional to `pages * page_size`. Java has the same behavior — it re-scans and re-aggregates. For very large aggregations, this means O(N^2/page_size) total work. Acceptable until we port streaming aggregation with partial-aggregate continuations.

- **Sort stability across transactions**: In-memory sorts materialize all input. Same restart-from-continuation behavior as aggregations. ORDER BY over >page_size rows re-scans. Java handles this the same way at the relational layer.

- **Non-deterministic reads**: Each transaction reads at a different FDB version. Concurrent writes between pages could cause inconsistencies (duplicate or missing rows). Java has the same limitation. Mitigated by short page times (4s) and FDB's MVCC isolation within each transaction.
