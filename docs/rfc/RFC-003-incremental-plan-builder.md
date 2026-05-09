# RFC-003: Incremental Plan Builder (eliminate selectQuery struct)

**Status:** Draft
**Author:** swingshift-81
**Java reference:** fdb-record-layer 4.11.1.0, fdb-relational-core QueryVisitor

## Problem

Go parses SQL into a `selectQuery` struct (~30 fields, 1700-line
parser), then a separate function (`buildLogicalPlanForSelect` +
`buildSelectShell`) interprets it into `LogicalOperator` nodes. A
third layer (`buildLogicalPlanForSelectWithCatalog` + `_postBuild`)
upgrades the tree with catalog-aware Values/Predicates.

This three-phase pipeline creates stale-state bugs:
- `sq.projCols` was stale after GROUP BY reclassification (fixed swingshift-81)
- `sq.postAggExprs` is mutated during build, read during upgrade
- `sq.orderBy[].colName` is rewritten by the harvest, breaking the
  rawExpr fallback path
- Any new field added to `selectQuery` requires coordinating parse,
  build, and upgrade phases

Java has none of this. `QueryVisitor.visitSimpleTable()` builds the
logical plan incrementally as it visits ANTLR nodes — no intermediate
struct, no phase coordination.

## Java Architecture

```
visitSimpleTable():
  1. FROM clause      → visit → LogicalOperator (scan/derived/join)
  2. WHERE clause     → generateSelectWhere(predicates) → wraps #1
  3. GROUP BY clause  → visitGroupByClause → collect expressions
  4. SELECT elements  → visitSelectElements → collect projections
  5. HAVING clause    → visitHavingClause → collect predicate
  6. ORDER BY clause  → visitOrderByClause → collect OrderByExpressions
  7. generateGroupBy(selectExprs, groupByExprs, ...) → wraps #2
  8. generateSelect(selectExprs, orderBys, ...) → wraps #7
```

Each step takes the current `LogicalOperator` and wraps it with the
next layer. State is ephemeral: expressions are collected, consumed,
and discarded. No struct survives across steps.

Key insight: **GROUP BY alias resolution** (circular dependency where
GROUP BY needs SELECT aliases and vice versa) is handled by
temporarily adding aliased columns to the operator's output, visiting
SELECT elements with those visible, then reverting. Clean and local.

## Go Port Plan

### Phase 1: Define the incremental builder

New file: `pkg/relational/core/embedded/plan_visitor.go`

```go
// PlanVisitor walks ANTLR parse tree nodes and builds a
// logical.LogicalOperator tree incrementally. Mirrors Java's
// QueryVisitor. Each visit method takes the current operator and
// returns a wrapped operator.
type PlanVisitor struct {
    md        *recordlayer.RecordMetaData
    cteScopes map[string]semantic.ScopeSource
    resolver  *expr.Resolver
}

// VisitSimpleTable is the main entry point for SELECT queries.
// Walks: FROM → WHERE → GROUP BY → SELECT → HAVING → ORDER BY →
// LIMIT → DISTINCT. Each step wraps the operator.
func (v *PlanVisitor) VisitSimpleTable(
    simpleTable *antlrgen.SimpleTableContext,
) (logical.LogicalOperator, error)
```

### Phase 2: Port each visit step

Each step becomes a method on `PlanVisitor` that takes the current
`op` and the ANTLR node, and returns a wrapped `op`:

```go
// Step 1: FROM → scan / derived / join
func (v *PlanVisitor) visitFrom(
    fromClause antlrgen.IFromClauseContext,
) (logical.LogicalOperator, error)

// Step 2: WHERE → LogicalFilter
func (v *PlanVisitor) visitWhere(
    op logical.LogicalOperator,
    where antlrgen.IWhereExprContext,
) (logical.LogicalOperator, error)

// Step 3-5: GROUP BY + SELECT + HAVING → LogicalAggregate + LogicalProject
func (v *PlanVisitor) visitGroupBySelect(
    op logical.LogicalOperator,
    groupBy antlrgen.IGroupByClauseContext,
    selectElements antlrgen.ISelectElementsContext,
    having antlrgen.IExpressionContext,
) (logical.LogicalOperator, error)

// Step 6: ORDER BY → LogicalSort
func (v *PlanVisitor) visitOrderBy(
    op logical.LogicalOperator,
    orderBy []antlrgen.IOrderByExpressionContext,
) (logical.LogicalOperator, error)

// Step 7: LIMIT / OFFSET → LogicalLimit
func (v *PlanVisitor) visitLimit(
    op logical.LogicalOperator,
    limit, offset int64,
) (logical.LogicalOperator, error)

// Step 8: DISTINCT → LogicalDistinct
func (v *PlanVisitor) visitDistinct(
    op logical.LogicalOperator,
    distinct bool,
) (logical.LogicalOperator, error)
```

