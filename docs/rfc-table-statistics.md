# RFC: Table Statistics for Cost-Based Plan Selection

**Status:** Deferred  
**Date:** 2026-05-21  
**Blocks:** `index_status_count` regression (equality on low-cardinality column)

## Problem

The cost model cannot distinguish high-cardinality equality (`customer_id = X` → 8 rows) from low-cardinality equality (`status = 'pending'` → 250K rows). Both look like "1 equality bound, non-unique" and get the same heuristic selectivity (50%).

Without per-index selectivity, the planner either:
- Always prefers index (fast for customer_id, 5x slow for status)
- Always prefers full scan (fast for status, 1500x slow for customer_id)

The current heuristic exempts equality from the fetch penalty (preferring index). This is correct for the common case but wrong for low-cardinality columns.

## Java Baseline

Java's `PlanningCostModel` has **zero table statistics**. It uses purely structural heuristics (static cardinality bounds from key structure, residual predicate counts, `PREFER_INDEX` configuration flag). `getSnapshotRecordCount()` and COUNT indexes exist in `FDBRecordStore` but are completely disconnected from query planning.

Java gets away with this because:
1. Apps rarely index low-cardinality columns alone
2. `CardinalitiesProperty` uses tight static bounds (point lookup = max 1)
3. `PREFER_INDEX` default works for typical workloads

## Proposed Fix: Covering Index for COUNT-Only Queries

The immediate regression (`SELECT COUNT(*) FROM orders WHERE status = 'pending'`) has a simpler root cause: the index scan does 250K PK fetches to read full records just to count them. The index entries alone suffice for COUNT(*).

**Fix:** When the only consumer is COUNT(*) (or other aggregates that don't need record columns), mark the index scan as covering. Skip PK fetch in execution. This makes the query fast regardless of selectivity — counting 250K index entries is cheap (sequential read, no random I/O).

Java equivalent: `IndexOnlyAggregateValue` / covering index optimization where the index entries provide all needed columns.

## Future: Full Table Statistics

If covering-for-count doesn't resolve all cases, implement lightweight stats:

### Level 1: Table Row Count (simplest)
- Read `RECORD_COUNT` key at record store open
- Feed into cost model as base cardinality instead of `LeafScanCardinality = 1e6`
- Benefit: cost model scales to actual data size

### Level 2: Per-Index NDV (Number of Distinct Values)
- Maintain approximate NDV per index (HyperLogLog or exact count for small cardinalities)
- Selectivity for equality = 1/NDV instead of fixed 0.5
- For status (NDV=4): selectivity = 0.25, estimated rows = 250K → fetch penalty kicks in
- For customer_id (NDV=100K): selectivity = 0.00001, estimated rows = 10 → no penalty

### Level 3: Index Entry Count (per-key range)
- COUNT index per indexed value (Java has this infrastructure)
- Exact selectivity without estimation
- Expensive to maintain for high-cardinality indexes

### Implementation Approach

```go
type StatisticsProvider interface {
    RecordTypeCardinality(recordTypeName string) float64
    IndexNDV(indexName string) (float64, bool)  // returns NDV, ok
}
```

The `StatisticsProvider` is already wired into `EstimateCostWith` and `CostLessWith`. Adding `IndexNDV` requires:
1. Computing NDV at index build time (store in index metadata)
2. Passing stats into `physicalIndexScanWrapper.HintCost` via a context parameter
3. Using `tableRows / NDV` as per-equality-bound selectivity

### Cost

- Level 1: ~20 LOC (read existing RECORD_COUNT key)
- Level 2: ~200 LOC (HLL per index, metadata storage, cost model integration)
- Level 3: ~500 LOC (COUNT index infra already exists, just wire to planner)

## Decision

Deferred. The immediate fix is covering-index-for-count-only (architectural, matches Java). Full table statistics are a Go extension beyond Java's capabilities — implement when more selectivity-dependent regressions surface.
