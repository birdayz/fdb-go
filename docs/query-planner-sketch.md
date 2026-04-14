# Query Planner — Minimum Viable Design

Status: **Phase 0 DONE** (dayshift-14). 9 plan types + 13 tests shipped.
Phase 1 (rule-based planner) is next.

## Approach

Port the **plan execution engine** first, WITHOUT the cascades optimizer.
Hand-constructed plans → existing cursor infrastructure → results.

The optimizer (104K Java lines) can come later. The execution engine alone
is useful for programmatic query construction.

## Phase 0: Plan Execution (DONE)

### Plan types implemented (10 of 46 Java types)

| Plan | What it does | Go cursor |
|---|---|---|
| `ScanPlan` | Full table scan | `ScanRecords()` |
| `IndexPlan` | Index scan + record fetch | `ScanIndexRecords()` |
| `FilterPlan` | Apply predicate to child | `filterCursor` |
| `IndexScanPlan` | Index-only (no record fetch) | `maintainer.Scan()` |
| `PrimaryKeyLookupPlan` | Single record by PK | `LoadRecord()` |
| `RangeScanPlan` | PK range scan | `ScanRecordsInRange()` |
| `UnionPlan` | Merge-union of two ordered scans | `Union()` |
| `IntersectionPlan` | Merge-intersect of two scans | `Intersection()` |
| `LimitPlan` | Limit N results | `LimitRowsCursor()` |
| `ReversePlan` | Reverse scan direction | `ReverseScan()` |

### Interface

```go
// RecordQueryPlan is a node in a query plan tree.
type RecordQueryPlan interface {
    // Execute returns a cursor over the results.
    Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*QueryResult]
    
    // Explain returns a human-readable description.
    Explain() string
}

// QueryResult wraps a record with optional computed fields.
type QueryResult struct {
    Record    *FDBStoredRecord[proto.Message]
    IndexEntry *IndexEntry // non-nil for index-only plans
}
```

### Example usage

```go
// "SELECT * FROM Order WHERE price > 100 ORDER BY price"
plan := NewIndexPlan(
    priceIndex,
    TupleRange.GreaterThan(tuple.Tuple{int64(100)}),
    ForwardScan(),
)

cursor := plan.Execute(store, nil, DefaultScanProperties())
for record := range Seq(cursor, ctx) {
    fmt.Println(record.Record)
}
```

### Dependencies

All cursor infrastructure exists:
- `RecordCursor[T]` interface ✓
- `FilterCursor` ✓
- `MergeCursor` (union/intersect) ✓
- `ScanRecords`, `ScanIndex`, `ScanIndexRecords` ✓
- Continuation tokens ✓

The plan types are thin wrappers around existing cursors. ~500 lines estimated.

## Phase 2: Rule-Based Planner

Simple rule-based planner that picks plans based on available indexes.
NOT the full cascades optimizer — just enough for common patterns:

- Single-index scan (field = value, field > value)
- Covering index (all needed fields in index)
- Union/intersection for OR/AND on indexed fields

~1000-2000 lines estimated.

## Phase 3: Cascades Optimizer

Full Cascades (Volcano-style) optimizer. 104K Java lines.
This is a multi-month project. Needs separate RFC.

## Not in scope

- SQL parsing (separate layer)
- Cost model (comes with cascades)
- Parallel execution (Go's concurrency model differs from Java's CompletableFuture)
