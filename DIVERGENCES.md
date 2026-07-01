# Divergences from Java fdb-record-layer-core 4.12.11.0

Comprehensive list of Go vs Java differences. All Cascades planner subsystems
fully ported: ~65 PlanningRuleSet rule instances, 5/5 RewritingRuleSet rules,
34/34 physical plan types, 48/48 value types, 18/18 properties, 12/12 match
candidate types, 24/24 comparison operators, 9/9 predicates. Remaining items
are execution-layer, wire-format, or intentional architectural choices.

Validated against a live Java **4.12.11.0** conformance run (the cross-engine corpus runs against
live 4.12 in `just test` with a stale-annotation guard, and the suite is green).

## Intentional Architectural Decisions (no functional difference)

### Go decomposes SelectExpression into separate logical operators

**Java:** `SelectExpression` is a unified node for filters, projections, and joins.
**Go:** Decomposes into `LogicalFilterExpression`, `LogicalProjectionExpression`, `LogicalSortExpression`, etc.

Go needs ~25 extra rewrite rules (Push/Pull/Merge per operator). Same functional behavior. Go's decomposition makes each operator's semantics explicit and simplifies rule correctness verification.

### NormalizePredicatesRule — RESOLVED

**Java:** Fires on all SelectExpressions including those with Existential quantifiers.
**Go:** Now fires on all SelectExpressions (matching Java). Hash-based dedup prevents the infinite normalization loop that previously required an existential guard.

### WithPrimaryKeyDataAccessRule is an explicit planner pass (UPDATED Phase 7.2)

**Java:** `CascadesRule<MatchPartition>`, fired via match-partition rule infrastructure. `createIntersectionAndCompensation` aggregates cross-candidate matches into physical intersection plans during PLANNING.
**Go (Phase 7.2):** Explicit pass in `Planner.pushDataAccessTasks()`. `WithPrimaryKeyIntersector` creates physical `RecordQueryIntersectionPlan` from cross-candidate `PartialMatch` pairs. `IndexIntersectionRule` (Go-only REWRITING rule) deleted. Guards: candidate cap (4), match cap (8), restricted-scan filter, idempotency.

Same timing and inputs. Go creates physical plans directly (single intersection strategy); Java goes through `LogicalIntersectionExpression` → `ImplementIntersectionRule` (supports multiple strategies).

### ImplementIndexScanRule is a Go-only second index-scan path (compensatability guarded at two layers)

**Java:** One rule family — `AbstractDataAccessRule` — turns a `PartialMatch` into a scan/index-scan/fetch via `toEquivalentPlan`. The "index-only value can't be a residual" property is enforced ONCE: `PredicateMultiMap.ofPredicate` stamps `isImpossible = predicateContainsUncompensatableValues(predicate)` (true when a predicate operand `instanceof Value.IndexOnlyValue`), and `applyCompensationForSingleDataAccessMaybe` drops any match whose compensation `isImpossible()`. No separate "implement index scan" rule exists, so the property can't leak.

**Go:** Two paths reach a physical index scan: (1) the data-access/compensation match path (`predicate_multi_map.go`), and (2) `ImplementIndexScanRule` — a Go-only fusion of Java's `ImplementPhysicalScanRule` + candidate matching that iterates `ComparisonPredicate`s directly and synthesizes residual filters itself, bypassing `Compensation`. So the index-only compensatability check is applied at BOTH layers: `valueContainsUncompensatable` via `values.IsIndexOnly` (match path) and the residual-skip loop in `ImplementIndexScanRule.OnMatch` (implement path). Both are load-bearing — removing either makes `TestVectorPlan_QualifyPlansToVectorScan` fail; the implement layer is pinned directly by `TestImplementIndexScanRule_SkipsIndexOnlyResidual`. This surfaced wiring up vector K-NN (RFC-045): the `DistanceRowNumberValue` operand is index-only, and a partition-only primary-scan candidate would otherwise leave the `DistanceRank` comparison as a residual filter (panics in `Comparison.EvalAgainst`).

