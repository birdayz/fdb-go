# Divergences from Java fdb-record-layer-core 4.12.11.0

Comprehensive list of Go vs Java differences. All Cascades planner subsystems
fully ported: ~65 PlanningRuleSet rule instances, 5/5 RewritingRuleSet rules,
34/34 physical plan types, 48/48 value types, 18/18 properties, 12/12 match
candidate types, 24/24 comparison operators, 9/9 predicates. Remaining items
are execution-layer, wire-format, or intentional architectural choices.

Validated against a live Java **4.12.11.0** conformance run (the cross-engine corpus runs against
live 4.12 in `just test` with a stale-annotation guard, and the suite is green).

## Intentional Architectural Decisions (no functional difference)

### PK-intersection declines a needed non-ForMatch compensation (conservative)

**Java:** `createIntersectionAndCompensation` reapplies ANY compensation
polymorphically via `applyAllNeededCompensations`.
**Go:** `compensateIntersection` (`intersector_primary_key.go`) reapplies only
the `ForMatchCompensation` form; a needed compensation of any other concrete
type declines the pair/triple like the impossible arm. Today only ForMatch
(and the monoid identities) reach that fold, so the arm is dead in practice;
if a new Compensation form becomes reachable there, the decline loses a plan
alternative — never rows. Revisit when a second realizable form exists.

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

