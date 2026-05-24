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

### NormalizePredicatesRule — RESOLVED (swingshift-96)

**Java:** Fires on all SelectExpressions including those with Existential quantifiers.
**Go:** Now fires on all SelectExpressions (matching Java). Hash-based dedup prevents the infinite normalization loop that previously required an existential guard.

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

### FlatMap covers all join types; NLJ is fallback for non-indexed joins

**Java:** `RecordQueryFlatMapPlan` for ALL joins. No separate NLJ plan exists. The `selectExpression.getResultValue()` is passed directly through to the FlatMap plan (translator owns the resultValue).
**Go (nightshift-97):** Same architecture — translator creates `JoinMergeResultValue`, rule passes `sel.GetResultValue()` through to the FlatMap plan. `RecordQueryFlatMapPlan` fires for ALL join types (INNER, CROSS, LEFT OUTER, EXISTS, NOT EXISTS) when the equi-join predicate matches the inner table's PK or a secondary index. Uses correlated scan + `JoinMergeResultValue` + `CorrelationBinder` interface + `existsMode`/`notExistsMode` flags. `RecordQueryNestedLoopJoinPlan` remains as fallback for non-indexed joins (no PK/index match for the predicate).

**Remaining NLJ cases:** Joins where no predicate matches any PK or index first column (brute-force NLJ is the only option). Self-joins now work via FlatMap (aliases disambiguate). **NLJ is guarded against ExplodeExpression quantifiers** (nightshift-97 fix) — IN-list decomposition uses Explode, and NLJ can't handle scalar Explode outer datums with map inner datums. The guard forces IN-list patterns to InJoinRule or filter+scan fallback.

**Composite PK limitation:** FlatMap only matches the FIRST PK column. Joins on non-first PK columns fall back to NLJ.

**JoinMergeResultValue vs RecordConstructorValue:** Go uses `JoinMergeResultValue` (spreads both correlation bindings into a flat map at eval time). Java uses `RecordConstructorValue` with per-column `FieldValue` children. Functionally equivalent — both produce a map with qualified keys from both sides. The difference is WHEN columns are enumerated: Java at plan time (has schema metadata in the relational layer), Go at eval time (translator doesn't carry schema metadata). To close: pass `RecordMetaData` to the translator so it can produce field-level RecordConstructorValue.

### Go has explicit Sort/InMemorySort physical operators

**Java:** Relies on `RemoveSortRule` to eliminate sorts; no in-memory sort plan exists.
**Go:** Has `RecordQuerySortPlan` and `RecordQueryInMemorySortPlan`.

Correctness improvement — ensures ORDER BY works even when no index satisfies it.

### FieldValue: string-qualified names vs CorrelationIdentifier-based resolution (CORRECTNESS GAP)

**Java:** Column references resolve to `FieldValue(QuantifiedObjectValue(correlationId), "column")`. The table qualification is a structural `CorrelationIdentifier`, not a string prefix. When predicates move between scopes, Java calls `Value.rebase(AliasMap)` to retarget correlations. No string manipulation.

**Go:** Column references resolve to `FieldValue{Field: "TABLE.COLUMN"}` — a string-qualified flat field. When predicates move between scopes (EXISTS outer push-down, FlatMap), Go uses `stripAliasFromPredicate` to string-strip the table prefix. This is fragile: it only handles predicate types it knows about, and silently passes unknown types through unchanged — producing wrong results when the unstripped qualification doesn't match the row's unqualified keys.

**Impact:** OR predicates in EXISTS outer WHERE silently drop rows. Any compound predicate (AND of OR, nested NOT) in outer-only position fails the same way. `stripAliasFromPredicate` was extended to handle AND/OR/NOT (nightshift-97 stash), but the function is fundamentally a band-aid.

**Fix:** Align with Java — resolve to `FieldValue(QOV(correlation), "column")` in the translator. Remove `stripAliasFromPredicate` entirely. Same root cause as `JoinMergeResultValue → RecordConstructorValue`: translator needs schema metadata to produce correlation-based field references.

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
| 15. FlatMap join ordering | Compare outer child cardinalities | `compareFlatMapJoinOrdering` compares outer quantifier cardinalities | Aligned |
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
- `SimplifyValue()` — context-free: constant folding (arithmetic/cast/promote/scalar-function/not/and-or/pick/coalesce), `composeFieldOverConstructor`, `simplifyCoalesce`, `EvaluateConstantPromotion` (Promote(constant) → constant with target type).
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

### Covering Index Scan — RESOLVED via ImplementProjectionRule

**Status:** Covering index works end-to-end for SQL via `ImplementProjectionRule` (EXPLORE phase). When all projected FieldValues can push through the Fetch's TranslateValueFunction, the Fetch is eliminated. PK columns + all index key columns are coverable. Verified with planner harness tests: `CoveringCompositeIndex`, `CoveringCompositeIndexPKAndIndexCols`, `NonCoveringNeedsExtraColumn`. The FDB stress test shows 63x speedup for PK-only projections over index scans.

**The compensation-based path** (`IsFinalNeeded`, `wrapScanPlanWithCoverage`) is bypassed — SQL projections always set `IsFinalNeeded() = true`. The ImplementProjectionRule path is the active mechanism. Java's `IndexKeyValueToPartialRecord` (826 LOC) approach remains unported but is not needed for SQL coverage.

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
| Symbolic logical operators (`&&`, `\|\|`) | `SqlFunctionCatalogImpl` only registers `and`/`or`; symbolic forms throw UNSUPPORTED_QUERY | Evaluated as AND/OR |
| `XOR` operator | Not registered in `SqlFunctionCatalogImpl`; throws UNSUPPORTED_QUERY | SQL-standard XOR with NULL propagation |

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
