# Divergences from Java fdb-record-layer-core 4.11.1.0

Comprehensive list of Go vs Java differences. Planning-layer divergences are
fully closed. Remaining items are execution-layer, architectural, or blocked
on infrastructure not yet built.

## Intentional Architectural Decisions (no functional difference)

### Go decomposes SelectExpression into separate logical operators

**Java:** `SelectExpression` is a unified node for filters, projections, and joins.
**Go:** Decomposes into `LogicalFilterExpression`, `LogicalProjectionExpression`, `LogicalSortExpression`, etc.

Go needs ~25 extra rewrite rules (Push/Pull/Merge per operator). Same functional behavior. Go's decomposition makes each operator's semantics explicit and simplifies rule correctness verification.

### NormalizePredicatesRule skips Existential quantifiers

**Java:** Fires on all SelectExpressions including those with Existential quantifiers.
**Go:** Skips SelectExpressions with Existential quantifiers.

Go's fixpoint architecture fires rules on all Reference members. Normalizing an EXISTS-bearing SelectExpression yields a new expression that downstream rules can't plan. The guard preserves the structural invariants EXISTS depends on. Same functional behavior.

### WithPrimaryKeyDataAccessRule is an explicit planner pass

**Java:** `CascadesRule<MatchPartition>`, fired via match-partition rule infrastructure.
**Go:** Explicit pass in `Planner.generateDataAccessWithConstraints()`.

No functional difference — same timing, same inputs, same outputs.

### Go uses NestedLoopJoinPlan instead of FlatMapPlan

**Java:** `RecordQueryFlatMapPlan` with correlation bindings.
**Go:** `RecordQueryNestedLoopJoinPlan` with explicit predicates.

Same execution semantics — for each outer row, evaluate inner with bound correlations, filter by predicate. The FlatMap join ordering criterion (criterion 15 in PlanningCostModel) is N/A — Go doesn't produce FlatMap plans.

### Go has explicit Sort/InMemorySort physical operators

**Java:** Relies on `RemoveSortRule` to eliminate sorts; no in-memory sort plan exists.
**Go:** Has `RecordQuerySortPlan` and `RecordQueryInMemorySortPlan`.

Correctness improvement — ensures ORDER BY works even when no index satisfies it.

## Planning-Layer Aligned (all fixed this shift)

### Cost Model: PlanningCostModelLess

All 16 criteria ported. Criterion-by-criterion analysis:

| Criterion | Java | Go | Status |
|---|---|---|---|
| 1. Physical beats non-physical | `instanceof RecordQueryPlan` | `isPhysical` | Aligned |
| 2. Max data access cardinality | CardinalitiesProperty gate + comparison | Data-access cardinality gate | Functionally equivalent |
| 3. Residual predicate count | NormalizedResidualPredicateProperty (CNF size) | `countResidualPredicates` using `cnfSize()` | Aligned |
| 4. Data access count | count(Scan, Index, Covering) | `scanCount + indexScanCount + coveringIndexCount` | Aligned |
| 5. Recursive CTE DFS > level | flipFlop(compareRecursiveCte) | `compareRecursiveCTE` | Aligned |
| 6. IN-plan SARG penalty | flipFlop(compareInOperator) | `compareInPlan` with `(int, bool)` flipFlop | Aligned |
| 7. Primary vs index scan | comparison-set analysis + PREFER_INDEX | `comparePrimaryScanVsIndexScan` + `isSingularIndexScanWithFetch` | Aligned (PREFER_INDEX default; comparison-set analysis redundant for default config) |
| 8. Type filter count | TypeFilterCountProperty | `len(GetRecordTypes())` per filter | Aligned |
| 9. Type filter depth | ExpressionDepthProperty | `expressionDepth` (min across all members) | Aligned |
| 10. Index scan fetches | count(PlanWithIndex, Fetch) | `indexScanCount + fetchCount` | Aligned |
| 11. Distinct depth | ExpressionDepthProperty | `expressionDepth` | Aligned |
| 12. Unmatched fields | UnmatchedFieldsCountProperty | `totalCols - boundCols` | Aligned (primary scans have no comparisons in Go, so always tie — N/A) |
| 13. InJoin count (more=better) | count(InJoinPlan) reversed | `inJoinCount` reversed | Aligned |
| 14. Map/filter count | count(Map, PredicatesFilter) | `mapCount + predicatesFilterCount` | Aligned |
| 15. FlatMap join ordering | Compare outer child cardinalities | N/A (Go uses NLJ, not FlatMap) | N/A |
| 16. Plan hash tiebreak | planHash(CURRENT_FOR_CONTINUATION) | `deepHashCode()` recursive | Aligned |

