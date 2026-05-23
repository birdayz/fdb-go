# RFC: Aggregate Index Support

## Status

Draft. Authored swingshift-100.

## Problem

GROUP BY queries on 1M-row tables take 5-10s because the planner does full table scan + in-memory sort + streaming aggregation. When an index covers the grouping keys, the planner should stream through the index instead. When a pre-computed aggregate index exists, the planner should read the result directly from a single FDB range scan with no per-row computation.

## What exists today

### Infrastructure (complete)

| Component | File | Status |
|-----------|------|--------|
| `RecordQueryAggregateIndexPlan` | `plans/aggregate_index.go` | Plan type + tests |
| `atomicMutationIndexMaintainer` | `recordlayer/atomic_mutation_index_maintainer.go` | Insert/update/delete maintenance |
| `atomicMutation` enum (COUNT, SUM, MIN, MAX, etc.) | `recordlayer/atomic_mutation.go` | 9 mutation types |
| FDB atomic ops (ADD, MIN, MAX, BYTE_MIN, BYTE_MAX, COMPARE_AND_CLEAR) | `fdbgo/fdb/transaction.go` | Full Go client support |
| `RecordQueryStreamingAggregationPlan` | `plans/streaming_agg.go` | Streaming plan type |
| `StreamingAggFromIndexRule` | `cascades/rule_streaming_agg_from_index.go` | Streaming agg from ordered regular index |
| `ImplementStreamingAggregationRule` | `cascades/rule_implement_streaming_agg.go` | Streaming agg with InMemorySort fallback |

### Gap (planner integration)

No Cascades rule connects GROUP BY queries to aggregate indexes. `RecordQueryAggregateIndexPlan` exists but no rule produces it.

## Design

### Three tiers of GROUP BY optimization

Each tier is independently useful. Higher tiers subsume lower ones but all should exist for different query shapes.

**Tier 1: Streaming aggregation from ordered regular index** (exists)

```
SELECT status, COUNT(*), SUM(amount) FROM orders GROUP BY status
-- Plan: StreamingAgg(keys=[status], IndexScan(idx_status, full-range))
-- Cost: N index reads + N PK fetches (non-covering)
```

The index provides ordering; aggregation streams through index entries. Each entry requires a PK fetch to access non-index columns (amount). Useful when the index is covering or the table is small.

Go extension: `aggregatesCoveredByIndex` marks the index scan covering when all aggregate operands are available in the index columns, eliminating PK fetches. COUNT(*) is trivially covering.

**Tier 2: Aggregate index scan** (this RFC)

```
-- Schema: CREATE INDEX idx_status_sum ON orders(SUM(amount)) GROUP BY (status)
-- Stored as: key=pack(status_value), value=atomic_sum_of_amount

SELECT status, SUM(amount) FROM orders GROUP BY status
-- Plan: AggregateIndex(SUM, idx_status_sum)
-- Cost: K reads where K = number of distinct groups (4 for status)
```

Pre-computed aggregates maintained atomically via FDB ADD/MIN/MAX mutations. O(groups) reads instead of O(rows). This is the high-impact optimization.

**Tier 3: Roll-up streaming aggregation** (future)

For queries that GROUP BY a prefix of the aggregate index grouping, scan grouped entries and re-aggregate. E.g., an aggregate index on `(region, status)` can answer `GROUP BY region` by scanning and re-summing the per-status entries within each region. Java supports this via `RecordQueryStreamingAggregationPlan` wrapping `RecordQueryAggregateIndexPlan`.

### FDB key-value format (Java-aligned)

Aggregate indexes use the same subspace layout as regular indexes but with **one key-value pair per group** instead of one per record:

```
Key:   indexSubspace.Pack(groupingKeyTuple)
Value: atomicMutationEncoding(aggregateValue)
```

Encoding depends on the aggregate type:

| Type | FDB Mutation | Value Encoding | On Insert | On Delete |
|------|-------------|----------------|-----------|-----------|
| COUNT | ADD | little-endian i64 | +1 | -1 |
| SUM | ADD | little-endian i64 | +value | -value |
| MIN_EVER | MIN | little-endian u64 | value | no-op (monotonic) |
| MAX_EVER | MAX | little-endian u64 | value | no-op (monotonic) |
| MIN_EVER_TUPLE | BYTE_MIN | tuple bytes | value | no-op |
| MAX_EVER_TUPLE | BYTE_MAX | tuple bytes | value | no-op |

This is wire-compatible with Java. Our `atomicMutationIndexMaintainer` already implements this.

### Cascades integration

#### A) `AggregateIndexMatchCandidate`

A new `MatchCandidate` implementation registered from metadata when the index type is COUNT/SUM/MIN/MAX.

```go
type AggregateIndexMatchCandidate struct {
    indexName        string
    indexType        string          // "COUNT", "SUM", "MIN", "MAX"
    groupingColumns  []string        // GROUP BY columns (leading key prefix)
    aggregatedColumn string          // column being aggregated (trailing key)
    recordTypes      []string
}
```

The match candidate tells the planner: "this index can answer GROUP BY queries on these grouping columns with this aggregate function."

Registration in `metadataPlanContext.GetMatchCandidates()`: iterate `md.GetAllIndexes()`, check `idx.IndexType` for aggregate types, create `AggregateIndexMatchCandidate` from the `GroupingKeyExpression`.