### Phase 3: Inline catalog-aware resolution

The current `_postBuild` phase walks the already-built tree and
upgrades predicates/values. In the incremental builder, resolution
happens inline during each visit step:

- `visitWhere`: walks the WHERE predicate through the resolver
  immediately, producing a `QueryPredicate` on the `LogicalFilter`.
  No separate `upgradeWherePredicate` pass.
- `visitGroupBySelect`: walks GROUP BY expressions and SELECT
  projections through the resolver immediately, producing
  `GroupKeyValues`, `AggregateOperands`, and `ProjectedValues`.
  No separate `upgradeAggregateOperands` / `upgradeProjectionValues`.
- `visitOrderBy`: resolves sort key Values immediately.
  No separate `upgradeSortKeyValues`.

### Phase 4: Migrate callers

Replace the three-phase pipeline:

```go
// Before (three phases):
sq, err := extractFromSimpleTable(simpleTable)  // Phase 1: parse
op := buildLogicalPlanForSelect(sq)              // Phase 2: build
op, err = postBuild(op, sq, md, cteScopes)       // Phase 3: upgrade

// After (one phase):
v := NewPlanVisitor(md, cteScopes)
op, err := v.VisitSimpleTable(simpleTable)
```

Wire this into `cascades_generator.go` where the Cascades path
builds its logical plan.

### Phase 5: Delete dead code

- Delete `selectQuery` struct and all 30+ fields
- Delete `extractFromSimpleTable` (~800 lines)
- Delete `buildLogicalPlanForSelect` (~100 lines, FROM-source only)
- Delete `buildSelectShell` (~250 lines, absorbed into visitor)
- Delete `buildLogicalPlanForSelectWithCatalog` + `_postBuild` (~300 lines)
- Delete all `upgrade*` functions (~400 lines)
- Delete `buildDerivedTableSource` / `buildDerivedTableSourceFromAgg`
  (scope building moves into `visitFrom`)
- Delete `buildProjectionResolverWithCTEScopes` / `buildSelectScope`
  (resolver built once in PlanVisitor constructor)

**Estimated deletion: ~1800 lines.**

### Phase 6: Keep proto path working

The proto/naive generator still uses `selectQuery` for execution
(not just planning). Either:
a) Keep `selectQuery` for the proto path only, build it from
   the ANTLR tree as before. The Cascades path uses the visitor.
b) Make the proto path also use the visitor's output (LogicalOperator
   tree) and execute from that.

Option (a) is safer — the proto path is legacy and will be deleted
when Cascades handles all shapes. Option (b) is cleaner but higher
risk.

## Execution order

- Phase 1-2: Port each visit step one at a time, test incrementally.
  Start with `visitFrom` (simplest — just builds scan/derived).
  Then `visitWhere`, `visitGroupBySelect`, `visitOrderBy`, etc.
- Phase 3: Inline resolution into each step as it's ported.
- Phase 4: Switch the Cascades generator to use the visitor.
- Phase 5: Delete dead code.
- Phase 6: Keep proto path on selectQuery (option a).

Each step is independently testable. The visitor and the old pipeline
coexist until Phase 4 switches the caller.

## Risks

- The proto path uses `selectQuery` fields directly for execution
  (column iteration, rowMap construction, sort comparator). Option
  (a) preserves this; option (b) requires porting the executor too.
- GROUP BY alias resolution circular dependency needs careful
  handling — Java's "temporarily add aliased columns" trick must
  be ported.
- The `aggSelectCol` struct carries both parse-time and build-time
  state. The visitor needs a clean separation — parse-time data
  (outExpr, aggFunc) feeds directly into logical operators without
  an intermediate representation.

## Estimated scope

- Phase 1-2: ~800 LOC new (visitor methods), 2 shifts
- Phase 3: ~200 LOC (inline resolution), folded into Phase 2
- Phase 4: ~50 LOC (wire caller), 0.5 shift
- Phase 5: ~-1800 LOC (deletion), 0.5 shift
- Phase 6: ~0 LOC (option a — keep proto path as-is)

Total: ~3 shifts. Net: ~-800 LOC.