A THIRD Go-only filter producer was `ImplementFilterRule` (synthesizes a `RecordQueryPredicatesFilterPlan` over the inner physical winner without routing through `Compensation`). **RESOLVED (RFC-151):** `ImplementFilterRule` now carries Java's `all(anyCompensatablePredicate())` / `!isIndexOnly()` gate (`ImplementFilterRule.java:62`, `QueryPredicateMatchers.java:66-68`) — it returns early for any index-only predicate, exactly like Java, so the leaking filter is **never built**. The old "guarding ImplementFilterRule is not viable — removing its member collapses the filter Reference and the data-access intersection is never built" claim was a *scheduling* artifact, not a memo entanglement: Go's `pushDataAccessTasks` ran inline at `ExploreExprTask` start, BEFORE the matching rules seeded the ref's partial matches, so the data-access consumption depended on `ImplementFilterRule`'s incidental physical-filter yield to re-trigger exploration. RFC-151 makes `TransformExprTask` re-run data-access whenever a rule grows the ref's partial-match set (Java's `getNewPartialMatches()` reaction, `CascadesPlanner.java:1058-1062`), so the legitimate vector scan is consumed directly and the gate is safe.

**The `validateNoIndexOnlyResidual` physical net is RETAINED as the catch-all backstop** — the gate covers ONLY `ImplementFilterRule`, but an index-only `DistanceRank` in the original query reaches a physical residual via other Go-only builders too: `ImplementSimpleSelectRule` (a `SelectExpression`'s predicates — the JOIN shape, where the distance is a Select predicate not a standalone LogicalFilter), the NLJ residual builder, and `ImplementIndexScanRule`'s residual loop. The net is the one place covering EVERY physical-filter path. A logical-side `findIndexOnlyLogicalResidual` check is ADDED for the complementary case (when the gate leaves the best plan non-physical, the physical walk sees nothing). Both surface the clean `UnplannableIndexOnlyResidualError`. Pinned by `TestVectorPlan_MetricMismatchDoesNotMatchVector` (single-table) + `TestVectorPlan_MetricMismatchInJoinDoesNotLeak` (join, the regression Graefe + Torvalds caught) + `TestVectorPlan_QualifyPlansToVectorScan` (legit). **End-state to fully retire the net:** gate `ImplementSimpleSelectRule` + NLJ on `!isIndexOnly()` (and retire `ImplementIndexScanRule`) so no physical builder can produce an index-only residual — a smaller separate follow-up.

**Phase-1 (RFC-148, Option A) update:** the data-access compensation path no longer uses the
`isSimpleResidualCompensation` predicate-shape allowlist. A standalone (non-join-leg) logical compensation
now routes through `yieldUnknown` → exploratory re-optimization (Java's `yieldUnknownExpression`), EXCEPT
when `compensationSafeForYield` is false. **RFC-151 update:** `compensationSafeForYield`'s index-only-predicate
branch is **retired** — the `ImplementFilterRule` `!isIndexOnly()` gate above is now the single structural
authority for "index-only predicate can't be a residual", so guarding it again here would be a redundant
second authority. The **inner-scan guard** (vector/aggregate inner) STAYS: it protects a *normal* residual
applied after a top-K / grouping (the `TrailingEqualityResidual` shape) — a different property the gate does
not cover, and the reason that sentinel must still be unplannable.

The earlier "match-level consumption" framing turned out to be a mis-diagnosis: Go's vector match already
BINDS the `DistanceRank` (via `flattenConjuncts`); the index-only value was never an unconsumed residual at
the match — the leak was purely the `ImplementFilterRule` scheduling coupling (above). So no match-candidate
rewrite was needed; the fix is the partial-match re-trigger + the gate. RFC-150 separately retires
`tryFlatMapPlan` + the join-leg coupling.

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
**Go:** Same architecture — translator creates `JoinMergeResultValue`, rule passes `sel.GetResultValue()` through to the FlatMap plan. `RecordQueryFlatMapPlan` fires for ALL join types (INNER, CROSS, LEFT OUTER, EXISTS, NOT EXISTS) when the equi-join predicate matches the inner table's PK or a secondary index. Uses correlated scan + `JoinMergeResultValue` + `CorrelationBinder` interface + `existsMode`/`notExistsMode` flags. `RecordQueryNestedLoopJoinPlan` remains as fallback for non-indexed joins (no PK/index match for the predicate).

**Remaining NLJ cases:** Joins where no predicate matches any PK or index first column (brute-force NLJ is the only option). Self-joins now work via FlatMap (aliases disambiguate). **NLJ is guarded against ExplodeExpression quantifiers** — IN-list decomposition uses Explode, and NLJ can't handle scalar Explode outer datums with map inner datums. The guard forces IN-list patterns to InJoinRule or filter+scan fallback.

**Composite PK limitation:** FlatMap only matches the FIRST PK column. Joins on non-first PK columns fall back to NLJ.

**JoinMergeResultValue vs RecordConstructorValue:** Go uses `JoinMergeResultValue` (spreads both correlation bindings into a flat map at eval time). Java uses `RecordConstructorValue` with per-column `FieldValue` children. Functionally equivalent — both produce a map with qualified keys from both sides. The difference is WHEN columns are enumerated: Java at plan time (has schema metadata in the relational layer), Go at eval time (translator doesn't carry schema metadata). To close: pass `RecordMetaData` to the translator so it can produce field-level RecordConstructorValue.

### Reference: finalMembers partially aligned

**Java:** `Reference` has `exploratoryMembers` (logical EXPLORE-phase) and `finalMembers` (physical PLANNING-phase). `advancePlannerStage` clears exploratory, promotes REWRITING winner, clears finals. `OptimizeGroup` prunes `finalMembers` to 1 winner. `ToPlanPartitions` reads only `finalMembers` via `propertiesMap`.

**Go:** Added `finalMembers` to `Reference`. Implementation rules (`InsertFinal`) and data access generation insert into `finalMembers`. `computeRefPlanProperties` and `reoptimizeRecursive` prefer `finalMembers` when non-empty. `advancePlannerStage` NOT ported (Go's PLANNING phase relies on EXPLORE-phase physical wrappers in inner References).

**Impact:** FDB integration tests pass without `promoteInJoinWinners`/`promoteByDataAccessCost` — `finalMembers` + real statistics is sufficient. Promotion hacks remain for unit tests without statistics.

### Go has explicit Sort/InMemorySort physical operators

**Java:** Relies on `RemoveSortRule` to eliminate sorts; no in-memory sort plan exists.
**Go:** Has `RecordQuerySortPlan` and `RecordQueryInMemorySortPlan`.

Correctness improvement — ensures ORDER BY works even when no index satisfies it.

### FieldValue: string-qualified names vs CorrelationIdentifier-based resolution (PARTIALLY CLOSED)

**Java:** Column references resolve to `FieldValue(QuantifiedObjectValue(correlationId), "column")`. The table qualification is a structural `CorrelationIdentifier`, not a string prefix. When predicates move between scopes, Java calls `Value.rebase(AliasMap)` to retarget correlations. No string manipulation.

**Go (Phase 7.1 + 7.3 + P1.2):** Four improvements landed:
1. **Quantifier aliases unified with table aliases** (7.1): `ForEachQuantifier` in the translator uses `NamedCorrelationIdentifier(tableAlias)`. `GetCorrelatedToOfPredicate` and `GetAlias()` return the same identifiers. Three band-aids removed (`rightAliasSet`, `planContainsJoin`, `collectPlanAliases`).
2. **EXISTS predicates use QOV-based FieldValues** (7.3): `qualifyBareFieldValue` now produces `FieldValue(QOV(alias), "column")` instead of flat `"ALIAS.COLUMN"`. All `predicateReferencesAlias` calls in the NLJ rule replaced with `GetCorrelatedToOfPredicate` correlation-set checks.
3. **SQL resolver produces QOV-based FieldValues** for multi-source scopes (JOIN, correlated EXISTS).
4. **All `stripAlias*` deleted** (P1.2, RFC-032): the NLJ rule and PushFilterBelowJoinRule no longer string-strip alias prefixes. Pushed/residual predicates retain `FieldValue(QOV(corr), col)` and filters use `PredicatesFilterPlanWithAlias`; the executor binds rows under their correlation alias. PushFilterBelowJoinRule uses `NamedForEachQuantifier` so the pushed-filter quantifier alias matches the QOV correlation.

**Remaining:** Single-source scopes still produce flat `FieldValue{Field: "COLUMN"}` (no QOV child); `fieldValueAliasAndCol` / `bareColumnName` survive in `matchJoinPKPredicate` + push-filter/push-projection rules to handle both QOV and flat formats. `mergeRows` / `qualifyOuterRow` still build executor row maps with string-qualified keys (`"ALIAS.COL"` + bare); this is the executor row representation, not planner Values — a separate, deeper cleanup.

**`producesMergedRows` allowlist (P1.2):** `executePredicatesFilter` decides whether to bind the row under the filter's `innerAlias` by checking `producesMergedRows(inner)` — a `switch p.(type)` listing `RecordQueryNestedLoopJoinPlan | RecordQueryFlatMapPlan`. This is a structural type-check, not Java's value-result-shape distinction. It is correct for today's plan set (only NLJ/FlatMap emit qualified-key merged rows) but is a fragile allowlist: a future merged-row operator (hash/merge join) must be added here, else a filter over it could bind the wrong alias and bare-resolve `qov(b).col` on a null-filled row. Prefer keying off the row/result shape if a third merged-row operator lands.

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
| 12. Unmatched fields | UnmatchedFieldsCountProperty (no guard) | `totalCols - boundCols`, guarded by `inMemorySortCount == 0` | **Go adds guard** — prevents double-counting unmatched fields when InMemorySort already accounts for ordering cost |
| 13. InJoin count (more=better) | count(InJoinPlan) reversed | `inJoinCount` reversed | Aligned |
| 14. Map/filter count | count(Map, PredicatesFilter) | `mapCount + predicatesFilterCount` | Aligned |
| 15. FlatMap join ordering | Compare outer child cardinalities | `compareFlatMapJoinOrdering` compares outer quantifier cardinalities | Aligned |
| 15b. FlatMap vs NLJ | (none) | `compareFlatMapVsNLJ` — FlatMap beats NLJ | **Go-only** — workaround until `advancePlannerStage` is ported |
| 15c. Scalar cost fallback | (none) | `EstimateCostWith` comparison | **Go-only** — breaks ties the ordinal criteria can't resolve |
| 16. Plan hash tiebreak | planHash(CURRENT_FOR_CONTINUATION) | `costExprHash`→`concretePlanHash`/`exprConcreteHash` (FNV-flavored) | **Shape-aligned, NOT byte-aligned** (RFC-167 §5) — both break cost ties by a structural plan hash so each engine is *intra-engine* stable, but Go uses an FNV-flavored hash (RFC-024 cache key) ≠ Java's `planHash(CURRENT_FOR_CONTINUATION)`, so Go and Java may pick **different** tie-winner indexes for the same query (rows identical; EXPLAIN may differ). Convergence is deferred until cross-engine continuation re-planning is a requirement (RFC-167 OQ#5). |

Go-only criteria 15b and 15c are workarounds for the missing `advancePlannerStage`. Java's OptimizeGroup prunes finalMembers to a single winner — ties are rare. Go's flat member list has more competing plans, requiring tiebreakers. Audited: removing criterion #12 guard causes GROUP BY regression (covering index scan penalized by unmatched trailing fields), removing criteria 15b/15c causes JOIN regression (NLJ chosen over FlatMap without real statistics).

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

### Vector scan multi-partition fan-out — CLOSED (RFC-046, was TODO 9.5)

**Java:** `VectorIndexMaintainer.scan` (`indexes/VectorIndexMaintainer.java` ~134-150) handles a partition prefix of ANY length. When `prefixSize > 0` it does `flatMapPipelined(prefixSkipScan(prefixSize, range), (prefixTuple, …) -> scanSinglePartition(prefixTuple, …))` — a skip-scan that enumerates the *distinct full partition prefixes* within the bound (possibly partial) range, runs one HNSW search per partition, and concatenates the per-partition top-K. So a `PARTITION BY (zone, region)` index queried with only `WHERE zone = 'z1'` does a multi-partition K-NN over all regions in `z1`. The planner reflects this: only the index-only distance placeholder is required for binding; partition placeholders are not (`VectorIndexExpansionVisitor`).

**Go (RFC-046):** ported. `vectorMultiPartitionCursor` (`vector_index_maintainer.go`) fans out when the bound prefix is shorter than `KeyWithValueExpression.SplitPoint()`: `findNextPartition` skip-scans one limit-1 KV per distinct partition (mirroring Java's `nextPrefixTuple`), `searchOnePartition` runs the per-partition HNSW search, and the per-partition top-K are concatenated — SQL `PARTITION BY` semantics give top-K *per partition*, no global re-merge; an outer SQL LIMIT rides in `ReturnedRowLimit` as a separate cross-partition cap. Cross-partition continuation is full Java-aligned via `FlatMapContinuation{outer=prefix, inner=per-partition VectorIndexScanContinuation}` (resume re-reads the saved partition, then advances past it). The planner binding fix: `ComputeBoundParameterPrefixMap` consumes only the contiguous *equality* partition prefix and always retains the index-only DistanceRank binding (so a partial prefix no longer drops the query vector); `parametersRequiredForBinding` is `{distanceAlias}` only, matching Java's `VectorIndexExpansionVisitor`.

A partition *inequality* is the one deliberate residual divergence: Go's executor encodes only an equality prefix tuple (`VectorDistanceScanRangeWithPrefix`), so `ComputeBoundParameterPrefixMap` stops at the first non-equality and leaves the inequality unconsumed — enforced as a residual filter above the fanned-out scan (the same mechanism as a filter on a non-indexed column). Java instead threads the inequality endpoint into `getPrefixRange` to narrow the skip-scan; doing that in Go is a perf follow-up, not a correctness gap. Pinned by `TestVectorPlan_PartialPrefixPlansMultiPartition`, `TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix`, and FDB E2E `TestFDB_VectorSearch_MultiPartition_{Fanout,InequalityResidual,Pagination}`.

### Covering Index Scan — RESOLVED via ImplementProjectionRule

**Status:** Covering index works end-to-end for SQL via `ImplementProjectionRule` (EXPLORE phase). When all projected FieldValues can push through the Fetch's TranslateValueFunction, the Fetch is eliminated. PK columns + all index key columns are coverable. Verified with planner harness tests: `CoveringCompositeIndex`, `CoveringCompositeIndexPKAndIndexCols`, `NonCoveringNeedsExtraColumn`. The FDB stress test shows 63x speedup for PK-only projections over index scans.

**The compensation-based path** (`IsFinalNeeded`, `wrapScanPlanWithCoverage`) is bypassed — SQL projections always set `IsFinalNeeded() = true`. The ImplementProjectionRule path is the active mechanism. Java's `IndexKeyValueToPartialRecord` (826 LOC) approach remains unported but is not needed for SQL coverage.

## Optimization-Quality Gaps (correctness unaffected)

| Gap | Status |
|---|---|
| CollapseRecordConstructorOverFieldsToStar | Blocked: needs field-level type metadata (ordinal positions) |
| ExtractFromIndexKeyValueRuleSet (3 rules) | Blocked: execution layer (partial record construction) |

## Go-Only Extensions (features Java 4.12.11.0 rejects)

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
| UNION ALL outer ORDER BY | Deterministic sorted output | Intermittent ordering |
| `WHERE pk_col = nonpk_col` | SQL-correct | `Missing binding` planner error |

4.12.11.0 fixed three former entries, now removed from this table — they run as plain cross-engine
equivalence in the corpus: PK literal-eq AND join predicate (`pk_literal_eq_in_join`) and 3-way join
shared driver key (`three_way_join_shared_driver`), both fixed by 4.12's "planner no longer drops
ANDed predicates" change; and `WHERE TRUE AND val > 5`, now planned by 4.12 (boolean literals in
WHERE, added in the 4.12 line — see `join-tests.yamsql` `WHERE TRUE`/`WHERE FALSE`). The former
`bare_bool_where_rejected` Go-side gap is now CLOSED — Go supports bare boolean WHERE forms
(`WHERE TRUE`, `WHERE FALSE`, `WHERE bool_col`, `WHERE NOT bool_col`, and combinations with column
predicates), verified 2026-06-28 and pinned by `bare_bool_where_probe_test.go` (literal forms) plus the
corpus `bare_bool_where` (`WHERE flag`). The remaining `WHERE pk_col = nonpk_col` "Missing binding" entry stays as not-yet-
fixed in 4.12: the corpus keeps that probe deliberately omitted (column-self-equality), so the live
4.12.11.0 run neither confirms a fix nor pins the divergence — it is retained on the not-yet-fixed
side per the corpus's omit comment.

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

## DML statement-layer routing (RFC-035)

All DML (INSERT VALUES/SELECT, UPDATE, DELETE) plans and executes through the
single Cascades path (planDML), matching Java's PlanGenerator.getPlan. One
intentional divergence at the statement layer:

| Aspect | Java | Go |
|---|---|---|
| DML via the rows-returning method (`executeQuery` / `Query`) | Executes the DML, counts rows, then throws "use executeUpdate" — the mutation still happens | Rejects **before** executing ("use Exec, not Query"); no mutation |

Go rejects up front to avoid a surprise write on a misused method; the plan
path is identical to Java, only the execute-then-throw side effect differs.

## Pure-Go FDB client (`pkg/fdbgo`) — deliberate divergences from `libfdb_c` 7.3.77

**Client option behaviour** (honored / `UnsupportedOptionError` / accepted-and-ignored) is documented
option-by-option, with the `libfdb_c` C++ reference for each, in
[`pkg/fdbgo/fdb/OPTIONS.md`](pkg/fdbgo/fdb/OPTIONS.md) (RFC-133).

### Cluster-file re-watch / coordinator-set rotation (RFC-111)

| Aspect | C++ `libfdb_c` | Go | Why |
|---|---|---|---|
| Forward-follow chain | Unbounded; relies on actor fair-scheduling to pace re-polls | Bounded by `maxForwardHops` (10), reset on each successful non-forward connect | A Go tight loop (immediate re-poll on a followed forward) would hot-spin on a pathological A→B→A forward cycle; the bound makes it back off. A legitimate long rotation chain still progresses (counter resets on each clean connect). |
| Mixed-TLS forward / file | Followed (per-entry TLS) | Declines to follow; stays on steady retry | `ParseClusterString` rejects mixed-TLS strings (uniform TLS is the real-cluster case); declining is safer than writing a lossy re-serialization to the shared cluster file. |
| Out-of-range IPv4 octet / trailing-junk port in a coordinator token | Accepted + silently truncated (`sscanf`/`std::stoi`) | Rejected (`net.ParseIP` + numeric port) | One-way SAFE tightening: Go-accept ⊂ C++-accept, so the re-watch persist path can never write a token C++/Java cannot parse. Unreachable on real inputs (forward/file strings are always `toString()`-normalized, octets 0-255). |
| Leader-election (`getLeader`) forward path | Present | N/A | The Go client uses only `OpenDatabaseCoordRequest`; the leader-nominee RPC path does not exist here. |
| IPv6 coordinator re-rendering | Canonicalized via boost `address_v6::to_string` in `toString` | Re-emitted verbatim from the stored token | Unreachable on real inputs (forward/file strings are always `toString()`-normalized); only a hand-written uppercase/expanded IPv6 in a user file would round-trip differently — and Go-accept ⊆ C++-accept still holds. |
| `atomicReplace` chown error | Hard-fails the whole replace; original file untouched | Keeps the write (mode already preserved → still parseable); only ownership may differ | Best-effort chown suits a client lib; chown-to-self (single-service-user deployment) always succeeds, so they match in practice. |
| Coordinator probing shape | Sequential round-robin (`monitorProxiesOneGeneration`) | Parallel race (`tryAllCoordinators`) | Benign: identical first-success outcome, lower latency; never contacts more than the coordinator set. |

**Coordinator topology adoption is a CONFIRMED NON-divergence (RFC-115 §3, FDB-C-dev verified).** The
libfdb_c client adopts cluster topology on the **first successful** coordinator reply, **not** a majority
quorum: `monitorProxiesOneGeneration` adopts the first successful `OpenDatabaseCoordRequest`
(`MonitorLeader.actor.cpp:919-937`), and the `majority` bool in `getLeader()` (`:578`) is server-side
leader-election metadata, not a client adoption gate (`monitorLeaderOneGeneration` calls `getLeader()`
with no quorum wait, `:604`/`:634`). Go's first-reply-wins therefore **matches** C++ semantics; adding a
quorum would make Go *stricter* than libfdb_c — a conformance violation. (Cluster-file re-read is
failure-gated in both, `:888-900` — RFC-111.)
