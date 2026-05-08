# RFC-002: Port Java's RequestedOrdering to Go

**Status:** Draft
**Author:** swingshift-81
**Java reference:** fdb-record-layer 4.11.1.0

## Problem

Go handles ORDER BY in aggregate queries by materializing sort-only
columns, sorting, then stripping them:

```
Parser:  harvest SUM(v) from ORDER BY → add as sortOnly aggCol
Plan:    Aggregate(grp, SUM(v)) → Sort(sentinel) → Project(grp)
```

Java never does this. Java carries ORDER BY as a `RequestedOrdering`
constraint that the Cascades planner propagates and satisfies:

```
Plan:    GroupByExpression(grp, SUM(v)) with RequestedOrdering(SUM(v)*2)
         → planner checks if index satisfies ordering
         → if not, ImplementInMemorySortRule wraps the physical plan
```

Go's approach causes:
- Column pollution (sortOnly/hidden flags, sentinel names)
- No sort elimination (the sort is always present, even when an index
  delivers rows in order)
- Fragile composition (sentinels break through derived tables, CTEs,
  UNION ORDER BY)

## Goal

Port Java's RequestedOrdering system 1:1. After this RFC:
- `sortOnly` and `hidden` flags on `aggSelectCol` are deleted
- `postSortStripProj` is deleted
- Sentinel `__orderby_aggexpr_N__` mechanism is deleted
- ORDER BY aggregate expressions work through the Cascades constraint
  system
- Sort elimination works when an index satisfies the ordering

## Java Architecture (reference)

### Data structures

```java
// RequestedOrdering.java (fdb-record-layer-core)
class RequestedOrdering {
    List<RequestedOrderingPart> orderingParts;  // (Value, SortOrder) pairs
    Distinctness distinctness;                   // {DISTINCT, NOT_DISTINCT, PRESERVE}
    boolean isExhaustive;

    RequestedOrdering pushDown(Value resultValue, ...);
    RequestedOrdering translateCorrelations(TranslationMap, ...);
}

// RequestedOrderingPart — inner record
record RequestedOrderingPart(Value value, RequestedSortOrder sortOrder);

// RequestedSortOrder — enum
enum RequestedSortOrder {
    ANY, ASCENDING, ASCENDING_NULLS_LAST,
    DESCENDING, DESCENDING_NULLS_FIRST
}
```

### Constraint integration

```java
// RequestedOrderingConstraint.java
class RequestedOrderingConstraint
    implements PlannerConstraint<Set<RequestedOrdering>> {

    // Cascades constraint key — rules declare dependency on this
    static final PlannerConstraint<Set<RequestedOrdering>> REQUESTED_ORDERING;

    // Combine merges ordering requests from different subtrees.
    // Subsumption: if current is exhaustive and its parts are a
    // prefix of new, the new one is subsumed.
    Set<RequestedOrdering> combine(Set<RequestedOrdering> current,
                                   Set<RequestedOrdering> additional);
}
```

### Cascades rules

**PushRequestedOrderingThroughSelectRule:**
```java
// For each RequestedOrdering in the constraint:
//   pushDown(resultValue) → rebase ordering values to upstream quantifiers
//   push to child reference
```

**PushRequestedOrderingThroughGroupByRule:**
```java
// collectCompatibleOrderings():
//   1. Push requested ordering through GroupBy's resultValue
//   2. Extract group-by key primitives
//   3. Build child ordering: [matched requested parts] + [other group keys with ANY]
//   4. Push to child reference
```

**RemoveSortRule:**
```java
// 1. Extract RequestedOrdering from LogicalSortExpression
// 2. Get inner plan's Ordering property
// 3. If inner ordering.satisfies(requested): yield inner (remove sort)
// 4. Else: keep sort
```

### SQL → Cascades bridge

```java
// QueryVisitor.visitSimpleTable() lines 269-290:
// 1. visitOrderByClauseForSelect() → List<OrderByExpression>
// 2. OrderByExpression.pullUp(orderBys, groupByResultValue, ...)
//    → rewrites ORDER BY values through GROUP BY output
// 3. Pass to generateSelect(..., orderBys, ...)
//    → builds SelectExpression with RequestedOrdering

// OrderByExpression.pullUp() lines 74-109:
// For each ORDER BY expression:
//   value.pullUp(groupByResultValue, correlationId, ...)
//   → resolves e.g. SUM(v)*2 against GroupBy output
//   → produces Value referencing GroupBy's result fields
```

### Physical sort

```java
// RecordQuerySortPlan evaluates sort keys via RecordQuerySortKey:
//   key.getAdapter(store, maxRecords) → SortAdapter
//   MemorySortCursor.createSort(adapter, innerCursor, ...)
// The sort key is a Value/KeyExpression — evaluated per record,
// no extra columns needed.
```

## Go Port Plan

### Phase 1: RequestedOrdering data structures

**New package:** `pkg/recordlayer/query/plan/cascades/ordering/`