#### B) `ImplementAggregateIndexRule`

A new implementation rule that matches `GroupByExpression` against `AggregateIndexMatchCandidate`:

```
Match: GroupByExpression(keys=[k1, k2, ...], aggs=[AGG(col)])
       where an AggregateIndexMatchCandidate exists with:
         - groupingColumns prefix-matches keys
         - aggregatedColumn matches col
         - indexType matches AGG

Yield: physicalAggregateIndexWrapper(RecordQueryAggregateIndexPlan)
```

The rule fires during EXPLORE/PLANNING and yields a physical wrapper for `RecordQueryAggregateIndexPlan`. The cost model picks it because its cardinality equals the number of groups (typically tiny — 4 for status), far cheaper than a full scan.

#### C) `physicalAggregateIndexWrapper`

Wraps `RecordQueryAggregateIndexPlan` as a `RelationalExpression` for the Memo. `HintCost` returns the number of groups as cardinality (derivable from the index type and grouping columns). For unknown group count, uses a heuristic `DistinctSelectivity * tableCardinality`.

#### D) Executor integration

The executor's `executeAggregateIndex` function:
1. Opens the aggregate index subspace
2. Scans the index entries (one per group)
3. Decodes the aggregate value from each entry
4. Returns `{groupKey, aggregateValue}` rows

This is essentially `atomicMutationIndexMaintainer.Scan()` with result mapping.

### Trade-offs

#### A) Java alignment

- **Aligned**: same FDB key-value format, same atomic mutations, same plan type name, same `GroupingKeyExpression` semantics.
- **Diverges**: Java has `AggregateIndexExpansionVisitor` which builds a traversal for the match candidate. Go uses direct column-name matching (simpler, same result for the common case). Java supports `IndexOnlyAggregateValue` (MAX_EVER/MIN_EVER as plan-only values that can't be streamed). Go defers these.

#### B) FDB optimization

- Aggregate indexes use **atomic mutations** (no read-before-write). Zero contention between concurrent writers. This is the main advantage over computing aggregates at query time.
- COUNT/SUM use ADD mutation (non-idempotent): retry-safe because FDB transactions are atomic. A retried transaction re-applies the mutation from scratch.
- MIN_EVER/MAX_EVER are idempotent (FDB MIN/MAX mutations): safe even under duplicate application.
- `CLEAR_WHEN_ZERO` option (via COMPARE_AND_CLEAR): keeps the index clean by removing zero-count entries. Prevents unbounded index growth when groups are frequently created and destroyed.

#### C) Cascades alignment

- Match candidates are the standard Cascades mechanism for introducing access paths. `AggregateIndexMatchCandidate` fits naturally alongside `ValueIndexScanMatchCandidate` and `PrimaryScanMatchCandidate`.
- The implementation rule follows the same pattern as `ImplementIndexScanRule` — match a logical expression, check available candidates, yield a physical plan.
- Cost model integration via `HintCost` on the physical wrapper, consistent with all other physical wrappers.

### What this does NOT cover

- **DDL syntax**: `CREATE AGGREGATE INDEX` or `CREATE INDEX ... STORING SUM(...)`. Aggregate indexes are defined in protobuf metadata, not SQL DDL. SQL syntax is a separate feature.
- **Partial aggregation / roll-up** (Tier 3): scanning grouped entries and re-aggregating for a grouping prefix. Deferred.
- **BITMAP aggregate indexes**: used for set membership. Different execution model. Deferred.
- **PERMUTED_MIN/PERMUTED_MAX**: permuted aggregate indexes that support ordered enumeration of groups by aggregate value. Deferred.

## Implementation plan

### Phase 1: `AggregateIndexMatchCandidate` + rule (core)

1. `AggregateIndexMatchCandidate` type in `cascades/` package
2. Registration in `metadataPlanContext.GetMatchCandidates()` for aggregate-type indexes
3. `ImplementAggregateIndexRule` that matches `GroupByExpression` against candidates
4. `physicalAggregateIndexWrapper` with `HintCost` returning group-count cardinality
5. Executor: `executeAggregateIndex` using `atomicMutationIndexMaintainer.Scan()`

### Phase 2: Tests

1. Unit tests for match candidate column matching
2. FDB integration tests: create aggregate index, insert rows, verify GROUP BY uses the plan
3. Stress test: `SELECT status, COUNT(*) FROM orders GROUP BY status` with aggregate index → expect <10ms on 1M rows
4. Conformance: aggregate index results match streaming aggregation results

### Phase 3: Cost model tuning

1. `HintCost` on aggregate index wrapper: cardinality = number of groups (from index scan range)
2. Cost model correctly prefers aggregate index over streaming agg over full scan + sort
3. Verify via EXPLAIN that the correct plan is selected

## Performance impact

| Query | Current | With Tier 1 covering | With Tier 2 agg index |
|-------|---------|---------------------|----------------------|
| `SELECT status, COUNT(*) GROUP BY status` | 5s (full scan + sort) | ~3s (covering index stream) | <10ms (4 reads) |
| `SELECT status, SUM(amount) GROUP BY status` | 5s (full scan + sort) | 5s (non-covering) | <10ms (4 reads) |
| `SELECT customer_id, SUM(amount) GROUP BY customer_id` | 10s (full scan + sort) | 10s (non-covering) | ~1s (100K reads) |
