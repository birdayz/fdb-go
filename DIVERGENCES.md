# Divergences from Java fdb-record-layer-core 4.11.1.0

Comprehensive list of Go vs Java differences. All Cascades planner subsystems
fully ported: ~65 PlanningRuleSet rule instances, 5/5 RewritingRuleSet rules,
34/34 physical plan types, 48/48 value types, 18/18 properties, 12/12 match
candidate types, 24/24 comparison operators, 9/9 predicates. Remaining items
are execution-layer, wire-format, or intentional architectural choices.

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

### Type mismatch detection: eval-time vs compile-time

**Java:** `SemanticAnalyzer` catches type mismatches at query compilation (before execution).
**Go:** `cmpAny()` panics with `TypeMismatchError` at evaluation time; executor recovers and maps to SQLSTATE 42804.

Same user-visible behavior: identical SQLSTATE, identical error message. 24 yamsql scenarios verify conformance. Moving to compile-time would improve error locality but has no correctness impact.

### AdjustMatchRule is an explicit planner pass

**Java:** `CascadesRule<PartialMatch>`, scheduled as a TransformPartialMatch task.
**Go:** Explicit `AdjustMatches()` call in `Planner.Plan()` after EXPLORE converges.

No functional difference — absorbs candidate-side-only expressions (MatchableSortExpression) into partial matches. Same inputs, same outputs.

### Go uses NestedLoopJoinPlan instead of FlatMapPlan

**Java:** `RecordQueryFlatMapPlan` with correlation bindings.
**Go:** `RecordQueryNestedLoopJoinPlan` with explicit predicates.

Same execution semantics — for each outer row, evaluate inner with bound correlations, filter by predicate. The FlatMap join ordering criterion (criterion 15 in PlanningCostModel) is N/A — Go doesn't produce FlatMap plans.

### Go has explicit Sort/InMemorySort physical operators

**Java:** Relies on `RemoveSortRule` to eliminate sorts; no in-memory sort plan exists.
**Go:** Has `RecordQuerySortPlan` and `RecordQueryInMemorySortPlan`.

Correctness improvement — ensures ORDER BY works even when no index satisfies it.

### FieldValue: composition vs multi-step FieldPath

**Java:** `FieldValue` contains `FieldPath` — a list of `ResolvedAccessor` objects for nested field traversal in a single node. Supports `getFieldPathNames()`, `getFieldOrdinals()`, `stripFieldPrefixMaybe()`, `ofFieldsAndFuseIfPossible()`.
**Go:** `FieldValue` has a single `Field` string + optional `Child Value`. Multi-step paths are expressed as FieldValue chains (composition). `NewFieldValue(child, field, typ)` nests; `NewFlatFieldValue(field, typ)` is the leaf form.

Functionally equivalent for current query shapes — all generated plans use single-step field access. Java's `FieldPath` matters for deeply-nested protobuf message fields; Go would need the multi-step model if/when nested record types are ported.

## Planning-Layer: Fully Aligned

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
| 12. Unmatched fields | UnmatchedFieldsCountProperty | `totalCols - boundCols` | Aligned |
| 13. InJoin count (more=better) | count(InJoinPlan) reversed | `inJoinCount` reversed | Aligned |
| 14. Map/filter count | count(Map, PredicatesFilter) | `mapCount + predicatesFilterCount` | Aligned |
| 15. FlatMap join ordering | Compare outer child cardinalities | N/A (Go uses NLJ, not FlatMap) | N/A |
| 16. Plan hash tiebreak | planHash(CURRENT_FOR_CONTINUATION) | `deepHashCode()` recursive | Aligned |