**Go:** Aligned by RFC-181 WS-P: convergence is EPOCH-driven (the ConstraintsMap tick/watermark port drives `NeedsExploration`; member growth pushes per-expression tasks at the insert sites like Java's executeRuleCall), dual insertion is retired (physical yields land in finals only; the OptimizeInputs guard reverted to `ContainsExactly`), OptimizeGroup prunes finals to the winner (+ ordering-retained finals in PLANNING — the Go-specific extension because Go wrappers resolve children at extraction where Java bakes concrete plans at rule time), and REWRITING finals route through OptimizeInputs so parent-chain-optimized groups cross the stage boundary pruned to their REWRITING winner. RESIDUAL: the UNIVERSAL prune-to-1 at the boundary requires PLANNING re-derivation parity first — a forced boundary prune lost canonical alternatives Go's PLANNING cannot re-derive (RFC-153 buried-leg, cross-join-EXISTS shapes); until Java's per-phase rule-set parity lands, unoptimized (satellite/mid-phase) groups cross with their full canonical set (documented at the boundary arm in unified_tasks.go).

**Impact:** FDB integration tests pass without `promoteInJoinWinners`/`promoteByDataAccessCost` — `finalMembers` + real statistics is sufficient. Promotion hacks remain for unit tests without statistics.

### Quantifier.GetCorrelatedTo — CLOSED (RFC-189 A4)

**Java:** `Quantifier.getCorrelatedTo()` delegates to `rangesOver.getCorrelatedTo()` — the transitive correlation set of the inner Reference.
**Go (now):** `expressions.Quantifier.GetCorrelatedTo()` delegates to `q.GetRangesOver().GetCorrelatedTo()` — Java parity. (Previously returned the empty set, an under-approximation; the one-line delegation was wired in RFC-189 A4 under the query-engine gate.)

Consumer follow-through at close:
- `rule_partition_select.go` — the `q.GetCorrelatedTo()` term now carries real sibling-correlation edges; the previously-redundant manual `q.GetRangesOver().GetCorrelatedTo()` walk was removed and its `dep != q.GetAlias()` self-filter carried onto the primary loop.
- `rule_push_requested_ordering_through_in_like_select.go` — the structural no-op guard was removed (the real correlation check runs downstream in `ImplementInJoinRule`/`ImplementInUnionRule`; rejecting here risks over-rejection).
- `rule_split_select_extract_independent.go` — retains its own `quantifierCorrelationSet` walk (unions merge-leg/without-children deps the way that rule needs).

Pinned by `TestQuantifier_GetCorrelatedTo_Transitive` (a quantifier ranging over a reference correlated to an external alias surfaces it; the bound inner alias does not leak).

### Go has an explicit in-memory sort physical operator

**Java:** `RecordQuerySortPlan` exists, but only the legacy `RecordQueryPlanner` constructs it.
Java's Cascades planner relies on `RemoveSortRule` to eliminate every logical sort; if no child
ordering satisfies the request, Cascades fails to plan rather than constructing a physical sort.
**Go:** Has `RecordQueryInMemorySortPlan` (produced by `ImplementInMemorySortRule`). The Java-ported `ImplementSortRule` still eliminates the sort via index ordering where possible; `RecordQueryInMemorySortPlan` is the fallback when no index satisfies the requested ordering. (A legacy `RecordQuerySortPlan` — an orphaned port of Java's legacy-planner sort plan — was removed as producer-less dead code: Go has no legacy planner, so nothing ever constructed it.)

This is a sanctioned read-side extension: it ensures `ORDER BY` works even when no access path
satisfies it. It is not a substitute for the Java ordered-variant enumeration described below.

### FlatMap/NLJ requested-order enumeration — RESOLVED (RFC-190.6 implementation)

**Java 4.12.11:** `ImplementNestedLoopJoinRule` partitions child alternatives by their source
ordering and applies three cases. Case 1 rolls max-one outers together and lets the inner determine
the result order, retaining every satisfying inner source-order partition only for an exhaustive
request. Case 2a uses an outer that satisfies the request by itself, retaining each satisfying
outer source-order partition only for a `DISTINCT` request (exhaustiveness does not widen this
case). Case 2b retains every viable distinct-outer source-order partition whose order does not
satisfy alone, appends a satisfying inner ordering, and retains every such inner partition only
for an exhaustive request. Each retained partition contributes its cheapest expression.

**Go:** The NLJ/FlatMap implementation and the bottom-up sort boundary now use that same Case
1/2a/2b source-order partition matrix while retaining the ordinary cost-best join. Ordered variants
freeze both selected children in private exact-final singleton references, so extraction cannot
replace an ordered leg with the shared group's cheaper unordered winner after the enclosing sort
is removed. The join ordering property is correspondingly conservative: it reports Case 1/2a/2b
ordering only for exact-final child edges.

Translation across Go's mixed value representations is deliberately fail-closed. A safe ordering
root bridge equates a source-local flat/baked `FieldValue` with a field under exactly one
`QuantifiedObjectValue` only when the complete accessor path is identical; different baked
ordinals, nested paths, and ambiguous roots do not collapse. Projected-EXISTS FlatMaps that declare
`inheritOuterRecordProperties` use an ordering-only record-constructor lens that qualifies direct,
correlation-free outer fields; it does not change the executable result value or claim inner,
literal, or ordinary-FlatMap fields for the outer.

If pruning has already hidden the needed child access, ordered-leg recovery reuses the existing
ordered primary/index-scan rules and retained partial matches in private candidate space. It can
recover a reverse unbounded primary scan from a retained forward final, but deliberately declines
bounded-scan synthesis and re-verifies the completed join through the rich ordering property.
Ambiguous/untranslatable requests and genuinely index-less `ORDER BY` queries still fall back to
`RecordQueryInMemorySortPlan`.

Final parent-to-worktree EXPLAIN census: 2,579 old entries and 2,581 new entries (the two
additional entries are the new regression queries), with 2,491 identical and 90 differing. Of the 88
comparable shape flips, 87 eliminate outer/unary sort enforcers; the remaining already-sorted
fixture changes shape because its fixture now declares the new index. The plan-error classification
contains zero regressions and zero recoveries. The release-gate results are recorded in
RFC-190/TODO rather than being inferred from this census.

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

### Explain rendering: ad-hoc Sprintf vs Java's typed ExplainTokens

**Java:** a dedicated explain package (`query.plan.explain`): every plan/value/predicate emits `ExplainTokens` — a typed token stream (keyword/identifier/alias tokens, precedence-aware nesting via `ExplainTokensWithPrecedence`) — rendered by a pluggable `ExplainFormatter` with an `ExplainSymbolMap` for stable alias naming and `ExplainLevel` for detail control. No node concatenates strings.
**Go:** every `Explain()` method hand-builds its own `fmt.Sprintf` string; predicates and values are elided or rendered inconsistently (`[N preds]`), and there is no detail-level or formatter abstraction.

Consequence: Go's EXPLAIN output cannot show predicate/value detail without editing each node's `Explain()`, and the rendering logic is scattered. Port target: `ExplainTokens` + `DefaultExplainFormatter` + the per-node `explain()` visitors. Not on the RFC-173 critical path (frozen behind it per owner directive); food for a post-RFC-173 slice.

## Planning-Layer: Java-aligned core with documented Go extensions

### Cost Model: PlanningCostModelLess

Java PlanningCostModel criteria #1–#17 are accounted for below; Go broadens/hoists #15 and adds
explicit sort-count, NLJ-predicate, and statistics-cost rungs. Criterion-by-criterion analysis:

| Criterion | Java | Go | Status |
|---|---|---|---|
| 1. Physical beats non-physical | `instanceof RecordQueryPlan` | `isPhysical` | Aligned |
| 2. Max data access cardinality | CardinalitiesProperty gate + comparison | Data-access cardinality gate | Functionally equivalent |
| 3. Residual predicate count | NormalizedResidualPredicateProperty (CNF size) | `countResidualPredicates` using `cnfSize()` | Aligned |
| 4. Data access count | count(Scan, Index, Covering) | `scanCount + indexScanCount + coveringIndexCount` | Aligned |
| 5. Recursive CTE DFS > level | flipFlop(compareRecursiveCte) | `compareRecursiveCTE` | Aligned |
| 6. IN-plan SARG penalty | flipFlop(compareInOperator) | `compareInPlan` with `(int, bool)` flipFlop | Aligned |
| 7. Primary vs index scan | comparison-set analysis + PREFER_INDEX | `comparePrimaryScanVsIndexScan` + `isSingularIndexScanWithFetch` | Aligned (PREFER_INDEX default; comparison-set analysis redundant for default config) |
| Go sort extension | Cascades `RemoveSortRule` eliminates redundant sorts before costing and has no physical-sort cost rung (`RecordQuerySortPlan` is legacy-planner-only) | `inMemorySortCount`, fewer wins, promoted before the structural block | Go read-side extension / cost-time analogue (RFC-190) |
| 8. Type filter count | TypeFilterCountProperty | `len(GetRecordTypes())` per filter | Aligned |
| 9. Type filter depth | ExpressionDepthProperty | Concrete depth plus logical fallback, with `InMemorySort` transparent | Aligned after RFC-190; unconditional with respect to sort |
| 10. Index scan fetches | count(PlanWithIndex, Fetch) | `indexScanCount + fetchCount` plus sort-transparent fetch depth | Aligned after RFC-190; ungated with respect to sort (the Java both-index applicability gate remains) |
| 11. Distinct depth | ExpressionDepthProperty | Sort-transparent concrete/logical depth | Aligned after RFC-190; unconditional with respect to sort |
| 12. Unmatched fields | `UnmatchedFieldsCountProperty`, unconditional for both `RecordQueryScanPlan` and index plans | `unmatchedFieldsForScan` + `unmatchedFieldsForIndex`, unconditional after equal sort count | Aligned after RFC-190: removed the Go sort gate and ported the missing primary-scan arm |
| 13. InJoin count (more=better) | count(InJoinPlan) reversed | `inJoinCount` reversed | Aligned |
| 14. Map/filter count | count(Map, PredicatesFilter) | `mapCount + predicatesFilterCount`, unconditional after equal sort count | Aligned after RFC-190 |
| 15. FlatMap join ordering | Compare FlatMap outer-child cardinalities | `compareJoinOrdering`, hoisted after recursive CTE; recursively compares concrete total cost for same-shape joins and CPU for NLJ-vs-FlatMap | **Documented Go broadening/order divergence** |
| Go NLJ-predicate extension | No materialized predicate-bearing NLJ counterpart | `nljPredicateCount`, more wins | Go read-side extension |
| 16. DefaultOnEmpty count | Count `RecordQueryDefaultOnEmptyPlan` with `onEmptyResult == NULL`, fewer wins | `numDefaultOnEmpty`, fewer wins | Aligned |
| 15c. Scalar cost | Java CostModel is purely heuristic (ordinal rungs, planHash tiebreak — no statistics rung) | `EstimateCostWith` comparison | **Go extension in the tiebreak slot** — a statistics discriminator before the hash tiebreak, NOT a prune workaround (retiring it regressed genuine selectivity decisions) |
| 17. Plan hash tiebreak | planHash(CURRENT_FOR_CONTINUATION) | `costExprHash`→`concretePlanHash`/`exprConcreteHash` (FNV-flavored) | **Shape-aligned, NOT byte-aligned** (RFC-167 §5) — both break cost ties by a structural plan hash so each engine is *intra-engine* stable, but Go uses an FNV-flavored hash (RFC-024 cache key) ≠ Java's `planHash(CURRENT_FOR_CONTINUATION)`, so Go and Java may pick **different** tie-winner indexes for the same query (rows identical; EXPLAIN may differ). Convergence is deferred until cross-engine continuation re-planning is a requirement (RFC-167 OQ#5). |

Criterion 15b (`compareFlatMapVsNLJ`) is RETIRED (RFC-181 WS-P stage (d)): under the epoch convergence with finals-only physical yields and prune-to-winner, its recorded JOIN regression no longer reproduces — deleted with its tests. 15c is RECLASSIFIED: Java's PlanningCostModel is self-described heuristic — its rungs end in a planHash tiebreak and no statistics rung exists — so 15c is a Go statistics EXTENSION occupying that role slot (cost discriminator before the hash tiebreak), not a literal Java rung; the stage-(d) retirement probe regressed equality-index preference and vector outer-limit folding, proving it load-bearing. The maxRoundsPerRef load cap (10) is obsolete under epoch convergence (both constraint lattices are finite chains, so rounds are structurally bounded); it remains only as a loud divergence tripwire at 100. RFC-190 removed the five Go-specific per-rung sort gates and promoted sort count. The GROUP BY/covering-index flip exposed the missing `RecordQueryScanPlan` unmatched-fields arm; porting Java's scan branch fixed the apples-to-oranges comparison, so it is not evidence for retaining the gate.

### Cost Model: RewritingCostModelLess

Java 4.12.11.0's `RewritingCostModel.compare()` has **six** ordered criteria: (0) `outerJoinCount`, (1) `selectCount`, (2) `tableFunctionCount`, (3) normalized CNF conjuncts, (4) predicate-count-by-level, (5) `semanticHashCode` tie-break. Go ports criteria **1–5** (`selectCount`, `tableFunctionCount`, CNF conjuncts, predicate-count-by-level, deep hash tie-break). `Planner.WithCostModel()` wires the cost model per phase.

**Criterion 0 — `outerJoinCount` — is DELIBERATELY NOT ported. This is an intentional, justified divergence, not a gap.**

In Java, `outerJoinCount` (penalize any surviving `OuterJoinExpression`, checked FIRST) is a **correctness guard**, not a heuristic. Java's `OuterJoinExpression` is a logical-only node with **no physical operator** and exactly one consumer (`RewriteOuterJoinRule`); it *must* be rewritten before planning. Java's single-final-expression prune keeps one survivor per group, and without `outerJoinCount` the un-rewritten `OuterJoinExpression` (0 selects) would beat the rewritten form (2 selects) on the `selectCount` tie-break, survive the prune, and hand the planning phase an **unimplementable** node — the query would fail to plan. `outerJoinCount` forces the implementable rewritten form to win. (Evidence: `OuterJoinExpression.java` is logical-only; `PlanningRuleSet.java` has no rule matching it; `RewritingCostModel.java:60-68` comment states the rationale.)

Go has **no such correctness problem**: Go's outer join is a `SelectExpression{joinType: LEFT/FULL OUTER}` that is **directly implementable** — `ImplementNestedLoopJoinRule` plans it as a materialized `RecordQueryNestedLoopJoinPlan` (RFC-152), a read-side extension Java lacks (Java has only the correlated `RecordQueryFlatMapPlan` re-scan). Go deliberately keeps the un-rewritten outer-join select as the REWRITING prune survivor (it wins on `selectCount`, 1<2), so PLANNING can derive **both** the materialized NLJ (scan the inner once) and the correlated FlatMap (re-scan) and cost-choose (RFC-152). **Porting `outerJoinCount` would force the rewritten form to win the prune, discard the outer-join select, and suppress the materialized-NLJ alternative — a plan regression, proven by `TestFDB_ArrayUnnestOrdinality` (asserts the materialized `NestedLoopJoin(LEFT OUTER, …)` box).** Pinned by `TestRewritingCostModel_KeepsUnrewrittenOuterJoin` / `TestRewritingBoundary_KeepsUnrewrittenOuterJoin`, which go RED if `outerJoinCount` is (re-)introduced.

**Companion divergence — `RewriteOuterJoinRule` runs in Go's PLANNING phase (`PlanningExplorationRules`); Java's `PlanningRuleSet` does not contain it.** Because Go keeps the un-rewritten outer-join select as the prune survivor (above), PLANNING re-fires the canonicalizer to re-derive the rewritten form and keep the correlated-FlatMap alternative available alongside the materialized NLJ. Kept as an intentional divergence (Java can drop it precisely because `outerJoinCount` makes the rewritten form the sole survivor — the guard Go must not adopt). Empirically the full outer-join FDB suite is green whether or not this PLANNING re-fire is present (the correlated FlatMap is also derivable directly by `ImplementNestedLoopJoinRule.yieldGeneralFlatMap`), so a future cleanup could remove it — but only after pinning the correlated-LEFT-OUTER index-nested-loop (RFC-042) path with a dedicated plan-shape test.

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
| In-memory sort | Cascades eliminates or fails; legacy `RecordQueryPlanner` alone can construct `RecordQuerySortPlan` | `RecordQueryInMemorySortPlan` fallback |
| Hash aggregation | Only streaming aggregation (requires ordered input) | `RecordQueryHashAggregationPlan` |
| `INFORMATION_SCHEMA` | Rejects (`Unknown reference INFORMATION_SCHEMA.TABLES`) | Working system tables |
| `NOT NULL` on scalar columns | Rejects (`NOT NULL is only allowed for ARRAY column type`) | SQL-standard behavior |
| Date-part functions | No temporal types | YEAR/MONTH/DAY/HOUR/MINUTE/SECOND/etc. |
| Simple CASE (`CASE expr WHEN val`) | `visitChildren` no-op (always falls through to ELSE) | Correct evaluation |
| Symbolic logical operators (`&&`, `\|\|`) | `SqlFunctionCatalogImpl` only registers `and`/`or`; symbolic forms throw UNSUPPORTED_QUERY | Evaluated as AND/OR |
| `XOR` operator | Not registered in `SqlFunctionCatalogImpl`; throws UNSUPPORTED_QUERY | SQL-standard XOR with NULL propagation |
| Scalar subqueries in expressions | Grammar has no `subqueryExpressionAtom` (parse error) | Translated via `ScalarSubqueryValue` (`DecorrelateValuesRule` covers the other values-box patterns) |

Go-only plan types: `RecordQueryHashAggregationPlan`, `RecordQueryInMemorySortPlan`, `RecordQueryLimitPlan`, `RecordQueryProjectionPlan`, `RecordQueryValuesPlan`, `RecordQueryNestedLoopJoinPlan`. `RecordQueryMergeSortUnionPlan` is Go's collapsed ordered-union counterpart, not a semantic extension; its `removeDuplicates=false` mode is an extension. Go also has a keyless concat shape named `RecordQueryUnionPlan`; Java's same-named class is keyed and ordered, so the Go shape—not the class name—is the extension.

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
| 2 ordered Union subclasses | `RecordQueryMergeSortUnionPlan` | Aligned for Java's deduplicating mode; Go additionally supports ordered UNION ALL |
| `RecordQueryUnorderedUnionPlan` | `RecordQueryUnorderedUnionPlan` plus extra keyless `RecordQueryUnionPlan` | Java-aligned unordered plan plus a duplicate Go concat implementation; cleanup tracked by RFC-190 |
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

---

## RFC-164 WS-5 — Go-only divergence reservoir audit

The bounded WS-5 acceptance: the KNOWN reservoirs where Go left the Java architecture are
written down with **"what invariant does Java carry that this drops?"** and each tagged
*covered by a WS-2/4 invariant* or *tracked*. (Not "all divergences found" — un-completable.)

| Go-only reservoir | Java invariant it drops | Risk class | Coverage |
|---|---|---|---|
| **Simplified `RequestedSortOrder`** (NULLS axis was elided) | Full sort order incl. NULL placement (ASC→NULLS FIRST etc.) | wrong rows on ORDER BY | **COVERED** — NULLS axis restored (RFC-165) + `rfc165_nulls_ordering_test.go`; the NULLS-ORDER hunt bug is fixed + pinned. |
| **Scalar cost fallback + Go-only tiebreakers** (15b `compareFlatMapVsNLJ`, 15c `EstimateCostWith`) — no `advancePlannerStage`, so Go's flat member list has ties Java's prune-to-1-winner avoids | Structural single-winner selection; total-order tie resolution | nondeterministic / wrong index pick | **PARTIAL** — cost ORDERING pinned by WS-4 `TestBoundSelectivity_CostMonotonicity` (#405 class); equality-tie determinism pinned by `TestPlanDeterminism_*` (#409). **TRACKED:** the InJoin inner correlated-equality tie (WS-4 #2, OPEN — RFC-167 Phase 1b). |
| **Hand-rolled `AggregateDataAccessRule`** (aggregate-index matching, not Java's generic data-access) | Guard(match)==consumer(build/execute) — one classifier | wrong agg result / wrong index match (COUNT-COL class) | **COVERED for the known drift** — WS-3 `expressions.IsCountStar` is the single source of truth for the planner candidate + the executor group cursors (#413); group-key matcher deduped via `groupColEqualityIndex` (RFC-163). **TRACKED:** the translator's OWN count-star normalization (`aggregateNamesStableForUnion`) is deliberately a SEPARATE classifier (`cascades_translator.go` — different question/scope) → audit whether it should share (WS-3 other-guard/consumer-pairs); the `COUNT(NULL)` fold fidelity (Graefe follow-up). |
| **`WithPrimaryKeyIntersector` was forward-PK-only and discarded `requestedOrderings`** (vs Java's rich common-ordering gate) | Every intersection leg shares the directional comparison ordering consumed by the sorted merge | wrong rows if reverse were widened piecemeal; safe misses for unsupported key encodings | **RESOLVED (RFC-190.5b):** rich common-order derivation, translated requests, fixed-binding dependency normalization, free-PK compatibility, redundancy proof, directional parts, reverse plan identity/execution/ordering/rewrites, and fan-out-leg PK distinct are one atomic path. Natural flat all-ASC/all-DESC keys execute; mixed/counterflow, ordered-bytes, non-flat, ambiguous-layout, stale-ordinal, and mixed structural/name-only PK-provider shapes decline safely. Multi-type layout remains separately tracked below. |
| **Cross-candidate PK-intersection pruning retains singleton alternatives** | Java builds compensated singles and all intersections in one shared `IntersectionInfo` map, evicts immediate subpartitions only after a useful replacement, then yields survivors once | safe plan-space widening / possible winner difference, never missing or wrong rows | **TRACKED SAFE RESIDUAL (RFC-190.5b review):** Go first yields candidate-local accesses, then its separate cross-candidate pass builds a private eviction map. It correctly prunes smaller intersection expressions internally but cannot retract already-yielded exact singleton scans. Do not delete memo members post hoc. Closing this requires a partition-level assembler that creates singles once, shares the map with intersection enumeration, and flattens survivors before any yield; regressions must preserve unrelated finals and safe logical compensations. |
| **Per-wrapper relink** (RFC-070 nil-inner shells across ~20 wrappers, vs Java's eager `memoizePlan` to concrete) | Every non-leaf plan has its child; deterministic relink to the cost winner; one final member | dropped/nil child (0 rows); nondeterministic plan/cache | **CLOSED for the shell half (RFC-183).** Rules now bake the concrete child at rule time, matching Java's memoizePlan-as-constructor-argument; `verifyChildrenMemoized` rejects a holed expression at yield, always-on (ports `CascadesRuleCall.verifyChildrenMemoized`); the ~600 lines of repair machinery are deleted, after instrumentation showed ZERO shells across the full suite and all 2407 corpus queries. `ValidatePlanInvariants` remains as the sink-side backstop. **NOW CLOSED (RFC-184 W2):** P5's terminal step landed — every physical wrapper is deleted, so the parent→child edge is no longer stored twice. A physical plan is its own cascades expression holding its children SOLELY as quantifiers (`RecordQueryFlatMapPlan`/`RecordQueryNestedLoopJoinPlan` carry `outerQ`/`innerQ` and no embedded plan-snapshot field; `GetChildren` resolves through them), so the dual-storage state is unrepresentable. The earlier BLOCKER — `rule_implement_nested_loop_join.go` building compensating filters that were never memoized, so the plan pointer held what EXECUTES while the quantifier held what the memo COSTS (9868 rule-time edges, 472 semantically different) — is resolved: those rules now memoize each compensating operator and advance the quantifier over that reference in lockstep (the FlatMap collapse), matching `rule_implement_simple_select.go`'s long-standing shape. The **LIVE DEFECT** that fell out of the same finding (the memo costing expressions that are not the ones that execute) is therefore gone — see the "memo costs an expression that is not the one that executes — CLOSED" section below. **STILL TRACKED (separate concerns):** prune-to-one-final-member; retiring `findBestPhysicalPlan` — extraction's ad-hoc cost pick outside the cost framework, wired to ONE site against `findPhysicalPlan`'s twenty (do NOT "fix" that asymmetry by propagating the cheapest-selector: it is ordering-blind and turns `ORDER BY … DESC` into ASC, measured, see RFC-183 §10); and the reachability tally still driving a residual population of unreachable edges to zero (`plan_reachability.go` — a completeness/hygiene concern, NOT the wrong-tree-costing defect, which is closed). **RETRACTED:** an earlier revision of this row cited "1186 references hold multiple finals, 1125 multiple PHYSICAL finals" as P5's blocker. That measurement was taken at RULE TIME while rules were still firing, where groups legitimately hold alternatives; it says nothing about the extraction-time property P5 needs, which does hold (`TestOneFinalPlanPerReference`). Note also the split's stated rationale in `plans/plan.go:23-28` — "physical and logical plan trees live in different namespaces in Java" — is FALSE: `QueryPlan<T> extends RelationalExpression` (`QueryPlan.java:51`), so a Java plan IS a RelationalExpression; the comment conflates package separation (real) with hierarchy separation (not real). |
| **Go-only physical-filter builders** (`ImplementIndexScanRule`, `ImplementSimpleSelectRule`, NLJ residual — extra paths past `Compensation`) | Index-only predicates never become an executable residual | panic / wrong plan on a vector `DistanceRank` residual | **COVERED** — `ImplementFilterRule` `!isIndexOnly()` gate (RFC-151) + the retained `validateNoIndexOnlyResidual` catch-all backstop (pinned by `TestVectorPlan_*`). **TRACKED end-state:** gate the remaining builders so the net can retire. |

**Method for the next reservoir found:** name the Java invariant it drops, classify the risk
(wrong-rows / nondeterminism / panic), and either wire a WS-2/4 structural invariant that makes
the class un-shippable or file a tracked TODO — never leave it as a silent reservoir.

## RFC-183 P5 residue — tracked, out of scope for the fully-linked-plans branch

Four pre-existing items surfaced by the P5 review. None is caused by P5; all are filed here
rather than fixed in-branch. Verified against Java 4.12.11.0.

### `canCorrelate` — three divergences from Java, one of them in the UNSAFE direction

`canCorrelate` decides whether an expression may be the ANCHOR of a correlation — whether
bound aliases propagate from one child to its siblings. Java's default
(`cascades/expressions/RelationalExpression.java:251`) is `false`.

1. **`RecordQueryRecursiveLevelUnionPlan` — FIXED (was UNSAFE).** Go now returns `false`
   (`plans/recursive_level_union.go`), matching Java's no-override default. The prior `true`
   claimed anchor status, which suppressed propagation of an outer alias a leg legitimately
   reads (a wrong-rows shape when a recursive CTE sits on the inner side of a lateral
   correlation and Go's human-readable alias reuse collides an outer alias with a leg's own).
   The recursion's level-to-level binding is satisfied by the cursor + the temp-table alias
   filter, not by anchoring here. The wrapper is gone (RFC-184 W2), so only the plan needed the
   flip; `plan_expression_flag_parity_test.go` pins `false`, and
   `plans/recursive_level_union_correlation_test.go` pins that a colliding outer alias now
   propagates. Java DOES override `canCorrelate() → true` on the sibling
   `RecordQueryRecursiveDfsJoinPlan.java:156` (Go matches, `:200`), so the divergence was
   specific to the LEVEL union. **Residual (tracked, non-blocking):** Java's physical level union
   pairs `canCorrelate=false` with an EXPLICIT `computeCorrelatedTo` filter that drops
   `tempTableScanAlias`/`tempTableInsertAlias` from the propagated set. Go ports the flag but not
   that filter — `TempTableScanExpression` surfaces its alias as a free correlation, so the temp
   aliases leak upward as apparent external correlations. This is NEUTRAL w.r.t. the flag flip
   (temp aliases are not quantifier aliases, so they leak identically under `true` or `false`) and
   is therefore neither introduced nor regressed here — a pre-existing unclosed divergence. Close
   it by overriding the level-union plan's transitive correlated-to to filter the two temp aliases
   (Java `computeCorrelatedTo`).
2. **`RecordQueryInJoinPlan`** — Java `:198` returns `true`; Go has no override, so `false`.
   Conservative direction (Go propagates where Java anchors) — safe, costs optimization reach.
   Deliberately not flipped here: adding anchoring changes plan shapes and belongs to a planner
   milestone with its own review lap, not the wrong-rows-truth pass.
3. **`RecordQueryInUnionPlan`** — Java `:230` returns `true`; Go has no override, so `false`.
   Same conservative direction as (2); same deferral rationale.

### Index metadata has a dual source of truth

`HintRichOrdering` and the cost model still read `physicalIndexScanWrapper`'s own
`columnNames`/`pkColumnNames`/`unique` fields, while `RecordQueryIndexPlan` now carries its own
stamped copies (`WithIndexMetadata`). Two records of one fact that must not drift.
`cascades/plan_rich_ordering_parity_test.go` pins that they agree today; the durable fix is the
wrapper deletion (blocked, RFC-183 §11), after which only the plan's copy remains.

### One `NewRecordQueryIndexPlan` construction is unstamped

The correlated-index-scan build inside `ImplementNestedLoopJoinRule`'s existential path
(`rule_implement_nested_loop_join.go`, the `correlatedIndexScan := plans.NewRecordQueryIndexPlan(`
site) never calls `.WithIndexMetadata(...)`. Every other construction goes through
`abstract_data_access_rule.go:454`, which stamps. An unstamped index plan answers
`HintOrdering`/`HintRichOrdering` with the empty ordering — today harmless, because the memo asks
the WRAPPER, which carries its own metadata. It becomes a live sort-elision bug the moment the
plan-side bodies go live (see above).

### The memo costs an expression that is not the one that executes — CLOSED (RFC-184 W2)

**Was a live bug; fixed by the wrapper deletion the row above once called blocked.** The root
cause was DUAL STORAGE: a physical wrapper held both a `plan` snapshot AND its quantifiers, and
`WithChildren` kept `plan: w.plan` while swapping only quantifiers, so a compensating operator a
rule built locally (472 of 9868 rule-time edges) lived in the plan snapshot while the memo cost
the bare quantifier tree — masked only because the divergent quantifier never reached execution.

RFC-184 W2 deleted every physical wrapper (`InMemorySort` was the last; see
`plan_expression_flag_parity_test.go`). A physical plan is now its OWN cascades expression and
stores its children SOLELY as quantifiers — e.g. `RecordQueryFlatMapPlan` /
`RecordQueryNestedLoopJoinPlan` carry `outerQ`/`innerQ` and nothing else, and `GetChildren`
resolves through them. There is no second copy to diverge from: the dual-storage state the bug
required is now UNREPRESENTABLE. The compensating-operator rules (the NLJ existential path and
its siblings) memoize each operator and advance the quantifier over that reference in lockstep,
so the plan and its quantifiers name the same expressions by construction
(`rule_implement_nested_loop_join.go`, the FlatMap collapse; `rule_implement_simple_select.go`
always had this shape). `verifyChildrenMemoized` (always-on at every yield) rejects a holed or
shell expression at the source, and the full suite is green with the mask (the wrapper) gone —
which it could not be if any divergence survived. Pinned by `verifyChildrenMemoized`,
`TestOneFinalPlanPerReference`, and the flag-parity test.

## Quantifier identity: SQL-visible aliases vs Java's always-unique ids (W4-left F3 ruling)

Java mints `CorrelationIdentifier.uniqueId` for EVERY quantifier; SQL-visible names exist only as
Expression qualifiers (`LogicalOperator.newNamedOperator`). Go binds the VISIBLE alias as the
quantifier correlation, minting fresh ids only where collision forces it (the W5 gather's
inner-leg fresh-id; the W4-left dup-alias legs). Ruled ACCEPTABLE as a coexistence measure only
(design ruling on the W4-left slice, F3): the alias-collision rejections are the standing
justification, and the blast radius of always-unique ids spans every alias-keyed subsystem.
REVISIT AT S4+: once the name model is gone, converge on Java's structure (unique ids + name
qualifiers on projections).

W4-left commit-4 → QP-REF-BIND item 1 (RESOLVED). The W4-left commit-4 cut approximated Java's
per-attribute 42702 at the FROM WALK (a column-aware rejection of shared-column duplicates),
leaving two DIVERGENT CORNERS the FROM-level view could not reach: (a) `SELECT * FROM p, q, p`
answered in Java (duplicate columns, unique ids) but rejected 42702 in Go; (b) the PREDICATED
disjoint-column dup form (`ta AS a, tb AS a WHERE a.id = …`) answered in Java per-attribute but
declined 0AF00 in Go (predicates could not bind over indistinguishable correlations).

**QP-REF-BIND item 1 (c1 mint plumbing + c2 front-end flip + c3 star layout) CLOSES both corners.**
Each FROM leg now carries a deterministic binding id (`Q$DUPN` minted once at leg registration,
first occurrence keeps the alias), the semantic scope accepts duplicate aliases and resolves
references PER-ATTRIBUTE (a reference matching >1 same-aliased source carrying the column →
Java's exact `Ambiguous reference X` at resolution; a uniquely-matching reference binds to that
leg), and the wedge gate keys on the binding so the ordinal seed distinguishes the legs
end-to-end. `SELECT *` over duplicates answers with Java's positional duplicate-column layout
(`[ID V QID ID V]`, served by the ordinal seed's positional row —
`deriveColumnsFromJoin`'s RV-divergence arm). Corpus PARITY: `dup_from_alias_disjoint_where`,
`dup_from_alias_select_star`, `dup_from_alias_cte_star` all flipped (annotations deleted).
Remaining marked corners are message-drift only (undefined table under a dup alias stays 42F01;
the generated-aggregate quoted reference; the `…, a AS b` 42712-vs-42F01 lazy quirk) and the
MULTI-source-inner half of the cross-scope-shadowed correlated fallthrough: a multi-source inner
scope still trips the plan-time `CorrelatedShadowError` (42703) in `expr.ResolveIdentifier`
(cross-scope binding ids for multi-source inners remain the booked follow-on). The SINGLE-source
half (`SELECT p.v FROM p WHERE EXISTS(… q AS p WHERE p.v=…)`) CLOSED with the RFC-173 S4
collision mint: the inner source is born under a unique CorrelationName, so the resolver's
`isLocal` guard no longer swallows the parent hit — the fallthrough emits QOV(outer) and the
query ANSWERS with Java's live-verified semantics. Both variants are pinned by
`TestFDB_DuplicateFromAliases`.

## Element-shadows-outer vs Java AMBIGUOUS_COLUMN (dup-label unnest, shared-surface, Go-only reach)

When a lateral unnest's element/AT alias DUPLICATES an outer column name (`SELECT SUB FROM t,
t.scarr AS "SUB"`, or the CTE-boxed `WITH S AS (SELECT * FROM t, t.scarr AS "SUB") …`), Go's
deployed RFC-142 semantics resolves the reference to the UNNEST ELEMENT (element-shadows-outer,
last-write-wins). Java 4.12.11.0's `SemanticAnalyzer.resolveIdentifier` (SemanticAnalyzer.java
~:417/:422) resolves a duplicate column reference as `AMBIGUOUS_COLUMN` — an ERROR. So on this
shared surface Go RETURNS ROWS (the element) where Java REJECTS.

This is INTENTIONAL and surface-wide, not a one-off: element-shadows-outer is Go's existing rule
across the whole unnest surface (the direct form already returns the element on master). Making
only the dup-label case throw would be a bolted-on special-case (design principle #10) and would
re-open two silent-wrong-rows bugs the S4-B star-CTE slice fixed (a colliding derived twin was
serving the OUTER scalar, and wrong-leg IDs). Wire compat is untouched (pure read path). Graefe
(RFC-173 S4 item B deletion review) endorsed keeping the uniform element-shadows-outer semantics
and booking this here as the known divergence. Pinned: `TestFDB_StarBodyCTEJoinLeg`
(colliding_label_shadow). Follow-on if strict Java parity is ever required: make the dup-label
resolution loud `AMBIGUOUS_COLUMN` UNIFORMLY (direct + CTE forms), never just the boxed case.

## UNION ALL trailing ORDER BY: combined-result vs Java's right-leg-only (RFC-180, live-probed)

Java 4.12.11.0 attaches a trailing `ORDER BY` after `… UNION ALL SELECT …` to
the RIGHT LEG ONLY (QueryVisitor.visitSetQuery visits legs independently; each
leg keeps its own ORDER BY) — live-probed: `SELECT id FROM a UNION ALL SELECT
id FROM b ORDER BY id DESC` returns the interleave of left-natural with
right-DESC (`[6],[1],[2],[5]`), not a total order. Java also has NO positional
`ORDER BY <n>` at all (ExpressionVisitor.visitOrderByExpression does no
SQL-92 ordinal resolution; a bare integer becomes a constant sort key →
UnableToPlan). Go deliberately implements the SQL-standard COMBINED-result
semantics for the trailing ORDER BY (documented in union_columns.yaml) and
supports positional keys as a read-side extension, binding them to the union
OUTPUT slot by ordinal (SortKey.Pos carried through the union lift;
translateSort bakes the slot). Both are Go read-side extensions on a surface
where Java's own behavior is non-standard; wire compat is untouched.

## Cursor/continuation surface — systematic hunt (RFC-180 wave 2)

Four parallel Java-vs-Go sweeps (FlatMapPipelinedCursor; union/intersection
family; core combinators; InJoin/aggregation/sort) after the RFC-180
continuation work. Every (a)-class bug found was fixed in the same batch
(kept-armed resume state in flatMap+NLJ; terminal-result replay guards in
flatMap/NLJ/skip/filter/map; Java `limitRowsTo` 0-is-unlimited semantics;
stateful `Empty()` close; `FromListWithContinuation` nil-vs-empty; the
intersection non-max `consume()` advance). What remains is classified below.

### Clean extensions (compose with Java's way)

- **FlatMap/NLJ PK check values where Java-Cascades passes `checker=null`**:
  Go always writes the outer PK as the continuation check value; Java's
  Cascades FlatMap plan disables the check entirely. Go uses exactly Java's
  designed non-null-checker path and is strictly safer against
  between-transaction outer shifts.
- **FlatMap armed-inner survival across an outer out-of-band stop**: Go's
  wrapOuterContinuation carries the armed inner + check value; Java drops the
  armed state at the sentinel wrap (a later resume would re-run the saved
  row's inner — duplicates). Go re-arms identically to the initial decode.
- **InJoin check-value nil degradation for non-scalar in-values**: the outer
  element is pinned by the positional in-list index over a fixed literal set;
  the nil check is Java's own `checker==null` path.
- **Aggregate resume seeds the flat inner position**: Java seeds the WHOLE
  parsed AggregateCursorContinuation as previousContinuationInGroup — a
  first-row group break then nests aggregate bytes into the LEAF plan's
  resume (latent Java mis-resume). Go seeds the decoded flat inner position:
  same emitted rows, resumable continuation. Go is the fix.
- **Ordered UNION-ALL merge (removesDuplicates=false)**: Java's UnionCursor
  always dedups; Go's merge-sort union adds a non-dedup mode on the same
  state machine.
- **Intersection continuation child-count/started validation**: Go validates
  presence per child count where Java relies on list length — a defensive
  superset with identical valid-token behavior.
- **ListCursor unsigned position decode**: a corrupt high-bit 4-byte token
  exhausts cleanly instead of Java's IndexOutOfBounds; unreachable from valid
  tokens.

### Architectural (by design, with reason)

- **Pipeline depth**: Java prefetches (pipelineSize) in FlatMap/InJoin; Go is
  serial pull-based. Same rows, same order, same continuation bytes — only
  which page an out-of-band stop lands on can shift.
- **Aggregation TO_OLD mode**: exists in Java solely to consume
  pre-partial-aggregation legacy continuations; Go has no legacy tokens to
  read and always runs Java's TO_NEW arm.
- **Sort codec**: Go-owned typed payload inside Java's MemorySortContinuation
  wrapper. Cascades emits no physical sort Java could resume (RemoveSortRule);
  resume semantics are row-set-equivalent to Java's minimum-key re-scan.
- **UnorderedUnion**: RESOLVED — Go now matches Java's UnorderedUnionCursor
  contract serially: per-child UnionCursorContinuation slots, a limit-stopped
  child parks while the rest keep emitting, strongest-reason terminal, full
  resumability. The deterministic child order remains a legal realization of
  Java's explicitly unspecified order. (The former eager concat's loud
  declines are gone; the dead concat cursor was deleted.)
- **ProbableIntersectionCursor / bloom-filter weak reads**: entirely
  unimplemented (no plan type reaches it); Java's own docs flag the
  Guava-serialized bloom bytes as a cross-compat hazard. Missing capability,
  not a divergence in shared code.
- **FutureCursor / fromFuture**: absent; single-future shapes are expressed
  via FromList/flatMap composition instead.
- **NoNextReason ordinals differ** (semantics and all predicates match); the
  enum is never serialized. Latent trap only if someone persists ordinals.
- **ConcatCursor carries no ScanProperties**: row-limit splitting and
  reverse ordering are the Go caller's composition concern; pull-based
  execution makes an outer LimitRows equivalent.
- **Construction-time vs first-OnNext continuation-parse errors**: Go defers
  all parse failures to an errorCursor on first pull (I/O-free construction);
  Java throws in the constructor. Both fail loudly; timing differs.


## PLANNING dual insertion: physical yields live in BOTH member sets (RFC-180 D2 audit)

**Go:** `TransformExprTask`'s PLANNING yieldFn inserts a physical yield into
BOTH the exploratory members (rule matching / convergence detection) and the
final members (OptimizeGroup selection). **Java:** no dual insertion —
`yieldUnknownExpression` routes a physical plan to the FINAL set only; the
exploratory set never holds plans.

Consequence: pruning (which trims finals only) leaves an exploratory copy of
every pruned loser behind, so identity guards that transpose Java's
`containsExactly` must demand FINAL survival instead
(`Reference.ContainsFinal` in `OptimizeInputsTask`) — a compensating
divergence for a structural one. The transform-task guards
(`TransformExprTask`/`TransformImplTask`) deliberately keep the both-sets
check: re-firing rules on a pruned-but-exploratory physical member matches
the convergence bookkeeping the dual insert exists for.

**Open question (Graefe review of 0aea06b48):** should the dual insertion
itself die in favor of Java's final-only routing, letting the exploratory
set hold only logical expressions? That would restore `containsExactly`
parity wholesale and shrink re-exploration work, but the convergence
detection (`NeedsExploration` keyed on member growth) currently counts
physical yields as exploration progress — unwinding it is a planner-core
arc, not a patch. Until then: any new identity guard on physical members
must use `ContainsFinal`.

## Ordering translation through renaming projections (performance-only gap)

Sort-elision satisfaction resolves an order-preserving wrapper through its
SOURCE GROUP without translating the requested ordering's Values across a
projection's renames (`orderingDelegator` — the request stays in output
space while the child orders in input space). A rename therefore fails
`orderingKeyFor` resolution → elision declines → an avoidable enforcer sort
(never a wrong order). Java translates requested orderings through
pulled-up value maps (`RequestedOrdering.pushDown` on the projection's
result value). Go has the machinery (`RequestedOrdering.PushDownThroughValue`)
— wiring it into the delegation walk is the follow-up; until then the
decline arm keeps correctness.

## Multi-type-index pk-merge intersections decline (safe parity gap)

Java can plan a pk-merge intersection whose legs are multi-record-type
indexes: `ValueIndexScanMatchCandidate.getBaseType()` returns a MERGED
`Type.Record` for multi-type candidates, so its comparison keys always
bind. Go's positional row model keeps each descriptor's own layout, so a
multi-type candidate flows no single row type (`metadataIndexDef.
IndexRowType` returns Unknown for `len(recordTypes) != 1`), and the
intersector's bake gate (`bakedIntersectionKeys`) DECLINES the candidate
— the query still answers via scan+filter, never a wrong row. Closing
this needs a merged positional layout for multi-type rows (the analogue
of Java's type-merge), which is a WS-N Phase D-adjacent arc; until then
the decline arm keeps correctness. Pinned by
TestIntersector_DeclinesLayoutlessLegs / DeclinesMixedLayoutLegs.

## SQL statement continuations are engine-private (RFC-181 C2 decision)

Java's fdb-relational wraps SQL continuations in a ContinuationProto
envelope (version + plan_hash + binding_hash + execution_state) and
gates resumes through PlanValidator. Go has no envelope and NO SQL
resume entry point at all — statement paging is internal to one
execution (`paginatingRows`), and tokens never cross the API boundary.

The RECORD-LAYER continuation framing below the SQL layer is
byte-identical and conformance-proven for the LEAF/structural cursors —
notably the magic `KeyValueCursorContinuation` wrapper and the union/
intersection/flat-map framing. It is NOT byte-identical for the
aggregate and in-memory-sort cursors, and this is now stated precisely
rather than folded into a blanket "byte-identical" claim:

- **StreamingAggregation** (`AggregateCursorContinuation` /
  `PartialAggregationResult`): Go reuses Java's proto message SCHEMA but
  packs a Go-PRIVATE layout into `AccumulatorState.state` — exactly
  `[count] ++ per-aggregate[count, sum, sumI, allInt, min, max]`
  (`1 + 6*numAggs` typed slots) with a lossless typed codec for MIN/MAX,
  which is not Java's `StreamGrouping` per-aggregate serialization. A
  Java-authored token would proto-Unmarshal cleanly and then be mis-read
  positionally, so `decodeAggregateContinuation` now REQUIRES the exact
  Go slot count and slot types and rejects any other shape loudly
  (`TestDecodeAggregateContinuation_ForeignShapeFailsLoud`) — never a
  silent zero-fill.
- **In-memory sort** (`MemorySortContinuation`): a Go extension with no
  Java counterpart at all (Java's Cascades has no physical sort
  operator), so there is nothing Java could produce or consume.

Both are SAFE because they never cross an engine boundary: the SQL layer
rejects an externally-supplied continuation with
`ErrCodeUnsupportedOperation`, and the record-layer executor that pages
them is Go's own engine. The shared proto message NAMES are a schema
convenience, not an interop promise; the payloads are engine-private and
now fail loud on any foreign shape rather than corrupting silently.

Decision: Go SQL tokens are ENGINE-PRIVATE until a real resume surface
exists. The boundary is loud, not silent: supplying api.OptContinuation
fails the statement with ErrCodeUnsupportedOperation
(cascadesPlan.Execute; pinned by TestOptContinuation_RejectsLoudly) —
never silently ignored, which would replay from row 1 while the caller
believes they resumed. Adopting the ContinuationProto envelope +
PlanValidator hashes is the follow-up arc if/when a resume surface
ships; hash values would deliberately differ per engine so cross-engine
resume attempts REJECT loudly in both directions.

## BIGINT-vs-DOUBLE comparison: exact narrowing, not lossy promotion

Java compares a BIGINT column against a DOUBLE constant by PROMOTING
the column LONG→DOUBLE — lossy above 2^53, so `v = 9007199254740992.0`
wrongly matches a stored 2^53+1. Go rewrites the CONSTANT instead
(narrowFloatConstAgainstInt): integral doubles narrow to the column's
integer type, non-integral and out-of-range bounds rewrite to the
tightest integer predicate — exact at every magnitude. Go-right
divergence, verified live by the bigint_eq_double_above_2p53 corpus
entry (DivergenceJavaWrongRowsGoCorrect).

## Bound parameters stay Unknown-typed at the plan gates

The plan-time promotion and cast-pair gates exempt UNKNOWN-typed
operands, which includes bound parameters on the exported
PlanRecordQueryWithMetadata path (the SQL driver substitutes `?` as
text, so driver-reachable binds arrive STRING-typed and take the
STRING arms). Java types parameters by inference and gates them too;
Go's parameter-inference arc would close this — until then a
LONG-bound parameter through the exported path evaluates leniently
where Java rejects at plan time.

## REWRITING prune: virtual (designation) vs Java's physical prune (RFC-186)

Java's REWRITING prunes every child reference to ONE final expression before parents
compare, and every cost property derives through that single final
(`ExpressionCountProperty.forReference`: `Verify(size()==1)` + `getOnlyElement`). Java can
afford the physical prune because its PLANNING re-derives discarded canonical forms via its
rule set. Go CANNOT: the universal boundary prune was tried and reverted (the RFC-153
buried-leg and cross-join-EXISTS shapes lost their only implementable form — see
`unified_tasks.go` ExploreGroupTask's stage-boundary comment), and the OptimizeInputs child
pruning is timing-dependent (finals promoted in a group's last exploration round get no
pass), so groups legitimately cross the stage boundary with their full canonical final set.

Go's equivalent is the DESIGNATED final (`designated_final.go`): every REWRITING cost
property derives through one deterministically-chosen final per child reference — chosen
with the same comparator OptimizeGroup uses, memoized keyed on a global finals generation
(any final-set mutation invalidates), cycle-guarded. Costing sees Java's post-prune world;
the memo keeps every alternative PLANNING needs. The coherence instrument
(`SetVerifyRewritingCoherence`, permanently on in the embedded plan harness) asserts the
stamped winner IS the designation wherever a REWRITING winner exists.

END-STATE: when PLANNING re-derivation parity lands, the physical prune becomes affordable,
the designation degenerates to the single final, and Java's `Verify(==1)` becomes
enforceable — at which point this divergence closes.