```go
type SortOrder int
const (
    SortAny SortOrder = iota
    SortAscending
    SortAscendingNullsLast
    SortDescending
    SortDescendingNullsFirst
)

type OrderingPart struct {
    Value     values.Value
    SortOrder SortOrder
}

type Distinctness int
const (
    NotDistinct Distinctness = iota
    Distinct
    PreserveDistinctness
)

type RequestedOrdering struct {
    Parts       []OrderingPart
    Distinct    Distinctness
    Exhaustive  bool
}

func (r *RequestedOrdering) PushDown(
    resultValue values.Value,
    alias values.CorrelationIdentifier,
) *RequestedOrdering

func (r *RequestedOrdering) Satisfies(provided *Ordering) bool
```

### Phase 2: Constraint integration

Extend the existing `PlanContext` to carry `RequestedOrdering`:

```go
// In plan_context.go:
type PlanContext struct {
    // ... existing fields ...
    RequestedOrdering *ordering.RequestedOrdering
}
```

Rules that need ordering declare it by reading the constraint
from the PlanContext during `OnMatch`.

### Phase 3: Cascades rules

Port these rules to `pkg/recordlayer/query/plan/cascades/`:

1. **PushRequestedOrderingThroughSelectRule** — pushDown through
   SelectExpression's resultValue.

2. **PushRequestedOrderingThroughGroupByRule** — collect compatible
   orderings from GroupByExpression's group keys and aggregates.
   Build child ordering parts: matched requested parts first, then
   remaining group keys with SortAny.

3. **RemoveSortRule enhancement** — check if the inner plan's
   physical ordering satisfies the RequestedOrdering. If yes,
   eliminate the sort entirely. This is the sort elimination win.

4. **ImplementInMemorySortRule enhancement** — when sort can't be
   eliminated, the physical sort plan evaluates sort key Values
   per record (already works this way in Go).

### Phase 4: SQL bridge (OrderByExpression.pullUp)

Port `OrderByExpression.pullUp` to the logical plan builder:

```go
// In logical_predicate.go or a new file:
func pullUpOrderBy(
    orderBys []logical.SortKey,
    resultValue values.Value,
    correlationID values.CorrelationIdentifier,
) []ordering.OrderingPart
```

Wire this into `buildLogicalPlanForQueryWithCatalog`:
1. After building the GROUP BY and SELECT expressions, call
   `pullUpOrderBy` to rewrite ORDER BY Values through the result.
2. Attach the resulting `RequestedOrdering` to the `LogicalSort`
   (or directly to the root `SelectExpression`).

### Phase 5: Delete sortOnly infrastructure

Once Phases 1-4 are working and all tests pass:

1. Delete `sortOnly` and `hidden` fields from `aggSelectCol`
2. Delete the harvest mechanism in `select_parser.go` lines 1397-1517
   (HAVING/ORDER BY aggregate harvest with sortOnly/hidden flags)
3. Delete `postSortStripProj` / `postSortStripAliases` from `selectQuery`
4. Delete the post-sort stripping projection in `buildSelectShell`
5. Delete `__orderby_aggexpr_N__` sentinel creation
6. Delete `orderByLess` proto-path sort evaluator (dead code)

### Phase 6: Ordering property on physical plans

For sort elimination to work, physical plans must report their
ordering:

```go
type PhysicalOrdering struct {
    Parts []OrderingPart
}

// On RecordQueryPlan interface:
GetOrdering() *PhysicalOrdering
```

Index scans report ordering based on the index key. Full scans
report no ordering. The sort plan reports the ordering it produces.
`RemoveSortRule` compares `PhysicalOrdering.Satisfies(RequestedOrdering)`.

## Dependencies

- Phase 1-2: standalone, no gates
- Phase 3: gates on Phase 1-2
- Phase 4: gates on Phase 1
- Phase 5: gates on Phase 3-4 passing all tests
- Phase 6: gates on Phase 3, enables sort elimination (optimization)

## Estimated scope

- Phase 1-2: ~200 LOC, 1 shift
- Phase 3: ~400 LOC, 1-2 shifts (rule porting is mechanical but needs
  careful Java reading)
- Phase 4: ~150 LOC, 1 shift
- Phase 5: ~-300 LOC (deletion), 0.5 shift
- Phase 6: ~200 LOC, 1 shift

Total: ~4-5 shifts. Phases 1-4 can land incrementally with tests.
Phase 5 is the cleanup. Phase 6 is the optimization payoff.

## Test plan

Each phase must pass `just test` (46 targets). Key tests:

- `TestFDB_OrderByAggregateExpression` — ORDER BY SUM(v)*2
- `TestFDB_CascadesOrderByNoIndex` — in-memory sort
- `TestFDB_GroupByDerivedTableComputedExpr` — derived table GROUP BY
- All yamsql conformance scenarios that touch ORDER BY

Phase 6 adds new tests for sort elimination via index ordering.

## Risks

- Phase 3 rule porting requires understanding Cascades constraint
  propagation deeply. Incorrect pushDown through GROUP BY would
  produce wrong sort orders or infinite planner loops.
- Phase 5 deletion must be atomic with Phase 3-4 — partial deletion
  breaks the proto fallback path.
- `Value.pullUp()` in Java is a complex recursive method. Go's
  values package may need extensions to support it.