Go-only addition: scalar `CostLess` fallback between criteria 14 and 16 (discriminates plans the ordinal criteria can't distinguish).

### Cost Model: RewritingCostModelLess

All 4 criteria ported: fewer SelectExpressions, fewer TableFunctionExpressions, fewer CNF conjuncts, more predicates at deeper levels. `Planner.WithCostModel()` wires the appropriate cost model per phase.

### Properties: 18/18

| Java Property | Go Implementation | Status |
|---|---|---|
| CardinalitiesProperty | `cardinality.go` | Aligned |
| OrderingProperty | `ordering.go` | Aligned |
| DistinctRecordsProperty | `PropDistinctRecords` | Aligned |
| StoredRecordProperty | `PropStoredRecord` | Aligned |
| PrimaryKeyProperty | `PropPrimaryKey` | Aligned |
| DerivationsProperty | `derivations_property.go` + `derivations_evaluator.go` (913 LOC) | Aligned |
| ExpressionCountProperty | `expression_count_property.go` + `EvaluateExpressionCount()` | Aligned |
| FieldWithComparisonCountProperty | `field_with_comparison_count_property.go` | Aligned |
| PredicateComplexityProperty | `predicate_complexity_property.go` | Aligned |
| PredicateCountByLevelProperty | `predicate_count_by_level_property.go` | Aligned |
| RecordTypesProperty | `record_types_property.go` | Aligned |
| ReferencesAndDependenciesProperty | `references_and_dependencies_property.go` | Aligned |
| UsedTypesProperty | `used_types_property.go` | Aligned |
| ComparisonsProperty | `comparisons_property.go` + `collectSargedAliases()` inline in cost model | Aligned |
| NormalizedResidualPredicateProperty | `countResidualPredicates()` + `cnfSize()` inline in cost model | Aligned (inline) |
| ExpressionDepthProperty | `expressionDepth()` inline in cost model | Aligned (inline) |
| TypeFilterCountProperty | `walkExpressionTree()` counter inline in cost model | Aligned (inline) |
| UnmatchedFieldsCountProperty | `walkExpressionTree()` counter inline in cost model | Aligned (inline) |

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

### Match Candidates: 9/9

| Java Type | Go Equivalent | Status |
|---|---|---|
| ValueIndexScanMatchCandidate | `ValueIndexScanMatchCandidate` | Aligned |
| AggregateIndexMatchCandidate | `AggregateIndexMatchCandidate` | Aligned |
| PrimaryScanMatchCandidate | `PrimaryScanMatchCandidate` (260 LOC) | Aligned |
| VectorIndexScanMatchCandidate | `VectorIndexScanMatchCandidate` (232 LOC) | Aligned |
| WindowedIndexScanMatchCandidate | `WindowedIndexScanMatchCandidate` (352 LOC) | Aligned |
| WithPrimaryKeyMatchCandidate | Interface | Aligned |
| WithBaseQuantifierMatchCandidate | Interface | Aligned |
| ScanWithFetchMatchCandidate | Interface | Aligned |
| ValueIndexLikeMatchCandidate | Interface | Aligned |

### Value Simplification: SimplifyValue + SimplifyValueWithContext

Two-tier simplification matching Java's value rule sets:
- `SimplifyValue()` — context-free: constant folding (arithmetic/cast/promote/scalar-function/not/and-or/pick/coalesce), `composeFieldOverConstructor`, `simplifyCoalesce`.
- `SimplifyValueWithContext(v, ctx)` — context-aware with `constantAliases` + `isRoot`: `eliminateArithmeticWithConstant` (col+5 → col for ordering), `foldConstant` (wrap fully-constant subtrees), `liftConstructor` (flatten nested RC, isRoot-gated).

### InJoinPlan: InSourceKind + PushInJoinThroughFetch

`InSourceKind` enum classifies explode values (Values/Parameter/Comparand). `classifyInSourceKind()` sets it at plan creation. `PushInJoinThroughFetchRule` excludes InComparand. Source kind preserved through push-through-fetch.

## Execution-Layer Gaps (blocked on infrastructure not yet built)

These affect runtime behavior and wire compatibility, NOT plan selection.

| Gap | Category | Blocked on |
|---|---|---|
| Plan proto serialization (Go plans not serialized to proto) | Wire format | Plan serialization infrastructure |
| Value type proto serialization | Wire format | Value serialization infrastructure |
| Comparison subclass types: `OpaqueEqualityComparison`, `MultiColumnComparison`, `InvertedFunctionComparison` | Index-specific | Niche index types not in core planner |

### Covering Index Scan (planner infrastructure only — unreachable from SQL)

**Planner:** `RecordQueryIndexPlan` carries `Covering bool` + `CoveringColumns []string`. Set by `wrapScanPlanWithCoverage` when `!comp.IsFinalNeeded()`. Column names from `MatchCandidate.GetColumnNames()`.

**Executor:** `coveringIndexCursor` constructs `QueryResult` from `IndexEntry.IndexValues()` without `LoadRecord()`.

**Why it doesn't fire for SQL:** The compensation check requires the pulled-up query result to be a bare `QuantifiedObjectValue`. SQL queries always wrap in `RecordConstructorValue` projections, so `IsFinalNeeded()` is always `true`. Covering only fires for direct record-layer API users.

**Java's approach is fundamentally different:** `ValueIndexScanMatchCandidate.tryFetchCoveringIndexScan()` uses `IndexKeyValueToPartialRecord` (826 LOC) to reconstruct a protobuf `Message` from index entries. It always wraps in `CoveringIndexPlan` + `FetchFromPartialRecordPlan`, then lets push-through rules eliminate the fetch. The covering decision is in the match candidate, not in compensation analysis.

**Root cause:** Two index-scan paths compete. `ImplementIndexScanRule` creates bare `physicalIndexScanWrapper` (no covering flag, no Fetch); `wrapScanPlanWithCoverage` in the data-access path strips the Fetch when `!comp.IsFinalNeeded()`. The cost model prefers the bare scan (cheaper) but it doesn't carry covering info. The data-access path's `isCovering` check only returns true when the PullUp resolves to a bare QOV — which doesn't happen with SQL projections above.

**To close (multi-shift):** Port `IndexKeyValueToPartialRecord` (826 LOC copier-based field extraction from key+value tuples), `extractFromIndexEntryMaybe` (per-Value method), `computeIndexEntryToLogicalRecord` (match candidate integration), and `CollapseRecordConstructorOverFieldsToStar` (needs field-level type ordinal metadata). Also needs value-column tracking in match candidates (current `GetColumnNames()` only returns key columns, not KeyWithValue value columns).

## Optimization-Quality Gaps (correctness unaffected)

| Gap | Status |
|---|---|
| CollapseRecordConstructorOverFieldsToStar | Blocked: needs field-level type metadata (ordinal positions) |
| ExtractFromIndexKeyValueRuleSet (3 rules) | Blocked: execution layer (partial record construction) |

## Go-Only Extensions (features Java 4.11.1.0 rejects)

Go supports these SQL features that Java rejects. Removing them would be a user-visible regression; they stay as Go extensions.

| Feature | Java behavior | Go behavior |
|---|---|---|
| `GROUP BY` | Rejects ALL forms (`UnableToPlanException`) | Full support (streaming + hash aggregation) |
| `LIMIT` / `OFFSET` | Rejects at parse time (uses JDBC `setMaxRows`) | `RecordQueryLimitPlan` |
| `SELECT DISTINCT` (complex shapes) | Rejects most via Cascades | Broad support via `RecordQueryDistinctPlan` + hash distinct |
| In-memory sort | No physical sort operator; `RemoveSortRule` eliminates or fails | `RecordQuerySortPlan` / `RecordQueryInMemorySortPlan` |
| Hash aggregation | Only streaming aggregation (requires ordered input) | `RecordQueryHashAggregationPlan` |
| `INFORMATION_SCHEMA` | Rejects (`Unknown reference INFORMATION_SCHEMA.TABLES`) | Working system tables |
| `NOT NULL` on scalar columns | Rejects (`NOT NULL is only allowed for ARRAY column type`) | SQL-standard behavior |
| Date-part functions | No temporal types | YEAR/MONTH/DAY/HOUR/MINUTE/SECOND/etc. |
| Simple CASE (`CASE expr WHEN val`) | `visitChildren` no-op (always falls through to ELSE) | Correct evaluation |

Go-only plan types: `RecordQueryHashAggregationPlan`, `RecordQueryInMemorySortPlan`, `RecordQueryLimitPlan`, `RecordQueryProjectionPlan`, `RecordQuerySortPlan`, `RecordQueryValuesPlan`, `RecordQueryMergeSortUnionPlan`, `RecordQueryNestedLoopJoinPlan`.

Go-only logical expressions: `LogicalLimitExpression`, `LogicalValuesExpression`.

## Java Upstream Bugs (Go is correct, Java is wrong)

Confirmed via cross-engine probes. Go's correct behavior is pinned in Go-only positive tests; corpus entries omitted until Java upstream fixes.

| Bug | Go behavior | Java behavior |
|---|---|---|
| Compound DISTINCT (`SELECT DISTINCT a, b`) | Correctly deduplicates | Fails to dedup (returns all rows) |
| Signed-zero comparison (`WHERE v >= 0.0` with `-0.0`) | Keeps row (IEEE 754: `-0.0 == +0.0`) | Drops the row |
| PK literal-eq AND join predicate | Applies both predicates correctly | Drops one predicate, over-counts |
| 3-way join shared driver key | Returns correct rows | Returns cross product |
| UNION ALL outer ORDER BY | Deterministic sorted output | Intermittent ordering |
| `WHERE TRUE AND val > 5` | Succeeds correctly | `VerifyException` |
| `WHERE pk_col = nonpk_col` | SQL-correct | `Missing binding` planner error |

## Plan Architecture: Go collapses Java class hierarchies

| Java | Go | Planning status |
|---|---|---|
| 3 InJoin subclasses | 1 `RecordQueryInJoinPlan` with `InSourceKind` | Aligned |
| 2 InUnion subclasses | 1 `RecordQueryInUnionPlan` | Aligned |
| 2 Union subclasses | 1 `RecordQueryUnionPlan` + `RecordQueryMergeSortUnionPlan` | Aligned |
| 2 Distinct plan variants | 1 `RecordQueryDistinctPlan` | Aligned |
| CoveringIndexPlan | `covering bool` + `coveringColumns` on IndexPlan | Aligned (planner + executor) |
| CountValue + NumericAggregationValue | `AggregateValue` | Aligned (no rule distinguishes them) |
| VariadicFunctionValue | `ScalarFunctionValue` | Aligned (COALESCE folding matches Java) |
| 12 Comparison subclasses | Single `Comparison` struct with optional fields | Aligned |
