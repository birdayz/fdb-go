# RFC-001: In-Memory Post-Processing Operators

## Status

Draft — swingshift-77

## Problem

The Cascades optimizer, ported 1:1 from Java's `fdb-record-layer-core`, has no physical sort operator. Java's `RemoveSortRule` either eliminates the sort via index ordering or fails the entire query. This is intentional in Java — users are expected to create indexes for every ORDER BY pattern.

In Go, this means 312 SQL shapes that work in simpler engines are rejected with `0AF00: Cascades planner could not plan query`. Common patterns like `SELECT * FROM T ORDER BY name` fail unless an index on `name` exists. This is the single largest gap for HN launch readiness.

## Proposal

Add a post-processing layer that runs **after** the Cascades-planned FDB operations complete. Cascades still handles all optimization (index selection, predicate pushdown, join ordering, sort elimination). Post-processing handles the shapes Cascades intentionally punts on.

### Operator categories

| Category | Memory | Examples | Needs buffering? |
|---|---|---|---|
| **Streaming** | O(1) | COUNT, SUM, MIN, MAX, AVG over sorted input | No — already works via `RecordQueryStreamingAggregationPlan` |
| **Hash-based** | O(groups) | GROUP BY on unsorted input, COUNT(DISTINCT) | No — one pass, hash table per group. `RecordQueryHashAggregationPlan` exists but needs wider rule matching |
| **Buffering** | O(n) | ORDER BY without index, DISTINCT on unsorted input | Yes — must see all rows before producing output |

Only the **buffering** category is new. Streaming and hash-based already exist.

### Architecture

```
SQL query
  │
  ▼
Cascades optimizer (Java-ported, unchanged)
  │
  ├─ Index scan, predicate pushdown, join ordering, sort elimination
  │  (all within FDB 5s transaction)
  │
  ▼
Plan tree with optional post-processing nodes
  │
  ▼
Executor
  ├─ FDB operations (scan/filter/join) — inside transaction
  ├─ Collect results into buffer
  └─ Post-processing (sort/distinct) — outside transaction, in-memory
```

### FDB transaction boundary

FDB enforces a 5-second transaction limit. All FDB reads (scan, index lookup, join probes) must complete within this window. Post-processing runs after the transaction closes — it operates on the materialized result set, not on FDB keys.

For small-to-medium result sets (fits in memory), this works. For large result sets, future work adds disk-spill capability behind the same interface.

### Cascades integration

`ImplementSortRule` currently:
```
OnMatch:
  try eliminate sort via inner plan's ordering
  if eliminated → yield inner plan
  if not        → yield nothing (query fails)
```

After this RFC:
```
OnMatch:
  try eliminate sort via inner plan's ordering
  if eliminated → yield inner plan (cost: low)
  if not        → yield InMemorySortPlan(inner, sortKeys) (cost: high)
```

Both alternatives exist in the same Memo Reference. The cost model ensures index-based sort elimination is always preferred. The in-memory sort only wins when it's the only option.

## New types

### `RecordQueryInMemorySortPlan` (Go extension)

```go
// Package plans.

// RecordQueryInMemorySortPlan materializes the inner plan's output and
// sorts it in memory. Go extension — Java's Cascades has no physical
// sort operator; Java's RemoveSortRule eliminates or fails.
//
// Use case: ORDER BY on a non-indexed column. Cascades still optimizes
// the inner plan (index scans for WHERE, join ordering); only the final
// sort is done in memory.
//
// Memory: O(n) where n = inner result count. For large results, a
// future ExternalSortPlan can spill to disk behind the same interface.
type RecordQueryInMemorySortPlan struct {
    inner    RecordQueryPlan
    sortKeys []SortKey  // column + direction (ASC/DESC)
}
```

File: `pkg/recordlayer/query/plan/plans/in_memory_sort.go`

### `physicalInMemorySortWrapper` (Go extension)

Physical wrapper for the Cascades expression tree. Reports sorted ordering via `HintOrdering()` so downstream operators know the output is sorted.

File: `pkg/recordlayer/query/plan/cascades/physical_in_memory_sort_wrapper.go`

### Executor dispatch

```go
case *plans.RecordQueryInMemorySortPlan:
    return executeInMemorySort(ctx, p, store, evalCtx, continuation, props)
```

```go
func executeInMemorySort(...) {
    // 1. Execute inner plan (inside FDB txn)
    innerCursor := ExecutePlan(ctx, p.GetInner(), store, ...)
    // 2. Materialize
    results := CollectAll(ctx, innerCursor)
    // 3. Sort (outside FDB txn — pure CPU)
    sortResultsByKeys(results, p.GetSortKeys())
    // 4. Apply skip/limit
    return applySkipLimit(FromList(results), props.Skip, props.Limit)
}
```

## Cost model

The in-memory sort must be more expensive than index-based sort elimination to ensure Cascades prefers indexes when available.

```
InMemorySortCost = innerCost + n * SortCPUPerRow * log(n)
IndexSortCost    = innerCost  (sort is free — already ordered)
```

`SortCPUPerRow` is a constant (e.g., 0.01) that makes the sort plan always more expensive than an index-based plan with the same inner plan.

## Labeling convention

All Go-extension types are marked in their doc comments:

```go
// RecordQueryInMemorySortPlan ... Go extension — Java's Cascades has
// no physical sort operator.
```

The `plans/` package already contains Java-ported plans (`RecordQueryScanPlan`, `RecordQueryFilterPlan`, etc.). Go-extension plans live in the same package but are clearly labeled. No separate package — they participate in the same plan tree and executor dispatch.

## What this fixes

Estimated impact on the 312 failing yamsql queries:

- ~200: ORDER BY on non-indexed column → in-memory sort
- ~30: GROUP BY + ORDER BY combos → streaming agg + in-memory sort
- ~20: DISTINCT + ORDER BY → hash distinct + in-memory sort
- Remaining ~60: scalar subqueries, CROSS JOIN syntax, recursive CTE (separate work)

## What this does NOT change

- Cascades optimizer logic (Java-ported rules unchanged)
- Index-based sort elimination (still preferred by cost model)
- Streaming aggregation (still O(1) memory when input is sorted)
- Hash aggregation (still O(groups) memory)
- FDB transaction semantics (5s limit still applies to reads)
- Wire compatibility with Java

## Future work

- `ExternalSortPlan`: disk-spill sort for result sets that exceed memory. Same `RecordQueryPlan` interface, swap in via configuration.
- `HashDistinctPlan`: in-memory hash-based DISTINCT for unsorted input.
- Scalar subquery evaluation: execute subquery once, inject as constant.
- Memory budget / backpressure: limit total buffered bytes, fail gracefully.