Go-only addition: scalar `CostLess` fallback between criteria 14 and 16 (discriminates plans the ordinal criteria can't distinguish).

### Cost Model: RewritingCostModelLess

All 4 criteria ported: fewer SelectExpressions, fewer TableFunctionExpressions, fewer CNF conjuncts, more predicates at deeper levels. `Planner.WithCostModel()` wires the appropriate cost model per phase.

### 5 Inlined Property Computations

| Java Property | Go Location | Status |
|---|---|---|
| NormalizedResidualPredicateProperty | `countResidualPredicates()` + `cnfSize()` | Aligned |
| ExpressionDepthProperty | `expressionDepth()` | Aligned |
| TypeFilterCountProperty | `walkExpressionTree()` counter | Aligned |
| UnmatchedFieldsCountProperty | `walkExpressionTree()` counter | Aligned |
| ComparisonsProperty | `collectSargedAliases()` | Aligned (intersection semantics for intersection plans) |

### Predicate Simplification: 12/12 Rules Covered

| Java Rule | Go Equivalent | Status |
|---|---|---|
| IdentityAndRule | AndConstantSimplifyRule | Aligned |
| IdentityOrRule | OrConstantSimplifyRule | Aligned |
| AnnulmentAndRule | AndConstantSimplifyRule (TriFalse short-circuit) | Aligned |
| AnnulmentOrRule | OrConstantSimplifyRule (TriTrue short-circuit) | Aligned |
| AbsorptionRule | AndAbsorbOrRule / OrAbsorbAndRule + `applyAbsorption` | Aligned |
| DeMorgansTheoremRule | DeMorganRule | Aligned |
| NotOverComparisonRule | NotComparisonRewriteRule (5 invertible operators) | Aligned |
| NormalFormRule (CNF) | `normalizeCNF` | Aligned |
| NormalFormRule (DNF) | `NormalizeDNF()` | Aligned |
| ConstantFoldingValuePredicateRule | ValuePredicateConstantFoldRule | Aligned |
| ConstantFoldingPredicateWithRangesRule | `foldPredicateWithRanges()` | Aligned |
| ConstantFoldingMultiConstraintPredicateRule | `foldPredicateWithRanges()` multi-constraint | Aligned |

### NotOverComparisonRule: Negate() matches invertComparisonType()

5 operators: EQUALS→NOT_EQUALS, LT↔GTE, LTE↔GT. All others rejected.

### InJoinPlan: InSourceKind + PushInJoinThroughFetch

`InSourceKind` enum classifies explode values (Values/Parameter/Comparand). `classifyInSourceKind()` sets it at plan creation. `PushInJoinThroughFetchRule` excludes InComparand. Source kind preserved through push-through-fetch.

### Value Simplification: SimplifyValue + SimplifyValueWithContext

Two-tier simplification matching Java's value rule sets:
- `SimplifyValue()` — context-free: constant folding (arithmetic/cast/promote/scalar-function/not/and-or/pick/coalesce), `composeFieldOverConstructor`, `simplifyCoalesce`.
- `SimplifyValueWithContext(v, ctx)` — context-aware with `constantAliases` + `isRoot`: `eliminateArithmeticWithConstant` (col+5 → col for ordering), `foldConstant` (wrap fully-constant subtrees), `liftConstructor` (flatten nested RC, isRoot-gated).

## Execution-Layer Gaps (blocked on infrastructure not yet built)

These affect runtime behavior and wire compatibility, NOT plan selection.

| Gap | Category | Blocked on |
|---|---|---|
| InSource abstraction (InParameter runtime lookup, InComparand extraction) | Execution | Query execution engine |
| CoveringIndexPlan struct (`IndexKeyValueToPartialRecord`, `recordTypeName`) | Execution | Partial-record reconstruction from index entries |
| Plan proto serialization (Go plans are not serialized to proto at all) | Wire format | Plan serialization infrastructure |
| Value type proto (CountValue, NumericAggregationValue, VariadicFunctionValue) | Wire format | Value serialization infrastructure |
| Comparison subclass types: `OpaqueEqualityComparison`, `MultiColumnComparison`, `InvertedFunctionComparison` | Index-specific | Niche index types not in core planner |

## Optimization-Quality Gaps (correctness unaffected)

| Gap | Status |
|---|---|
| CollapseRecordConstructorOverFieldsToStar | Blocked: needs field-level type metadata (ordinal positions) |
| EliminateArithmeticValueWithConstantRule | **DONE** — `eliminateArithmeticWithConstant()` in `SimplifyValueWithContext` |
| FoldConstantRule | **DONE** — `foldConstant()` in `SimplifyValueWithContext` |
| LiftConstructorRule | **DONE** — `liftConstructor()` in `SimplifyValueWithContext` (isRoot-gated) |
| ExtractFromIndexKeyValueRuleSet (3 rules) | Blocked: execution layer (partial record construction) |

## Plan Architecture: Go collapses Java class hierarchies

| Java | Go | Planning status |
|---|---|---|
| 3 InJoin subclasses | 1 `RecordQueryInJoinPlan` with `InSourceKind` | Aligned |
| 2 InUnion subclasses | 1 `RecordQueryInUnionPlan` | Aligned (Cascades-relevant variant covered) |
| 2 Union subclasses | 1 `RecordQueryUnionPlan` + `RecordQueryMergeSortUnionPlan` | Aligned |
| 2 Distinct plan variants | 1 `RecordQueryDistinctPlan` | Aligned (Cascades produces PK-based only) |
| CoveringIndexPlan | `covering bool` on IndexPlan | Aligned for planning; execution gap |
| CountValue + NumericAggregationValue | `AggregateValue` | Aligned (no rule distinguishes them) |
| VariadicFunctionValue | `ScalarFunctionValue` | Aligned (COALESCE folding matches Java) |
| 12 Comparison subclasses | Single `Comparison` struct with optional fields | Aligned: `ParameterName`, `TextTokenizerName`, `TextAnalyzerName`, `TextMaxDistance`, `TextStrictPrefix`, `Escape` cover SimpleComparison, ParameterComparison, ValueComparison, TextComparison variants. Niche types (OpaqueEquality, MultiColumn, InvertedFunction) not yet needed |
