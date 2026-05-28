# RFC-028: Cap Unbounded Materialization in NLJ and Other Executor Paths

**Date:** 2026-05-28
**Status:** Implemented
**Author:** Johannes Bruederl

## Problem

`executeNestedLoopJoin` calls `CollectAll` to drain the **entire** inner cursor into `[]QueryResult` with no memory or row bound. Any ad-hoc join where the inner relation is large enough materializes unbounded rows into memory — OOM risk.

The same unbounded `CollectAll` pattern appears in five more executor paths:

| Call site | File | Context |
|-----------|------|---------|
| `executeNestedLoopJoin` | `executor.go` | NLJ inner side |
| `executeUnionBuffered` | `executor.go` | Each UNION branch |
| Recursive CTE initial | `executor.go` | Seed query |
| Recursive CTE recursive | `executor.go` | Each recursion level |
| Recursive CTE DFS root | `executor.go` | DFS root collection |
| Recursive CTE DFS children | `executor.go` | DFS child collection |

Note: DML paths (`executeDelete`, `executeInsert`, `executeUpdate`) do NOT use `CollectAll` — they stream rows one-at-a-time with inline side effects. They accumulate results but process each row as read.

Note: `ClearRowAndTimeLimits()` clears the **application-level** 4-second page timeout, not FDB's hard 5-second transaction limit. NLJ does not break at 5 seconds for practical table sizes — the real risk is unbounded memory, not transaction timeouts.

## Fix

Added `CollectAllBounded` — a bounded variant of `CollectAll` that returns a `MaterializationLimitExceededError` when the row count exceeds a configurable limit. Applied to all 6 `CollectAll` call sites in the executor.

### Changes

**`pkg/recordlayer/scan_properties.go`:**
- Added `MaterializationLimit int` field to `ExecuteProperties`
- Added `DefaultMaterializationLimit = 100,000` constant
- Added `WithMaterializationLimit(limit)` and `GetMaterializationLimit()` methods
- `GetMaterializationLimit()` falls back to `DefaultMaterializationLimit` when zero

**`pkg/recordlayer/query/executor/executor.go`:**
- Added `MaterializationLimitExceededError` struct with `Limit` and `Context` fields
- Added `CollectAllBounded(ctx, cursor, limit, context)` function
- Replaced all 6 `CollectAll` call sites with `CollectAllBounded` using `props.GetMaterializationLimit()`
- Each call site provides a descriptive context string for the error message
- Original `CollectAll` retained for test code (draining small result sets)

### Error message

```
materialization limit exceeded (100000 rows): nested loop join inner side; consider adding an index on the join column or increasing the materialization limit
```

### Why not delete NLJ entirely?

The original RFC proposed replacing NLJ with FlatMap (cursor-based re-execution). After discussion:

1. **NLJ works correctly** within FDB's transaction model. The 5-second timeout claim was wrong — `ClearRowAndTimeLimits` clears the app-level timeout, not FDB's hard limit. NLJ handles practical table sizes fine.
2. **NLJ is faster** for small-to-medium inners. It materializes once and does N*M in-memory comparisons. FlatMap would re-execute the inner plan N times (one full scan per outer row), causing N*M FDB reads for unindexed joins.
3. **The hash-index optimization** in `nljCursor.tryBuildHashIndex` gives O(1) inner lookups for equijoins on materialized data — real value for medium-sized joins.
4. **Spill-to-disk is wrong** for this architecture — the data is already on disk (FDB). If the inner exceeds the cap, the answer is "add an index," not "add a second storage layer."
5. **The right future fix** for large unindexed equijoins is a first-class `RecordQueryHashJoinPlan` with its own cost model, not removing NLJ.

Capping with a clear error is the right balance: NLJ stays fast for its sweet spot, blows up safely with an actionable message when the inner is too large.

## Testing

1. All 46 test targets must pass (verified)
2. `MaterializationLimitExceededError` is a typed error matchable via `errors.As()`
3. Limit is configurable via `ExecuteProperties.WithMaterializationLimit()` — tests or callers can raise/lower it

## Future work

- **P1.1 Wire statistics:** with real cardinality estimates, the cost model can choose between NLJ and FlatMap at plan time
- **HashJoinPlan:** first-class physical operator for large unindexed equijoins (separate RFC)
