# Divergences from Java fdb-record-layer-core 4.14.2.0

Comprehensive list of Go vs Java differences. All Cascades planner subsystems
fully ported: ~65 PlanningRuleSet rule instances, 5/5 RewritingRuleSet rules,
34/34 physical plan types, 48/48 value types, 19/19 properties, 12/12 match
candidate types, 24/24 comparison operators, 9/9 predicates. Remaining items
are execution-layer, wire-format, or intentional architectural choices.

Validated against a live Java **4.12.11** conformance run (the cross-engine corpus runs against
live 4.12 in `just test-full` and PR CI with a stale-annotation guard, and the suite is green).

## Intentional Architectural Decisions (no functional difference)

### Default format version for a NEW store: Go writes 14, Java writes 7 (DECIDED — keep 14)

Java's default is **`CACHEABLE_STATE` (7)** — `FormatVersion.getDefaultFormatVersion()`
(`FormatVersion.java:215-217`), which `FDBRecordStore.Builder` seeds itself with
(`FDBRecordStore.java:5437`). Go's default is `formatVersionDefault = 14`, which is
`FULL_STORE_LOCK(14)` (`FormatVersion.java:173`) — the same value as Java's
`MAX_SUPPORTED_VERSION` (`:182`). So a new store created by Go declares a **newer
on-disk format than a new store created by Java**, and Go also upgrades any store it
opens to that version.

**DECISION: keep Go's default at 14.** Not for tidiness and not by default — the
question was carried to the Java source and the answer settles it.

**What Java actually does, read rather than assumed.** `validateFormatVersion`
(`FormatVersion.java:224-231`) throws `UnsupportedFormatVersionException` only when the
candidate is `< getMinimumVersion()` **or** `> getMaximumSupportedVersion()`. At
4.12.11 `MAX_SUPPORTED_VERSION` is computed as the maximum enum value (`:182`) and
that value **is** `FULL_STORE_LOCK(14)`. So 14 ≤ 14 and the check passes. Then
`checkPossiblyRebuild` (`FDBRecordStore.java:4627-4630`):

```java
final int oldFormatVersion = info.getFormatVersion();
final int newFormatVersion = Math.max(oldFormatVersion, formatVersion.getValueForSerialization());
final boolean formatVersionChanged = oldFormatVersion != newFormatVersion;
formatVersion = FormatVersion.getFormatVersion(newFormatVersion);
```

A Java instance at its default 7 opening a Go store at 14 computes `max(14,7) = 14`,
leaves `formatVersionChanged` false so **nothing is written**, and **adopts 14** for
that store instance. It does not throw, and it does not downgrade. **There is no
interop break at the pinned spec**, which is the premise the case for changing rested
on.

**The cost of aligning, established from the write gates rather than from a suite run.**
Format versions 8–14 gate store-header features — header user fields (8),
`READABLE_UNIQUE_PENDING` (9), record-count state (11), store lock state (12),
incarnation (13), full store lock (14). A default of 7 disables **all** of them by
construction: every one is guarded by `requireFormatVersion`, which reads the header,
so a store born at 7 refuses each with `UnsupportedFeatureForFormatVersionError`. Java
users at Java's default accept exactly this and opt in via `setFormatVersion`; the
difference is that Go's feature set is newer, so the default would be turning off more.
No measured interop benefit against a definite functional cost.

**The real blocker was one level down, and it is FIXED.** This entry previously said
the remaining work was "a decision about the DEFAULT, not a missing capability." That
was wrong. `formatVersionCurrent` served **both** of the roles Java keeps apart —
the validation ceiling (`store_builder.go:185`, `:1338`, rejecting `stored > current`)
and the creation/upgrade target (`:199`, `:1005`, `:1200`) — where Java separates
`getMaximumSupportedVersion()` (`:203`) from `getDefaultFormatVersion()` (`:215-217`).
Fused, **lowering the default would have lowered the ceiling with it, and every store
Go had already created at 14 would have been rejected on open** with
`UnsupportedFormatVersionError`. "Just change the default" was a change that could not
be made safely at all. Now split into `formatVersionMaxSupported` and
`formatVersionDefault` (both 14, zero behaviour change), so the default is a free
decision and the existing-store path is safe by construction: the ceiling does not move
when the default does. Pinned by `format_version_split_test.go`, including the case the
fusion made inexpressible — a store pinned BELOW its own header still opens, because
validation consults the ceiling and never the target.

**THE STANDING COUNTER-ARGUMENT, and the condition that flips this decision.** Go at
default 14 opening an existing **Java** store at 7 upgrades it to 14 and writes that,
permanently narrowing the set of readers that store can be opened by. Java at its
conservative default never does this to someone else's store. It is harmless at the
pinned spec — Java 4.12.11 reads 14 — and it is the strongest argument that exists
for lowering the default.

It flips the decision if any of these becomes true:
- a deployment mixes Go with a Java version whose `MAX_SUPPORTED_VERSION` is **below**
  Go's default, because `validateFormatVersion` then rejects the store outright;
- Go's default is ever raised past Java's max (guarded:
  `TestFormatVersion_DefaultIsWithinTheCeiling` fails on it);
- the 8–14 feature set stops being wanted by default, removing the cost side.

Until one of those holds, 14 stays. Callers that need Java's conservative default have
`StoreBuilder.SetFormatVersion` — the mechanism, unlike the decision, was never the
missing piece.

### Version-gated header features: WRITES and READS both gated like Java (CLOSED)

**The gap was an AMBIGUITY, not a leniency — which is why this entry's own defence was
the wrong test.** It used to argue "the read paths write nothing, so no bytes diverge",
reasoning from bytes on disk when the hazard was a CALLER'S INFERENCE. A caller could
not distinguish:

- "this store has no incarnation yet" from "this store's format is too old to have one"
  — both were `0`. A migration keyed off incarnation reads a too-old store as a
  never-migrated one;
- "this user field is unset" from "this format cannot hold user fields at all" — both
  were `nil`.

**And the pair was ASYMMETRIC.** `SetHeaderUserField` refused loudly at exactly the
version where `GetHeaderUserField` answered "absent" — the shape that makes a caller
conclude its own write silently failed to persist. A write that errors beside a read
that shrugs is worse than either alone.

**The concrete cost was one call site away from a real index defect.**
`key_expression.go:1480`, the incarnation key expression, swallowed the missing gate into
a `0`: every record in a too-old store would have keyed under a single index entry. That
is not "match Java's strictness" — that is a live defect the gate prevents.

---

Java guards every version-gated store-header feature at its site with
`if (!getFormatVersionEnum().isAtLeast(V)) throw` — header user fields
(`FDBRecordStore.java:3222`), record-count state (`:3443`), store lock state
(`:3478`/`:3494`), incarnation (`:3503`/`:3517`) — and gates the record-count key/state
written at store creation independently (`:5950-5957`).

Go ports **every WRITE gate** (`FDBRecordStore.requireFormatVersion`, returning
`UnsupportedFeatureForFormatVersionError`), because those are the wire-compat hazard: a
v12 store lock written into a header that still declares 11 is a lock a correctly-behaved
older reader does not understand and therefore silently ignores. Pinned by a table-driven
spec per feature plus a contrast case at the current version.

**The READ side is now gated too, and the previous objection is dissolved.** This entry
used to record the gap as deliberate, on the grounds that closing it "changes two public
signatures from value-returning to `(value, error)`, which is an API break worth its own
change." The project is pre-release and Go API breaks are acceptable, so that objection
does not survive contact with the actual cost.

`GetIncarnation()` now returns `(int32, error)` and `GetHeaderUserField()` returns
`([]byte, error)`, both gated through the same `requireFormatVersion` the writes use —
Java's `getIncarnation()` (`:3502-3506`) and `validateCanAccessHeaderUserFields()`
(the shared validator at `:3221-3226`, reached from getHeaderUserField at
`:3249`).


The read entries live in the same table as the writes, deliberately, so the two ends
cannot drift apart on a version. The contrast case asserts the absent-value stays
REACHABLE at a supporting version — an incarnation of `0` and a `nil` field with no error
— because separating the absent-value from the error is the entire point; a reader that
always errored would satisfy the gate table on its own.

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
**Go:** A SQL query block is now one `SelectExpression`, as in Java (TODO.md, "A SQL query block is one SelectExpression"), and Go has no projection expression or projection plan; the top-level query is a `LogicalSortExpression` over the block, Java's `Sort(Select)`.

### NormalizePredicatesRule — RESOLVED

**Java:** Fires on all SelectExpressions including those with Existential quantifiers.
**Go:** Now fires on all SelectExpressions (matching Java). Hash-based dedup prevents the infinite normalization loop that previously required an existential guard.

### WithPrimaryKeyDataAccessRule is an explicit planner pass (UPDATED Phase 7.2)

**Java:** `CascadesRule<MatchPartition>`, fired via match-partition rule infrastructure. `createIntersectionAndCompensation` aggregates cross-candidate matches into physical intersection plans during PLANNING.
**Go (Phase 7.2):** Explicit pass in `Planner.pushDataAccessTasks()`. `WithPrimaryKeyIntersector` creates physical `RecordQueryIntersectionPlan` from cross-candidate `PartialMatch` pairs. `IndexIntersectionRule` (Go-only REWRITING rule) deleted. Guards: candidate cap (4), match cap (8), restricted-scan filter, idempotency.

Same timing and inputs. Go creates physical plans directly (single intersection strategy); Java goes through `LogicalIntersectionExpression` → `ImplementIntersectionRule` (supports multiple strategies).

### RESOLVED — the Go-only second index-scan path is gone (`ImplementIndexScanRule`, retired by RFC-076)

**Java:** One rule family — `AbstractDataAccessRule` — turns a `PartialMatch` into a scan/index-scan/fetch via `toEquivalentPlan`. The "index-only value can't be a residual" property is enforced ONCE: `PredicateMultiMap.ofPredicate` stamps `isImpossible = predicateContainsUncompensatableValues(predicate)` (true when a predicate operand `instanceof Value.IndexOnlyValue`), and `applyCompensationForSingleDataAccessMaybe` drops any match whose compensation `isImpossible()`. No separate "implement index scan" rule exists, so the property can't leak.

**Go:** ONE path reaches a physical index scan — the data-access/compensation match path (`predicate_multi_map.go`), matching Java. The second path this entry was written about, `ImplementIndexScanRule`, was **retired by RFC-076**: there is no such type, constructor, file or rule-set registration anywhere in `pkg/` (only stale comments still name it). Two claims that stood here were false and are struck: the rule was described as a fusion of a Java `ImplementPhysicalScanRule`, which does not exist in the Java tree either (Java scans come from `AbstractDataAccessRule`); and the "implement layer" was said to be pinned by a named test that matched no test function at all — running that filter prints PASS over zero tests, so the pin was never capable of failing. (The phantom name is deliberately not repeated here; git history has it, and reproducing it would only put it back in front of the next reader.)

The compensatability property is therefore enforced ONCE, as in Java: `valueContainsUncompensatable` via `values.IsIndexOnly` on the match path, pinned by `TestVectorPlan_QualifyPlansToVectorScan`. The property matters because of vector K-NN (RFC-045): the `DistanceRowNumberValue` operand is index-only, and a partition-only primary-scan candidate would otherwise leave the `DistanceRank` comparison as a residual filter (panics in `Comparison.EvalAgainst`).

A THIRD Go-only filter producer was `ImplementFilterRule` (synthesizes a `RecordQueryPredicatesFilterPlan` over the inner physical winner without routing through `Compensation`). **RESOLVED (RFC-151):** `ImplementFilterRule` now carries Java's `all(anyCompensatablePredicate())` / `!isIndexOnly()` gate (`ImplementFilterRule.java:62`, `QueryPredicateMatchers.java:66-68`) — it returns early for any index-only predicate, exactly like Java, so the leaking filter is **never built**. The old "guarding ImplementFilterRule is not viable — removing its member collapses the filter Reference and the data-access intersection is never built" claim was a *scheduling* artifact, not a memo entanglement: Go's `pushDataAccessTasks` ran inline at `ExploreExprTask` start, BEFORE the matching rules seeded the ref's partial matches, so the data-access consumption depended on `ImplementFilterRule`'s incidental physical-filter yield to re-trigger exploration. RFC-151 makes `TransformExprTask` re-run data-access whenever a rule grows the ref's partial-match set (Java's `getNewPartialMatches()` reaction, `CascadesPlanner.java:1058-1062`), so the legitimate vector scan is consumed directly and the gate is safe.

**The `validateNoIndexOnlyResidual` physical net is RETAINED as the catch-all backstop** — the gate covers ONLY `ImplementFilterRule`, but an index-only `DistanceRank` in the original query reaches a physical residual via other Go-only builders too: `ImplementSimpleSelectRule` (a `SelectExpression`'s predicates — the JOIN shape, where the distance is a Select predicate not a standalone LogicalFilter) and the NLJ residual builder. That is TWO live ungated producers, not three: the third listed here was `ImplementIndexScanRule`'s residual loop, which went with the rule in RFC-076. The net is the one place covering EVERY physical-filter path. A logical-side `findIndexOnlyLogicalResidual` check is ADDED for the complementary case (when the gate leaves the best plan non-physical, the physical walk sees nothing). Both surface the clean `UnplannableIndexOnlyResidualError`. Pinned by `TestVectorPlan_MetricMismatchDoesNotMatchVector` (single-table) + `TestVectorPlan_MetricMismatchInJoinDoesNotLeak` (join, the regression Graefe + Torvalds caught) + `TestVectorPlan_QualifyPlansToVectorScan` (legit). **End-state to fully retire the net:** gate `ImplementSimpleSelectRule` + NLJ on `!isIndexOnly()` so no physical builder can produce an index-only residual — a smaller separate follow-up.

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

### Sibling leg aliases stay bound at runtime (`bindMergedOuterLegs`) — RETIRED (RFC-235)

**The divergence is gone.** `executor.bindMergedOuterLegs` is deleted, along with
its census and the three test-only `EvaluationContext` hooks that drove it.

It existed because Go's two-level NLJ→FlatMap lowering collapsed a multi-source
outer into one join whose row was a merged concat, so binding only the join's own
alias left the source aliases the inner still referenced unbound. Java never has
that row: where its planner collapses several sources into one quantifier it
RE-ANCHORS every reference to a sibling by ordinal
(`PartitionSelectRule.java:296-303`) and the sibling alias ceases to exist.

The producer of those merged rows was `ImplementNestedLoopJoinRule`'s Go-only
three-quantifier arm, which RFC-235 retired: `[ForEach, ForEach, Existential]`
now decomposes through Java's Case-1 existential peel, so no merged multi-leg
outer row is built and there is nothing for a runtime namespace to serve.

**Measured before removal**, over the whole real-FDB corpus: the binder bound
**15,032 windows across 155 distinct shapes and was READ ZERO TIMES** — down from
3 reads across 303 shapes while the arm existed. It ran once per OUTER ROW, so it
was work on the row-rate path with no consumer.

The retirement condition this entry carried for its whole life — "a leg-correlated
read is rewritten to `ofOrdinalNumber` against the merged quantifier before
execution, and no sibling alias needs to be resolvable at runtime at all" — is
met by removing the shape rather than by converting the reads. Every point
measurement this entry used to quote (174 of 174, 190 of 190, `MergedReAnchor` 0,
the 102/60/108 firing split, `existentialRebase` 962 → 1086) described populations
that no longer exist. They are not corrected here because there is nothing left to
correct: the arm, the censuses that measured it, and the binder are all deleted.

**Both shapes the retirement briefly cost are restored, the way Java has them.**
Projected EXISTS over a LEFT JOIN, and the projected-EXISTS-with-duplicate-alias
fold, both lost their plan when the arm went. RFC-235 §16 put them back:
`RewriteOuterJoinRule` now BOXES the outer join into a single quantifier — Java's
`OuterJoinExpression` shape — so the enclosing select is BINARY and
`ImplementNestedLoopJoinRule` matches it directly, and the duplicate-alias fold
was a bipartition wrongly admitted by a live-existential guard that now requires a
result-live existential to be ALONE in its lower.

`conformance/projected_exists_left_join_java_probe_test.go` keeps the parity claim
measured against the live 4.12.11 JVM rather than inherited from prose.
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

### Distinct union: merge states per leg, not the partition cross product

Java's `ImplementDistinctUnionRule` walks the cross product of the legs' plan
partitions, pruning a failing prefix with `skip()`, and yields one union per
compatible combination and comparison key. The successes alone grow as the
product of each leg's compatible partitions: the fixed-factor seed
(`TestPlanHarness_FixedFactorUnionJavaComparable`) builds unions of 18 legs with
1-5 partitions each, 1.66e9 combinations, and Java fails the same query with a
StackOverflowError (`conformance/projected_exists_left_join_java_probe_test.go`).
Go (`reachableUnionMergeStates`) carries the distinct merge states leg by leg —
the merged ordering plus the merged legs' record identity, which is all a prefix
contributes to the rest of the merge — and yields once per comparison key, each
leg the cheapest member of any of its partitions that delivers that key. The
yields are the cheapest of Java's per comparison key, as Cascades' winner per
required property. Java also lacks Go's per-leg record-identity check
(`RecordIdentityWithin`), which Go keeps in place of
`isPrimaryKeyCompatibleWithOrdering`. Plans agree with Java on the primary-key
disjunctions in `conformance/or_union_merge_java_probe_test.go`.

## Planning-Layer: Java-aligned core with documented Go extensions

### Cost Model: PlanningCostModelLess

Java PlanningCostModel criteria #1–#17 are accounted for below; Go broadens/hoists #15 and adds
explicit sort-count, NLJ-predicate, and statistics-cost rungs. Criterion-by-criterion analysis:

| Criterion | Java | Go | Status |
|---|---|---|---|
| 1. Physical beats non-physical | `instanceof RecordQueryPlan` | `isPhysical` | Aligned |
| 2. Max data access cardinality | CardinalitiesProperty gate + comparison — index arm has TWO criteria: PK-bound-by-equalities, falling through to unique-index | Data-access cardinality gate — implements the uniqueness criterion only | **Deliberate divergence — Go's sargable surface excludes the PK, so Java's criterion 1 has no constructible input (see below)** |
| 3. Residual predicate count | NormalizedResidualPredicateProperty (`getMetrics(p).getNormalFormFullSize()`) | `countResidualPredicates` using `normalFormSize(p, false, normalFormCNF)` | Aligned (RFC-240 — was NOT aligned: the old `cnfSize` recursed through a NOT without swapping the major/minor roles, so `NOT(a OR b)` counted 1 where Java counts 2) |
| 4. Data access count | count(Scan, Index, Covering) | `scanCount + indexScanCount + coveringIndexCount` | Aligned |
| 5. Recursive CTE DFS > level | flipFlop(compareRecursiveCte) | `compareRecursiveCTE` | Aligned |
| 6. IN-plan SARG penalty | flipFlop(compareInOperator) — evaluates the LEFT argument only and returns a PRESENT tie for a SARGed in-plan | `compareInPlan` ranks BOTH sides on the penalty | **Deliberate divergence — Java's rung is not antisymmetric (see below)** |
| 7. Primary vs index scan | `flipFlop(comparePrimaryScanToIndexScan)` — a pair-restricted guard, plus a comparison-SUBSET sub-case, plus the configured `IndexScanPreference` (Cascades default `PREFER_SCAN`) | `comparePrimaryScanVsIndexScan` ranks BOTH sides on `primaryVsIndexRankOf`, a tiered ladder whose contested band orders by comparison-set SIZE | **Deliberate divergence — Java's rung is not transitive, and its adjudicated verdicts are themselves cyclic (see below)** |
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

#### Criterion 2 — max data access cardinality — DELIBERATE DIVERGENCE (Go candidate-model constraint)

**Unlike criteria 6 and 7, this is NOT a Java defect. Java is correct here and Go proves strictly
less. Read-side plan choice only — nothing here touches the wire.**

Java's `CardinalitiesProperty.visitRecordQueryIndexPlan` (`CardinalitiesProperty.java:313-355`) has
**two** criteria for an index plan, the first falling through to the second on a miss:

1. `:329-337` — if the candidate is a `WithPrimaryKeyMatchCandidate` and the scan's
   `equalityBoundValues` contain all of `primaryKeyValues`, return `atMostOne()`. This applies to
   value-index candidates: `ValueIndexScanMatchCandidate` implements `ScanWithFetchMatchCandidate`
   (`:52`), which extends `WithPrimaryKeyMatchCandidate` (`ScanWithFetchMatchCandidate.java:44`).
2. `:339-352` — if the candidate `isUnique()` and is a `ValueIndexScanMatchCandidate`, rebase the
   index key values and trim to `getColumnSize()`; if the equality-bound set covers them, return
   `atMostOne()`.

Go implements **only criterion 2** (`indexProvableMaxCard`, `planning_cost_model.go:503`).

**Cause — the sargable-surface invariant.** Criterion 1 requires equalities *binding primary-key
positions*. Java can express that because its `indexKeyValues` span index key **plus** the PK suffix
— which is exactly why criterion 2 must call `.limit(getColumnSize())` to trim the PK back off. Go's
candidates deliberately never fold the PK into the sargable surface
(`match_candidate_index.go:678-681`: "The PK is NOT part of the sargable surface … that invariant
must hold"), so a constructed index plan's comparison count never exceeds its index key column
count and criterion 1 has **no constructible input**. Note the two criteria compare in the same
value domain, so this is a genuine gap and not a Java no-op: `primaryKeyValues` come from
`ScalarTranslationVisitor.translateKeyExpression` rooted at `Quantifier.current()`
(`ScalarTranslationVisitor.java:230`), and `equalityBoundValues` are built through the same
`toResultValue(Quantifier.current(), …)` (`ValueIndexLikeMatchCandidate.java:139-141`) — which is
why criterion 1 needs no rebase while criterion 2 does.

**Cost — this is decisive, not silent.** `PlanningCostModel.java:127-132` returns on one-sided
abstention: the side whose max-of-max data-access cardinality is unknown **loses outright**. The
outer guards at `:121`/`:125` are disjunctive, so they tolerate only the both-abstain case. For a
PK-bound non-unique index scan, Java ranks the plan first on this rung and Go ranks it last, and the
comparison never reaches the structural tie-breakers below.

**Why it is not closed here.** Folding the PK into the sargable surface is the only way to make
criterion 1 reachable, and it re-opens a regression this repo already paid for: counting the PK
suffix in `unmatchedFieldsForIndex` over-counts a fully-bound index probe and mis-ranks criterion
#12 toward a full-scan join driver (`planning_cost_model.go:2833-2835`). That change belongs to its
own RFC with its own plan-diff, not to a cost-model unification. Pinned by
`TestRFC219_IndexPlanWidthInvariant`, which drives all four production index-plan construction sites
and fails if any can exceed the index key width — the event that would make criterion 1 reachable
and this entry stale. Measured `len(comps)/len(columnNames)` per site: `match_candidate_index` 3/3,
`windowed_index_match_candidate` 2/2, `aggregate_index_candidate` 2/2 (clamp proven by feeding
`physicalGroupingPrefixCount = 5`), `rule_implement_nested_loop_join` 1/2, plus a positive control
at 2/1 confirming the predicate fires. Three of the four are structural; the **windowed** site is a
caller contract — `columnNames` and `groupingAliases` are independent constructor parameters with no
clamp. That arm is additionally unreachable today (`NewWindowedIndexScanMatchCandidate` has no
non-test caller), so its unclamped parameter space is not exploitable; it is nonetheless the first
arm to re-check if a production caller ever appears or if criterion 1 ever looks reachable.

#### Criterion 6 — IN-plan SARG penalty — DELIBERATE DIVERGENCE (Java upstream defect)

**Java's rung is not antisymmetric, so it can make the winner depend on the order the memo's members
arrived in rather than on what they are. Go ranks both sides instead. Read-side plan choice only —
nothing here touches the wire, so Java and Go still read and write byte-identical records.**

Java (`PlanningCostModel.java`, tag 4.12.11):

- `compareInOperator(leftExpression, rightExpression)` (`:433`) declares its second parameter
  `@SuppressWarnings("unused")` (`:434`) and never reads it. It returns `OptionalInt.empty()` when the
  LEFT expression is not an in-plan (`:436`), `OptionalInt.of(1)` when the left in-plan's bindings
  never became search arguments (`:456`, `:463`), and `OptionalInt.of(0)` — a PRESENT tie — otherwise
  (`:467`).
- `flipFlop` (`:511`) returns variant A's result directly whenever it is present (`:514-515`), so a
  present 0 stops the evaluation and the `(b, a)` orientation is never asked.
- The call site (`:177-181`) then guards with `getAsInt() != 0`, so a present 0 falls through to the
  remaining rungs. Java's own Javadoc says as much — *"That in turn causes the remainder of the
  tie-breaking code to be used"* (`:429-430`) — and contains a typo, writing `OptionalInt.of(1)` twice
  where the second occurrence means `of(0)` (`:427-428`).

Writing `X` for a SARGed in-plan, `Z` for an unSARGed one and `Y` for a non-in-plan, that yields two
independent antisymmetry violations:

| pair | Java | Java reversed | antisymmetric? |
|---|---|---|---|
| `X, Z` | `0` (abstains, chain continues) | `+1` (short-circuits) | **no** |
| `Z, Z'` | `+1` (`Z` is worse) | `+1` (`Z'` is worse) | **no** |

A comparator built by lexicographic composition of criteria is transitive only if every criterion is
itself a total preorder, and criterion 6 is not one. The consequence is not a visible "both are less"
contradiction — winner selection asks `less` in both directions — it is quieter: for two unSARGed
in-plans `less` is false BOTH ways, so every later rung, including the fetch/type-filter/sort rungs
and the full cost model, is short-circuited and the winner falls to the plan-hash tie-break, or, in
`OptimizeGroup`'s fold (which compares without a tie-break), straight to member insertion order. A
dramatically better unSARGed in-plan can lose to a worse one for no reason connected to its cost.
`X` versus `Z` fails the same way whenever a later rung happens to prefer `Z`.

The correct shape already exists elsewhere in the same Java class: `compareRecursiveCteOperator`
(`:492`) returns `of(-1)` only for the exact `(DFS, LevelUnion)` pair and `empty()` otherwise — never
a present tie — so `flipFlop` always gets to ask the reverse question. `comparePrimaryScanToIndexScan`
is likewise safe on this axis: its applicability guard cannot hold in both orientations at once.

Go's `compareInPlan` evaluates `compareInOperator` for both arguments and compares the penalties. It
reduces to a total preorder on a rank in `{0, 1}` — non-in-plans and SARGed in-plans both rank 0,
unSARGed in-plans rank 1 — so `cmp(a,b) == -cmp(b,a)` holds for every pair and the rung composes
transitively with the rest of the chain:

| pair | Go | Go reversed |
|---|---|---|
| `X, Z` | `-1` | `+1` |
| `Z, Z'` | `0` (falls through) | `0` |
| `X, X'` | `0` (falls through) | `0` |
| `Z, Y` | `+1` | `-1` |
| `X, Y` | `0` (falls through) | `0` |

Behaviourally this changes exactly two things versus Java: a SARGed in-plan now beats an unSARGed one
outright instead of deferring to the later rungs, and two equally-penalised in-plans now tie so those
later rungs decide them. Java's SARGed-abstains intent (its Javadoc's "remainder of the tie-breaking
code") is preserved for the `X`-versus-`Y` and `X`-versus-`X'` cases, where it was never ambiguous.

Pinned by `TestCostModel_InPlanComparisonIsOrderIndependent` (brute-force antisymmetry / transitivity
/ totality / permutation sweep over an in-plan-rooted corpus of IN-joins and IN-unions, SARGed and
not, mixed with non-IN plans), `TestCostModel_UnsargedInPlansRankedByRealRungs`,
`TestCostModel_SargedInPlanBeatsUnsargedThroughFullChain`, `TestCompareInPlan_SargedBeatsUnsarged`,
and `TestOptimizeGroup_InPlanWinnerIsInsertionOrderIndependent` (all 24 insertion orders of a
four-member group elect the same winner, through the real `OptimizeGroupTask`), plus the SQL-level
corpus file `pkg/relational/conformance/yamsql/testdata/in_plan_winner_stability.yaml`, which reaches
the both-IN arm from SQL deliberately rather than incidentally.

**Reachability and impact are measured, not assumed.** Byte-identical plans on their own are evidence
of neither reachability nor safety — an arm that is never reached also changes nothing — which is why
both were instrumented.

*Reachability.* The both-IN arm fires 265 times across the six SQL-level suites (embedded,
explaindiff, plandiff, memoinvariant, rowdiff, yamsql), and every one is the `penaltyA=1,
penaltyB=1` case where Java answers `+1` and Go answers `0`. Over one pass of the plan-shape corpus
(339 files, 2452 queries) the arm fires 27 times: `in_list_pushdown.yaml` ×14,
`in_plan_winner_stability.yaml` ×7, `in_over_intersection.yaml` ×4, `e2e_inventory.yaml` ×2.

*Impact.* Reconstructing the pair's winner both ways over those 27: **21 agree** with the pre-fix
hash tie-break and **6 flip** — `in_over_intersection.yaml` ×4 (two InJoin/InJoin, two
InUnion/InJoin) and `e2e_inventory.yaml` ×2 (InUnion/InJoin). The new corpus file contributes 0
flips. The plans nevertheless hold, for a reason that differs per file and was checked rather than
assumed:

- In `in_over_intersection` the elected plan contains **no IN operator at all** — a third candidate
  beats both members of the flipped pair (`PredicatesFilter(Fetch(Intersection(IndexScan(IDX_B,
  [=]), IndexScan(IDX_C, [=]))), [1 preds])`, with the IN left as a residual), so the pair's local
  verdict never reaches the output.
- In `e2e_inventory#4` the elected plan **is** one member of the flipped pair — the
  `InUnion(TypeFilter([STOCK], Scan(STOCK, [=])), bindings=1, ASC)` — and it is elected despite this
  rung's verdict now pointing at the InJoin. The query carries `ORDER BY product_id, warehouse_id`
  and the winner is sort-free, so the ordered-member retention path, not criterion #6, is what
  selects it.

The whole-corpus proof is independent of both explanations: regenerating the entire plan-shape
baseline with the pre-fix rung forced back on yields output **byte-identical** to the fixed rung's,
which is in turn byte-identical to the committed golden.

#### Criterion 7 — primary scan versus index scan with fetch — DELIBERATE DIVERGENCE (Java upstream defect)

**Java's rung is not transitive — not merely because it abstains on most pair shapes, but because its
verdicts on the pairs it DOES adjudicate already contain a cycle. Go ranks both sides on a scale
instead. Read-side plan choice only — nothing here touches the wire, so Java and Go still read and
write byte-identical records.**

Java (`PlanningCostModel.java`, tag 4.12.11):

- `comparePrimaryScanToIndexScan(primaryScan, indexScan, …)` (`:370`) is guarded by an applicability
  test (`:376-379`): the first side must be exactly one `RecordQueryScanPlan` with no
  `RecordQueryPlanWithIndex`, the second must have no `RecordQueryScanPlan` and must satisfy
  `isSingularIndexScanWithFetch` (`:474-478`). Outside that shape it returns `OptionalInt.empty()`
  (`:414`) and the call site (`:190-195`) falls through to the remaining rungs.
- Inside the guard there are two branches. The type-filter sub-case (`:381-406`): when the primary
  side carries a type filter and the index side none, it takes `Sets.difference` both ways
  (`:392`, `:394-395`) and returns `of(1)` — prefer the index — iff `primary − index` is empty AND
  `index − primary` is non-empty, i.e. iff the primary's comparison set is a STRICT SUBSET of the
  index's. Otherwise the config branch (`:408-412`): `PREFER_SCAN` returns `of(-1)`, the
  index-preferring configurations `of(1)`.
- `flipFlop` (`:511`) asks the reverse orientation when the first returns empty. The guard cannot hold
  in both orientations at once (one side needs `scanCount == 1`, the other `scanCount == 0`), so
  ANTISYMMETRY is fine here — unlike criterion 6. The defect is transitivity only.

**Defect 1 — pair-restriction.** A criterion that adjudicates only some pair shapes is not a total
preorder, and a lexicographic composition is transitive only if every rung is one. The failure is the
indifference relation: the rung is indifferent between two index scans it ranks on OPPOSITE sides of
the same primary scan, so a rung further down closes the ring. Writing `P` for a lone primary scan and
`I` for a singular index-scan-with-fetch:

	sargedIndex+3fetches   < primaryScan+typeFilter   the type-filter sub-case: the index SARGs
	                                                  strictly more and pays no type filter
	primaryScan+typeFilter < plainIndex+1fetch        the config branch, PREFER_SCAN (the default)
	plainIndex+1fetch      < sargedIndex+3fetches     the rung ABSTAINS for an index/index pair;
	                                                  the fetch rung decides, 2 < 4

No IN operator is involved. Over the corpus of `TestCostModel_InPlanComparisonIsOrderIndependent` the
pre-fix rung produces **69 transitivity violations at the 28 plans that corpus held when the scoping
was introduced, and 138 at the 32 plans it holds now — 100% of them involving a primary scan** in both
cases, with antisymmetry clean (0 violations) either way. That is why the sweep used to scope its
transitivity and permutation properties to the index-rooted subset.

**Defect 2 — the adjudicated verdicts are themselves cyclic**, so the repair could NOT simply extend
Java's answers to the abstained pairs. The sub-case decides on the SUBSET relation between the two
sides' comparison sets, and a subset relation is a partial order, not a total preorder. Four plans,
every consecutive comparison a `(P, I)` pair inside Java's guard (`P` sides type-filtered, `I` sides
not):

| pair | Java | why |
|---|---|---|
| `P1{a}` vs `I2{b,c}` | primary wins | `primary − index = {a}` is non-empty: sub-case skipped, `PREFER_SCAN` |
| `I2{b,c}` vs `P2{b}` | index wins | `{b} ⊊ {b,c}`: sub-case fires |
| `P2{b}` vs `I3{c,a}` | primary wins | `primary − index = {b}` is non-empty: skipped again |
| `I3{c,a}` vs `P1{a}` | index wins | `{a} ⊊ {c,a}`: sub-case fires |

`P1 < I2 < P2 < I3 < P1`. Reproduced in Go on the pre-fix rung by
`TestCriterion7_AdjudicatedCycleIsGone`. No total preorder can reproduce a cyclic relation, so any
repair necessarily changes some verdicts; the only question is which.

**Go's rung** (`comparePrimaryScanVsIndexScan` → `primaryVsIndexRankOf`) ranks each plan
independently on a ladder, lower is better, and compares the ranks. Under `PREFER_SCAN` (the Cascades
default):

| tier | class | within-tier order |
|---|---|---|
| 0 | a lone primary scan with no type filter; and every plan with no stake in the trade-off (covering index needing no fetch, multi-access plan, …) | flat |
| 1 | the contested band: a type-filtered lone primary scan, and a singular index-scan-with-fetch with no type filter | more search arguments wins; a tie goes to the primary scan |
| 2 | a singular index-scan-with-fetch that also pays a type filter | flat |
| 3 | any plan carrying an in-memory sort | flat |

The index-preferring configurations penalise the lone primary scan instead (tier 1) and leave
everything else at tier 0 — Java's sub-case is moot there because both of its branches already return
"prefer the index".

Two things change versus Java, and nothing else:

1. **The subset test becomes a size test.** Inside the contested band the index wins iff it carries
   strictly MORE distinct comparisons (`distinctSargCount`, the cardinality of the same set Java takes
   differences of). That agrees with `primary ⊊ index` on every COMPARABLE pair — a strict subset has
   strictly fewer members, equal sets have equal counts and the tie goes to the primary exactly as
   Java's empty `indexMinusPrimary` does — and differs only on INCOMPARABLE sets, which is precisely
   the configuration that makes Java cyclic.

   **The price, stated plainly.** On incomparable sets the size test is BLIND TO WHICH comparisons
   they are, so an index that is MISSING a comparison the primary scan has can still win, purely on
   count: `primary{a,b}` versus `index{c,d,e}` goes to the index (3 > 2), where Java's
   `primaryMinusIndex = {a,b}` is non-empty and it goes to the primary. Java's guard is the more
   informative test — it will not hand the win to an index that dropped a search argument the primary
   holds — and the repair gives that up. It has to: `primaryMinusIndex.isEmpty()` is a subset test,
   and the four-plan cycle above is built from nothing but subset tests, so keeping it is keeping the
   cycle. Size is the coarsest scale that reproduces Java wherever Java is self-consistent. The
   exposure is bounded by the guard the rung keeps: this only ever arises when the primary side pays
   a type-filter discard and the index side pays none. It does not arise on the corpus measured
   below — all 24 contested-band consultations carry one comparison per side.
2. **The in-memory-sort guard becomes symmetric.** Go's `ImplementInMemorySortRule` is a read-side
   extension Java's ordinal rungs never see, and Go already refused to treat an `InMemorySort(Scan)`
   as a bare primary scan. A rank has no "abstain", so a sort-bearing plan has to land somewhere;
   ranking it last reproduces the verdict of the sort-count rung immediately below, and extends the
   same guard to the index side, where an `InMemorySort(IndexScan)` could previously win here and
   pre-empt that rung.

Pinned by `TestCriterion7_AbstentionCycleIsGone` and `TestCriterion7_AdjudicatedCycleIsGone` (the two
cycles above, red on the pre-fix rung), `TestCriterion7_RankIsATotalPreorder` (brute-force
irreflexivity / antisymmetry / strict-transitivity / INDIFFERENCE-transitivity over a 12-plan corpus
covering every tier, for all three `IndexScanPreference` values),
`TestPlanningCostModel_PrimaryIndexSARGRichIndexWins` (the contested band end to end, isolated
differentially: removing the index's extra comparison flips the winner back to the primary scan),
`TestPlanningCostModel_PrimaryVsIndexRungIgnoresSortBearingPlans`, `TestDistinctSargCount`, and the
widened `TestCostModel_InPlanComparisonIsOrderIndependent`, whose transitivity and
permutation-independence sweeps now cover the WHOLE corpus, primary scans included (29,760 ordered
triples, 64 permutations, 496 pairs over 32 plans).

**Reachability and impact are measured, not assumed.**

All figures below come from one instrumented pass over the six SQL-level suites (embedded,
explaindiff, plandiff, memoinvariant, rowdiff, yamsql). They are stable to about a tenth of a percent
across runs — an independent reviewer's pass measured 362,592 consultations against the 362,879 here
(0.08%) — because memo exploration order is not bit-reproducible between processes. Treat the totals
as ±0.1%; the ZERO counts below are exact and did reproduce exactly.

*Reachability.* The rung is consulted **362,879** times. **197,807** of those are pairs the pre-fix
rung ADJUDICATED — overwhelmingly primary-versus-index with no type filter (191,735 + 6,048), plus 24
type-filtered-primary-versus-index pairs, the shape the sub-case exists for.

*Impact on the adjudicated pairs.* **Zero** of the 197,807 change verdict. The size test never
disagrees with the subset test on this corpus because the comparison sets are always comparable there
— all 24 contested-band consultations carry one comparison on each side.

*Impact on the abstained pairs.* **26,083** pairs get a rung verdict where the pre-fix rung had none:

| pairs | shape | decided the same way by |
|---|---|---|
| 10,786 | primary-with-type-filter versus primary-without | the type-filter-count rung directly below (fewer wins) |
| 14,147 | sort-bearing versus sort-free | the sort-count rung directly below (fewer wins) |
| 1,150 | index-scan-with-fetch versus covering-index-needing-no-fetch | the fetch rung below (`indexScanCount + fetchCount`, 2 against 0) |

The 1,150 are the only genuinely NEW class, and all 1,150 are the identical shape:
`I(tf=0, sarg=k, idx=1, fetch=1)` against `E(tf=0, sarg=k, cov=1, fetch=0)` — equal search arguments,
equal type filters, the covering side winning every time. Pinned by
`TestCriterion7_FetchPayingIndexLosesToCoveringIndex`, which asserts BOTH this rung's verdict and the
fetch rung's metric so the two cannot silently diverge.

The index-versus-index search-argument ordering — the one dimension that could pre-empt the
fetch-count and unmatched-field rungs below — **does not fire anywhere in the SQL corpus**: all 49,091
index/index consultations have equal search-argument counts (unequal: 0). It is emphatically NOT
unreachable in general, and is directly pinned by
`TestPlanningCostModel_SargRichIndexBeatsSargPoorIndex`, whose shapes come from a pre-existing unit
test that had to be adjusted for this change.

*Net effect on the comparator.* The decisive measurement is not the rung but the CHAIN. Evaluating
every full-comparator comparison twice in the same process — once with the pre-fix rung and once with
the ranked rung — over **967,069** comparisons yields **zero sign differences** (729,088 `+1`,
167,010 `-1`, 70,971 ties, identical both ways). Every one of the 26,083 rung-level changes is
absorbed by a rung below returning the same sign. That, not the rung's own statistics, is why the
elected plans and the stress numbers do not move.

*Elected winners.* Independently: recording the extracted plan of every planning across those six
suites — **42,560 plans** — and diffing the pre-fix run against the fixed run modulo run-to-run
correlation-identifier counters yields **zero differences**. Byte-identical output is not on its own
evidence of safety, which is why the 223,890 non-abstaining consultations and the 967,069 chain
comparisons above were counted first.

*Stress.* The 1M stress suite is unchanged: 23/23 subtests pass on both sides with identical row
counts and a byte-identical EXPLAIN set (see TODO.md's stress table).

### Cost Model: RewritingCostModelLess

Java's `RewritingCostModel.compare()` has six ordered criteria: (0) `outerJoinCount`, (1) `selectCount`, (2) `tableFunctionCount`, (3) normalized CNF conjuncts, (4) predicate-count-by-level, (5) `semanticHashCode` tie-break. Go ports all six; its outer join count counts LEFT OUTER selects, Go's `OuterJoinExpression` (`isLeftOuterJoinSelect`, rewriting_cost_model.go). The canonical form `RewriteOuterJoinRule` yields therefore survives the REWRITING prune as in Java, so `SelectMergeRule` dissolves it into its block and `PredicatePushDownRule` pushes a preserved-side conjunct into the preserved leg (`WHERE t.id = 2` over `t LEFT JOIN u` probes `t` by primary key, as Java plans it).

**Go-only: the materialized outer join.** Go plans a LEFT OUTER select as a materialized `RecordQueryNestedLoopJoinPlan` (RFC-152), which scans the null-supplying leg once where the correlated FlatMap re-scans it per preserved row; Java has only the FlatMap. `OuterJoinMaterializationRule` (PLANNING only) re-forms the LEFT OUTER select from the canonical form so both compete on cost, keeping the canonical select's own predicates (WHERE conjuncts) above it over a box quantifier. `RewriteOuterJoinRule` runs in REWRITING only, as in Java. A LEFT select survives REWRITING only where `RewriteOuterJoinRule` declines (a scalar subquery's strict edge); `PredicatePushDownRule` turns that one into an inner join under a predicate rejecting its null-extended row, which Java does through `EliminateNullOnEmptyRule` after dissolving the outer join. Pinned by `TestRewritingCostModel_PrefersCanonicalOuterJoin`, `TestRewritingBoundary_KeepsCanonicalOuterJoin` and the `TestOuterJoinMaterializationRule_*` tests.

### Properties: 19/19

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
| NormalizedResidualPredicateProperty | `countResidualPredicates()` + `normalFormSize(..., normalFormCNF)` inline in cost model | Aligned (inline) — RFC-240 replaced the negate-blind `cnfSize` |
| ExpressionDepthProperty | `expressionDepth()` inline in cost model | Aligned (inline) |
| TypeFilterCountProperty | `walkExpressionTree()` counter inline in cost model | Aligned (inline) |
| UnmatchedFieldsCountProperty | `walkExpressionTree()` counter inline in cost model | Aligned (inline) |
| ContinuableWithoutDuplicatesProperty | `plans/continuable_without_duplicates_property.go` | Invariant aligned; **false set diverges** (see below) |

#### `ContinuableWithoutDuplicatesProperty` — same invariant, empty false set

**The invariant is identical and is ported exactly:** never stream-aggregate over a plan that
can re-emit a row across a continuation. An aggregate FOLDS each input row into an accumulator,
so a row delivered twice is counted twice — it is the one consumer for which a re-emitted row is
wrong rather than merely redundant. Wired into `ImplementStreamingAggregationRule` at Java's own
filter point (`ImplementStreamingAggregationRule.java:68-78`, upstream of the ordering check at
`:118-119`).

**The FALSE SET diverges, and that is the point.** Java's visitor returns `false` for exactly two
plans — `RecordQueryUnorderedPrimaryKeyDistinctPlan` and `RecordQueryUnorderedDistinctPlan` —
because *Java's executor* rebuilds their dedup set per execution
(`RecordQueryUnorderedPrimaryKeyDistinctPlan.java:100-104` mints a fresh `HashSet` and passes the
inner's continuation through untouched), so a duplicate spanning a resume is silently re-admitted.
That is a fact about Java's executor, not about what a DISTINCT plan means. **#621 removed that
premise in Go:** the seen-set is carried across pages BY REFERENCE through the statement-scoped
`ExecutionScratch`, with an adoption/retirement lifecycle keyed on continuation nameability. Go's
counterparts are therefore continuation-safe and answer `true`, at exactly the arms Java overrides
so the divergence is recorded at the decision site. Importing Java's *conclusion* while Go had
removed its *premise* is the same error the `strictlySorted` refusal already names.

**Go's false set is consequently EMPTY**, audited rather than assumed across every plan the rule
can sit over: each either resumes positionally (scan, index, nested-loop join and flat-map restore
an exact index and verify it against a saved key), serializes its whole state into the continuation
(in-memory sort carries its remaining sorted buffer; unordered union a per-child slot; the recursive
union its temp-table frontier on every emitted row), or parks state in the scratch (the two
distincts). The admission filter is therefore **vacuously true today** and is
kept as a correct-by-construction guard; the emptiness is pinned as a negative result
(`TestContinuableWithoutDuplicates_FalseSetIsEmpty`) naming what re-arms it.

**Open, and load-bearing: Go has no hash aggregation.** Streaming aggregation is Go's only
aggregation strategy, and the rule's in-memory-sort path is a sort, not a fallback — the property's
default-from-children declines a sort over a re-emitting inner too. So if the false set ever becomes
non-empty, `GROUP BY` over the declined shape would **fail to plan** rather than fall back. A hash
aggregation has to land before anything is added to the false set. Stated at the rule, and asserted
at the point of failure by the emptiness test.

**Placement divergence:** Java files this under `cascades/properties`; Go cannot, because `plans`
imports `properties` (`cardinality_bounds.go`), so a property dispatching on plan types would be an
import cycle. It lives beside the other plan-level properties in `plans/`.

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
| ConstantFoldingValuePredicateRule | ConstantFoldingValuePredicateRule (`predicates.FoldComparisonMaybe`), and ConstantFoldingBooleanValuePredicateRule for Go's boolean ValuePredicate as `value = TRUE` | Aligned |
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

`InSourceKind` enum classifies explode values (Values/Parameter/Comparand). `classifyInSourceKind()` sets it at plan creation and the push-through-fetch preserves it. Go pushes EVERY InJoin through a fetch; Java does not push the comparand InJoin SQL IN-lists produce — see the next entry.

### Plan choice: an ordered IN over a non-covering index runs as Fetch(InJoin), Java's as Fetch(InUnion) (RFC-191)

**Divergence.** `SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a` over a non-covering index on `a`: Java elects `[IN ...] INUNION q0 -> { COVERING(IA [EQUALS q0]) } COMPARE BY (_.A, _.ID, _.K) | FETCH`; Go elects `Fetch(InJoin(IndexScan(IA, [=] COVERING), binding ASC))`. Descending, Java elects `INJOIN SORTED DESC -> { ISCAN(IA [EQUALS q0]) }` and Go the same `Fetch(InJoin(...))` with the fetch above. Rows are identical; `conformance/in_join_ordering_java_probe_test.go` measures both engines (4.14.2.0) and flags either side moving.

**Mechanism.** Both cost models prefer the plan with more in-join sources (`PlanningCostModel.java:263-273`; Go's `inJoinCount` rung in `planning_cost_model.go`), but Java reaches that rung only for DESC: for ASC its fetch-depth tiebreak decides first, because `PushInJoinThroughFetchRule` is registered for `RecordQueryInValuesJoinPlan` and `RecordQueryInParameterJoinPlan` only (`PlanningRuleSet.java:151-152`), never for the `RecordQueryInComparandJoinPlan` every SQL IN-list builds, so Java's InJoin cannot put its fetch at depth 0 while its IN-union can. Go's `PushInJoinThroughFetchRule` has no source-kind gate (`rule_push_in_join_through_fetch.go`; `TestPushInJoinThroughFetchRule_Fires` pins the comparand arm).

**Why Go keeps it.** The exclusion reads as an omission, not a design: the rule is generic over `RecordQueryInJoinPlan` and needs no change to run for comparands; the comparand class arrived nine months after the two registrations and every other visitor of the InJoin trio handles all three; nothing in the Java source justifies it. Both shapes do the same N bounded index reads and one fetch per row, and the IN-union's merge is degenerate here (leg i emits only `a = v_i`, legs visited in order). Measured on real FDB with `BenchmarkFDB_InFetch_*` (5 rows per value, 4-6 interleaved pairs per N, 2026-10-04): N=3 InUnion 0.8% faster (4/4 pairs, sign test p≈0.06), N=10 InUnion 0.6% faster (3/4), N=100 InJoin 0.8% faster (6/6, p≈0.016) — no N at which the IN-union is significantly better. Those runs predate the IN-union size check: a relational IN-union now refuses more than 24 values ("too many IN values"), so past 24 Java's choice fails where Go's InJoin answers, and the N=100 pair is now N=24.

**Wire compatibility.** A read-path plan choice only. Continuation content differs between the two plans, but a continuation never crosses engines: Go refuses a caller-supplied statement continuation outright (`cascades_generator.go`, the `OptContinuation` check in `cascadesPlan`), and the executor dispatches a continuation only to the plan shape that minted it (`UnsupportedContinuationError` on a mismatch).

**Reversal.** Registering `PushInJoinThroughFetchRule` for `RecordQueryInComparandJoinPlan` in Java closes this entry. Evidence that the exclusion is deliberate, or an `InFetch` benchmark where the IN-union wins at some N, flips Go to Java's choice.

**Reconciled with the RFC-257 acceptance.** The WS-F oracle's acceptance map (`wsfAcceptance` in `conformance/ws_f_probe_conformance_test.go`) set Java's path as the target for `w8_in_order_by_col1_explain`, `w8_in25_order_by_col1_*`, `w8_in4x6_explain`, `w8_in5x5_*` and `w8_in_no_order_explain`, owned by F-6 and F-7b. Neither step moves them. Go's covering emission already follows Java's gate (`ValueIndexScanMatchCandidate.ToScanPlan` is Java's `toEquivalentPlan`: a fetch over the covering entry reader, or the bare index scan when no reader can be built). F-7b's IN-union changes leave the in-join ahead. What separates the engines is this entry's push of a comparand in-join through its fetch, and with F-7b's size check it now separates answers too: past 24 values (or 24 combinations of two lists) Java's IN-union fails with "too many IN values", while Go's sorted in-join answers. The rows are declared `DIFF-PATH rfc-191` and `DIFF`, and the Reversal criteria above still decide.

### Plan choice: an index read past the record-type coordinate serves ORDER BY the primary key (RFC-257 WS-F 4.3)

**Divergence.** `SELECT * FROM T1 WHERE col1 = 10 ORDER BY id` over a one-column index `I1 (col1)` of a relational table: Java plans `SCAN([IS T1]) | FILTER _.COL1 EQUALS ...`, its only root member (`w8_root_eq_order_by_id_*`); Go plans `IndexScan(I1, [=])`. Rows are identical (`w8_eq_order_by_id_rows`).

**Mechanism.** A relational table's primary key begins with the record-type key, so an I1 entry is `(COL1, record type, ID)`. Java's data-access rule checks a match against the requested ordering over its matched ordering parts (`AbstractDataAccessRule.satisfiesRequestedOrdering`, `:779-845`), and at match time the record-type part carries no comparison: the check stops there and ID is never reached. Its plan-ordering computation binds the same part as an implicit equality (`computeEqualityBoundImplicitOrderingParts`), so the target disagrees with itself; its intersection ordering also treats the part as equality-bound (`AbstractDataAccessRule.java:1060-1093`). Go's candidate continues into the primary-key suffix (`ValueIndexScanMatchCandidate.ComputeMatchedOrderingParts`, pinned by `pk_suffix_ordering_test.go`). For a record-layer type whose primary key has no record-type key both engines reach the suffix (`w8_rl_default_in2_order_by_pk`).

**Why Go keeps it.** The read is correct for a single-type index, it touches no stored byte, and it lets Go read an index prefix where Java scans the table: "ORDER BY PK + index filter" (`SELECT id, amount FROM orders WHERE customer_id = 0 ORDER BY id`) is one of the 1M stress queries, and the target's stop would make it a full scan. Precondition: a primary key that begins with the record-type key, read through a single-type index.

**Where it stops.** The extension must not create IN-unions the target never builds, because the target fails an IN-union over more values than its configured size where it answers the scan. The index plan's rich ordering marks each primary-key key it reaches only past the record-type coordinate (`RichOrdering.WithPastRecordTypeHorizon`, from `RecordQueryIndexPlan.HintRichOrdering`). Fetches, filters and maps pass the mark along with the ordering, and a merge of legs drops it. Only `ImplementInUnionRule` reads it. It builds no in-union for a request naming a marked key, while a marked key may still be a free comparison-key suffix. An index that names `id` in its own key reaches it through a key part and keeps the in-union in both engines (`w8_explicit_id_in_order_by_id_*`). Pins: `in_union_record_type_horizon.yaml`, `TestImplementInUnionRule_RecordTypeHorizon`, `TestRichOrdering_RecordTypeHorizon`. `col1 IN (10, 20) ORDER BY id` is now `InMemorySort(Fetch(InJoin(...)))` in Go and `SCAN | FILTER` in Java; see the next entry.

**Upstream.** Reporting the target's inconsistency is an owner decision and is not filed from here.

### Plan choice: an IN ordered by a key no probe provides is probed and sorted (RFC-257 WS-F F-7c)

**Divergence.** `SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY id` over an index `I1 (col1)`: Java plans `SCAN([IS T1]) | FILTER _.COL1 IN ...`, Go `InMemorySort([ID ASC], Fetch(InJoin(IndexScan(I1, [=] COVERING))))`; likewise with 25 values and over `T5` (`w8_in_union_explain`, `w8_in25_order_by_id_explain`, `w8_tie_in_order_by_id_explain`, declared `DIFF-PATH in-memory-sort`). Rows are identical.

**Mechanism.** Java's planner has no in-memory sort: an ORDER BY is satisfied by an order-providing plan or not at all, and the only plan ordered by `id` here is the primary scan with the IN as a residual (its root has no other member). Go's `RecordQueryInMemorySortPlan` lets the IN probes, sorted afterwards, compete with that scan, and the cost model ranks the probes (one data access on the index's search arguments against a full scan with a residual).

**Why Go keeps it.** The probes read the matching entries only; the scan reads the table. Emulating Java by ranking the in-memory-sort count first was measured and reverted (2026-10-06): it flips 93 corpus plans, several clearly worse (`cte.yaml#20` runs a streaming aggregate per row of a full scan; `comma_join_exists.yaml#2` reverses the driving table). The single-element IN collapse stays with this (WS-E section 4 step 5): without it the one-value form becomes the same sorted probe.

**Reversal.** A Java planner with an in-memory sort, or a measurement in which the ordered scan beats the sorted probes for these shapes, moves Go to Java's plan through the cost model, not by removing the sort.

### Plan choice: a one-value IN is an equality (RFC-257 WS-E section 4 step 5)

**Divergence.** Go rewrites `x IN (v)` (and a list whose values are all equal) to `x = v` before planning (`rule_in_to_explode.go`, the single-element arm); Java keeps a one-value IN-join over the same probe. On the record layer, `price IN (10) ORDER BY price` is Java's `[10 SORTED] | INJOIN q -> { COVERING(wsf_price [EQUALS q]) } | FETCH` and Go's `IndexScan(wsf_price, [=])` (`w8_rl_default_in1_order_by_price`, declared `DIFF-PATH single-element-in`); the same ids. Go's EXPLAIN shows `[=]` where Java's shows an IN source.

**Why Go keeps it.** The WS-E design deletes the collapse as Go-only. Deleting it was measured twice (on F-7b's tree and after F-7c) and reverted: 13 to 15 corpus plans move, and the one-value IN ordered by the primary key goes from a streaming equality probe to `InMemorySort(InJoin(...))`, because Go's cost model keeps the sorted probes for an IN ordered by a key no probe provides (previous entry). The equality reads the same entries in the order the query asks for.

**Reversal.** Binding a one-value in-join's value as a FIXED ordering part (a Go extension needing an owner decision), or the sorted-probe entry above reversing, lets the collapse go.

### IN-union size check: Go's product of IN-list sizes does not wrap

**Divergence.** Both engines refuse an IN-union whose number of child executions, the product of its IN lists' sizes, exceeds the plan's maximum ("too many IN values", XXXXX; 24 in the relational configuration, `PlannerConfiguration.java:161`). Java multiplies in an `int` (`RecordQueryInUnionPlan.getValuesSize`, `:331-337`), so two lists of 65536 values multiply to 0 and the union answers as if a list were empty. Go's product saturates at `MaxInt64` and refuses (`inUnionValuesSize`, `TestInUnionValuesSize`).

**Why Go keeps it.** Java's answer there is wrong rows (none), reached only past 2^32 combinations; refusing is the check's own intent. No stored byte is involved.

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

### Covering Index Scan — RESOLVED

**Status:** Covering index works end-to-end for SQL. The value candidate builds its logical record from the entry (`computeIndexEntryToLogicalRecord`, the three `ExtractFromIndexKeyValueRuleSet` rules, `IndexEntryToRecordValueHelper`); the data-access rule builds the covering plan only when it does (`wrapScanPlanWithCoverage`, Java `ValueIndexScanMatchCandidate.tryFetchCoveringIndexScan`), the covering cursor fills rows from that reader, and `PushMapThroughFetchRule` eliminates the fetch when the block's Map reads only fields the record covers, as Java's push-through-fetch rules do. Verified with planner harness tests: `CoveringCompositeIndex`, `CoveringCompositeIndexPKAndIndexCols`, `NonCoveringNeedsExtraColumn`.

## Optimization-Quality Gaps (correctness unaffected)

| Gap | Status |
|---|---|
| CollapseRecordConstructorOverFieldsToStar | Blocked: needs field-level type metadata (ordinal positions) |

### A nested EXISTS inside a JOIN ON of a correlated EXISTS (Go refuses)

`… WHERE EXISTS (SELECT 1 FROM a JOIN b ON EXISTS (SELECT 1 FROM g WHERE g.c = o.c))`:
Java answers; Go refuses with 0A000 "a nested subquery inside a JOIN ON clause is not
supported". The correlated EXISTS lowering keeps a join's ON on the join node, which carries
no existential edges, and lifting the ON to the EXISTS level would drop the join's
emptiness (an empty `a JOIN b` must answer false). A correlated conjunct in an OUTER join's
ON, or in an inner ON before a later RIGHT join, is answered as Java does: it stays in its
ON. Pinned by `ExistsInnerShadowJavaProbe` (`on_nested_exists` asserts the refusal and
Java's [2]) and `TestFDB_CorrelatedExistsNestedSubqueryInOnDeclines`.

### An aggregate in QUALIFY (Java fails, Go answers)

`SELECT a2, COUNT(*) FROM a GROUP BY a2 QUALIFY COUNT(*) > 1`: Java fails with an internal error
(XXXXX); Go evaluates the QUALIFY as a HAVING conjunct over the aggregate's output, which is
where Java places QUALIFY in an aggregated block (QueryVisitor.visitSimpleTable), and answers.
QUALIFY over grouping keys, with or without HAVING, answers as Java
(`CorrelatedHavingExistsJavaProbe`).

### An EXISTS in an aggregated block's QUALIFY (Go refuses)

`SELECT b1 FROM b GROUP BY b1 QUALIFY EXISTS (SELECT a1 FROM a)`: Java conjoins QUALIFY after its
grouping check and answers; Go cannot plan an existential over the aggregate's output and refuses
with 0AF00. An EXISTS in HAVING is 42803 in both (`CorrelatedHavingExistsJavaProbe`).

### A LEFT JOIN over a lateral unnest (Go refuses)

`SELECT … FROM t1, t1.arr AS r LEFT JOIN t2 ON r = t2.id`: Java answers; Go fails with 0AF00
"lateral unnest did not ordinalize". The unnest's ordinal seed cannot sit on the preserved side
of an outer join. An INNER join after the unnest, and a LEFT JOIN before it, answer as Java
(`CommaJoinJavaProbe`). TODO.md "An unnest leg before an outer join".

## Go-Only Extensions (features Java 4.12.11 rejects)

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
| DATE and TIMESTAMP values | No temporal types: a `DATE`/`TIMESTAMP` column is 42F18, `CAST(… AS DATE/TIMESTAMP)` is 42601 | Types of VALUES only (CAST, `CURRENT_DATE`/`CURRENT_TIMESTAMP`, a bound `time.Time`), carried as canonical UTC text in 0000-9999 whose order is the instant order. A DATE meeting a TIMESTAMP (comparison, IN, CASE, COALESCE, IFNULL, GREATEST, LEAST) is promoted to its midnight. A column spelled DATE/TIMESTAMP is STRING (the type is not persisted, so catalog bytes stay Java's), and a value against it compares text. Pinned by `temporal_promotion.yaml` and `expr/temporal_promotion_property_test.go` |
| Simple CASE (`CASE expr WHEN val`) | `visitChildren` no-op (always falls through to ELSE) | Correct evaluation |
| Symbolic logical operators (`&&`, `\|\|`) | `SqlFunctionCatalogImpl` only registers `and`/`or`; symbolic forms throw UNSUPPORTED_QUERY | Evaluated as AND/OR |
| `XOR` operator | Not registered in `SqlFunctionCatalogImpl`; throws UNSUPPORTED_QUERY | SQL-standard XOR with NULL propagation |
| Scalar subqueries in expressions | Grammar has no `subqueryExpressionAtom` (parse error) | Translated via `ScalarSubqueryValue` (`DecorrelateValuesRule` covers the other values-box patterns) |
| Direct-API insert of a bare `UUID ARRAY` | `RecordTypeTable.toDynamicMessage` (4.14.2.0) converts a UUID attribute (#4243) but its repeated-field path calls `addRepeatedField` with the unconverted `java.util.UUID`, which protobuf refuses for the UUID message field (read from source, not measured) | Each element is written as the two-word UUID message (`embedded/direct_access.go` `directFieldValue`); UUIDs inside structs in an array match Java |

Go-only plan types: `RecordQueryInMemorySortPlan`, `RecordQueryLimitPlan`, `RecordQueryValuesPlan`, `RecordQueryNestedLoopJoinPlan`. `RecordQueryMergeSortUnionPlan` is Go's collapsed ordered-union counterpart, not a semantic extension; its `removeDuplicates=false` mode is an extension. Go also has a keyless concat shape named `RecordQueryUnionPlan`; Java's same-named class is keyed and ordered, so the Go shape—not the class name—is the extension.

Go-only logical expressions: `LogicalLimitExpression`, `LogicalValuesExpression`.

### Go-only SQLSTATEs

`pkg/relational/api/errcode.go` tracks Java's `ErrorCode` enum: **every one of Java's 77 codes (4.14.2.0) exists in Go** — that direction has no gap. Go defines seven codes Java has no member for:

| Code | Go constant | Why it exists |
|---|---|---|
| `21000` | `ErrCodeCardinalityViolation` | SQL-standard class 21. Java's enum has no class-21 member at all, and its relational layer has no scalar-subquery cardinality check, so nothing there reports "subquery returned more than one row". Go enforces the cardinality and needed a code to name the violation. |
| `22003` | `ErrCodeNumericValueOutOfRange` | SQL-standard code for arithmetic overflow. Java's enum has no member for it; overflow there escapes as a raw Java exception and reaches `ExceptionUtil`'s final fallthrough, reported as `UNKNOWN`. |
| `22008` | `ErrCodeDatetimeFieldOverflow` | A bound `time.Time` whose UTC year is outside 0000-9999 (an array element included). Its canonical TIMESTAMP text would neither parse back nor sort by instant. DATE and TIMESTAMP values are a Go extension; Java has no temporal type, so the condition cannot arise there. |
| `22012` | `ErrCodeDivisionByZero` | SQL-standard code for division by zero. Java's enum has no member for it; the operation raises `java.lang.ArithmeticException`, which is not a `RecordCoreException`, so `ExceptionUtil.toRelationalException` reports `UNKNOWN`. |
| `22021` | `ErrCodeCharacterNotInRepertoire` | SQL text or a string parameter that is not valid UTF-8. A Java `String` is UTF-16 and cannot hold such text, so the condition cannot arise there; Go refuses it before lexing or binding instead of replacing the bytes with U+FFFD, and the record layer refuses to save such a string (`InvalidUTF8StringError`). |
| `40003` | `ErrCodeStatementCompletionUnknown` | FDB `commit_unknown_result` (1021) on a non-retrying explicit transaction (RFC-198 Decision 7). Java has no member: 1021 falls to `isRetryable`'s default and `ExceptionUtil` reports `UNKNOWN`. |
| `54F02` | `ErrCodePlanComplexityLimitReached` | An exhausted planner budget, set through a Go-only `MAX_*` connection option; Java's SQL layer exposes and enables none of its planner caps, so the condition cannot arise there. Detailed below. |

The first three predate this list; it was written when a claim that there was only one Go-only code turned out to be false. The list is not maintained by prose: `goOnlyErrorCodes` in `errcode.go` carries the same seven with their reasons, and `TestErrorCodesMatchJava` diffs the Go enum against a captured snapshot of Java's, failing if the difference and the list disagree in either direction — a new unlisted Go-only code, a stale entry, or a Java code missing from Go. Regenerate the snapshot on a Java version bump using the command in its doc comment.

#### `54F02` in detail

Java's Cascades planner defines three complexity caps — `maxTotalTaskCount`, `maxTaskQueueSize`, `maxNumMatchesPerRuleCall` — and throws `RecordQueryPlanComplexityException` from `CascadesPlanner.java:456`, `:501`, and `:1081` when one trips. All three guards are gated on a positive bound (`CascadesPlanner.java:334-344`); the proto fields have no default (`record_planner_config.proto:53-55`), so each is `0`, documented as "unbound" (`RecordQueryPlannerConfiguration.java:243,251`). **Java's SQL layer never enables any of them**: `PlannerConfiguration.buildRecordQueryPlannerConfiguration` (`fdb-relational-core/.../query/PlannerConfiguration.java:158-169`) sets index scan preference, in-join union size, index fetch method, disabled rules, right-deep joins and the vector engine preference, and none of the three cap setters, and no relational `Options.Name` reaches them.

Go matches those defaults: the planner is unbounded unless configured (`Planner.MaxTasks`, `MaxTaskQueueSize`, `MaxNumMatchesPerRuleCall` all default to 0), the guards trip at Java's points (a task bound N runs N+1 tasks; the queue is checked after each task), and planning is bounded in time only by the caller's context. A Go connection may opt into the caps with the Go-only options `MAX_TOTAL_TASK_COUNT`, `MAX_TASK_QUEUE_SIZE` and `MAX_NUM_MATCHES_PER_RULE_CALL`. Exhausting one is therefore a condition no Java SQL user can reach, with no shared surface to conform to.

Porting Java's mapping would have been wrong twice over: `ExceptionUtil.recordCoreToRelationalException` (`ExceptionUtil.java:59`) matches `RecordQueryPlanComplexityException` against no `instanceof` arm, so it would land on `ErrorCode.UNKNOWN` (`XXXXX`) — a code Java's own javadoc says "shouldn't be used in general" (`ErrorCode.java:173-175`) — reached only through a path Java's SQL layer never walks.

Class 54 ("program limit exceeded") is the honest class: the planner gave up because the query exceeded a configured budget, and simplifying it or raising the budget is a real remedy. `54F01` (`EXECUTION_LIMIT_REACHED`) is deliberately NOT reused — it means an *execution*-time limit and is tied to a scan/row continuation reason, so conflating plan-time with it would corrupt a meaning Java owns. `54F02` is unused in Java's enum, leaving room for Java to adopt it should it ever enable the caps.

The Go error carries the same context Java's `addLogInfo` attaches (`max_task_count`/`task_count` and the queue and rule-match equivalents) via `cascades.PlannerBudgetExceededError`.

## Java Upstream Bugs (Go is correct, Java is wrong)

Confirmed via cross-engine probes. Go's correct behavior is pinned in Go-only positive tests; corpus entries omitted until Java upstream fixes.

| Bug | Go behavior | Java behavior |
|---|---|---|
| Compound DISTINCT (`SELECT DISTINCT a, b`) | Correctly deduplicates | Fails to dedup (returns all rows) |
| Signed-zero comparison (`WHERE v >= 0.0` with `-0.0`) | Keeps row (IEEE 754: `-0.0 == +0.0`) | Drops the row |
| Signed-zero equality (`WHERE v = 0.0` with `-0.0`) | Keeps row (IEEE) | Drops the row — see below |
| NaN self-equality (`WHERE v = v` with `NaN`) | **TRUE — Go MATCHES Java here, and both diverge from the SQL standard.** An earlier revision of this row claimed Go returned FALSE "(IEEE, SQL standard)". That was asserted, never measured, and is wrong — see the NaN section below. | **TRUE** |
| `SELECT v = 0.0` vs `WHERE v = 0.0` on the same `-0.0` | Agree (both IEEE) | **Contradict each other** — see below |
| UNION ALL outer ORDER BY | Deterministic sorted output | Intermittent ordering |
| `WHERE pk_col = nonpk_col` | SQL-correct | `Missing binding` planner error |
| PK-intersection whose legs fix DIFFERENT primary-key components (`PRIMARY KEY (pk1, pk2)`, indexes `(b, pk1)` and `(pk2)`, `WHERE b = 1 AND pk2 = 3`) | Intersects on `(pk1, pk2)`, the order both legs deliver; correct rows (RFC-245 declined the merge, RFC-247 widened the key) | Intersects on `COMPARE BY (_.PK1)` and returns every `pk2 = 3` record regardless of `b` (`COUNT(*)` 4 for a 1-row answer) — see below |
| IN-union over a projection that drops the primary key (`SELECT s, b … WHERE a IN (1, 2) ORDER BY s, b`, index `(a, s, b)`) | Merges only on a key that identifies rows; otherwise sorts. Correct rows | Merges `COMPARE BY (_.S, _.B)` and drops records that tie on `(s, b)` across IN branches — see below |
| EXISTS in a disjunction of a join condition (`ON (c.a_id = a.id AND EXISTS (…)) OR c.id > 100`) | Reapplies the existential predicate; correct rows | Drops it when the select carrying it is matched to an index: extra rows in the ON form, none in the WHERE form — see below |

4.12.11 fixed three former entries, now removed from this table — they run as plain cross-engine
equivalence in the corpus: PK literal-eq AND join predicate (`pk_literal_eq_in_join`) and 3-way join
shared driver key (`three_way_join_shared_driver`), both fixed by 4.12's "planner no longer drops
ANDed predicates" change; and `WHERE TRUE AND val > 5`, now planned by 4.12 (boolean literals in
WHERE, added in the 4.12 line — see `join-tests.yamsql` `WHERE TRUE`/`WHERE FALSE`). The former
`bare_bool_where_rejected` Go-side gap is now CLOSED — Go supports bare boolean WHERE forms
(`WHERE TRUE`, `WHERE FALSE`, `WHERE bool_col`, `WHERE NOT bool_col`, and combinations with column
predicates), verified 2026-06-28 and pinned by `bare_bool_where_probe_test.go` (literal forms) plus the
corpus `bare_bool_where` (`WHERE flag`). The remaining `WHERE pk_col = nonpk_col` "Missing binding" entry stays as not-yet-
fixed in 4.12: the corpus keeps that probe deliberately omitted (column-self-equality), so the live
4.12.11 run neither confirms a fix nor pins the divergence — it is retained on the not-yet-fixed
side per the corpus's omit comment.

### PK-intersection comparison key: the soundness proof is per leg, not over the union of legs

**Java** (`AbstractDataAccessRule.isCompatibleComparisonKey`, called from
`WithPrimaryKeyDataAccessRule.createIntersectionAndCompensation`) accepts a comparison key when
it contains every primary-key component that is not in `equalityBoundKeyValues` — the UNION of
every leg's equality-bound matched ordering parts. A component fixed in ONE leg is thereby dropped
from the requirement for ALL legs. Over `PRIMARY KEY (pk1, pk2)` with indexes `(b, pk1)` and
`(pk2)`, `WHERE b = 1 AND pk2 = 3` merges the two covering scans on `(pk1)`: the `(pk2)` leg is a
single record per pk1, but the `(b, pk1)` leg carries several records per pk1 differing only in
pk2, so "equal comparison keys" no longer means "the same record" and the merge emits records the
other leg never matched. Measured on 4.12.11
(`conformance/pk_intersection_leg_bound_key_java_probe_test.go`): plan
`COVERING(TI_PK2 [EQUALS …]) ∩ COVERING(TI_B_PK1 [EQUALS …]) COMPARE BY (_.PK1)`, four rows and
`COUNT(*) = 4` where the answer is the single record `(3, 3)`. With an `ORDER BY` Java picks the
covering scan + residual filter and is correct — that arm is the probe's control.

**Go** (`intersector_primary_key.go`, `primaryKeyComponentsToCompare`) states the proof as its
real invariant: a primary-key component may leave the comparison key only when EVERY leg fixes it
AND every leg fixes it to the same comparison; every other component must be compared. A
partition where every leg fixes the same component to the same constant (indexes `(a, pk2)` and
`(b, pk2)`, both bound on `pk2 = 3`) still intersects on `(pk1)`. For the shape above Go compares
on `(pk1, pk2)` — the order both legs deliver, the `(pk2)` leg trivially — a merge Java cannot
express soundly (RFC-247; RFC-245 declined it). Legs that fix a component to DIFFERENT constants
compare on it too, so the merge finds nothing, correctly (RFC-245's proof let that key through;
found and fixed in RFC-247). Go used to port Java's union and returned the `(b, pk1)` leg's four
records. Pinned by `TestFDB_PkIntersectionLegBoundComponent` (rows, and the merge per query), the
`intersector_leg_bound_pk_test.go` unit arms (widen / accept / different constants / three-way /
two declines / the vacuous direction claim), corpus entry
`pk_intersection_leg_bound_component_count` (`DivergenceJavaWrongRowsGoCorrect`), and
`TestFDB_MetamorphicCompositePrimaryKey` (the composite-PK axis of the indexed/unindexed twin, which
found it). Booked in TODO.md section 9 for the upstream report.

### IN-union comparison key: the merge's dedup must not fire on the join it implements

**Java** `ImplementInUnionRule` yields a `RecordQueryInUnionPlan` for every comparison key the
inner's ordering admits, and `UnionCursor` drops each row whose key ties one already emitted. Over
an inner that projects the primary key away (`MAP (_.S AS S, _.B AS B)` over a covering `(a, s, b)`
scan) the key `(s, b)` ties distinct records of different IN branches, and Java returns one of
them. `Ordering.pullUp` keeps `isDistinct` through that projection, though no rule reads it here.
Measured on 4.14.2.0 (`conformance/in_union_projection_dedup_java_probe_test.go`).

**Go** (`rule_implement_in_union.go`, `inUnionMergeKeyIdentifiesRows`) bakes the merge only when
the pinned inner's ordering still holds a claim whose coordinates all lie in the comparison key:
a distinctness claim, storage-key completeness, or the record-identity claim an index scan stamps
over its primary-key coordinates (`RichOrdering.RowsIdentifiedBy`). Each claim is coordinate-bound
and dropped by a projection that loses one of its coordinates. A per-stream claim is sound across
branches because its explode-bound coordinates are in the key; record identity is a key across all
of them. The InUnion's own output claims distinctness over its comparison key, which its dedup
guarantees, so a nested IN-union still merges. Otherwise the query sorts. Pinned by
`TestFDB_InUnionMergeKeyMustIdentifyRows` and `TestRichOrdering_RowsIdentifiedBy`; found by
`TestFDB_MetamorphicCompositePrimaryKey`. Booked in TODO.md section 9 for the upstream report.

### Existential predicate for an outer existential: reapplied, not dropped

**Java** `ExistentialValuePredicate.computeCompensationFunction` (`:77-88`) returns
`noCompensationNeeded()` when the predicate's existential is not a quantifier of the matched
select. A disjunction containing an EXISTS stays in the lower select of a join, correlated to the
existential one level up, so matching that select to an index silently removes the predicate.
Measured on 4.14.2.0 (`conformance/exists_under_or_java_probe_test.go`): the ON form returns a row
whose EXISTS is false, the WHERE forms return nothing.

**Go** (`select_subsumption_predicates.go`, `selectSubsumptionExistentialPredicateCompensation`)
reapplies the predicate in that case: no other select applies it, and over the outer row it is an
ordinary residual. Pinned by `TestFDB_ExistsInOn` and
`select_subsumption_existential_compensation_test.go`. Booked in TODO.md section 9 for the
upstream report.

## Plan Architecture: Go collapses Java class hierarchies

| Java | Go | Planning status |
|---|---|---|
| 3 InJoin subclasses | 1 `RecordQueryInJoinPlan` with `InSourceKind` | Aligned |
| 2 InUnion subclasses | 1 `RecordQueryInUnionPlan` | Aligned |
| 2 ordered Union subclasses | `RecordQueryMergeSortUnionPlan` | Aligned for Java's deduplicating mode; Go additionally supports ordered UNION ALL |
| `RecordQueryUnorderedUnionPlan` | `RecordQueryUnorderedUnionPlan` plus extra keyless `RecordQueryUnionPlan` | Java-aligned unordered plan plus a duplicate Go concat implementation; cleanup tracked by RFC-190 |
| 2 Distinct plan variants | 1 `RecordQueryDistinctPlan` | Aligned |
| CoveringIndexPlan | `RecordQueryCoveringIndexPlan` holding `*RecordQueryIndexPlan` as a FIELD | Aligned (RFC-220) — no longer collapsed |
| CountValue + NumericAggregationValue | `AggregateValue` | Aligned (no rule distinguishes them) |
| VariadicFunctionValue | `ScalarFunctionValue` | Aligned (COALESCE folding matches Java) |
| 12 Comparison subclasses | Single `Comparison` struct with optional fields | Aligned |

## RFC-220 — the result over a covering scan — RESOLVED

Java's `MergeProjectionAndFetchRule` drops a `LogicalProjectionExpression` over a
covering fetch, but fdb-relational never builds that expression: a SQL query block
is a `SelectExpression`, and `PushMapThroughFetchRule` keeps the block's Map over
the covering plan (`RecordQueryMapPlan` over the fetch's child). Go builds blocks
the same way and has no projection expression, so the covering plan reads
`Map(IndexScan(… COVERING))` in both engines.

## DML statement-layer routing (RFC-035)

All DML (INSERT VALUES/SELECT, UPDATE, DELETE) plans and executes through the
single Cascades path (planDML), matching Java's PlanGenerator.getPlan. One
intentional divergence at the statement layer:

| Aspect | Java | Go |
|---|---|---|
| DML via the rows-returning method (`executeQuery` / `Query`) | Executes the DML, counts rows, then throws 02F01 "does not return result set" — the mutation still happens | Rejects with the same 02F01 **before** executing; no mutation |
| A row-returning statement (SELECT, UPDATE/DELETE … RETURNING) via `executeUpdate` / `Exec` | Executes it, then throws 42F61 "returns a result set" — a RETURNING mutation still happens | Rejects with the same 42F61 **before** executing; no mutation |

Go rejects up front to avoid a surprise write on a misused method; the plan
path is identical to Java, only the execute-then-throw side effect differs.

## Pure-Go FDB client (`pkg/fdbgo`) — deliberate divergences from `libfdb_c` 7.3.77

**Client option behaviour** (honored / `UnsupportedOptionError` / accepted-and-ignored) is documented
option-by-option, with the `libfdb_c` C++ reference for each, in
[`pkg/fdbgo/fdb/OPTIONS.md`](pkg/fdbgo/fdb/OPTIONS.md) (RFC-133).

### Collation keys are not ICU's (pre-existing; target ICU 78.3)

A `collate_jre` / `collate_icu` function key expression writes `golang.org/x/text/collate` (CLDR)
sort keys (`pkg/recordlayer/collate_function_key_expression.go`). Java writes
`java.text.CollationKey` or ICU `CollationKey.toByteArray()` bytes, and Java 4.14.2.0 moved its ICU
module from 69.1 to 78.3, whose keys are again version-specific. Go orders and reads its own
collated indexes correctly, but a collated index is not shared with Java: neither side can read
the other's index entries. No ICU byte baseline is checked in; matching it would mean porting
ICU's collation and sort-key format at 78.3.

### Client knobs (Java 4.14 #4488)

`FDBDatabaseFactory.SetKnob` / `SetKnobByName` validate and record a knob as Java's factory does, and
set it (`knob` network option, `name=value`) before the factory's first open, or at once after it.
On the libfdb_c backend that is Java's behaviour. The pure-Go client has no knob table: a recorded
knob fails the open with `fdbclient.UnsupportedKnobError` (2006, invalid_option_value) instead of
being silently ignored.

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
| **Scalar cost fallback + Go-only tiebreakers** (15b `compareFlatMapVsNLJ`, 15c `EstimateCostWith`) — Go's PLANNING member list has ties Java's prune-to-1-winner avoids | Structural single-winner selection; total-order tie resolution | nondeterministic / wrong index pick | **PARTIAL** — cost ORDERING pinned by WS-4 `TestBoundSelectivity_CostMonotonicity` (#405 class); equality-tie determinism pinned by `TestPlanDeterminism_*` (#409). **TRACKED:** the InJoin inner correlated-equality tie (WS-4 #2, OPEN — RFC-167 Phase 1b). |
| **Hand-rolled `AggregateDataAccessRule`** (aggregate-index matching, not Java's generic data-access) | Guard(match)==consumer(build/execute) — one classifier | wrong agg result / wrong index match (COUNT-COL class) | **COVERED for the known drift** — WS-3 `expressions.IsCountStar` is the single source of truth for the planner candidate + the executor group cursors (#413); group-key matcher deduped via `groupColEqualityIndex` (RFC-163). **RESOLVED (RFC-242):** the translator's own count-star normalization (`aggregateNamesStableForUnion`) was deleted with the union join-leg gate it served, so `IsCountStar` is the one classifier; the `COUNT(NULL)` fold fidelity question stays with it. |
| **`WithPrimaryKeyIntersector` was forward-PK-only and discarded `requestedOrderings`** (vs Java's rich common-ordering gate) | Every intersection leg shares the directional comparison ordering consumed by the sorted merge | wrong rows if reverse were widened piecemeal; safe misses for unsupported key encodings | **RESOLVED (RFC-190.5b):** rich common-order derivation, translated requests, fixed-binding dependency normalization, free-PK compatibility, redundancy proof, directional parts, reverse plan identity/execution/ordering/rewrites, and fan-out-leg PK distinct are one atomic path. Natural flat all-ASC/all-DESC keys execute; mixed/counterflow, ordered-bytes, non-flat, ambiguous-layout, stale-ordinal, and mixed structural/name-only PK-provider shapes decline safely. Multi-type layout remains separately tracked below. |
| **Merge UNIONs (`InUnion`, `MergeSortUnion`) decline a MIXED-direction comparison key** | Java encodes direction and NULL placement into the physical key with `ToOrderedBytesValue` (`ProvidedOrderingPart.comparisonKeyValue`), so it can merge on `(a DESC, b ASC)` | plan-space narrowing only — never wrong rows; the request falls back to an in-memory sort | **TRACKED, fail-closed.** Go's `ToOrderedBytesValue` has no evaluator, so the executable comparison key is the raw Value and a merge runs in exactly one direction. Both merge-union rules now gate on `properties.NaturalComparisonKeyValues(parts, isReverse)` — the same gate the intersection plans already used — and decline any candidate whose parts disagree with the resolved direction, instead of building a plan whose merge front compares one key the wrong way round. Newly REACHABLE rather than newly introduced: until descending merges were enumerated at all, every comparison key was ascending. **Measured** over the 2834-query corpus: the IN-union gate declines 6 of 424 candidate evaluations, each a two-part key mixing ASC and DESC; the merge-sort-union gate declines none of the 15 candidate evaluations that reach it. Both gates are reachable — the union merge keeps an ID the legs bind to different constants as a key the request gives a direction to, as Java's union merge or-s those bindings — and both are pinned at RULE level, by `TestInUnionRuleRefusesMixedDirectionMerge` and `TestDistinctUnionRuleRefusesMixedDirectionMerge`, each verified red with its gate removed — the corpus scenarios do NOT pin it, since the mixed-direction shapes plan an in-memory sort with or without the gate. Closing it means porting the `ToOrderedBytesValue` evaluator, which also closes the counterflow-NULLS decline in `EnumerateSatisfyingComparisonKeyValues`. |
| **`PrimaryScanMatchCandidate.ComputeMatchedOrderingParts` SKIPS two branches of Java's shared implementation, and softens a third from an assert to a decline** | `ValueIndexLikeMatchCandidate.computeMatchedOrderingParts` (`ValueIndexLikeMatchCandidate.java:63-118`) hard-asserts `Verify.verify(ordinalInCandidate >= 0)` on an unknown sort parameter, branches on `normalizedKeyExpression.createsDuplicates()`, and de-duplicates emitted ordering VALUES through a `normalizedValues` set | plan-space narrowing only, never wrong rows | **TRACKED, all three deliberate.** (a) A primary key is a flat list of scalar columns, so `createsDuplicates()` is false at every position and the fan-out branch has nothing to act on. (b) With no fan-out and distinct key columns, the `normalizedValues` dedup can never fire. (c) Java ASSERTS an unknown sort parameter is impossible; Go ends the reported prefix instead (`primary_scan_match_candidate.go`, pinned by `TestPrimaryScanMatchCandidateStopsAtUnknownParameter`). Java's assert is the stronger statement and Go should eventually match it, but a panic in library code is forbidden by design principle 4, and truncating is the fail-closed reading — it can only cost an access path, never claim an order the records lack. Revisit (a) and (b) together if a primary key ever gains a fan-out key expression. |
| **Cross-candidate PK-intersection pruning retains singleton alternatives** | Java builds compensated singles and all intersections in one shared `IntersectionInfo` map, evicts immediate subpartitions only after a useful replacement, then yields survivors once | safe plan-space widening / possible winner difference, never missing or wrong rows | **TRACKED SAFE RESIDUAL (RFC-190.5b review):** Go first yields candidate-local accesses, then its separate cross-candidate pass builds a private eviction map. It correctly prunes smaller intersection expressions internally but cannot retract already-yielded exact singleton scans. Do not delete memo members post hoc. Closing this requires a partition-level assembler that creates singles once, shares the map with intersection enumeration, and flattens survivors before any yield; regressions must preserve unrelated finals and safe logical compensations. |
| **Per-wrapper relink** (RFC-070 nil-inner shells across ~20 wrappers, vs Java's eager `memoizePlan` to concrete) | Every non-leaf plan has its child; deterministic relink to the cost winner; one final member | dropped/nil child (0 rows); nondeterministic plan/cache | **CLOSED for the shell half (RFC-183).** Rules now bake the concrete child at rule time, matching Java's memoizePlan-as-constructor-argument; `verifyChildrenMemoized` rejects a holed expression at yield, always-on (ports `CascadesRuleCall.verifyChildrenMemoized`); the ~600 lines of repair machinery are deleted, after instrumentation showed ZERO shells across the full suite and all 2407 corpus queries. `ValidatePlanInvariants` remains as the sink-side backstop. **NOW CLOSED (RFC-184 W2):** P5's terminal step landed — every physical wrapper is deleted, so the parent→child edge is no longer stored twice. A physical plan is its own cascades expression holding its children SOLELY as quantifiers (`RecordQueryFlatMapPlan`/`RecordQueryNestedLoopJoinPlan` carry `outerQ`/`innerQ` and no embedded plan-snapshot field; `GetChildren` resolves through them), so the dual-storage state is unrepresentable. The earlier BLOCKER — `rule_implement_nested_loop_join.go` building compensating filters that were never memoized, so the plan pointer held what EXECUTES while the quantifier held what the memo COSTS (9868 rule-time edges, 472 semantically different) — is resolved: those rules now memoize each compensating operator and advance the quantifier over that reference in lockstep (the FlatMap collapse), matching `rule_implement_simple_select.go`'s long-standing shape. The **LIVE DEFECT** that fell out of the same finding (the memo costing expressions that are not the ones that execute) is therefore gone — see the "memo costs an expression that is not the one that executes — CLOSED" section below. **STILL TRACKED (separate concerns):** retiring `findBestPhysicalPlan` — extraction's ad-hoc cost pick outside the cost framework, wired to ONE site against `findPhysicalPlan`'s twenty (do NOT "fix" that asymmetry by propagating the cheapest-selector: it is ordering-blind and turns `ORDER BY … DESC` into ASC, measured, see RFC-183 §10); and the reachability tally still driving a residual population of unreachable edges to zero (`plan_reachability.go` — a completeness/hygiene concern, NOT the wrong-tree-costing defect, which is closed). **RETRACTED (RFC-224):** an earlier revision cited "1186 references hold multiple finals, 1125 multiple PHYSICAL finals" as P5's blocker, then claimed singleton finals held at extraction. The first measurement was taken while rules were firing, and the second assertion mistook Java's mechanism for Go's property. Go deliberately retains one winner per required physical property; `TestExtractionIsUnambiguous` now pins the actual requirement — a total winner/compatible-physical-fallback selection on the path extraction dereferences, with zero dead ends and coherent retained alternatives. Note also the split's stated rationale in `plans/plan.go:23-28` — "physical and logical plan trees live in different namespaces in Java" — is FALSE: `QueryPlan<T> extends RelationalExpression` (`QueryPlan.java:51`), so a Java plan IS a RelationalExpression; the comment conflates package separation (real) with hierarchy separation (not real). |
| **Go-only physical-filter builders** (`ImplementSimpleSelectRule`, NLJ residual — extra paths past `Compensation`; `ImplementIndexScanRule` was the third until RFC-076 retired it) | Index-only predicates never become an executable residual | panic / wrong plan on a vector `DistanceRank` residual | **COVERED** — `ImplementFilterRule` `!isIndexOnly()` gate (RFC-151) + the retained `validateNoIndexOnlyResidual` catch-all backstop (pinned by `TestVectorPlan_*`). **TRACKED end-state:** gate the remaining builders so the net can retire. |

**Method for the next reservoir found:** name the Java invariant it drops, classify the risk
(wrong-rows / nondeterminism / panic), and either wire a WS-2/4 structural invariant that makes
the class un-shippable or file a tracked TODO — never leave it as a silent reservoir.

## RFC-183 P5 residue — tracked, out of scope for the fully-linked-plans branch

Four pre-existing items surfaced by the P5 review. None is caused by P5; all are filed here
rather than fixed in-branch. Verified against Java 4.12.11.

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
`TestExtractionIsUnambiguous`, and the flag-parity test.

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

**SelectMergeRule renames a capturing binding.** A pulled-up quantifier keeps its SQL-visible name
in Go, so it can share it with a correlation the merged select reads from an enclosing scope (a CTE
body over `LB` inside a subquery correlated to the outer `LB`); `SelectMergeRule` renames it rather
than letting it capture that read. Java's unique ids make the collision impossible. A read of a leg
a merged box flows whole keeps its binding, as that read names the box's declared window
(`TestSelectMergeRule_RenamesABindingThatWouldCaptureAnOuterRead`,
`TestSelectMergeRule_KeepsABoxLegASiblingReads`, `TestFDB_CTEBoxUnnestOnResolutionProbe2`).

## Element-shadows-outer vs Java AMBIGUOUS_COLUMN (dup-label unnest, shared-surface, Go-only reach)

When a lateral unnest's element/AT alias DUPLICATES an outer column name (`SELECT SUB FROM t,
t.scarr AS "SUB"`, or the CTE-boxed `WITH S AS (SELECT * FROM t, t.scarr AS "SUB") …`), Go's
deployed RFC-142 semantics resolves the reference to the UNNEST ELEMENT (element-shadows-outer,
last-write-wins). Java 4.12.11's `SemanticAnalyzer.resolveIdentifier` (SemanticAnalyzer.java
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

Java 4.12.11 attaches a trailing `ORDER BY` after `… UNION ALL SELECT …` to
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
- **mergeSortCursor leg selection: binary heap vs Java's per-row linear scan**:
  **Ordering-neutral by construction — this extension cannot diverge from
  Java observably at all.** The heap and the replaced linear scan select the
  exact same leg every round (same comparison key, same first-leg-wins tie
  rule), consume the exact same legs, and produce the exact same emitted row
  sequence and continuation bytes; a caller of `mergeSortCursor` cannot tell
  which selection algorithm ran underneath. Java's `UnionCursor.chooseStates`
  (`provider/foundationdb/cursors/UnionCursor.java:101-131`) rescans every leg
  on every emitted row to find the minimum comparison key — no heap, no
  tournament tree; `computeNextResultStates` (:74-98) is the same O(N) shape
  for the exhaustion/limit check. This is not a Go bug relative to Java: Java
  simply never needed better than O(N), since its N is bounded by query shape
  (a two-cursor UNION, a handful of OR branches). Go's InUnion plan turns a
  SQL IN list into one leg per value, so N can run into the hundreds or
  thousands, and the O(N)-per-row rescan dominates at that scale. Go replaces
  the scan with a `container/heap` binary min-heap
  (`pkg/recordlayer/query/executor/executor_new_plans.go`,
  `mergeSortCursor`/`mergeSortHeap`), giving O(log N) selection after an
  unavoidable O(N) initial build, while reproducing Java's tie-break exactly
  (the heap orders by comparison key, then by original leg index — the same
  first-leg-wins rule `chooseStates`'s single forward pass produces).
  Differential-tested against the replaced linear scan (kept only as a test
  oracle) across randomized leg counts/keys/reverse/dedup/resumption,
  including a fuzz target (1.04M executions, 0 divergences under Bazel); a
  dedicated error-injection suite additionally proves a leg's transient pull
  error mid-admit-batch is retried, not orphaned — no row lost, no panic
  (`merge_sort_heap_error_test.go`). One caveat the differential harness
  cannot see: the heap can surface a cross-type comparison-key invariant
  violation on a leg pair the linear scan never happened to compare directly
  (`chooseStates` only ever compares each leg against the running minimum,
  not every pair) — not a new bug, since the keys were already
  invariant-violating, just an earlier, heap-shape-dependent discovery point
  for one that already existed. In-process CPU-only measurement: heap loses
  to linear scan below N≈10 (heap bookkeeping overhead exceeds the tiny
  linear cost) and breaks even just above it, then wins by ~2.1x at N=100 and
  ~3.9x at N=1000 (7 replicates per point, tight confidence intervals — see
  `BenchmarkMergeSortCursor_HeapVsLinear`). End-to-end against real FDB
  (`BenchmarkFDB_InUnionMergeSort_N*`, 5 replicates per point) shows no
  measurable difference at N≤100 (FDB round-trip cost dominates), and an
  ~11.7% rows/sec improvement at N=1000 with non-overlapping 95% CIs
  (8366 vs 9348 rows/sec).

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

## INT-vs-LONG stays sargable in Go; Java promotes and loses the probe

**Direction: Java is correct-by-its-own-lights but plans WORSE, and Go
proves strictly MORE. This is a sanctioned read-side extension, NOT a
gap to close.** Anyone "fixing" Go to match Java here would be deleting
a working index probe.

Read this beside "Criterion 2 — max data access cardinality" (the
RFC-219 entry above, "Java is correct here and Go proves strictly
less"). Same shape, opposite sign: both are read-side plan choice only,
both touch no wire byte, and in one Go proves less than Java while here
Go proves more. The pair is what makes either legible — a lone entry in
this family reads as a parity gap someone should close, which is
exactly how the INT/LONG item got filed as open work in the first
place. It also belongs with "Go-Only Extensions" (Java rejects
outright) and "Java Upstream Bugs (Go is correct, Java is wrong)"
(Java is plainly wrong); this is the third case — Java is internally
consistent and simply plans a worse access path.

Java injects the promotion unconditionally on the sargability path.
Not in `RelOpValue.encapsulate` (which leaves INT/LONG alone and
dispatches a mixed-type physical operator, `EQ_IL` at
`RelOpValue.java:566`), but in `promoteOperands`, reached from
`toQueryPredicate`: `Type.maximumType(leftType, rightType)` then
`PromoteValue.inject` on BOTH operands (`RelOpValue.java:209,217-218`).
`inject`'s only escape hatches are type identity and a
nullability-only `canResultInType` (`PromoteValue.java:444-449`), which
`FieldValue` does not override — so INT→LONG over a column always
mints a real node. Nothing removes it later:
`DefaultValueSimplificationRuleSet.java:39-54` holds four rules, none
promote-related, and `EvaluateConstantPromotionRule` covers only
NULL / untyped-`[]` / nullability and is not in the default set.

On the match side `Value.matchAndCompensateComparisonMaybe`
(`Value.java:763-785`) sees through CANDIDATE-side `InvertableValue`
wrappers only. `PromoteValue` overrides neither `equalsWithoutChildren`
nor `semanticEquals`, so the class-identity default
(`Value.java:480-486`) rejects `PromoteValue` against the placeholder's
bare `FieldValue`. The wrapper is transparent on the COMPARAND side
only, via `PromoteValue implements Value.RangeMatchableValue`
(`PromoteValue.java:70`).

Consequence in Java, from its own checked-in plan baseline, on a schema
where `col3` (INTEGER) leads an index (`sql-functions.yamsql:24,27`):

```
sql-functions.metrics.yaml:204
ISCAN(T1_IDX1 <,>) | FILTER promote(_.COL3 AS LONG) EQUALS promote(@c9 AS LONG) | FLATMAP …
```

Unbounded scan plus a residual filter — `T1_IDX2` unused. Same shape at
`:180, :192, :218, :232, :246`.

Go instead declines to insert the promotion at all
(`sharesIntegerWireEncoding` in `expr.promoteColumnColumnNumeric`): INT
and LONG funnel to the SAME FDB tuple encoding, so the wrapper cannot
change a single wire byte and only costs the match. On
`intCol <op> <long-typed expr>` Go produces an index probe where Java
produces a full scan. **Wire compat is untouched** — identical encoding
is the entire premise of the guard — so this is read-side reach, which
is explicitly allowed.

The asymmetry matters and is easy to misread: `maximumType(INT,LONG)`
is LONG, so the wrapper always lands on the INT side. It costs the
probe only when the INT side IS the indexed column. `bigintCol = 5`
promotes the LITERAL and stays sargable in both engines; a promoted
COMPARAND stays range-matchable in both. A bare integer literal that
fits int32 is typed INT (`expr.intLiteralType`), so the only literal
spelling that can express the defect is an explicitly-LONG one (`20L`,
or a magnitude exceeding int32).

Pinned by `pkg/relational/sqldriver/int_long_sarg_matrix_probe_test.go`
(25 cells: literal and correlated-column comparands × both directions ×
`=`,`>`,`>=`,`<`,`<=`, each asserting the `IndexScan` AND the rows —
the degradation returns correct rows, so only the plan is the alarm).
10 of those cells are mutation-proven: removing the
`sharesIntegerWireEncoding` skip reddens the 5 `correlated_bigint`
cells; removing that plus the `IsConstantValue` early return also
reddens the 5 `long_literal` cells. The remaining 15 are
regression coverage this mechanism structurally cannot reach, which the
test file states rather than leaving as accidental green.

**Evidence class — read this before acting on the comparison.** The Go
half is MEASURED (live FDB, `--nocache_test_results`, red-green
mutation both ways). The JAVA half is INFERRED from 4.12.11 sources
plus the checked-in plan golden above. That golden is Java's own
recorded planner output, which makes it strong, but **the Java planner
was not run**. What would upgrade it: reproducing
`sql-functions.metrics.yaml:204` from an actual Java run. That was
deliberately not done — nothing is blocked on it, and the cost is not
justified by the claim's current use, which is only to explain why Go's
guard must not be "fixed" back toward Java.

## Bound parameters stay Unknown-typed at the plan gates

The plan-time promotion and cast-pair gates exempt UNKNOWN-typed operands,
which includes parameters on the exported PlanRecordQueryWithMetadata path.
The SQL driver does not reach this: it binds each `?`/`?x`/`$x` to a constant
typed from its Go value (Java's Type.fromObject), so a driver parameter takes
the same gates as a literal of that type. A bound constant is planned in, so
the plan-cache key carries the bindings; Java instead caches one plan per
parameter types.

## Engine-wide plan cache: literals are part of the key

Go's driver shares one `RelationalPlanCache` across every connection of a connector, with Java's
three stages, sizes, TTLs and PLAN_CACHE_* counts (`embedded/relational_plan_cache.go`). As in Java,
the primary key is the template, and the query key holds the template version and the planner
configuration with the store's readable-index view, so every schema of one template shares a plan.
Java's secondary key is the literal-extracted query; Go plans literals and bound values in, so its
secondary key is the token-rendered query with its literals and its tertiary key the bound values.
A plan ranked on a store's collected statistics (PLANNER_STATISTICS, a Go extension) stays keyed to
its database and schema. The stored-query warm-up is Java's (`WarmStoredQueries`: at the
connector's start, every template's stored queries are planned with no store, every index
readable); its OFFLINE_STORED_QUERIES_* counts are `Connector.StoredQueryWarmUp` and a log line, as
Go has no relational metric registry. For the same reason Java's per-phase planning timers
(RelationalEvent LEX_PARSE, CACHE_LOOKUP, CACHE_BYPASS, OPTIMIZE_PLAN, TOTAL_GET_PLAN_QUERY) are one
record per statement in Go: the plan-cache outcome and the planning duration
(`PlanGenerationInfo.Cache`, `PlanningDuration`). A DDL on any connection drops every shared plan (Java invalidates by key version);
CREATE/DROP TEMPORARY FUNCTION does not, and neither does a session reset: the transaction's
temporary functions are part of the key (as Java's transaction-bound metadata is), and a temporary
function whose body holds a parameter makes its statements uncached.

## Java's float `=` is bit identity, and contradicts itself (upstream bug)

Go's `=` treats `-0.0 = 0.0` as TRUE, matching the SQL standard, Postgres and
CockroachDB, where Java says FALSE. That signed-zero divergence is deliberate on
our side and is what this section is about.

NaN is a SEPARATE question and points the other way: Go returns TRUE for
`NaN = NaN`, matching Java. An earlier revision of this paragraph claimed Go
returned FALSE "matching the SQL standard, Postgres and CockroachDB" -- wrong
twice over. Go returns TRUE (measured), and Postgres deliberately treats NaN as
equal to itself and greater than every non-NaN, precisely so btree indexes have
a total order. See the NaN section below; do not read this section as covering
it.

**Java's PREDICATE path uses bit identity.** `Comparisons.java:246` is the
deciding line — `toClassWithRealEquals(value).equals(comparand)`, which for a
`Double` is `Double.equals` → `doubleToLongBits`. Ordering comparisons go
through `Double.compareTo` at `Comparisons.java:237-239`, the same total order.
So in Java:

- `WHERE d = 0.0` does NOT match a stored `-0.0`
- `WHERE d = d` is **TRUE for NaN** — a flat SQL-standard violation

Java's index probe agrees with its own filter here (FDB tuple encoding preserves
the sign bit, so the two zeros are distinct adjacent keys), so Java is at least
self-consistent between filter and index.

**But Java contradicts itself between predicate and projection.** `RelOpValue`
carries a second, independent evaluation path — the `BinaryPhysicalOperator`
lambdas used when a `RelOpValue` is evaluated AS A VALUE rather than converted
to a `QueryPredicate`. `RelOpValue.java:583`'s `EQ_DD` is
`(l, r) -> (double)l == (double)r`: primitive IEEE. Same expression, two answers:

| expression | mechanism | `-0.0` vs `0.0` | `NaN` vs `NaN` |
|---|---|---|---|
| `WHERE d = 0.0` | `SimpleComparison` → `Double.equals` | false | true |
| `SELECT d = 0.0` | `EQ_DD` lambda → IEEE `==` | **true** | **false** |

Exact opposites on both values. `SELECT d = 0.0 FROM t WHERE d = 0.0` over a
stored `-0.0` yields zero rows; drop the `WHERE` and the projected column reads
`true`. Note `RelOpValue.java:1149` explicitly delegates the ARRAY case back to
`Comparisons.evalComparison` — the scalar DOUBLE/FLOAT cases simply do not.

**Neither behaviour is pinned by any Java test.** `grep -rn -- "-0\.0"` over
`fdb-record-layer-core` `src/main` and `src/test` returns only
`TupleOrderingTest.java:81` (tuple byte order, unrelated to comparison) and the
`Half` 16-bit-float utilities. It is emergent from `Double.equals`, not a
defended contract.

**Why Go diverges deliberately.** "Match Java" is not a coherent instruction
when Java gives two answers for one expression, and porting the predicate path
would import a standard violation (`NaN = NaN` TRUE). Comparison semantics are
NOT wire format — the hard line is key encoding, record/index format and
continuations, all of which Go matches byte-for-byte — so the read-side
semantics are ours to get right. CockroachDB, the reference for calls Java does
not settle, agrees with IEEE.

**Consequence, stated plainly:** on a shared cluster, `WHERE d = 0.0` over a
stored `-0.0` returns the row in Go and not in Java. That is a real cross-engine
row difference, accepted because the alternative is importing a bug. It does NOT
affect what either engine writes.

**Related, and NOT the same question:** Go's DISTINCT / GROUP BY / uniqueness
split the two zeros (value identity is tuple-key identity), which DOES match
Java. `=` and dedup therefore disagree with each other in Go — an accepted,
documented asymmetry, forced by the aggregate-index wire format. See
`packedDedupKey`'s doc comment and TODO CQ-28 for the full argument.

## A NaN equality over an index returns every stored NaN

Java probes an index or primary key with its NaN's packed bits, one key, so it
answers the stored NaNs of that payload only, and claims the probe fixes the
coordinate for ordering. Go reads both NaN key blocks (negative NaNs below
-Inf, positive above +Inf) and answers every stored NaN, as its per-row `=`
and Java's per-row `=` do; it does not claim order through the coordinate, so
`WHERE d = NaN ORDER BY g` sorts in Go where Java's plan does not (equal rows).
Since RFC-257 WS-E both engines write the same bits for a NaN made by CAST, so
the answers differ only for NaNs of other payloads (arithmetic, bound values,
the record-layer API). Pinned by `yamsql/testdata/nan_index_equality.yaml` and
`executor/nan_block_binding_test.go`.

A NaN equality followed by other constrained index components reads the same
two blocks and filters each entry's later components below the continuation
(Java's plan keeps `[EQUALS, EQUALS]` and probes one key).

Still refused, loudly, before storage: an aggregate-index read bound to a NaN group key (each
NaN payload is its own stored group, while Go's GROUP BY puts every NaN in one
group), and a NaN vector-partition prefix (each partition is its own graph).
Java answers each from its probe's one key.

## NaN comparison follows Java's total order, NOT IEEE (open question)

MEASURED, not inferred. Rows with `v/z` evaluating to NaN:

    WHERE (v/z) = (v/z)    -> ALL rows      IEEE says FALSE (NaN != NaN)
    WHERE (v/z) <> (v/z)   -> NO rows       IEEE says TRUE
    WHERE (v/z) > 0        -> ALL rows      IEEE says FALSE (every NaN comparison is false)
    ORDER BY v/z           -> NaN sorts after +Inf (a stable total order)
    SELECT DISTINCT v/z    -> two NaNs collapse to one value
    GROUP BY v/z           -> two NaNs form one group

This is DELIBERATE, not an accident. `predicates/comparisons.go` falls through
to `values.CompareFloat64`, the `Double.compare` total order with NaN greatest,
and its own comment states the intent: "NaN vs NaN resolves to 0 here (matching
Java Double.equals)."

**So for NaN, Go matches Java — and that is far less lonely than it first
looks.** Postgres deliberately treats NaN as EQUAL to itself and greater than
every non-NaN, precisely so btree indexes have a usable total order, and
CockroachDB does the same. Strict IEEE, where every NaN comparison is false, is
what the bare standard says and what almost no INDEXED engine implements — an
index needs a total order, and IEEE NaN does not provide one.

An earlier revision of this section asserted the opposite about Postgres without
checking it, and the table row above claimed Go returned FALSE. Both were wrong,
and both were written while documenting Java's signed-zero bug — the same
session, the same file, the same failure to measure the other engine before
describing it.

So the posture here is NOT the mirror of signed zero. There, Go keeps IEEE and
diverges from Java. Here, Go, Java, Postgres and CRDB all agree on a total
order, and only the unindexed reading of the standard disagrees.

**Why this is left as an open question rather than "fixed" here.** The two are
not symmetric:

- Signed zero has a defensible IEEE answer that costs nothing elsewhere: `-0.0`
  and `+0.0` genuinely are the same number, and treating them as equal breaks no
  other invariant.
- NaN under strict IEEE is not merely "not equal to itself" — it makes the
  comparator NON-TRANSITIVE and destroys the total order that sorting, index
  ordering, merge joins and dedup all rely on. `values.CompareFloat64`'s total
  order (NaN greatest) is what keeps ORDER BY, the tuple key order and the
  merge-sort comparators mutually consistent. Making PREDICATE comparison IEEE
  while ORDER BY stays total-order is coherent (it is exactly the split already
  documented for signed zero via RFC-082), but it is a real semantics change
  across every comparison site and needs its own design gate.

The DISTINCT / GROUP BY behaviour (two NaNs are one value) follows from
`values.CompareFloat64` canonicalizing every NaN bit pattern, and would NOT
change even if predicate comparison moved to IEEE. That asymmetry is the same
one already accepted for signed zero, just pointing the other way.

**Correction — "every NaN packs to the same key" is false, and it was the
premise of a real wrong-answer bug.** An earlier revision of this paragraph
justified the DISTINCT/GROUP BY behaviour by asserting that value identity is
tuple-key identity because all NaNs pack alike. They do not. MEASURED, packing
`float64` through `pkg/fdbgo/fdb/tuple`:

    negNaN(0xFFF8000000000001) -> 210007fffffffffffe   (BEFORE -Inf)
    -Inf                       -> 21000fffffffffffff
    +Inf                       -> 21fff0000000000000
    posNaN(0x7FF8000000000002) -> 21fff8000000000002   (AFTER +Inf)

NaN payloads are neither canonicalized nor contiguous: encoding flips every bit
of a negative double and only the sign bit of a non-negative one, so NaN lands
in TWO disjoint blocks at opposite ends of the key space. The comparator
meanwhile ranks all NaN equal and greatest. So a NEGATIVE NaN is the physically
first row and the logically last one, and one logical tie class spans two
disjoint physical ranges. Pinned by
`values.TestFloatPhysicalOrderDivergesFromLogicalOrder`.

The "ORDER BY v/z -> NaN sorts after +Inf" row in the table above holds only
for POSITIVE NaN, which is what that probe happened to produce; a negative NaN
sorts first under an index scan.

The consequence for the planner is a separate, now-fixed defect — see
"Float-leading ordering claims terminate (Java is unsound here)" below.

Tracked in TODO.md; no behaviour changed by this entry, which only corrects a
false claim about what Go does today.

## Float-leading ordering claims terminate (Java is unsound here)

**Java is wrong and Go deliberately is not.** This is a read-side planner
divergence only; the wire is untouched (identical bytes are written and read),
so it falls under the port's "read-side query surface may go beyond Java" rule
rather than being a parity break.

**The defect.** An ordering claim asserts that the PHYSICAL order in which a
scan returns rows equals the LOGICAL order the comparator imposes. FDB tuple
encoding flips every bit of a negative double and only the sign bit of a
non-negative one, which lays the domain out as

    negative NaN < -Inf < … < -0.0 < +0.0 < … < +Inf < positive NaN

while `values.CompareFloat64` (faithful to `java.lang.Double.compare`)
canonicalizes every NaN to one value ranked GREATEST. Two independent
consequences follow:

1. A NEGATIVE NaN is the physically FIRST row and the logically LAST one, so a
   scan claiming to deliver `ORDER BY <double>` delivers something else.
2. All NaN payloads are ONE logical tie class spread across TWO disjoint
   physical ranges, so no SUBSEQUENT sort column is ordered within that tie —
   which is why a float coordinate must TERMINATE an ordering claim rather than
   merely be reordered. No range-set can repair (2).

Positive NaN alone does NOT expose this (it is physically and logically last),
and signed zero does not either (`-0.0` packs immediately before `+0.0` and the
comparator agrees). The defect requires a negative NaN, or both signs present.

**Java has the same bug — verified, not assumed.** Nothing on Java's path from
index key to sort removal consults the column's type:

- `ScanComparisons.getComparisonType` (`ScanComparisons.java:150-169`) maps
  operators to EQUALITY/INEQUALITY from `comparison.getType()` — the operator
  enum — alone. The file does not import `cascades.typing.Type` at all.
- `ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons`
  (`ValueIndexLikeMatchCandidate.java:166-196`) emits `Binding.sorted(...)` for
  every key column that is not `createsDuplicates()`. `getBaseType()` is used
  only to build the `Value` (lines 179-181); the resulting type is never
  inspected.
- `Ordering.satisfies` (`Ordering.java:330-352`) checks Value identity,
  ASC/DESC compatibility and a topological permutation. No type.
- `RemoveSortRule.onMatch` (`RemoveSortRule.java:105-110`) drops the sort on
  `ordering.satisfies(...)` alone.
- `AbstractDataAccessRule` references a data type nowhere; its only `Type` hits
  (lines 793, 809, 1121) are `ComparisonRange.Type.EQUALITY`.

Java's only NaN awareness is `CastValue.java:139-169` (rejecting NaN in casts
to INT/LONG) and the half-precision vector type — neither is about ordering.
The tuple layer *documents* the split it creates, two levels below a planner
with no channel to hear it. Both citations below are in the **fdb-java
bindings** (the `foundationdb` repo, tag 7.3.77 — the MODULE.bazel pin), NOT in
the record-layer tree —
`bindings/java/src/main/com/apple/foundationdb/tuple/`:

- `IterableComparator.java:49-51` — "For floating point types, negative NaN
  values are sorted before all regular values, and positive NaN values are
  sorted after all regular values."
- `TupleUtil.java:184-187` —

      private static long encodeDoubleBits(double d) {
          long longBits = Double.doubleToRawLongBits(d);
          return (longBits < 0L) ? (~longBits) : (longBits ^ Long.MIN_VALUE);
      }

  `doubleToRawLongBits` is the non-collapsing variant, so the payload and sign
  bit survive verbatim — deliberately NOT canonicalizing NaN. Contrast
  `Half.floatToShortBitsCollapseNaN` in `fdb-extensions`, which does collapse,
  proving the distinction is known upstream.

Java is additionally inconsistent with its own index order:
`Comparisons.compare` (`Comparisons.java:236-239`) special-cases `byte[]`,
`ByteString` and `UUID` to unsigned wrappers *specifically* to match tuple
order, but lets Double/Float fall through to `Double.compareTo` — the same
negative-NaN disagreement Go had.

**What Go does.** `values.TypeTerminatesOrderingClaim` /
`ColumnCanExtendOrderingClaim` / `ClaimableOrderingPrefix`
(`cascades/values/ordering_claim.go`) are the ONE predicate. The producers that
ask it, enumerated rather than counted:

- `plans/ordering.go`: `PKScanOrdering` (behind
  `RecordQueryScanPlan.HintOrdering`), `RecordQueryIndexPlan.HintOrdering`, both
  of their `HintRichOrdering` forms, and — the two AGGREGATE producers —
  `RecordQueryStreamingAggregationPlan.HintOrdering` and
  `RecordQueryAggregateIndexPlan.HintOrdering`;
- both match candidates' `ComputeMatchedOrderingParts`
  (`match_candidate_index.go`, `primary_scan_match_candidate.go`);
- both sort-elision rules (`rule_ordered_index_scan.go`,
  `rule_ordered_primary_scan.go`), which previously name-matched sort keys
  against column names without consulting any ordering-capability machinery at
  all.

**Correction — an earlier revision of this paragraph said "every ordering-claim
producer asks it: `plans/ordering.go`'s four derivations".** It was false in the
way this repo's failure mode always is: it asserted an invariant the code did
not have. The two aggregate producers above did not ask, and the omission was
not theoretical — MEASURED on a real cluster,
`SELECT d, SUM(a) FROM t GROUP BY d ORDER BY d` with `d DOUBLE` returned the
negative-NaN group FIRST from both a `StreamingAgg` over an ordered index scan
and a direct aggregate-index read, while the unindexed oracle returned it last.
The streaming-aggregation case is NOT derivative of the inner scan:
`StreamingAggFromIndexRule` matches grouping keys against index column NAMES and
never reads the inner's ordering claim, so terminating the scan leaves the
aggregate stating the same false order. Pinned by
`TestFDB_FloatOrderingClaim_Aggregate_Differential`.

The remaining producers in that file do not ask, and the reason each does not is
stated at the head of `plans/ordering.go` rather than left to be assumed.
`RecordQueryInMemorySortPlan` restates what it sorted BY, with the comparator.
The four merge-set producers (`MergeSortUnion`, `Intersection`, `InUnion`,
`MultiIntersectionOnValues`) restate a COMPARISON KEY derived from their legs'
provided orderings — and those now terminate at the float, so no common ordering
containing one exists and the merge is never built. That is EMERGENT, so it is
pinned rather than asserted: `TestFloatNeverReachesAMergeComparisonKey` shows
the identical shape still intersecting over an INTEGER coordinate and falling
back to a materialized sort over a DOUBLE one.

An equality-bound float prefix column is exempt: a fixed binding pins one
physical point and claims no order, so columns after it stay claimable.

The predicate is only as good as the layout it reads, and that layout was half
blind. `executor.PositionalTypeForDescriptor` — the single authority for a
stored record's row shape, shared by the runtime rows and by every sargable
match candidate — stamped `UnknownType` on every field, while the plain
full-scan leaf (`cascades_translator.fieldTypeForFD`) typed the same columns
fully. Two derivations of one table's layout, disagreeing. The predicate could
therefore prove a column was a DOUBLE on the scan leaf and could not prove it on
the index candidate, so `... WHERE d > 5.0 ORDER BY d` kept the unsound elision
that the unpredicated `ORDER BY d` had already lost. The two mappings are now
one — `values.FieldTypeForProtoField` — and both call sites delegate to it.

The cost is a materialized sort where Java elides one. That shows up in the
cross-engine harness as a deliberate plan-shape difference, not a surprise.

Scope of the plan-shape change, MEASURED rather than estimated, by running the
RFC-201 factory determinism check under each half of the change separately:

| ordering-claim termination | typed row layout | corpus files with drifted plan shape |
| --- | --- | --- |
| off | off | 0 (baseline) |
| on  | off | 0 — the fix cannot fire without the types |
| off | on  | 0 — typing the layout changes nothing on its own |
| on  | on  | 9 |

The third row is the load-bearing one: filling in a layout's field types is the
kind of change that could plausibly perturb any type-directed decision in the
engine, and across the corpus it perturbs none.

The last row is **9 files / 27 scenarios**, re-enumerated from scratch against
master by regenerating every committed header and comparing. Every one of the 27
is an `ORDER BY <float column>[ NULLS FIRST], id` — the ordering-claim
termination landing on an index whose key columns include the `DOUBLE` or
`FLOAT` column, which is the intended behaviour change.

**"18 files" stood here as a MEASURED figure and is retracted.** It was a true
measurement of an EARLIER, over-broad revision of the fix, which also terminated
the claim on equality-bound floats and drifted 57 scenarios — 30 of them plan
REGRESSIONS with correct rows. It was never a measurement of what shipped. The
retraction previously lived only in
`factorycorpus/RETIREMENT_LEDGER.md`, which is not where a reader of this table
would find it, so the number is corrected where it stands.

Extending the termination to the two AGGREGATE producers drifts the corpus by a
further **0 scenarios** — MEASURED, all 5000 headers re-derived, none moved.

That zero is not "no scenario groups by a float": `grep -ril "group by"
testdata/ | wc -l` is **0**, so not one of the 309 committed files contains
GROUP BY at all. The gap is ARCHITECTURAL — the TLP partition oracle declines
aggregates by construction (`factory/candidate.go:101-109`, because one row per
group is not a row set the input partition maps onto), so no factory batch will
EVER cover the two aggregation producers. Their proof is an FDB differential
permanently, not provisionally, and the zero is evidence about the corpus's
reach rather than about the producers' soundness.

Scenarios are re-blessed through the factory, never edited in place.

## A nullable UNIQUE key does not make a scan strictly sorted (Java is unsound here)

Sibling of the float-ordering entry above, and the same shape: one rule, one
absent type consultation, storage distinguishing what the claim says is
indistinguishable. That entry is about NaN; this one is about NULL.

**Java's claim.** `RemoveSortRule.strictlyOrderedIfUnique`
(`RemoveSortRule.java:144-156`) decides the whole question at `:153`:

```java
return matchCandidate.isUnique() && numKeys >= matchCandidate.getColumnSize();
```

There is **no nullability term** — not in that method, and not on the path into
it from `:132`. Nothing consults the key columns' types or their nullability.

**Why that is false.** Under `NULLS DISTINCT` the uniqueness check is SKIPPED
when an indexed component is NULL, so a `UNIQUE` index on a nullable column
legitimately holds

    (NULL, pk=1), (NULL, pk=2), (NULL, pk=3)

— three entries whose *claimed* sort key is identical. Java stamps
`strictlySorted` on that scan, and a consumer that reads the flag as "no two rows
compare equal on the sort key" is being told something false **by construction**,
not in an edge case. `strictlySorted` is a proposition the plan asserts to its
consumers so they can skip work; there is no partial version of it, and no
downstream operator that catches it being wrong.

**What Go does.** `indexHasStreamEnforcedUniqueKey`
(`cascades/rule_implement_sort.go`) routes through
`properties.SecondaryUniqueKeyEnforcedOnStream`, whose nullability clause refuses
exactly this case unless the stream itself rules the NULLs out. The divergence is
documented at that call site so it is not mistaken for a redundant check and
"simplified" back into Java's shape.

The cost is a retained sort where Java elides one, on a nullable unique key. That
shows up in the cross-engine harness as a deliberate plan-shape difference. The
alternative is a wrong answer, so it is not a close trade.

**Reachability, and the reason the divergence is currently invisible.** The gate
is `false` for every SQL-expressible secondary unique index today, because the
SQL DDL rejects a `NOT NULL` scalar column (deliberately, for Java parity — `NOT
NULL` is accepted only on `ARRAY` types). So Go's claim was not merely stricter
than Java's, it never fired from SQL at all. RFC-210 §5.7 is what makes it
reachable on the streams where it is TRUE — a NULL-rejecting predicate empties
the index's exempt set — and it does so WITHOUT relaxing the clause. Note there
is deliberately no residual-dedup analogue for the sort claim; the RFC states
that as a prohibition rather than an omission.

The evidence a *scan range* rejects NULL is not the same evidence a *predicate*
does, and the difference was carried as an open question before it was settled.
It is settled: a scan range's admission rests on two binder facts — a NULL-valued
equality binds to an EMPTY range rather than seeking `[null]`, and a bare upper
bound installs an exclusive low at the NULL boundary. Both are pinned in
`executor/scan_range_null_boundary_test.go`, because a sort claim has no residual
that could catch either one changing.

**To report upstream — READY TO FILE, not filed.** It is a soundness bug in
Java's own terms rather than a Go/Java modelling difference, so it is expected to
be fixed upstream rather than to persist as a permanent divergence.

It is not filed from here, and the reason is procedural rather than technical:
filing publishes a defect claim about someone else's project under this repo
owner's identity, on a repository this project does not control, and no entry in
this file has ever referenced a filed upstream issue — there is no established
convention to follow. That is an owner decision. The report is therefore written
out in full below so that filing it costs one paste, and so the analysis does not
have to be re-derived by whoever files it.

> **Title:** `RemoveSortRule.strictlyOrderedIfUnique` claims `strictlySorted` for
> a UNIQUE index on a NULLABLE column, which is unsound under NULLS DISTINCT
>
> **Where:** `RemoveSortRule.java:153`
>
> ```java
> return matchCandidate.isUnique() && numKeys >= matchCandidate.getColumnSize();
> ```
>
> **The claim.** `strictlySorted` asserts to downstream consumers that no two
> rows of the scan compare equal on the sort key, which is what licenses removing
> a sort and skipping duplicate handling.
>
> **Why it is false.** Under `NULLS DISTINCT` the uniqueness check is SKIPPED
> when an indexed component is NULL, so a `UNIQUE` index on a nullable column
> legitimately holds
>
> ```
> (NULL, pk=1), (NULL, pk=2), (NULL, pk=3)
> ```
>
> — three entries whose claimed sort key is identical. The path into `:153`
> carries no nullability term anywhere, so the claim is made for these indexes
> too. This is not an edge case reached by unusual input: it is false by
> construction for every unique index over a nullable column, on any store
> holding two or more NULLs in it.
>
> **Consequence.** `strictlySorted` is a proposition the plan asserts so
> consumers can skip work; there is no partial version of it and no downstream
> operator that catches it being wrong. A consumer that trusts it is handed
> duplicate sort keys.
>
> **Suggested fix.** Gate the claim on the key components being non-nullable, or
> on the scan's range excluding the NULL boundary — the latter keeps the claim
> available on the streams where it is true (a `WHERE col IS NOT NULL` or any
> lower bound above the NULL boundary compiles to exactly such a range, per
> `RangeConstraints.java:650-653`).
>
> **How it was found.** Porting this rule to a Go reimplementation of the record
> layer. The Go port declines the claim for nullable keys and re-admits it only
> when the scan range excludes NULL; that divergence is what surfaced the
> question.
## Non-integer explicit record type keys now reach the key bytes (RESOLVED — read the migration note)

`RecordType.getRecordTypeKey()` is written verbatim into primary keys and index
entries whenever a primary key starts with `RecordTypeKey()`. Java normalizes it
once through `TupleTypeUtil.toTupleEquivalentValue` (`RecordType.java:73` for the
explicit key, `:174` for the derived one) — narrower integers widen to `Long`,
`byte[]` becomes a `ByteString`, and strings and everything else pass through
untouched — and then encodes whatever that produced.

Go used to resolve the key from a `map[string]int64` built at metadata-build time
and bound onto the shared `RecordTypeKeyExpression`. Only integers fit in that
map, so a **string or bytes** explicit key was silently absent from it and the
evaluation fell back to the proto message's **type NAME**. Setting
`SetRecordTypeKey("k")` therefore wrote `"Order"` into the key while
`GetRecordTypeKey()` reported `"k"` — bytes Java reads as an entirely different
key, and bytes that disagreed with Go's own metadata. The key is now resolved
from the record's own resolved `RecordType`, so the explicit key reaches the
bytes exactly as Java writes them.

Go's normalization additionally widens `uint8`/`uint16`/`uint32`, which Java has
no equivalent of. This is not an extension: Java widens *its* narrower integer
types, and Go's set is larger. Without it those values reach the tuple encoder
raw, which handles only `int`/`int64`/`uint`/`uint64` and **panics** on the save
path for a key the builder accepted. `uint`/`uint64` are deliberately left
alone — they can exceed `MaxInt64`, the encoder handles them natively, and for
in-range values the bytes are identical.

**Migration.** Nothing this repository writes is affected: the SQL catalog's
record type keys are `int64` (`catalog/system_tables.go`), and the one
non-integer key in the tree sits on a record type whose primary key has no
`RecordTypeKey()` prefix, so it never reached key bytes. The hazard is for a
**Go-only** deployment that used a string or bytes explicit record type key
*together with* a record-type-prefixed primary key: its existing records live
under the old message-type-name prefix and the new code will not find them.
Those records were already unreadable to Java — the old bytes were Java-invalid
in the first place — so this converts a silent cross-engine divergence into a
one-time re-key, not a regression against any previously correct state.

Pinned by `metadata_builder_test.go` "record type key packs to Java's bytes":
byte-level assertions that every narrower integer width collapses to one key,
that a string key reaches the bytes rather than the type name, and that a bytes
key keeps tuple type code `0x01` rather than being folded into a string.

### VECTOR index metadata validation: Go validates only windowed indexes, Java has `VectorIndexValidator` (OPEN — RFC-257 WS-D)

This entry replaces the current section of the same subject (cur.md:2194, "(OPEN — owner decision)"), whose "Go:" paragraph is stale.

**Java:** `VectorIndexMaintainerFactory.VectorIndexValidator.validate`
(`indexes/VectorIndexMaintainerFactory.java:96-111`) runs when the metadata is built.
It does three things. It runs the base `IndexValidator.validate`. It runs
`validateStructure()` (`:132`): the root must be a `KeyWithValueExpression`, must
not contain a grouping expression, and must have at least one column after the split
point, and the index must not be unique. It then calls `VectorIndexHelper.validate(index)`
(`VectorIndexHelper.java:47`), which delegates to the engine the index's
`vectorEngine` option selects. Any `IllegalArgumentException` from that call is
rethrown as `MetaDataException("incorrect index options")`. The dimension count is
MANDATORY. `VectorIndexOptionsHelper.getNumDimensions` throws "need to specify the
number of dimensions" when it is absent (`VectorIndexOptionsHelper.java:55`).

**Go (before RFC-257 WS-C):** Go had no vector validation at metadata time. The
maintainer read options permissively: each option went through an "if it parses and
is in range, use it" guard. So a mistyped `hnswM`, an out-of-range
`hnswEfConstruction` or an unknown `hnswMetric` fell back to a DEFAULT. The index then
served queries with a graph whose connectivity, or notion of "nearest", was not the
declared one. The maintainer now reads options the way Java does (see below). The
build-time half for a plain index is still open.

**What is closed:** the OPTION half, for windowed vector indexes only. It is the
delegate call that Java's `SlidingWindowIndexValidator` ends with:
`validateVectorIndexOptionsAtBuild` (`vector_index_validation.go:19`). `validateIndex`
calls it after `validateSlidingWindowIndex` for a windowed index
(`index_validator.go:427-451`).

A plain VECTOR index runs no part of the validator at `Build`, because
`validateIndexType` (`index_validator.go:104`) has no VECTOR arm.

`validateVectorIndexOptionsAtBuild` dispatches on the engine (`VectorEngineOf`):
- GuardiANN goes through `parseGuardiannConfig`.
- HNSW gets Java's `VectorIndexHelper.validate`. First, an option set under both its
  `hnsw*` name and its `vector*` alias is refused with "vector index option specified
  under more than one name" (`hnswAliasConflict`, `hnsw_options.go:83`). Then the
  configuration is parsed as `HnswVectorIndexEngine.parseConfig` parses it
  (`readHNSWOptions`, `hnsw_options.go:120`):
  - a shared option is read under its alias when its name is absent;
  - parsing follows `Integer::parseInt`, `Double::parseDouble`,
    `Boolean::parseBoolean` and `Metric::valueOf` (`javaParseInt` and
    `javaParseDouble` in `java_parse.go`; any case of `true`; the four constants'
    names);
  - `Config`'s constructor checks run with Java's messages.

If parsing or a check fails, the result is a `MetaDataError` "incorrect index options"
with the failure as its cause (`Unwrap`, a `NumberFormatError` or an
`IllegalArgumentError` with Java's text). A missing count is "need to specify the
number of dimensions". Pinned by the JVM specs in
`conformance/key_validation_conformance_test.go`: "A windowed VECTOR index's options
parse as Java parses them", "... is validated as Java validates it", "... configuration
is checked as Java's Config checks it".

The maintainer uses the same reader (`parseHNSWConfig`, `hnsw_options.go:253`). So the
configuration Go maintains is the one Java's engine reads, and a configuration Java
refuses makes the maintainer fail instead of taking a default. The JVM spec "A windowed
VECTOR index's options are read as Java reads them" compares the whole configuration.

**RaBitQ with 9 to 15 extra bits.** Java's `Config` admits this, but Java's
`RaBitQuantizer` refuses it. Go refuses it where Java constructs the quantizer: when an
operation first quantizes (`hnswGraph.raBitQuantizerAdmits`, `hnsw.go:178`). The effects:
- A Euclidean index accepts saves until its centroid is established. After that it
  refuses every save that inserts or removes a vector entry, every search, and every
  delete of a present node.
- A cosine or dot-product index refuses its first save.
- A save maintained in its own transaction that leaves a record's vector entry unchanged
  is served in both engines, because it makes no graph call. Java's
  `StandardIndexMaintainer.update` removes the entry common to the old and new record,
  and so does Go's `vectorIndexMaintainer.Update` via `removeCommonEntries`
  (`vector_index_maintainer.go:264-294`).
- Two paths still delete and re-insert such a node in both engines, so both are refused
  after the centroid. This was read from both sources; no JVM row covers it.
  - A save queued for a WRITE_ONLY_WITH_QUEUE index commits (both entries are
    serialized, with no graph call), but its drain is refused (the replay deletes, then
    inserts).
  - A windowed index's sliding window calls its delegate once with the old record and
    once with the new.
- A search of an empty index is served.

Pinned by the JVM spec "An HNSW index with more RaBitQ extra bits than the quantizer
encodes is refused where Java constructs it" (`conformance/vector_index_conformance_test.go`).
The refusal is an `IllegalArgumentError`, Java's class. Guava's `checkArgument` gives
no message in Java; Go's message names the range.

For a PLAIN vector index the maintainer still accepts two Go forms that the windowed
validator refuses (`goForms`, `hnsw_options.go:116-136`): the lower-case metric names
(`cosine`, `inner_product`, `euclidean`), and 128 dimensions when none are given.
Java's structure half is not ported.

**What is open, and why it is an owner call rather than a deferral:** applying the same
validation to PLAIN vector indexes was implemented and MEASURED, and it breaks the
existing suite, because Go builds vector indexes without `hnswNumDimensions`, which Java
requires. Closing it means Go starts REJECTING metadata it accepts today, which can make
an existing Go-authored store fail to open. The structure half is wider still: test
sites build vector indexes on roots that are not `KeyWithValueExpression`, which Java
refuses outright.

The owner ruled that data written by pre-release Go builds is not supported (RFC-257,
"Verification and review gates" item 9). That removes the reason this stayed open: the
port refuses what Java refuses and migrates the call sites. The work belongs to RFC-257
WS-D, alongside the rest of the vector index's Java alignment.

(The comment at `vector_index_maintainer.go:213-217` still calls the non-KeyWithValue
root a "Java divergence". This entry documents it.)

---

## Identifier resolution: Go over-resolves case, Java compares exactly

**Java's rule.** Identifiers are normalized ONCE, at the parse boundary
(`SemanticAnalyzer.normalizeString`: quoted keeps its case, unquoted folds
UPPER), and every comparison after that is exact — `Identifier.equals` is
`String.equals` on the normalized name. There is NO configuration under which
Java resolves `foo` against a column called `Foo`; `CASE_SENSITIVE_IDENTIFIERS`
only selects which branch of `normalizeString` runs and makes Java *more*
case-sensitive, never less.

Go honours `CASE_SENSITIVE_IDENTIFIERS` (2026-10-07) by quoting every unquoted
`uid` of a statement as written before it is planned, which is exactly the
branch Java's `normalizeString` takes; the relaxed second pass below is
unchanged by it.

**Go's rule.** Presentation matches Java exactly (RFC-237). Lookup adds a
SECOND PASS at each scope level: exact first, then an unambiguous
case-insensitive match, then the parent. It counts candidates, so a folded
reference matching two case-variants is 42702.

**Measured against a live fdb-relational 4.12.11**, over
`CREATE TABLE QCASE (id BIGINT, "KeepCase" BIGINT, plain BIGINT, …)`:

| query | Java | Go |
|---|---|---|
| `SELECT "KeepCase"` | `[KeepCase][[42]]` | same |
| `SELECT *` | `[ID KeepCase PLAIN]` | same |
| `SELECT KeepCase` | 42703 | `[KeepCase][[42]]` |
| `SELECT "KEEPCASE"` | 42703 | `[KeepCase][[42]]` |
| `SELECT "keepcase"` | 42703 | `[KeepCase][[42]]` |
| `SELECT "plain"` (unquoted DDL) | 42703 | `[PLAIN][[7]]` |

and, reached through USING:

```sql
SELECT q1."id" FROM q1 JOIN q2 USING ("k") JOIN q3 USING ("K")
java: Unknown reference K      go: [[1]]
```

Pinned as `goOnly` arms in `conformance/quoted_identifier_case_java_probe_test.go`
and in `conformance/join_using_chain_java_probe_test.go`, so either engine
moving reddens a test.

**Why Go keeps it.** The populations differ, not the correctness. Java's SQL
surface is always DDL-fed, so a descriptor whose field names are not already the
normalized SQL spelling is a corner case. Here `rlcatalog.Wrap(md)` over a
user's own hand-written `.proto` is a first-class entry point, and those
descriptors carry lower/snake names — so `SELECT order_id` over a field
literally called `order_id` has to keep working. Read-side only; never reaches
the wire.

**What closing it would actually take** — and this is the part that was
recorded WRONG in two places before: not plumbing `CASE_SENSITIVE_IDENTIFIERS`
(it would not help, see above) but **preserving the QUOTING BIT through
`functions.NormalizeIdentifier` and `semantic.FromNormalized`**, which
discard it today. `FromNormalized` hard-codes `wasQuoted: false` and is used
~59 times on the reference path, so the engine cannot currently tell `"K"` from
`K` at resolution time at all. Probed: gating the relaxed pass on
`!want.WasQuoted()` is INERT for exactly that reason.

---

## Index fields are exported in Go and `private final` in Java

**Java.** `Index` holds `private final` `name`, `type`, `rootExpression`,
`options` and `predicate` (`Index.java:63-78`), exposed by getters only
(`:294`, `:310`, `:329`). There is no `setName` and no `setRootExpression`.
Because the fields cannot be rewritten, `RecordMetaDataBuilder.build` passes
`indexes`, `universalIndexes` and `formerIndexes` DIRECTLY into
`new RecordMetaData(...)` (`RecordMetaDataBuilder.java:1457-1459`) and shares
them safely.

**Go.** Those fields are exported. `Build` copies every CONTAINER but shares the
`*Index` OBJECTS, so a caller that keeps the index it passed to `AddIndex` — the
normal pattern, and one Java REQUIRES, since `build` sets
`primaryKeyComponentPositions` on that very object (`:1466`) — can rewrite the
built metadata afterwards. Each field is its own hazard: `RootExpression` reads
existing entries under a new expression, `Type` dispatches to a different
maintainer, `Predicate` filters by something the entries were never built with,
`Name` desynchronises the registry key so `ToProto` emits an empty record-type
list that reloads as universal.

**Why the objects are shared and not copied.** Copying them was tried and
reverted. Java shares, so it is a Go-only invention; a shallow copy does not
isolate anyway, because the KeyExpression graph exposes exported mutators
(`DimensionsKeyExpression`'s fields, which bound `CanDeleteWhere` at runtime;
`RecordTypeKeyExpression.Nest`; the live child slice from
`CompositeKeyExpression.SubKeyExpressions`); and it splits "the caller's index"
from "the metadata's index" across several hundred sites that pass a pre-`Build`
`*Index` straight to `ScanIndex`, `RebuildIndex` or `SetIndex`. In
`OnlineIndexer` that split degraded the containment check from "is the
metadata's definition" to "shares a name with it". It also broke cross-engine
conformance (`go: pk=[]` vs `java: pk=[1]`).

THE EXACT FIGURE IS WITHDRAWN, and how it broke is worth more than the number
was. Two revisions of this branch carried 545 here and 544 in a test's failure
message. That reads as one number copied wrong, and it was first "fixed" by
deleting the second copy. It was not a copy at all: `grep -rn
"ScanIndex(\|RebuildIndex(\|SetIndex(" --include='*.go' pkg/ cmd/ conformance/ |
wc -l` gives 545, and the same command piped through `grep -vc 'parser/gen/'`
gives 544. Both were measured, both were right, and the single site between them
is `relational_parser.go:50302`'s `SetIndex(v IExpressionAtomContext)` — an
ANTLR accessor with no `*Index` in it. Neither figure counts what the sentence
above them claimed: the total also swallows 6 declarations (two of the others
being `BenchmarkScanIndex` and `runGoScanIndex`, name substrings rather than
calls), 2 comment lines, and every call passing an index NAME rather than an
object. Reconciling two counts by deleting one is how a contaminated measurement
survives — the answer was to ask why they differed.

**The fix is encapsulation**, not copying: unexport those fields behind getters,
the pattern already used on the same struct for `subspaceKey`/
`SubspaceTupleKey()`. Then sharing is safe for the same reason it is safe in
Java.

SIZING IT NEEDS A TYPE-AWARE SEARCH, NOT GREP, and the numbers an earlier
revision of this entry quoted are withdrawn rather than corrected. `.Name` and
`.Options` are carried by many other types, so a textual count is contaminated
in both directions: `grep -rn '\.RootExpression\b'` counts 248 READS and misses
58 writes (`'RootExpression:'`); `'idx\.Options'` is case-sensitive and misses
`vecIdx`, `mdIdx`, `spfIdx` and friends; and the call-site count for the verbs
includes their own `func … SetIndex(` declarations, one of which is an ANTLR
parser method with no `*Index` in sight. What is defensible is a floor — several
hundred sites across `pkg/`, `cmd/` and `conformance/` — and that the exact
work-list wants `go/types`, not a regex. Whoever takes it should produce that
list first; treating a grep total as the scope is what made this paragraph wrong
twice.

**AND `Options` IS NOT CLOSED BY UNEXPORTING FIELDS**, which the paragraph above
would otherwise imply. It is a `map[string]string` shared with the built
metadata, and Go has two EXPORTED methods that mutate it in place:
`Index.SetUnique` does `idx.Options[IndexOptionUnique] = "true"` and
`Index.SetClearWhenZero` sets or `delete`s its key. So
`md.GetIndex("x").SetUnique()` flips uniqueness on already-built metadata
with no field assignment at all, and unexporting `Options` would leave both
setters working exactly as before.

Java has neither method — `grep -n "setOption\|options.put\|setUnique\|
setClearWhenZero" Index.java` returns 0, positive control `grep -c "public "`
returns 47 — and `Index.java:131` stores `ImmutableMap.copyOf(options)`, so even
`getOptions().put(…)` throws. Closing this needs the setters to be build-time
only (or gone), not just the fields hidden.

Pinned by `TestBuiltMetadataSharesIndexObjectsWithTheBuilder`, which fails when
the divergence closes. NOTE the pin covers `*Index` identity and `.Name` only —
`Type`, `RootExpression`, `Options` and `Predicate` are named here but driven by
no arm.

---

## `universalIndexes` is a Map in Java and a slice in Go

Java's `RecordMetaDataBuilder.universalIndexes` is a `Map<String,Index>` and
`removeIndex` does `universalIndexes.remove(name)`
(`RecordMetaDataBuilder.java:1189-1198`) — a removal both the builder and any
metadata sharing the map see cleanly. Go's is a `[]*Index` and
`removeIndexFromSlice` compacts it IN PLACE (`result := indexes[:0]`,
`metadata.go`), which rewrites the backing array.

Go therefore COPIES the slice into the built metadata where Java shares the map.
Sharing it reproduced a state Java cannot reach: after `Build` then
`RemoveIndex`, `[uni_a uni_b]` became `[uni_b uni_b]` — the survivor duplicated,
so its maintainer runs twice on every save and a COUNT/SUM aggregate takes a
non-idempotent double atomic ADD, while the removed index stayed registered with
no record types, the orphan state `Build` refuses at construction.

The general rule, which "share what Java shares" does not capture: sharing is
safe only when mutation is not in-place-destructive. Same field name, different
container, different answer. `formerIndexes` is copied for the same reason even
though it is append-only today — that is luck, not a property, and its elements
are copied too because `FormerIndex.SubspaceKey` is `any` and may hold a
`[]byte` whose backing array would otherwise be shared.

Pinned by `TestPostBuildRemoveIndexLeavesUniversalIndexesIntact`.

---

## Go snapshots a record type's index lists; Java shares everything

Java's `RecordType` constructor assigns the builder's live lists
(`RecordType.java:70-71`: `this.indexes = indexes;`), and `removeIndex` mutates
them through `recordType.getIndexes().remove(index)`
(`RecordMetaDataBuilder.java:1194-1195`). It ALSO shares the index registry
itself: `private final Map<String,Index> indexes` (`:119`) is passed uncopied at
`:1459` and stored by reference (`RecordMetaData.java:155`), so
`indexes.remove(name)` (`:1190`) removes the index from the built metadata's
registry too, and `toProto` — which seeds from `indexes.entrySet()`
(`RecordMetaData.java:664-665`) — never emits it.

So Java's post-`build()` `removeIndex` is a COHERENT removal: gone from the
registry, gone from the record type, gone from the serialized form. An earlier
revision of this entry said Java's built metadata "DOES lose the association"
and left the reader to infer an empty record-type list that reloads as
universal. The first half is right; the consequence was imported from a Go bug.
Java has no such state.

Go COPIES both — the registry map and the record-type slices — so a post-`Build`
`RemoveIndex` changes neither. That is a deliberate Go-only difference:
snapshot-at-Build rather than Java's live view. It is not obviously worse, and
it is not obviously better; what it is NOT is the port, and this entry
previously claimed it was.

Note the empty-record-type-list state that motivated so much of this work is
reachable only by copying ONE of the two and sharing the other. Go did that
briefly and it produced exactly that bug. Copy both or share both; the mixture
is what breaks.

---

## FIXED: Go assigned primaryKeyComponentPositions to multi-type and universal indexes

Kept as a record rather than deleted, because the shape is a template: a Go-only
"improvement" over Java that changes index entry BYTES is a wire break wearing
the costume of an optimisation, and the tests that shipped with it asserted the
divergence as the desired behaviour.

`primaryKeyComponentPositions` decides whether an index entry carries the
record's primary key whole or with the components already present in the index
key removed (`Index.TrimPrimaryKey`). Java assigns it at exactly one place --
`RecordMetaDataBuilder.java:1466`, its only `setPrimaryKeyComponentPositions`
call site in MAIN sources (the Java tree holds 8 more, all under `src/test/`) --
inside `for (Index index : recordTypeBuilder.getIndexes())`.
`getIndexes()` and `getMultiTypeIndexes()` are separate fields
(`RecordTypeIndexesBuilder.java:43 and :45`), and `addMultiTypeIndex` routes by arity:
zero names to `universalIndexes`, exactly one to `getIndexes()`, two or more to
`getMultiTypeIndexes()`. So Java never assigns positions to a genuinely
multi-type or to a universal index.

Go assigned them to all three, and both halves reached the wire.

**Against Java.** Two record types keyed on the same field, with a multi-type
index on that field: Go computed positions `[0]`, `TrimPrimaryKey` returned an
EMPTY tuple, and the entry key was `(price)` where Java writes `(price, pk)`.
Different bytes in FDB for identical metadata.

**Against itself, which is worse.** Universal indexes took "the first record
type's primary key" by `break`ing out of a range over `b.recordTypes` -- a MAP.
With record types whose primary keys differ, the choice varied per `Build`
inside a single process: 40 builds of one metadata produced positions 33 times
and nil 7 times. Two Go stores opened from the same metadata could write index
entries that disagree with each other, and nothing about the metadata would
explain why.

The Go-side tests covering the shape asserted the divergence directly --
"Multi-type index entries had full redundant PKs instead of trimmed PKs" was
written up as the bug being fixed. The redundancy IS the format. That is why the
divergence shipped green: the only coverage of the behaviour encoded it, and a
single shared "PK is deduped" expectation was applied across all three
registration arms in `index_registration_matrix_test.go`, so the one arm out of
three that Java actually dedups made the other two look uniform rather than
wrong. Its universal arm additionally `Skip`ped, on the true-but-irrelevant
grounds that `order_id` is not a field of `Customer` -- a fact about the key that
matrix picked, which left the arm whose behaviour was wrong unrun.

Pinned by `TestPositionsAreAssignedOnlyToSingleTypeIndexes` (all four
registration spellings, with the single-type arms as the control that keeps the
negative assertions honest) and by
`TestUniversalIndexPositionsDoNotDependOnMapIterationOrder` (40 builds, record
types deliberately given DIFFERENT primary keys, since with identical ones every
choice agrees and the map order stops mattering).

### UPGRADING BREAKS EXISTING DATA FOR THE AFFECTED INDEXES, SILENTLY

Read this before deploying. It is not a code gap; it is an operational step the
fix cannot perform for you.

The code this describes is the positions loop in `RecordMetaDataBuilder.Build`
(`pkg/recordlayer/metadata.go`), whose comment names this section back — neither
half is findable from the other otherwise, and a pointer that only runs one way
rots without anyone noticing.

**Who is affected.** An index registered as multi-type (two or more record
types) or universal, whose key expression overlaps the primary key of a record
type it covers. Single-type indexes are untouched — their positions are
unchanged.

**What happens.** `primaryKeyComponentPositions` is derived at every `Build` and
NEVER persisted: `grep -rn 'primary_key_component_positions'` over Java's
`*.proto` tree returns nothing (positive control: `last_modified_version`
returns hits). So there is no field on disk recording which layout an existing
entry used. Entries written by an older Go for these indexes are TRIMMED; this
build writes them whole and reads them with nil positions.

Reading a trimmed entry does not error, and **the symptom depends on how much of
the primary key the index key covers.** Do not go looking for only one of them:

- **Full overlap** (the index key IS the primary key). `Index.getEntryPrimaryKey`
  returns `entryKey[colSize:]` when positions are nil, and here the trimmed
  entry's length equals `colSize`, so `colSize < len(entryKey)` is false and it
  returns an EMPTY tuple. Pinned by
  `TestPreUpgradeTrimmedEntryReadsBackWithAnEmptyPrimaryKey`.
- **Partial overlap**, which is the commoner shape and does NOT look broken.
  Index key `(price)`, primary key `(price, id)`: the old positions were
  `[0, -1]`, so the old entry is `(price, id)` — length 2. Read with nil
  positions, `colSize = 1 < 2`, so it returns `entryKey[1:]` = `(id)`: a
  one-element primary key where the real one has two. Non-empty, plausible and
  wrong. An operator told to watch for empty primary keys concludes these stores
  are unaffected. Pinned by
  `TestPreUpgradeTrimmedEntryWithAPartialOverlapReadsBackAShortWrongPrimaryKey`.

**And the old entries never go away on their own.** Post-upgrade, deleting or
re-saving a record computes the UNTRIMMED index key and clears that; the old
trimmed entry sits at a key nothing touches, so an unremediated store
accumulates orphans and deletes silently fail to remove them.

**What that LOOKS like depends on the scan, and it is usually not duplicates.**
An earlier revision of this paragraph said "scans return duplicate rows"
unconditionally, which is true of the index entries and false of most rows — and
it reintroduces, one paragraph later, exactly the trap the partial-overlap
bullet above was added to prevent:

- A **raw index scan**, or a covering scan satisfied entirely from the entry,
  shows the extra ENTRY: the orphan sits alongside the correct one. The ROW it
  produces is not a clean duplicate, though — a covering row built from a
  partial-overlap orphan fills the primary-key slots from the shifted tuple, so
  it carries the wrong value in one and a NULL in another.
- A **record-fetching scan** does not get that far. The orphan's derived primary
  key is empty (full overlap) or short and wrong (partial overlap), so it
  resolves to no record, and `ScanIndexRecords` raises `RecordCoreStorageError`
  rather than skipping it — Go matches Java's default `IndexOrphanBehavior.ERROR`
  deliberately, so corruption surfaces loudly. A hard failure on a query that
  used to work is the commoner first symptom.

Watch for both. An operator told to look only for duplicate rows will read the
storage errors as an unrelated fault.

**Why no automatic guard fires.** `metadata_evolution_validator.go` compares two
BUILT metadata objects. After the upgrade both sides derive nil positions, so
`!oldHas && !newHas` and the check passes. It is structurally incapable of
seeing this, because the thing that changed was never in the metadata it
compares.

**Why it is not a version gate.** A gate needs an on-disk discriminator and
there is none; worse, the old format was not even a single format. BOTH the
universal and the multi-type paths depended on Go map iteration order — the
removed code guarded on `positions == nil` inside `for _, rt := range
b.recordTypes`, a map, so for multi-type the first covered type yielding
non-nil positions won, exactly as the first record type won for universal. Two
stores written by the same old binary could disagree. There is nothing coherent
to gate on.

**The remedy, in full, because the short version does not work.** "Bump
`lastModifiedVersion`" is the right idea and is NOT sufficient on its own. Two
earlier revisions of this section were wrong in different ways — the first said
only that, the second added the surrounding steps but still described the bump
in a form that is SILENTLY INERT.

**Which of the five apply depends on your path**, and steps 3, 4 and 5 are
universal. Step 2 is needed only when the metadata goes through
`FDBMetaDataStore` — an in-process metadata provider runs no evolution validator
at all. Step 1 applies wherever an old binary can still reach the store.

Step 5 is universal even though only PART of it is conditional, and getting that
backwards is how the whole procedure silently fails. An earlier revision said
step 5 "applies wherever the rebuild is not inline", which contradicts the trap
step 5 exists to describe: on the inline path (`recordCount <= 200`) the rebuild
runs during the open, so nothing prompts you to check — and a wrongly bumped
index was never selected, was never touched, and is sitting there `READABLE` and
stale looking exactly like a success. It is the `OnlineIndexer` RUN that is
conditional on the rebuild not being inline. The VERIFICATION is not conditional
on anything, and it is the only thing that distinguishes a migration that ran
from one that did nothing.

**On steps 3 and 4 this section defers to `cq90BumpIndexVersion`** in
`pkg/relational/sqldriver/cardinality_stale_key_rebuild_fdb_test.go`, which
calls itself THE production recipe, and to `road-to-prod.md`; where either
disagrees with steps 3 or 4 here, they are right and this section is stale.
They are NOT authorities on steps 1, 2 and 5 — `cq90BumpIndexVersion` mutates a
proto in process, so it drains nothing, never invokes the validator and never
runs `OnlineIndexer`. Reading it and finding no drain step is not evidence that
step 1 is stale; it is a test exercising the bump, not the deployment.

1. **Drain every old binary — readers included, not just writers.** Positions are
   derived at `Build`, so any old process that loads the bumped metadata
   recomputes the OLD positions. An old WRITER starts producing trimmed entries
   again, undoing the rebuild. An old READER is just as wrong and less obvious:
   given a rebuilt untrimmed entry `(price, price, id)` and old positions
   `[0, -1]`, `getEntryPrimaryKey` takes `entryKey[0]` for the first component
   and then `entryKey[1]` for the trailing one, deriving `(price, price)` where
   the true key is `(price, id)` — wrong data returned by a process that never
   writes. Stop them all before step 2, or keep the affected indexes unavailable
   to them.

2. **Allow index rebuilds on the metadata store, or the save is refused.**
   `NewFDBMetaDataStore` installs `DefaultMetaDataEvolutionValidator()`, and that
   validator returns `MetaDataEvolutionError` — "last modified version of index
   %q changed" — for precisely this edit unless `allowIndexRebuilds` is set.
   Build a validator with `SetAllowIndexRebuilds(true)` and install it via
   `SetEvolutionValidator` before `SaveRecordMetaData`.

3. **Raise the AFFECTED index's `lastModifiedVersion` PAST the metadata version,
   and raise the metadata version to match. Do not merely increment it.**

   Affected means: registered multi-type (2+ record types) or universal, AND with
   a key expression overlapping the primary key of a record type it covers.
   Single-type indexes need nothing. (This definition sat in step 4 for one
   revision, so the runbook told you to bump before telling you what to bump.)

   This is the step most likely to look done and not be. Rebuild selection is
   `idx.LastModifiedVersion > version` in `GetIndexesToBuildSince`, where
   `version` is the STORE HEADER's metadata version. An index sitting at
   `lastModifiedVersion` 3 in a store whose header is at 10 is NOT selected by
   bumping it to 4: `Build` accepts it, the validator accepts it,
   `SaveRecordMetaData` succeeds — and the open advances the header anyway, so no
   later open ever selects it either. The index is permanently skipped with no
   error anywhere.

   Leave `AddedVersion` ALONE. The evolution validator requires it unchanged, and
   only `LastModifiedVersion` drives the rebuild, so bumping both makes the
   evolution illegal for no gain.

4. **Bump ONE index at a time.** Every index named in `indexesToBuild` is
   EXCLUDED from the record-count sources that decide the rebuild policy, because
   an index being built holds no entries and would report 0 for a full store. The
   whole-store count is resolved from a UNIVERSAL COUNT index. Bump a set
   containing it and the count degrades to
   `MaxInt64`, so EVERY index lands `DISABLED` regardless of how small the store
   is, including the small stores step 5 says will rebuild inline. (A store with
   a `RecordCountKey` is immune, because that source is not filtered by the
   exclusion set — do not rely on having one.)

   Note the count index need NOT itself be affected by this bug for that to
   happen — exclusion is by membership in `indexesToBuild`, not by affectedness.
   An earlier revision said the count source was "exactly the class this bug
   affects", and a later one over-corrected by calling the selected index
   UNGROUPED. Both are wrong. `snapshotTotalRecordCount` REQUESTS
   `GroupAll(EmptyKey())`, but the request is not the index: `isGroupPrefix`
   accepts an operand whose grouping is a PREFIX of the index's, and an empty
   grouping prefixes every grouping, so a universal COUNT index grouped by
   anything is selectable and the aggregate rolls up every group. What is true
   is narrower: the count source need not be in the affected class at all,
   because exclusion is by membership in `indexesToBuild` and nothing else. The
   instruction stands on that alone, and neither claim about the index's shape
   was needed to support it.

   With more than one affected index, REPEAT steps 3 through 5 per index rather
   than batching the bumps. Each round leaves the header at the previous round's
   version, so the next round selects only the index bumped in it.

5. **Run the rebuild to completion, and verify it RAN — `READABLE` alone does not
   prove that.** Opening with the new metadata consults
   `DefaultIndexRebuildPolicy`, which returns `READABLE` (inline rebuild) only for
   `recordCount <= 200` or an index on new record types, and `DISABLED`
   otherwise; a store whose count cannot be derived reports `MAX_VALUE` and takes
   the `DISABLED` branch too. So on any store worth remediating the index lands
   `DISABLED` — not rebuilt, not readable, quietly excluded from query plans —
   until an `OnlineIndexer.BuildIndex` run finishes. That call handles
   `DISABLED` → `WRITE_ONLY` → `READABLE` itself.

   That paragraph describes the DEFAULT policy. An application that installs
   `WriteOnlyIfTooLargePolicy` lands `WRITE_ONLY` rather than `DISABLED` on the
   same stores, and one that sets `SetSkipPossiblyRebuild(true)` gets no
   transition at all, because the store never calls `checkPossiblyRebuild`. The
   procedure survives both — the rebuild still has to be run and completed — but
   the state you observe partway through will not match the wording above.

   **The trap:** if step 3 was done wrong, the index was never selected, was never
   touched, and is therefore STILL `READABLE` from before. Asserting `READABLE`
   passes on exactly the failure this procedure exists to prevent. Verify the
   rebuild by something that distinguishes ran from never-ran — the index appears
   in `GetIndexesToBuildSince(oldHeaderVersion)` before the open, or
   `BuildIndex` reports a non-zero scanned count — not by the end state alone.

**Why shipping it anyway is right.** Java assigns no positions to these indexes
either, so it applies the same untrimmed decode to a trimmed entry and derives
the same wrong primary key described above — its DECODE misreads rather than
errors, exactly as Go's does. (Its scan does not necessarily stay quiet: Java's
default `IndexOrphanBehavior` is `ERROR` too, so a record-fetching scan on the
derived key fails there in the same way. "Java does not fail on them" was a
claim about the decode stated at the scan level, which is the same slip the
duplicate-rows paragraph above corrects.) The old bytes were therefore never
Java-compatible, which is the whole point of the port. The choice is between
data that disagrees with Java forever and one rebuild.

### Filter-over-join predicate pushdown (RFC-244)

Java's `PredicatePushDownRule` matches a Select and pushes predicates whose
transitive correlations exclude its other owned quantifiers. Go's generic rule
has that shape; `PushFilterBelowJoinRule` additionally handles Filter(Select)
in the same memo. It now consumes the same complete predicate correlation
contract rather than walking selected predicate kinds and one-accessor fields.
Nested fields, whole-object values, existential predicates and range comparands
therefore cannot hide a sibling dependency. Classification uses typed owned
quantifier identities; source labels are checked separately for agreement with
their labels. Null-on-empty and strict-single edges are barriers.

The specialized two-leg rule deliberately keeps predicates referencing neither
owned leg above the join, where Java's per-quantifier rule can push them. There
is no uniquely referenced leg to choose in this specialized rule. This is a
conservative admission difference, not different SQL semantics or a second
execution pipeline. The generic Select rule remains Java-shaped.

### ChainedCursor encoding failures (intentional error-path difference)

Java's `ChainedCursor.onNext` advances `lastValue` before
`RecordCursorResult.withNextValue` validates the continuation. A missing encoder
or an encoder returning null fails that result; calling `onNext` again can then
advance past the generated value that was never emitted.

Go's `chainedCursor.continuationErr` in `pkg/recordlayer/chained_cursor.go`
deliberately latches a `ContinuationEncodeError` instead. Library callers receive
an explicit error rather than a panic, and retrying cannot invoke a stateful
generator again and silently skip an un-emitted value. Generator errors retain
Java's non-latched behavior; an exhausted generator never needs an encoding.

This changes only invalid-encoding error handling, not valid continuation bytes,
record formats, or index formats. `TestChainedCursorNilEncode` pins rejection and
single generator invocation both directly and through Concat, and
`TestChainedCursorEmptyWithNilEncode` pins the exhausted-generator control.

### Projected existential over an independent outer block (RFC-256)

Go's live-existential partition guard excludes a result-live existential from a
lower partition containing another quantifier. For a select with at least two
ordinary independent ForEach sources and exactly one result-projected
existential, that can leave the canonical outer cross product as the only
implementable split. `PartitionSelectRule` now preserves exactly that split
(all ordinary ForEach below, sole projected existential above) through both
cross-product deferral and disconnected-lower pruning. Hard dependencies,
cycles, null-on-empty/strict-single sources, additional existential/physical
quantifiers, liveness and exact-row checks retain their exclusions.

Java's `PartitionSelectRule` also has cross-product deferral; this is NOT a claim
that Java bypasses that configuration. It is a bounded Go search-admissibility
difference necessitated by Go's narrower live-existential lowering alternatives.
The retained `DerivedSourceReference` JVM test proves same-alias, star/empty-body
and both-live-outer-row EXISTS queries; direct rule tests drive both deferral
settings and verify the exact canonical lower block. See RFC-256's final sections
for the rejected broader exemptions, reference outcomes and verification scope.

## GuardiANN: a refused deferred insert writes no vector identity

Java writes the vector's identity row (`VectorMetadata`, subspace tag 5) before the
deferred-mode hard-cap check (`Insert.java:293`, `:330-338`), so a caller that catches
`ClusterCapacityExceededException` and commits keeps an identity with no reference, and a
later insert of that key is a no-op until the record is deleted. Go checks the cap first
(`insertIntoClusters`, `guardiann_ops.go`); nothing between the two positions reads identity
rows and the identity UUID is not drawn from the operation RNG, so every successful insert
writes Java's bytes. Pinned by "GuardiANN deferred hard cap".

## GuardiANN refuses a maintenance task that would loop forever

A split, merge or reassign task without precomputed neighbours fetches them (width
`guardiannSplitNumNearestClusters`, `guardiannMergeNumNearestClusters`, or 1 +
`guardiannReassignNumNeighboringClusters`, pipelined at `guardiannSplitMergeConcurrency` /
`guardiannReassignConcurrency`) and writes itself back, at high priority, carrying them. Java stores
"not fetched" and "fetched none" as the same empty list, so a width or pipeline below 1 writes the
same task back forever and starves the work behind it (measured: `splitNumNearestClusters 0` grows
one cluster to 30 primaries; `mergeNumNearestClusters 0` leaves empty clusters with work pending).
Go raises a typed `VectorCapabilityError` at the point Java writes that task; it poisons the
transaction like any task error, so the task stays queued. The neighbour counts are immutable, so
such an index's splits (or merges, or reassigns) fail until it is rebuilt; the concurrencies are
mutable and evolving them repairs it. Where Java instead throws (a concurrency below 1 handed to
`MoreAsyncUtil.forEach`: a task with precomputed neighbours, collapse, bounce, delete), Go throws the
same `IllegalArgumentException` message at the same statement. RFC-257 WS-D declared (c); pinned by
"GuardiANN knob consumers".

## GuardiANN splits what Java cannot

Where Java's split or merge task throws, and so fails the same way on every later run, Go ends it.
In deferred mode Java strands the cluster at the hard cap; inline, every later insert into the
partition fails. RFC-257 WS-D declared (h). Pinned by "GuardiANN unsplittable clusters" and
`TestGuardiannPeelAdmission`.

- **Candidate with fewer cleaned vectors than k.** Java hands it to `KMeans.fit`, which throws
  (KMeans.java:136), even beside a feasible candidate. Go scores it INVALID without calling KMeans.
- **Split with no usable candidate and no collapse.** Java throws at orElseThrow
  (SplitMergeTask.java:397), for example on a lone cluster whose KMeans isolates a group smaller than
  `minChildFraction` (10 points and 1 outlier).
  - Go first runs an iterated outlier peel. Each round removes the undersized child from the fitted
    mass; at least 2^(r+1) - 1 members have left by round r. It refits k = 2 on what remains,
    reassigns every primary and takes the first partition that is not INVALID. That is at most
    floor(log2(n - 1)) refits.
  - The peel runs only when its work floor(log2(n-1)) * n * d * max(I*(R+1), 32) / 32 is at most
    1.96e7, which covers n = 2000 up to d = 980.
  - Otherwise Go reconciles the cluster in place: it removes stale references, recomputes counts and
    statistics (the stored distance maximum is never lowered), clears SPLIT_MERGE and applies the
    ordinary merge rule.
  - If the reconciled count is above `primaryClusterHardMax`, Go fails with
    `ClusterUnsplittableError`. This error poisons the transaction and is deliberately not the insert
    cap's capacity error.
  - The reconcile reads the replicas' identities too, which adds read conflicts Java's task does not
    have.
- **New split/merge child with no primary.** Final ownership against the new and the neighbouring
  centroids can leave a child with no primary; Java asserts against this (SplitMergeTask.java:909).
  Go drops that child before any write and recomputes replicas without it. The cause set stays every
  minted id, and the bounce names only the surviving children.
- **Merge whose core holds no live primary.** Java calls KMeans with k = 1 on nothing. Go keeps the
  lowest-UUID core cluster, empty and stateless with its lifetime peak, and deletes the rest of the
  core. It force-reassigns the neighbours and lets the kept cluster take the ordinary undersized-merge
  rule, so deleting everything ends at one retained empty cluster.

## GuardiANN keeps primaries and underreplication counts exact

In each case below Go matches the invariant, not Java's drift. RFC-257 WS-D declared (h); pinned by
"GuardiANN reference counts" and the structure test's per-cluster check.

- **Underreplicated count.** Java never decrements a cluster's underreplicated count when an
  underreplicated primary is deleted. It also drops an underreplication-only metadata change
  (Primitives.java:1254 writes only on a primary or replica delta), so the count can exceed the
  cluster's primaries. Go keeps it equal to the physical underreplicated primaries.
- **Reference dedup.** Java's cleanup dedup (`mergeVectorReference`) replaces an earlier primary
  with a later replica of the same vector, so a repartitioning can lose the primary. Go keeps the
  primary in either encounter order.

## GuardiANN inline deletes skip a head task Go would refuse

Java's inline delete runs the partition's head task first and fails whenever that task throws, so
one bad task blocks every inline delete of the partition. Go decides first, at snapshot isolation,
whether a Go consumer would refuse the head task (`consumerOutcome`, which walks the task kind's
prologue in Java's statement order: the no-op exits, the false alarm, the neighbour fetch and its
declared (c) refusals, forEach's parallelism, and for a bounce the dependency its own RNG picks). If
so, the delete runs no task and succeeds. Otherwise it runs the task exactly as Java does.

- The skipped task stays queued and counted; the next drain meets the refusal.
- A task the KMeans knobs would refuse only if some candidate has at least k vectors is skipped
  conservatively; the prologue cannot know the cleaned populations.
- A skipping delete adds no read conflict on the queue head (Java's serializable head read does).

RFC-257 WS-D declared (d); pinned by "GuardiANN inline delete and the head task's consumer outcome".

## GuardiANN refuses inserts no search can find

With `guardiannInsertMaxCandidateClusters` below 1 Java writes an inserted vector's identity and no
reference, so no search ever returns it, and the option is immutable (measured: `found=0`). Go
refuses the insert with a typed `VectorCapabilityError`, at the engine and at pending-queue enqueue
(so a queued index refuses the save instead of holding an entry no replay can apply), and poisons
the transaction (`vectorCapability/<store>/<index>` commit check) so the record write cannot commit
without its entry. Deletes, `deleteWhere`, clear, disable and drop still work. RFC-257 WS-D
declared (b); pinned by "GuardiANN insert admission".

## HAVING without GROUP BY over a select list without an aggregate (upstream bug)

SQL treats a query with HAVING and no GROUP BY as one group: `SELECT 7 FROM t HAVING COUNT(*) > 0`
returns one row when the predicate holds. Java 4.14.2.0 returns one row per input row (three over a
three-row table) and no row over an empty one, where `COUNT(*) = 0` holds. A query whose select list
has an aggregate (`SELECT COUNT(*) FROM e HAVING COUNT(*) = 0`) agrees in both engines. Go answers the
SQL result. Both engines' answers are pinned in `conformance/count_on_empty_conformance_test.go`.

## HNSW efSearch beyond memory

Java's `Search.beamSearchLayer` sizes its result queue `new PriorityQueue<>(efSearch + 1)`, so a
window option `EF_SEARCH = 2147483646` fails with `OutOfMemoryError` and `2147483647` with an
`IllegalArgumentException` (the capacity overflows). Go bounds the up-front allocation
(`hnswSearchCapacityHint`, `hnsw.go`) and searches the whole layer, answering the k nearest rows.
Every value either engine accepts searches the same beam; both answers are pinned in
`conformance/window_options_conformance_test.go`.

## A commit inside a transaction route's body is refused before it lands

A context that `FDBDatabase.Run`, its variants or `FDBDatabaseRunner.RunWithRetry` hands its body
belongs to that route, which commits it. If the body commits it itself, Go refuses that commit
with Java's `RecordContextNotActiveException` class and message ("Transaction is no longer
active.") before the commit checks, so nothing lands. Java lands the body's commit and then fails
the runner's own commit in `ensureActive` with the same class (FDBRecordContext.java:479-481,
531, 548-551), reporting a durable commit as a failure. As in Java, any context is deactivated by
its first commit whatever the outcome, and a second commit is refused with that class. Pinned by
`record_context_active_fdb_test.go`.

## An auto-continuing cursor also retries a transaction timeout

`AutoContinuingCursor` retries what Java's `FDBExceptions.isRetriable` retries (a
`RecordCoreRetriableTransactionException`, or the first FDB error in the chain when it is
retryable), and Go adds transaction_timed_out (1031), which that predicate excludes: the cursor
resumes from its saved continuation in a fresh transaction, so a scan that runs into FDB's
five-second limit before the application's own time limit continues where it stopped instead
of failing. The transaction runner's rule (`isRetriableAnyCause`, any cause in the chain) does
not retry 1031, as the target's does not.

## Run's bounded attempts: the Go-only parts

`FDBDatabase.Run`, its variants, `RunRead` and `FDBDatabaseRunner.RunWithRetry` retry as Java's
runner does (`FDBDatabaseRunnerImpl.RunRetriable`: at most `MaxAttempts`, default 10, while any
cause is retriable, with `ExponentialDelay` between attempts; each attempt a fresh transaction
whose backend retry limit is 0). What Go adds or leaves out:

- An SPFresh foreground write that meets only SEALED postings raises `SPFreshSplitWindowError`
  (it wraps not_committed, 1020), and the loop retries it without counting an attempt, bounded by
  the caller's context: RFC-094's foreground contract that a write re-runs until the split
  publishes. It still takes the delay. The target has no SPFresh.
- Under a simulated environment (`dst.NewSim`) the delay is drawn, so the seeded stream matches a
  real run's, and not waited: the simulation does not model the retry delay's time, and nothing
  persisted reads it. Since that would make a stalled seal a hot loop, a simulated run fails the
  call with `SPFreshStalledSealError` after 100 consecutive retries that met the same sealed
  postings (SimFDB's former retry backstop). A real environment has no such bound.
- SPFresh's background lifecycles (`spfreshRun`) keep the transactor's own retry loop
  (`runClientLoop`), unbounded on the pure-Go client and libfdb_c and capped at 100 retries on
  SimFDB.
- A transactor passed to `NewFDBDatabaseWithTransactor` that does not implement
  `AttemptTransactor` is called once per attempt and sees every attempt, but not the call's
  identity (`AttemptCall`).
- A context that ends between attempts returns an error wrapping both the context's error and the
  last attempt's; Java has no context.

Pinned by `attempt_loop_test.go` and the hunt's exhaustion fixtures (`exhaustion_test.go`).

## A vector search holds its partition lock for the search, not the cursor

Both engines read-lock `LockIdentifier(partitionSubspace)` for a vector scan, the key a write to
that partition write-locks. Java's `AsyncLockCursor` keeps the read lock until the cursor is
exhausted or closed; Go releases it once the search has materialized the page
(`searchOnePartition`). The page is already materialized in Java too, so what a scan returns is the
same; the longer hold only delays a same-context writer, which in synchronous Go would deadlock
shapes that work (a LIMIT abandoning the vector cursor, `INSERT ... SELECT` over a vector scan,
UPDATE/DELETE with a vector subquery whose cursor is open). Pinned by
`vector_partition_lock_fdb_test.go`.

## Named macro arguments (upstream bug)

Java 4.14.2.0 validates a named macro call by name (`UserDefinedFunctionCatalog.lookup`) but
`SemanticAnalyzer.resolveFunction` then re-wraps the values with `CallSiteArguments.withArguments`,
which turns `NamedArguments` into `PositionalArguments` in call order: `st1_d(z => 5, y => 4)`
binds `y = 5, z = 4`. Go binds each value to the parameter it names. A built-in ignores names in
both engines. Both answers are pinned in `conformance/named_call_conformance_test.go`.

## Recursive CTE rows that do not fit the seed

Java types a recursive CTE's temporary table by the seed's row (`SemanticAnalyzer.getRecursiveCteType`)
and writes later iterations into it unconverted, so a mismatch surfaces only where something reads
it: an INT written into a BIGINT or DOUBLE column fails with an internal `IllegalArgumentException`
when read, and a NULL in a NOT NULL column survives until a record is built from it (a `COUNT(*)`
over it answers, and `IS NULL` on it is folded to false). Go keeps the same seed-typed row but fits
each recursive value when it is written (`recursiveSlotValue`, `values.NarrowValue`): it promotes
what promotes, and refuses a NULL into NOT NULL or a value of another type with XXXXX at once. Both
answers are pinned in `conformance/recursive_column_list_conformance_test.go`.


## Long-arithmetic key candidates (WS-J 3.5)

A value index over a long-arithmetic key function (`d & 1`, `d + 1`,
`bitmap_bucket_offset(id)`) is a match candidate whose column Value is the
arithmetic of the same logical operator, as Java's
`LongArithmethicFunctionKeyExpression.toValue` builds it
(`cascades/key_expression_expansion.go`, `functionKeyToValue`). Go declines two
keys Java expands:

- **An argument that is not INT or LONG.** The maintainer stores
  `getNullableLong` of each operand (truncated toward zero), so the entries are
  not the value of the query expression `d + d` over a DOUBLE. Java matches the
  index anyway and answers `d + d = 3` with no row where the record's `d` is
  1.5; Go answers from the records (conformance "WS-J long arithmetic key
  functions over non-integer operands", `wsjNonIntGoPins`).
- **A key `encapsulate` refuses**, such as the `long_value` bitmap entry size a
  Go build before the literal-carrier fix stored (the bitmap functions have no
  (LONG, LONG) lane). Java's `VerifyException` escapes candidate expansion
  (`MatchCandidateExpansion` catches only `UnsupportedOperationException`) and
  fails every query of the table; Go plans the query from the other access
  paths. Any other expansion failure of a stored key, which neither engine's DDL
  produces, declines too: Go builds candidates over every table, so failing
  would refuse queries of tables that do not hold the key.

## Macro routine description

Java's `SchemaTemplate.getInvokedRoutines()` describes a stored macro function by
`UserDefinedMacroFunction.toString()`, which the class does not override, so the
description is the object's class name and identity hash, different on every load
(`RecordMetadataDeserializer.generateInvokedRoutineBuilder`). Go describes a macro by
its function name. A SQL-bodied function and a view are described by their stored
definition in both engines.

## Constant-fold defects and fold ties (WS-E 5.4(g)/(h))

Java 4.14.2.0 fails on some well-typed predicates its simplification touches, with internal
errors; Go answers them:

- `WHERE COALESCE(TRUE, 1 / 0 = 1) AND n > 0` and `WHERE NOT COALESCE(FALSE, 1 / 0 = 1)`: Java
  raises XX000 `VerifyException` (upstream bug). Go plans both. The AND row keeps the fold on the
  conjunct count and answers the rows with `n > 0`. The NOT row is a hash tie that keeps the
  unfolded `NOT (COALESCE(FALSE, ...) = TRUE)`, and a COALESCE evaluates every argument as Java's
  does, so it raises 22012.
- `WHERE id IN (1, 2) AND 1 / 0 = 1 AND 1 = 2`: Java raises XXXXX `VerifyException` beside the IN
  list; Go raises the division (22012), as Java does for the same shape without the IN list.
- `WHERE CASE WHEN id > 0 THEN 1 ELSE 1 / 0 END IS NULL`: Java raises 22000 (INCOMPATIBLE_TYPE on the
  CASE branch); Go evaluates the CASE per row, and every row takes the literal branch, so no rows.

A fold to NULL ties the unfolded predicate on every cost rung but the semantic hash. Java decides
by `semanticHashCode`, so its answer for `WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1` depends on the
schema: `FILTER null` and no rows over a two-table template, the division over a seven-table one.
Go's walker collapses arithmetic over a typed NULL when it builds the value, so its unfolded member is
`NULL = 1` and both members answer no rows: the hash picks Go's plan, never its answer. Go answers no
rows over both templates, so it differs from Java on the template where Java raises the division.
`COALESCE(1 / 0, 5) IS NULL` folds to FALSE when Go builds it, with no tie, matching the answer Java's
prune gives. The rows are declared in `wseGoDivergences`
(`conformance/ws_e_probe_conformance_test.go`); Go's answers and plans are pinned by the WS-E
oracle's Go pins and `TestFoldTiePins_DecidingRung` (`pkg/relational/core/embedded`), which also
records the rung that decides each row.

## Full index reads under PREFER_INDEX (F-7c): what still differs

Go keeps a full index scan with no search argument as an access path, as Java does, and PREFER_INDEX
ranks it against the primary scan; both engines read the same index for the queries measured
(TODO.md, WS-F F-7c). Plan-level differences that remain, answers equal:

- A LEFT JOIN leg whose null-on-empty flag a WHERE predicate eliminated keeps its nullable row in the
  select's result value, in both engines. When PartitionSelectRule merges such a leg, Java's merge slot
  takes the leg's NOT NULL row and its Reference accepts the narrower member; Go's memo refuses a member
  whose type differs from its group's, so Go's slot keeps the nullable row, and Go's executor admits the
  leg's NOT NULL rows under that nullable carrier (`PositionalRow.checkLayoutAttachable`, the only type
  disagreement it admits).
- Java skips a data-access match that satisfies none of the requested orderings
  (AbstractDataAccessRule.java:660-662); Go keeps it, because the request sets Go passes differ from
  Java's (the check alone moves 336 corpus plans). Under an ORDER BY no probe provides, Java cannot
  plan the query and Go's in-memory sort needs the leg's probes, so the skip waits on the sort
  requesting PRESERVE below it.
- Among several full index reads that tie, the engines may pick different indexes: `SELECT id,
  COALESCE(customer_id, 0) FROM orders` reads `IDX_AMOUNT` in Java and the covering `IDX_CUSTOMER` in
  Go; `SELECT COUNT(*) FROM t` over IDX_A and IDX_B reads `ISCAN(IDX_A <,>)` in Java and the covering
  IDX_B in Go, and `SUM(a)` reads IDX_B in Java and IDX_A in Go.

## Implementation rules that still choose a child (F-8)

Java's implementation rules pre-select no child plan: each yields per plan partition over a reference
restricted to the partition's plans, and the planner's group optimization chooses. Go's type-filter,
insert, temp-table insert, intersection and recursive (DFS join, level union) rules do the same
(TODO.md, WS-F F-8). Two rules still choose at rule time:

- `ImplementLimitRule` has no Java counterpart (Java SQL has no LIMIT). It implements LIMIT over the
  cheapest child plan satisfying each requested ordering, the approved Go extension.
- `ImplementNestedLoopJoinRule` takes the leg groups' preserve winners (for an existential select's
  outer, also the cheapest member per ordering requested of the outer group, Java's per-ordering
  roll-up). Java rolls the existential inner
  up into one partition, so its FirstOrDefault ranges over every inner plan and that reference's own
  cost comparison resolves it; Go's correlated-inner rewrites need a concrete plan, so the rule takes
  the same local choice with the same comparator. Yielding per inner member instead moved 3 golden and
  15 factory plans, several to a fetch before the residual, because the choice then moves to the
  parent group; per outer member exhausts the task budget on a union-heavy outer.

## Queued index states require format 15 and a queue-capable index

Java's `markIndexWriteOnlyWithQueue` and `clearAndMarkIndexWriteOnlyWithQueue` write state 4
(WRITE_ONLY_WITH_QUEUE) for any index at any format version (`FDBRecordStore.markIndexNotReadable`).
That includes a rebuild policy that answers WRITE_ONLY_WITH_QUEUE
(`rebuildOrMarkIndex`). A store below format 15 has no pending-write queue, and an index whose
maintainer cannot replay queued entries (or whose key has version columns) can never drain one, so
such an index stays queued forever. Go refuses the request instead: `MarkIndexWriteOnlyWithQueue`,
`ClearAndMarkIndexWriteOnlyWithQueue` and the open-time rebuild return
`UnsupportedFeatureForFormatVersionError` below format 15, and a `RecordCoreError` ("index does not
support queued writes") for an index that cannot be queued. Nothing is written. Pinned by
`rebuild_write_only_with_queue_test.go`: the format-15 rebuild clears and queues the index, and the
format-14 open is refused and leaves the store at its old metadata.

## Pending-queue overflow disables every overflowing store's index

Java registers its disable-on-overflow commit check as `DISABLE_INDEX_COMMIT_HOOK + index name`
(`IndexingPendingWriteQueue.registerDisableOnOverflowCommitCheck`, :213), with no store in the key.
A transaction whose two stores have same-named queued indexes that both overflow therefore gets one
check: the first store's index is disabled, and the second store keeps a full queue that refuses every
later write until something drains it. Go keys the check by store subspace and index name, so each
overflowing store's index is disabled. Pinned by `index_queued_dispatch_test.go`, "disables an
overflowing index of each of two stores in one transaction" (keying by name alone reddens it).

## A connection to a database that does not exist yet opens; Java's connect refuses it (Go-only reach)

**Java.** `EmbeddedRelationalDriver.connect` asks each storage cluster's `loadDatabase`
(`RecordLayerStorageCluster.java:102-124`). That returns `null` for a missing database.
With a `?schema=` it does so after `loadSchema` reports UNDEFINED_SCHEMA and
`doesDatabaseExist` is false. The driver then throws 42F00 `Database <path> does not
exist` from `connect` itself (`EmbeddedRelationalDriver.java:62-94`).

**Go.** The driver opens the connection without reading the catalog. So a program may
connect to `fdbsql:///MYAPP` and run `CREATE DATABASE /myapp` on it. The first statement
that needs the schema reports what Java's connect reports. `loadSchemaOfDatabase`
(`pkg/relational/core/embedded/connection.go:526`) turns UNDEFINED_SCHEMA over a missing
database into 42F00 `Database <path> does not exist`: the same code and message, one call
later.

A refusal at connect time was written and then removed. The Go examples, the `frl` CLI
and the test harnesses create their database through the connection they open. The
refusal would have taken that away for no difference on the wire.

**Names on both sides.** The DSN's path and `?schema=` value are taken as given, as Java
takes them (`parseConnectionQueryString` upper-cases the option name, not the value).

DDL folds unquoted identifiers, and folds a database path as a whole:
- `CREATE DATABASE /test/x` stores `/TEST/X`;
- `CREATE SCHEMA /test/x/s1` stores (`/TEST/X`, `S1`).

So a connection must name in upper case what unquoted DDL created.

Pinned against the JVM by `conformance/ws_j_schema_dsn_conformance_test.go:31`
("RFC-257 the DSN's schema option reaches the schema Java's does"). The spec covers:
- the missing-database arm;
- a bare `DROP SCHEMA`, refused 42F63 both on a database connection and on the catalog,
  as Java's `visitDropSchemaStatement` refuses a uid that names no database;
- template names written unquoted in lower and mixed case, and quoted in upper and mixed
  case. They are folded as Java's `visitUid` folds them; a quoted mixed-case name is
  reached only by its quoted spelling.

**`SHOW DATABASES WITH PREFIX` is honoured.** Java reads the prefix
(`MetadataPlanVisitor.visitShowDatabasesStatement`, the same `visitUid` fold as DDL). Its
`CatalogQueryFactory` then lists every database anyway ("TODO(bfines) make use of this
prefix", `CatalogQueryFactory.java:47`).

Go lists only the databases under the prefix. The match is per path segment, and the
prefix is folded the way a DDL path is, so a lower-case prefix finds what unquoted DDL
stored. This is read-side only.

Pinned on both sides by the conformance spec "RFC-257 SHOW DATABASES WITH PREFIX: Java
lists every database, Go the prefix" (`ws_j_schema_dsn_conformance_test.go:300`); Java's
listing holds `/__SYS` for any prefix. Pinned in Go by `TestFDB_MultiTenantCatalogScoping`
(`pkg/relational/sqldriver/mt_catalog_scope_fdb_test.go:134`).

**`SHOW DATABASES` without a prefix lists the connection's database scope.** Java lists
every database, whatever the connection (the same `CatalogQueryFactory` listing). Go
limits a bare `SHOW DATABASES` to the session's database path. This is the tenant scoping
of `docs/mt-saas.md` ("`SHOW DATABASES` is scoped the same way").

Go refuses it with 08F01 when the connection has no usable path. No spelling lists every
database, because `WITH PREFIX /` is refused as a path (`validateDatabasePath`,
`pkg/relational/core/embedded/ddl.go:1377`). This is read-side only: the listing reads
the catalog Java writes.

Pinned in Go by:
- `TestFDB_MultiTenantCatalogScoping` (the scoped listing);
- `TestShowDatabasesWithNoScopeIsRefused`
  (`pkg/relational/core/embedded/ddl_scope_guard_test.go:183`), the 08F01 on a session
  with no path or the bare root. The SQL driver cannot open such a session, since
  `ParseDSN` refuses a DSN without a path.

### DeleteStore cancels pending replacement retirement (RFC-257 WS-B)

After a store is deleted in the same context, Java 4.14.2.0 can recreate an original
index's DISABLED state key. Its named callback for retiring a replacement index survives
the clear and runs at commit.

The live Java probe `probeDeleteWithPendingReplacementRetirement` (pinned by "pins Java
deletion with a pending replacement retirement callback",
`conformance/store_lifecycle_conformance_test.go:212-225`) observed exactly one remaining
row, no header, and original state 2.

Go's `DeleteStore` (`pkg/recordlayer/store_api.go:196-219`) cancels that subspace's
pending work before clearing:
- the retirement commit check (`replacementRetirementCheckName`, `index_state.go:510`);
- the subspace's pending-write commit checks;
- the retirement metadata in the session.

Cancelling marks the registration inactive (`removeCommitCheck`, `database.go:1142-1161`).
`runCommitChecks` skips an inactive entry even when it is already in the snapshot it is
iterating (`database.go:1177-1193`).

This corrects the boundary; it is not exact parity for Java's defective sequence. It does
not change record or index wire encodings.

Go tests (`pkg/recordlayer/index_state_test.go`) cover:
- deleting before commit and from an earlier commit check, asserting the whole store
  range is empty ("does not resurrect deleted stores before commit" / "... from an
  earlier commit check", :2154-2183);
- that only the deleted subspace's retirement is cancelled ("cancels retirement only for
  the deleted subspace", :2127);
- that a recreated store does not reuse the old callback (:2077).

The upstream report is unpublished.

### checkAnyOngoingOnlineIndexBuilds follows Java's documented contract, not its arithmetic

Java documents `OnlineIndexer.checkAnyOngoingOnlineIndexBuildsAsync(store, index)` as
true when a session heartbeat is "less than DEFAULT_LEASE_LENGTH_MILLIS old"
(OnlineIndexer.java:442). But it computes `heartbeatTime < now + lease` over
`getIndexingHeartbeats` (OnlineIndexer.java:458-463), where an unparseable heartbeat
carries time 0.

**Go's predicate.** Go's `CheckAnyOngoingOnlineIndexBuilds`
(`indexing_heartbeat_admin.go:151`), and the `OnlineIndexer` method (`:215`) and store
helper (`:170`) around it, answer with session admission's own predicate,
`heartbeatBlocksSession` (`indexing_heartbeat.go:116`). A heartbeat counts when it parses
and `-1 day < now - heartbeatTime < lease` (IndexingHeartbeat.java:106-107). This is the
same function admission calls, applied to each indexer's surviving heartbeat (below).
- Over one key per indexer id, "ongoing" therefore means "a new exclusive session would be
  refused". (Mutual admission refuses no live peer, IndexingHeartbeat.java:90-93.)
- Over two keys for one id, the two can differ, because admission reads every key.

**Key parsing.** Every heartbeat key is parsed first, as Java's `getIndexingHeartbeats`
does (`heartbeatKeyToIndexerId` outside its try, IndexingHeartbeat.java:148). A non-UUID
key is an `IndexingHeartbeatKeyError` (`indexing_heartbeat.go:224`) in Go and a throw in
Java. Measured in Java: a ClassCastException from a String key, which sorts before every
UUID key, and from a Versionstamp key, which sorts after them. This holds even beside a
live heartbeat. Pinned by the Go spec "reports an ongoing build by admission's predicate
over each indexer's surviving heartbeat" (`indexing_heartbeat_admin_test.go:155`) and by
the JVM pins in the conformance spec below; a single-pass parse that stops at the first
live heartbeat fails both. As in Java, only element 0 is read, so a (UUID, x) key is that
UUID's heartbeat in both engines (measured).

**A (U) key and a (U, x) key together** name ONE indexer. Java's `getIndexingHeartbeats`
keeps the later key's value (a `HashMap.put`, IndexingHeartbeat.java:151), and that is
the only value its ongoing check reads. Go's check reads the same collapsed population
(`collectIndexingHeartbeats`, `indexing_heartbeat_admin.go:56`). So (all measured; keeping
the first key's value instead fails the Go line of the JVM spec and the Go spec):
- a live (U) beside a (U, x) a day ahead is not ongoing in either engine;
- the reverse pair is ongoing in both;
- a live (U) beside an unparseable (U, x) is the invalid-only population below: Java says
  ongoing, Go does not.

Admission checks each key on its own in both engines (IndexingHeartbeat.java:94-125), so
it is unaffected: it refuses a new exclusive session over the live (U) in both pairs. Go's
admission fails closed on the same keys.

**Where the engines disagree.** Over the UUID-keyed heartbeats that survive the collapse,
they disagree on three populations. All are pinned against the JVM by the spec "matches
Java's heartbeat administration, and pins Java's ongoing answer on every declared
population" (`conformance/index_state_conformance_test.go:194`):
- a stale heartbeat left by a crashed session (one hour old): Java true forever, Go false;
- an invalid-only heartbeat: Java true (time 0), Go false (admission ignores it,
  IndexingHeartbeat.java:118-124);
- a heartbeat dated more than the lease but less than a day ahead (one hour): Java false,
  Go true (admission refuses a session against it).

They agree on live heartbeats, which is every case Java's own tests exercise
(OnlineIndexerSimpleTest.java:1007/1020, OnlineIndexerBuildIndexTest.java:305). They also
agree on a heartbeat more than a day ahead, which both treat as bad data. If Java changes
its arithmetic, the spec fails and this entry is revisited.

Reads, the maxCount valve and `clearIndexingHeartbeats` agree exactly on the same state.
No wire bytes are involved. The upstream report is unpublished (publication is not
authorized).

### OnlineIndexer session start and build catcher: where Go differs (RFC-257 WS-C)

Session start and the build catcher follow Java (`IndexingBase.handleStateAndDoBuildIndexAsync`,
`OnlineIndexer.indexingCatcher`). Go differs deliberately in the places below, each
pinned where named.

**A clear or a fresh WRITE_ONLY mark admits no live peer, mutual or not.** Java admits
mutual peers based on the stamp's method, a REBUILD included. Go admits a live mutual
peer only to a CONTINUED mutual build, because it never resets state another session is
building (`prepareIndexingState`, `online_indexer_queue.go:135`). Pinned by "admits a live
mutual peer to a continued mutual build but not to a mutual REBUILD"
(`online_indexer_test.go:4207`).

**An out-of-date stored metadata version requires quiescence at open.** Go's indexer
opens the store through the ordinary open, which reconciles the metadata and may rebuild
or disable indexes. `checkOpenHeartbeats` (`online_indexer_queue.go:85`) then refuses any
other live heartbeat. When the version is current, it refuses only legacy and malformed
heartbeat keys.

Java's indexer also reconciles on open, without looking at heartbeats: it opens through
`openAsync` (`IndexingBase.java:129-130`), which runs `checkVersion`
(`FDBRecordStore.java:6015`) and `checkPossiblyRebuild` (`:2689`, `:4841-4986`). So Java
reconciles under a live peer, while Go refuses until the peer's lease expires (the
metadata cells of the admission matrix).

**With a live peer, the lock error wins over a stamp conflict.** Go admits a session
before any of its writes, so a refused session buffers nothing. Java checks each target's
stamp before its heartbeat (`IndexingBase.java:459-460`).

Take a live peer that the admission check refuses (every live peer of an exclusive
session, and of a mutual session that is not continued), holding the index, with a stamp
that does not match:
- Go reports `SynchronizedSessionLockedError` on the first attempt.
- Java reports it only where its catcher's path ends on a heartbeat check: a stamp it
  continues, or a REBUILD retry, whose next session then meets the peer's heartbeat.
- Everywhere else Java ends as its catcher does. That includes:
  - the `PartlyBuiltException` of a blocked stamp, or of a MULTI_TARGET stamp the
    takeover rules refuse (each after six sessions);
  - the `PartlyBuiltException` of a MUTUAL stamp under CONTINUE;
  - the `PartlyBuiltException` of any mismatch under ERROR or MARK_READABLE;
  - the catcher's decode or metadata error for a saved BY_INDEX source that does not
    resolve.

A live mutual peer admitted to a continued mutual session is not refused, so there Go
raises the stamp error, as Java does. A follower whose state differs from the primary's
is refused before admission, as Java refuses it before its heartbeats.

**The catcher's retries keep the adapted throttle limit.** Java builds a new indexer per
attempt, and its throttle starts again from the configured limit. Go's next attempt
continues from the limit the previous one adapted to (`buildIndexAttempt`,
`online_indexer.go:1383-1388`).

**MARK_READABLE over a queued index with a non-empty queue fails.** Java's store erases
the pending write queue as it marks the index readable, losing the queued writes. Go's
`MarkIndexReadable` refuses a non-empty queue (`IndexNotBuiltError{PendingWrites}`,
`index_state.go:926`). A session that did not drain that queue returns the refusal at
once. Pinned by "fails a MARK_READABLE session over a non-empty queue at once rather than
retrying a drain it never runs" (`index_queued_dispatch_test.go:440`).

**A target disabled under the session fails its next drain or merge transaction.** Java's
follow-up heartbeat update skips a target that is not write-only
(`IndexingBase.java:969-972`) and commits. What fails is whatever runs next:
- a following build transaction's state check (`RecordCoreStorageException` "Unexpected
  index state(s)");
- or, after the last range, `markIndexReadable` (`IndexNotBuiltException`);
- or, with `SetMarkReadable(false)`, nothing: Java's session then succeeds after the last
  range and leaves the target DISABLED.

Go's follow-up (`refreshFollowupHeartbeats`, `online_indexer_queue.go:441`) refuses at the
first transaction that sees the state, with the state check's class and message, and
commits nothing.

**A standalone `MergeIndexes` holds a session.** Java creates a heartbeat only in a build
session's stamp step (`IndexingBase.java:457`). Its standalone merge
(`OnlineIndexer.java:409-419`, `IndexingBase.java:1085-1096`) therefore passes the
follow-up with no heartbeat (`IndexingBase.java:969-972`): it writes none, checks no index
state, and proceeds under a running build.

Go's `MergeIndexes` (`indexing_merger.go:174-178`) runs as a session under the indexer's
identity, with heartbeat info "explicit index merge". That is Go's own choice, not
something WS-C requires. WS-C makes every backend write transaction consume the heartbeat
callback, and Java's callback over a null heartbeat does nothing, which Go could have
ported. Go instead has the merge hold an exclusive session.

Consequences, each pinned by a test in `indexing_merger_test.go`, Describe "a standalone
MergeIndexes session" (:208):
- Over a WRITE_ONLY target, the merge writes a live heartbeat key and clears it afterwards
  ("writes its heartbeat over a WRITE_ONLY target for the merge and clears it", :252).
- An EXCLUSIVE builder that starts meanwhile is refused by that key, a Java one included,
  and so is a fresh Go mutual session.
- A mutual builder is not refused. Java's heartbeat check under `allowMutual`
  (MUTUAL_BY_RECORDS and SCRUB_REPAIR) only writes its own key
  (`IndexingHeartbeat.java:88-93`, `IndexingBase.java:454-457`). Go's continued mutual
  session skips every peer (`checkAdmission`, `indexing_heartbeat.go:123`).
- Such a builder runs beside the merge. Any merge transaction that runs while the builder's
  heartbeat is live has an exclusive heartbeat check, so it fails the MERGE with
  `SynchronizedSessionLockedError`. That is the pre-commit refresh in `indexing_merger.go`
  through `online_indexer_queue.go`. The mutual builder carries on, since its own check
  writes only its key.
- A VALUE target's merge is one transaction. A builder admitted after it lets that merge
  complete, and the next merge fails ("admits a mutual builder beside the merge, and a
  merge transaction after it fails, not the builder", :295).
- Java's `OnlineIndexer.getIndexingHeartbeats` lists the merge's key as a session.
- A live peer's heartbeat refuses the merge itself with `SynchronizedSessionLockedError`,
  where Java's merge ignores it ("is refused by a live peer over a WRITE_ONLY target,
  which Java's merge would ignore", :268).
- A DISABLED target, disabled under the merge or before it, fails the merge with the
  follow-up's `RecordCoreStorageError`, where Java's merge proceeds ("fails over a
  DISABLED target, where Java's merge proceeds", :327).
- Over a READABLE target the merge runs and writes no heartbeat, because the heartbeat
  step skips it, as Java's does ("merges a READABLE target without a heartbeat", :338).

What Java's and Go's builders do on meeting the merge's key was read from their source,
not run.

**A target that moved between WRITE_ONLY and WRITE_ONLY_WITH_QUEUE mid-session is
refused.** Java does not look at the queue flag in its per-transaction state check. Go's
queued targets are session state, so the move is a `RecordCoreStorageError` "Unexpected
index state(s)" (`online_indexer_queue.go:540-548`), as Java's check reports any other
unexpected state. It is never a validation error, which the catcher would answer with a
records-scan fallback.

**`OnlineIndexer.LastBuildOutcome` is Go-only** (`online_indexer.go:544`). Java's
`buildIndex` returns nothing.

**State.** No stored format differs. Several refusals above leave state that Java would
have changed:
- the clear or fresh WRITE_ONLY mark under a live mutual peer (Java refuses an exclusive
  peer too);
- the reconcile under a live peer;
- MARK_READABLE over a non-empty queue;
- the follow-up over a disabled target;
- the move between WRITE_ONLY and WRITE_ONLY_WITH_QUEUE (Java keeps building);
- the standalone merge under a live peer or over a DISABLED target.

The standalone merge's heartbeat is the one key Go writes that Java would not. It is a
key in Java's own heartbeat layout, under Go's indexer identity. A Java exclusive builder
honours it as a live session; a Java mutual builder does not.

### An INT long-arithmetic key at the edge of its lane answers where the record's expression overflows (RFC-257 WS-J)

The non-integer half of the old entry is now "Long-arithmetic key candidates (WS-J 3.5)". What remains is the INT case. Go matches such a key only when every operand is INT or LONG (`functionKeyToValue`, `pkg/recordlayer/query/plan/cascades/key_expression_expansion.go:616-639`). So an INT operand stays a match, as it does in the target.

The key stores `i + 1` in long arithmetic, so the entry for `i = 2147483647` is the LONG 2147483648. The query's `i + 1` is an INT-lane value, and it overflows on that row. In both engines, a read served from the index can answer where the same read over the record fails. The two engines serve different reads from the index, so their answers differ. Spec "WS-J long arithmetic key functions over non-integer operands" pins the target's answers (`int_max_plus_one`, `conformance/ws_j_index_fidelity_conformance_test.go:1998-2016`, `:2040-2056`) and Go's (`wsjNonIntGoPins`, `:2200-2227`):

- `WHERE i + 1 = 6`: both engines serve it from the index (a covering index scan). Both return [[2]] and never evaluate the overflowing row.
- `WHERE i + 1 = 2147483648`: the target promotes, scans the index, filters per record and fails with XXXXX "integer overflow". Go probes the index with the LONG 2147483648 (see "INT-vs-LONG stays sargable in Go; Java promotes and loses the probe") and returns [[1]].
- `WHERE i + 1 > 0 ORDER BY id`: the target fails with the overflow. Go returns [[1] [2]].
- `SELECT i + 1 FROM T WHERE id = 1` evaluates the expression from the record. Both engines fail: the target with XXXXX, Go with 22003.

No wire bytes differ.

### An index over a synthetic record type is refused on load; the target loads it (RFC-257 WS-J)

Java resolves an index's record types among the stored types and also among the joined and unnested (synthetic) types (RecordMetaDataBuilder.java:178-185 collects the synthetic types, :187-219 resolves each index). Go carries synthetic types verbatim and does not model them. So Go refuses an index whose record type is synthetic on load, as an unknown record type ("Unknown record type <name>", `pkg/recordlayer/metadata_proto.go:292-300`). A store whose metadata carries one does not open in Go. This predates RFC-257: the loader before the upgrade refused the same index (`e48f5b496:pkg/recordlayer/metadata_proto.go:285-296`). Synthetic record types are outside the port's scope (CLAUDE.md), and the SQL layer creates none. No wire bytes differ.

### DeleteTemplateVersion refuses a version schemas still bind; DROP SCHEMA TEMPLATE does not (RFC-257 WS-J)

`DeleteTemplateVersion(t, v)` is the target's `deleteTemplate(txn, t, v, throwIfDoesNotExist)` and uses its texts (RecordLayerStoreSchemaTemplateCatalog.java:317-325). The target deletes the row whether or not a schema binds it. While any schema binds (t, v), Go refuses the delete with 42F59 INVALID_SCHEMA_TEMPLATE. Both catalogs do this: `pkg/relational/core/catalog/fdb_template_catalog.go:325-351`, `template_catalog.go:255-276` and `errBoundOnDelete` (`template_bindings.go:37-41`). Tests: `TestFDB_VersionGuard_DeleteBoundVersionRefused` and `TestInMemory_VersionGuard_DeleteBoundVersionRefused`.

The reason is that a schema bound to a version that no longer exists has no exit that keeps its data:
- `RepairSchema` fails the gone-version check ("SchemaTemplate=<t>, version=<v> is not in catalog", `errTemplateVersionNotInCatalog`).
- Re-issuing (t, v) is the silent rebind the version guard refuses.

If the target's `deleteTemplate` removes a bound version, a shared catalog is left in that state. Go then refuses to re-issue the version. Above the latest stored version, the version guard refuses it. At or below the latest, `CreateTemplate` refuses it on every build path (see "CreateTemplate refuses more than an exact duplicate, and carries a new version"). `TestFDB_CreateTemplate_RefusesAReIssueBelowTheLatest` (`template_carry_fdb_test.go:31`) pins both sequences: a raw delete of a bound version below the latest, and a DROP followed by a restore of the latest version or of the version a schema still binds.

DROP SCHEMA TEMPLATE keeps the target's behaviour and drops regardless (`DeleteTemplate`, `fdb_template_catalog.go:283`, runs no guard). For a schema it strands, the only way back is `fleet.RestoreTemplateVersion` (`pkg/relational/core/fleet/migrate.go:78`; `catalog.RestoreTemplateVersion`, `template_restore.go:89`) with the dropped version's exact metadata bytes. The restore refuses:
- when any bound store's header records a metadata version above the restored one (a header below is normal: the schema was rebound and has not been opened since, and it upgrades on its next open);
- when no schema binds that version at all. A restore exists to re-create a bound version, and restoring one that nothing binds could put a dropped history's bytes beside a new one.

Tests: `TestFDB_Restore_RefusesStoredOrUnboundVersion`, `TestFDB_Restore_ReadsEveryBoundHeader` (`template_restore_fdb_test.go`).

### The function value of a long-arithmetic index is never read from its entry (RFC-257 WS-J)

This mostly records parity. The target never serves a function-key value, or an operand of one, from the index entry. These plans were measured (`conformance/ws_j_index_fidelity_conformance_test.go:2048-2056`):
- An index-served equality fetches the record and recomputes. `SELECT i + 1 FROM T WHERE i + 1 = 6` plans `ISCAN(IX [EQUALS ...]) | MAP (_.I + @c9 AS _0)`, and `SELECT d FROM T WHERE d + d = 2` plans `ISCAN(ARITH_D [EQUALS ...]) | MAP (_.D AS D)`.
- `COVERING` appears only when the query reads nothing but the primary key.

Go does the same. Its logical record reads from an entry only what `ExtractFromIndexEntry` can match: a field, or an order-wrapped field read back through the inverse (`computeIndexEntryToLogicalRecord`, `pkg/recordlayer/query/plan/cascades/match_candidate_index.go:178-215`; `values/index_entry_extraction.go:9-40`). An arithmetic Value is neither, so such an index covers only its primary-key columns. Go's pins show the same plan shape: `Map(IndexScan(IX, [=]), {_0: (_current.I#1 + 1)})` (`wsjNonIntGoPins`, `:2217`).

One read differs. Take `SELECT i + 1 FROM T ORDER BY i + 1` over an index on `i + 1`. The target cannot plan it: 0AF00 "Cascades planner could not plan query", for EXPLAIN as well. Go plans it as `Map(IndexScan(IX, [*]), {_0: (_current.I#1 + 1)})`, recomputing the value per record. With `i = 2147483647` stored, Go fails on that row with 22003 (spec "WS-J long arithmetic key functions over non-integer operands", `int_max_plus_one`).

### Saving a template version is refused while a schema still binds a dropped version above the latest stored (RFC-257 WS-J)

Go saves new versions of a stored template (CREATE SCHEMA TEMPLATE over a stored name, `fleet.SaveTemplate`, `pkg/relational/core/fleet/migrate.go:53`). The target's DDL has no such path, and its catalog does not look at bound schemas when a template is dropped.

`CreateTemplate` refuses a save of (t, v′) with 42F59 INVALID_SCHEMA_TEMPLATE while some schema binds t at a version above the latest one stored, whatever v′ the save names. The error names the first bound schema ("schema template <t> version <v′> cannot be created: schemas are still bound to its dropped version <v> (<db>/<schema>)", `errBoundOnCreate`, `pkg/relational/core/catalog/template_bindings.go:30-35`; the check is at `fdb_template_catalog.go:207-220`). Two cases follow:
- A fresh CREATE SCHEMA TEMPLATE t is refused while any dropped version of t is still bound.
- A new version is refused while a binding of a dropped version above the latest dangles.

The target accepts the same DDL. It binds those schemas to whatever the new build holds and re-reads their rows and index entries under different metadata. The guard applies to both Go catalogs. The in-memory catalog applies it over its schema rows, because its `RepairSchema` rebinds by name. Tests: `TestFDB_VersionGuard_FreshTemplateRefusedWhileDroppedVersionBound`, `TestFDB_VersionGuard_DanglingBindingAboveLatestRefusesNewVersion` (`template_version_guard_fdb_test.go`), and the `TestInMemory_VersionGuard_*` counterparts (`template_version_guard_test.go`).

The only way back for such a schema is `fleet.RestoreTemplateVersion(ks, t, v, md)` with the dropped version's bytes. Inside its restoring transaction, the restore checks that some schema still binds (t, v). It reads the bound stores' headers through the keyspace the caller names; that keyspace is now Java's layout byte for byte (`pkg/relational/core/keyspace/keyspace.go:1-10`). Restoring the dangling versions in turn unblocks the saves. For example: Restore(1), then a new v2, which the dangling binding of 3 refuses; then Restore(3), after which v4 is accepted. These sequences are pinned by `TestFDB_Restore_GuardSequences` and `TestFDB_Restore_GuardSequence` (`template_restore_fdb_test.go`).

### A new template version may not re-add, under its old name, an index a carried FormerIndex holds (RFC-257 WS-J)

A new template version carries the stored version's record-type and index numbering (`carryNumbering`, `pkg/relational/core/catalog/template_carry.go:46`). An index that the stored version had and the new one lacks becomes a FormerIndex keyed by its name.

A new index whose name is the subspace key of a carried FormerIndex is refused at template save. The error is 42F59 INVALID_SCHEMA_TEMPLATE: "index <ix> cannot be added: its name is the subspace key of index <ix> dropped at version <v>; add it under another name" (`template_carry.go:96-103`). Test: `TestFDB_Carry_ReAddingADroppedNameIsRefused` (`pkg/relational/sqldriver/carry_rule_fdb_test.go:511`).

Without the refusal, a store opening under the new metadata would clear the former index's subspace while the new index is built into it. Both engines' validators refuse such metadata ("Same subspace key <k> used by index <ix> and former index <ix>", MetaDataValidator.java:106-113, `pkg/recordlayer/index_validator.go:409`).

The target's DDL creates no second version of a template, so it never meets this case. The refusal is Go's, on a Go-only path. It asks for a new name rather than inventing a Go-only subspace key.

### An arithmetic function key with no lane is refused when hand-built metadata is saved as a template (RFC-257 WS-J)

Take an index key of the ArithmeticValue family (the arithmetic, bit and bitmap functions) whose operand types name no row of Java's operator table. The table has 107 rows (ArithmeticValue.java:406-522, `values.LookupArithmeticLane`, `pkg/recordlayer/query/plan/cascades/values/arithmetic_lanes.go:184`). The target cannot plan such a key, and every query of its record type fails.

Each operand is typed by the result type of the Value the target builds for it:
- an order-wrapped or collated operand is BYTES, so `bitand(order_desc(a), 1)` is refused;
- a CARDINALITY is INT, so `x & cardinality(a)` has a lane.

**From DDL**, both engines refuse a lane-less key at the clause, with the target's XX000 and message. Go's check is `encapsulateLane` (`pkg/relational/core/query/ddl/generator.go:802`), Java's encapsulate check (ArithmeticValue.java:213-231). Spec "WS-J bit and bitmap index keys over an operand with no lane" (`conformance/ws_j_index_fidelity_conformance_test.go:2947`) measures:
- ten single-fault key shapes;
- a lane-less key followed by a later table fault, which the target reports first;
- a lane-less operator in the index's WHERE, which Go now refuses as the target does, since the lane is resolved when the Value is built (`NewArithmeticValue`, `values/values.go:3732-3745`).

One precedence differs and is pinned with both answers: a lane-less key against a fault in the index query's WHERE. The target reports the WHERE's 42703; Go reports the key's XX000.

**Metadata built by hand** and saved through `CreateTemplate` (either catalog) is refused by Go with 42F59 INVALID_SCHEMA_TEMPLATE, naming the index and the operand types (`checkIndexLanes`, `pkg/relational/core/catalog/index_lanes.go:35`). Java's programmatic API stores such a key, and its queries fail later. An operand with no Java Value (a Go-only function key) is typed UNKNOWN and refused. Tests: `TestCreateTemplate_LaneCheck` (both catalogs, a fresh name and a new version) and `TestFDB_CreateTemplate_LaneCheckReadsOnlyTheIndexesASaveDefines` (`template_carry_fdb_test.go`).

The check runs only over the indexes a save defines. For a new version these are the NEW and CHANGED indexes (`carryTemplate`, `template_carry.go:282`); it never checks an index carried unchanged from the stored version. Suppose Go stored a template, before this check existed, with one of the eight lane-less DDL shapes:
- a hand-built new version keeps the index;
- a new version made by DDL restates it and is refused at the clause;
- the index goes when a version omits it.

`fleet.RestoreTemplateVersion` writes such bytes unchanged, because it runs no build-path check (`template_restore.go:20-38`).

### BY_INDEX resume over a nested-tuple source key (RFC-257 WS-C)

An index build stamped BY_INDEX records its source index's subspace key. A later build resolves that key in two steps:
- `Index.decodeSubspaceKey` decodes it (Index.java:80-86; OnlineIndexer.java:203-210).
- `RecordMetaData.getIndexFromSubspaceKey` looks it up (IndexingByIndex.java:97; RecordMetaData.java:329-336).

When the key is a nested tuple, Java's decoded item is a `Tuple`, which never equals the source index's normalized `List` key. So Java fails the resume with "Unknown index subspace key". This was read from Java's source, not measured. The failure is a `MetaDataException`, not a validation exception, so the catcher at OnlineIndexer.java:228 (`IndexingByIndex.isValidationException`) does not fall back to a rebuild.

Go normalizes the decoded item the same way it normalizes the index's key (`subspaceKeyIdentity`, `pkg/recordlayer/subspace_key_identity.go:63`; `RecordMetaData.GetIndexFromSubspaceKey`, `pkg/recordlayer/metadata.go:1965-1973`). So `OnlineIndexer.savedSourceIndex` (`pkg/recordlayer/online_indexer.go:1370-1380`) resolves the index the stamp names and resumes. Go does not port this Java defect: the stamp was written by the same build for that very index. Every other key form (strings, integers, bytes) resolves alike in both engines, except that Go also finds an int64 key from an int argument.

### CreateTemplate refuses more than an exact duplicate, and carries a new version (RFC-257 WS-J)

Java's `createTemplate` stores the template it is given. It refuses only an exact duplicate of a stored (t, v), with DUPLICATE_SCHEMA_TEMPLATE (RecordLayerStoreSchemaTemplateCatalog.java:229-245). Go's `CreateTemplate` does more, in both catalogs (`pkg/relational/core/catalog/fdb_template_catalog.go:161-232`, `template_catalog.go:145`). It refuses the exact duplicate first, with the same code and text. It then refuses, in this order:
- a version at or below the latest stored, with INVALID_SCHEMA_TEMPLATE (`refuseBelowLatest`, `template_carry.go:242-249`);
- a version the relational evolution validator refuses against the stored latest (same function);
- a version the version guard refuses (see "Saving a template version is refused while a schema still binds a dropped version above the latest stored");
- an index key with no lane (see "An arithmetic function key with no lane is refused when hand-built metadata is saved as a template");
- a new version that the evolution validator, with index rebuilds allowed, refuses against the stored latest (`carryTemplate`, `template_carry.go:260-295`).

For a new version of a stored name, Go does not store the template it is given. It carries forward from the stored latest version (`carryNumbering`, `template_carry.go:46`):
- the record-type keys, union fields and since-versions are kept;
- each unchanged index's stored Index message is spliced in;
- a changed index gets a last-modified version above the stored metadata version, so stores rebuild it;
- a dropped index becomes a FormerIndex.

`fleet.SaveTemplate` returns the template as stored (`pkg/relational/core/fleet/migrate.go:53`). The first two refusals moved into `CreateTemplate` from the save action, which now calls only `CreateTemplate`, as Java's does (`pkg/relational/core/ddl/save_schema_template.go:22-23`).

So a Java library caller can store a version below the latest, or a renumbered one, and a Go caller cannot. A template that the target's DDL created is carried as the target stored it (JVM spec "WS-J a new version carried from the target's template", `conformance/ws_j_index_fidelity_conformance_test.go:3466`).

### The template restore's refusals, and the inverted history with no exit (RFC-257 WS-J)

Java has no restore. Go's `fleet.RestoreTemplateVersion` (`pkg/relational/core/catalog/template_restore.go:89`) admits a dropped (t, v) only when it forms one history with every stored version of t (`carryCompatible`, `template_restore.go:393-440`). It refuses everything else, and none of these refusals has a Java counterpart (test `TestFDB_Restore_OneHistory`, `template_restore_fdb_test.go`).

The restore refuses an inverted template history, where a lower template version has a higher metadata version (`template_restore.go:418-423`). Go has no exit for such a history:
- The bound schemas bind a version that is not stored, so `RepairSchema` fails the gone-version refusal. The target's does too: its `repairSchema` loads the schema's template (RecordLayerStoreCatalog.java:272-279).
- The version guard refuses a new version of t while that binding dangles.

Java admits the second, because it has no guard, and refuses the first.

### Function names in stored key expressions are loaded whether or not Go registers them (RFC-257 WS-J)

The target creates a `FunctionKeyExpression` at load through its classpath's registry. It refuses a name that no module registers with "Function not defined" (FunctionKeyExpression.java:118-122). Its Lucene and Geophile spatial modules, and applications, add names.

Go's registry is its own. Go loads any function name (`functionFromProto`, `pkg/recordlayer/key_expression_proto.go:425-446`). It fails only when it maintains or evaluates an index whose function it does not know ("unknown function key expression", `FunctionKeyExpression.Evaluate`, `pkg/recordlayer/key_expression.go:1725-1736`). Refusing at load would lock Go out of every Java store with such an index.

The column size of an unregistered name is 1 in Go (`key_expression.go:1746-1751`); in the target it is the function's own. A function that Go does register is checked at load against the target's bounds, with the target's refusal. Go's `Build` refuses an in-code `FunctionExpr` of a name Go does not register, as the target's `create` refuses it (`FunctionExpr`, `key_expression.go:1715-1721`; `functionConstructionFault`, `:1685-1694`). Conformance: "RFC-257 key functions: the registry and create's refusals as Java's" (`conformance/function_registry_conformance_test.go:37`).

### A refused SetSubspaceKey on an index of built meta-data (RFC-257 WS-C)

Java's `Index.setSubspaceKey(null)` throws ("Index subspace key cannot be null",
`Index.java:89-93`, `:413-416`). Go's `Index.SetSubspaceKey` returns the index for chaining
(`pkg/recordlayer/index.go:514`). So it records the refusal on the index, and every
`RecordMetaDataBuilder.Build` the index was handed to returns it, first in program order among
the builder's faults (`firstFault`, `pkg/recordlayer/metadata.go:2309`). An index that already
belongs to a built `RecordMetaData` has no `Build` left. There the refused set changes nothing
(key and explicit mark kept) and `Index.SubspaceKeyError` (`index.go:534`) reports it, where Java
would have thrown. The call site is `Index.SetSubspaceKey`.

### An index with no root is refused by Build and never written (RFC-257 WS-C)

Java's `Index` constructors take a `@Nonnull` root, and an index built with a null one fails
Java's meta-data validation with a `NullPointerException`. A Go struct-literal `Index` can have
no root. `Build` refuses it ("Index X has no root expression", `pkg/recordlayer/metadata.go:969-978`),
and `indexToProto` refuses to serialize it (`pkg/recordlayer/metadata_proto.go:476-480`). So Go
never stores an index every reader refuses ("Exactly one root must be specified for an index",
`key_expression_proto.go:148`). The refusal's class and text are Go's, where Java's is an NPE.

### R-tree configurations Go refuses to maintain (RFC-257 WS-C)

Java's `MultiDimensionalIndexHelper.getConfig` takes any int for `rtreeMinimumM`,
`rtreeMaximumM` and `rtreeSplitS` (`Integer.parseInt`, `MultiDimensionalIndexHelper.java:48-56`).
Go parses them the same way; the option checks of the evolution validator compare the parsed
values. Go's `NewRTree` (`pkg/recordlayer/rtree.go:21`) then refuses to maintain an R-tree whose
configuration cannot keep its node invariants: `MinM < 1`, `MaxM` outside [2, 1000],
`SplitS < 1`, or `SplitS * MaxM < (SplitS + 1) * MinM`. Java maintains such a tree and fails
later, reading a node whose size is out of range ("packing of non-root is out of valid range",
`fdb-extensions/.../async/rtree/AbstractStorageAdapter.java:183`). The call site is
`ValidateRTreeConfig` (`pkg/recordlayer/rtree_types.go:260`), reached from the maintainer at
`multidimensional_index_maintainer.go:586`.

### How Go maintains an R-tree's node slot index (RFC-257 WS-C, same bytes)

Not a divergence in what is stored. Java's change sets write and clear node slot index entries
slot by slot, and Java finds the leaf to modify, and its parents, through the index. Go keeps
the index as the difference an insert or delete made to the child slots of the intermediate
nodes it touched (`rtreeStorage.flushNodeSlotIndex`, `pkg/recordlayer/rtree_storage.go:362`).
Go finds the leaf by walking down from the root, which reaches the same leaf the index names
(the first whose largest Hilbert value and key are at or above the target, else the last).
The entries stored are the ones Java stores. The conformance spec "MULTIDIMENSIONAL index
R-tree options" (`conformance/multidimensional_options_conformance_test.go:35`) decodes every
intermediate node's child slots from the raw bytes after each step, whichever engine wrote them.
It builds each expected entry (the child's level, its largest Hilbert value, its largest key's
items, its id) without Go's encoder, and requires the whole stored index to equal that set.
Java then deletes through the entries Go wrote. The unit spec "RTree storage layouts and the
node slot index" (`pkg/recordlayer/rtree_layout_test.go:106`) builds its expected entries the
same way.

A BY_SLOT node is written whole: Go clears the node's key range and writes every slot, where
Java's change sets write and clear only the slots that changed. The bytes left are the same,
and so are the conflicts in effect, since every writer of a node has first read its whole
range. The call site is `rtreeStorage.writeSlots` (`rtree_storage.go:279`).

### Build's record-type checks: the order, and one Go-only refusal (RFC-257 WS-C)

`Build` runs Java's record-type checks in Java's order with Java's texts and classes, but it
walks the record types by NAME (`recordTypeNames`, `pkg/recordlayer/metadata.go:880`) where
Java walks its builder's HashMap. So where several record types are at fault, the one a message
names can differ; each run of Go names the same one. The same holds for a record-type key
collision's pair. One refusal is Go-only: a primary key with no columns (`EmptyKey()`) is
refused ("... produces no columns", `metadata.go:932-937`). Java builds it, and its split
record's clear range is then the whole records subspace, every other record type's records
included. (An empty `Concat()` is Java's refusal, "Then must have at least 2 children", which
Java throws where the Then is built.) The call site is the record-type loop of `Build`
(`metadata.go:929-967`).

### Build does not refuse an index type Go does not maintain (RFC-257 WS-C)

Java's `MetaDataValidator` asks its classpath's registry for each index's validator and refuses a
type no factory registers ("Unknown index type for ...", `IndexMaintainerFactoryRegistryImpl.java:93`).
Go runs Java's validator for every type Go maintains except VECTOR (`validateIndexType`,
`pkg/recordlayer/index_validator.go:104-149`; VECTOR's is open, see "VECTOR index metadata
validation: Go has none, Java has `VectorIndexValidator`"). It builds an index of any other type
without one (the switch's fall-through, `index_validator.go:148`), because Go must load meta-data
a Java program wrote with a module Go does not implement (Lucene, for one): refusing it would make
the whole store unopenable. Such an index fails where Go would maintain or scan it. The Go-only
`vector_spfresh` type (`index.go:43`) is Go's extension and has no Java validator to port.

### A bitmap index's entry size is read as Java reads it, and a size of zero or below is refused where it is used (RFC-257 WS-C)

`BitmapValueEntrySizeOption` (`pkg/recordlayer/bitmap_value_index_maintainer.go:72-88`) is Java's
`BitmapValueIndexMaintainer` constructor (`BitmapValueIndexMaintainer.java:101-103`):
`Integer.parseInt`, 10000 when absent, "entry size option is too large" above 250000. Java
accepts a size of zero or below and fails at the index's first write (a division by zero, or a
negative array size). Go refuses it with `RecordCoreArgumentError` ("entry size option must be
positive") when it builds the maintainer, since the same arithmetic would panic. The call sites
are the maintainer (`bitmap_value_index_maintainer.go:47`), the chaos model
(`pkg/recordlayer/chaos/verify_bitmap.go:55`), and the planner's bitmap aggregate candidate,
which declines the index instead (`pkg/relational/core/embedded/cascades_generator.go:4134`).

### The compensation correlation guard refuses a residual on an outer alias the probe does not feed (RFC-150)

Java has no such guard: a compensation filter over a data-access match is explored and
implemented whatever its residual references. Go's `compensationResidualCorrelationSafe`
(`pkg/recordlayer/query/plan/cascades/planner.go:1589-1640`, reached from
`compensationSafeForYield`, `:1462`, at `:1506` and `:1511`) refuses a compensation whose residual
is correlated to an outer alias the compensation's own probe does not feed
(`compensationProbeCorrelations`, `:1736`). It guards the case where the bound-prefix signal
classifies a leg as uncorrelated while its residual carries the join key, for example
`O JOIN T ON t.fk = o.id WHERE t.k = 5` over a constant-bound `T` probe. A residual on an alias
the probe already feeds is admitted as a secondary filter.

The guard makes no exception for the alias of an EXPLODE quantifier (the iteration variable of an
IN list or an array). This is one reason Go does not build the two-source IN-union Java plans for
two IN lists under an ordering. Go nests a one-source IN-union and keeps the second IN as a
residual instead (`in_plan_winner_stability.yaml#7`: `InUnion(FlatMap(IndexScan(IDX_VAL, [=]),
... PredicatesFilter(Explode ...)), bindings=1, ASC)`). The two-source IN-union is still open
(TODO.md, item 10). Two ports of it were measured and reverted: one lost Go's in-join over one
list with the other as a residual, and the other cost five times the planner tasks. The port
waits on Go's in-join rules accepting a select with several explodes.

Any later exception must stay per alias. Admitting every residual once the probe is correlated
at all would admit `O JOIN T ON t.fk = o.id WHERE t.k IN (1, 2)`'s join key onto the
explode-probed leg. The guard is not removed: it also bounds the search. Without it, a
nine-conjunct OR exhausts the planner's task budget.

### A map's entry order when Go saves a map Go's caller changed or built (RFC-257 WS-C)

Java's default serializer (`DynamicMessageRecordSerializer`) reads a stored record as a
DynamicMessage. Its map field is the list of its entries in stored order, a key written twice
included. A load-then-save writes that list back, each entry re-encoded with its key and its
value, then the fields the entry carries that its type does not declare. Go's load-then-save
writes the same map entries (`rewriteMaps`, `pkg/recordlayer/record_wire_map_order.go:425`), an
entry's unknown fields included. Two JVM specs compare both engines' re-saves byte for byte:
"Map entries are maintained in the record's wire order, as Java maintains them"
(`conformance/key_validation_conformance_test.go:324`) and "A record re-saved unchanged is written
as Java's load-then-save writes it" (`:667`). Their cases cover proto2 and proto3, zero keys and
values, entries missing their key or their value, a key written twice with message values, maps in
map values, and entries and records with unknown fields. They run over the bytes Java's save wrote
and over raw bytes Java never wrote. The first spec's index is unchanged by either re-save.

The bytes differ in two ways the second spec pins. First, protobuf-go's deterministic marshal
writes a oneof member after the other fields (and an extension first), where Java writes fields
in number order. Second, a message's unknown fields are written as they are stored. Java writes
them as its `UnknownFieldSet` holds them: by field number and, within one field, varints,
fixed32s, fixed64s, then length-delimited (measured; groups last, from protobuf-java's source,
not measured). Java also re-encodes each minimally. That was measured for one value: a varint
stored in a non-minimal encoding is written minimally by Java and as stored by Go
(`key_validation_conformance_test.go:783-794`). That tags and lengths are re-encoded minimally too
is read from protobuf-java's source. The two agree over any bytes Java wrote, which are in its
order and encoding already. Both engines read either as the same record.

A Go map holds one value per key and has no insertion order. So where Go's caller CHANGED a map,
or built the record, the order is Go's:
- A changed key is written once, in its first stored position, from Go's map. That map keeps no
  unknown fields of an entry (protobuf-go drops them when it decodes a map).
- A new key follows the stored ones, in key order.
- A new record's maps are in key order.

An element of a repeated message field is matched to the stored elements by content
(`elementPriors`, `record_wire_map_order.go:524-634`). The match runs along a longest common
subsequence of the two lists and then by content in order. So an unchanged run keeps its own
order when elements are inserted, removed, changed or swapped around it, as Java's does.
`TestElementPriorsMatching` (`record_wire_map_order_test.go:1045`) pins two exceptions:
- An inserted element equal in content to a stored one is not told from it. Stored [B] saved as
  [B', B] pairs the stored B with B', the element at its position, and writes the B that was
  stored as a new element, its maps in key order, where Java keeps B's order.
- Past `maxLCSCells` (2^20, `:637`) cells, the product of the two lists' lengths, no
  subsequence is computed and elements are matched by content in order. An unchanged element can
  then take an earlier stored element equal to it rather than its own. Stored [A1, B, A2], A1 and
  A2 equal but for their maps' order, saved as [B, A]: A takes A1's order where Java's keeps A2's.

A changed element takes the stored element at its position when no other element took that one.
Otherwise it writes its maps in key order, where Java's keeps its own. A Go message carries no
identity across a load and a save. So equal elements reordered among themselves are matched in
order, and a changed element that also moved cannot be told from a new one.

In Java the order is whatever the caller built: a generated message's map in insertion order (a
key written twice collapsed to its first position), a DynamicMessage's in list order. The
difference is not a wire incompatibility. Each engine indexes a record from the bytes it wrote,
and reads the other's bytes as it reads its own. It shows only where two entries of one map write
one index key, whose value is the last entry's, or share a TEXT token.

### Key-expression shapes Java loads and Go refuses on load (RFC-257 WS-J)

`KeyExpressionFromProto` (`pkg/recordlayer/key_expression_proto.go`) refuses, as untyped errors,
four shapes Java's constructors take and fail on later, if at all. The meta-data loader refuses
a fifth.
- A key expression nested deeper than `maxKeyExpressionDepth` (128, `:128`, checked at `:140`;
  Go's recursion bound). From stored bytes a meta-data proto never reaches it. Both engines parse
  it with protobuf-java's recursion limit: the root and 100 nested messages
  (`recordlayer.UnmarshalAsJava`, `proto_closed_enums.go:113`). The JVM spec "RFC-257 a key
  expression nested past protobuf's recursion limit"
  (`conformance/key_expression_absent_child_conformance_test.go:304`) tests an index root at
  2d+2 levels and a record-count key at 2d+1, each at its last admitted depth and one past it. So
  a key expression nests at most 50 levels there. An in-memory proto nested past 128 is Go's
  alone to refuse.
- A grouping whose `grouped_count` is outside `[0, columns]` (`key_expression_proto.go:339-343`).
  Java's `GroupingKeyExpression(Grouping)` stores it unchecked (`GroupingKeyExpression.java:55-57`).
- A key-with-value whose `split_point` is outside `[0, columns]` (`key_expression_proto.go:359-363`;
  Java's `KeyWithValueExpression.java:63-65` leaves it unchecked).
- A split whose size is below one (`key_expression_proto.go:452-455`; Java's
  `SplitKeyExpression.java:58-60` leaves it unchecked, and an evaluation divides by it).
- An index predicate's comparison operand with no value. Java's
  `IndexComparison.SimpleComparison(proto)` refuses it with a `NullPointerException`
  (`Objects.requireNonNull`, `IndexComparison.java:175`). Go refuses it with `RecordCoreError`
  "index comparison operand has no value" (`pkg/recordlayer/index_predicate.go:287`). The JVM
  spec is "RFC-257 an index predicate's operand Java cannot read is refused as Java refuses it"
  (`key_expression_absent_child_conformance_test.go:239`). An operand with two values is Java's
  own `RecordCoreException`, "More than one value encoded in value", in both engines.

That no index of either engine can maintain the first four is read from Java's source, not
measured. Java's constructors store the count or size unchecked, and the first use fails (an
out-of-range column index, a division by zero); Go refuses where it reads the proto instead.
Inside a meta-data proto these refusals are not wrapped in `MetaDataProtoDeserializationError`,
since they are not Java's `DeserializationException`.

### An unknown group nested past protobuf-java's recursion limit is read (RFC-257 WS-J)

Every decode of bytes a Java engine shares reads them as protobuf-java parses them
(`pkg/recordlayer/proto_closed_enums.go`, `javaDecodeRule`, `:38-70`):
- known messages under Java's recursion limit: a root and 100 nested messages, or 100 for a
  record, which sits one level below Java's union (`javaRootRule` and `javaRecordRule`, `:64-70`);
- a closed enum's undeclared number as an unknown field, occurrence by occurrence;
- the required fields checked after.

vtproto's decoders have no limit, so they read only types whose known messages cannot nest past
it (`vtBounded`, `:167-172`). Neither Go decoder bounds an UNKNOWN group. protobuf-go skips it
with its own limit (10,000 levels) and vtproto with none, where protobuf-java's `skipMessage`
counts it against the 100. Bytes carrying a group nested 101 levels in a field the reader does
not know load in Go and are refused by Java ("Protocol message had too many levels of nesting").
No engine writes such bytes: neither writes groups, and an unknown field is kept only as it was
read.

## Enum values are scoped as protobuf-java scopes them (in memory only)

**Java.** protobuf-java scopes an enum value under its enum type. So a records file may hold two
enums that share a value name (`e1('X', 'Y')`, `e2('Y', 'Z')`), or an enum with a value spelled
like the enum (`status('STATUS', 'DONE')`). Java's relational DDL stores both, and Java reads them.

**Go.** protobuf-go, like protoc, scopes an enum value BESIDE its enum, in the enclosing scope. Such
a file does not build as stored. `protoscope.ScopeEnumValuesAsJava`
(`pkg/recordlayer/protoscope/protoscope.go:54`) rewrites the in-memory descriptor only. Each enum
whose values collide in its scope moves into a synthetic holder message
(`__fdbgo_enum_scope__<i>_<name>`, `protoscope.go:39`, `:158`). An enum with a value spelled like
itself is also renamed inside the holder (`protoscope.go:48`), because no depth of scoping
separates those two names. The stored records file is never rewritten: Go writes back Java's
bytes. Every name Go reports is Java's. `protoscope.JavaFullName` and `JavaName`
(`protoscope.go:177`, `:187`) read it back from the holder. The metadata type
(`pkg/relational/core/metadata/proto_types.go:224`,
`pkg/recordlayer/query/plan/cascades/values/proto_field_type.go:264`), the evolution validator's
messages (`pkg/recordlayer/metadata_evolution_validator.go:1380`) and the executor's enum errors
(`pkg/recordlayer/query/executor/executor.go:6090`) use them.

Wire: none. Pinned by `TestScopeBuildsWhatJavaAccepts`
(`pkg/recordlayer/protoscope/protoscope_test.go:57`), and by
`TestEnumsSharingAValueNameLoadAsJavaLoadsThem` and `TestAnEnumValueNamedLikeItsEnumLoadsAsJavaLoadsIt`
(`pkg/recordlayer/enum_value_scope_test.go:89`, `:98`). Those two load, then write back, and the
records file written back is `proto.Equal` to the one loaded (`enum_value_scope_test.go:136`).
Against the JVM it is pinned by the WS-J census shapes `enums_sharing_value_name` and
`enum_value_named_like_enum` (`conformance/ws_j_index_fidelity_conformance_test.go:659`, `:682`),
whose whole-template MetaData bytes are equal.

## TransformedRecordSerializer: what Go reads beyond Java, and what it does not implement (RFC-257)

**Java.** A store reads and writes through ONE `RecordSerializer`. The default,
`DynamicMessageRecordSerializer`, reads a bare union message and nothing else. A
`TransformedRecordSerializer` reads a prefixed record (clear, compressed, encrypted, or both) and,
by `decodePrefix`'s rule, a bare one. `TransformedRecordSerializerJCE` encrypts with whatever
cipher the key manager names, through the JCE. `KeyStoreSerializationKeyManager` opens its key
store with `KeyStore.getInstance(File, password)` (`KeyStoreSerializationKeyManager.java:196`),
which detects the type (PKCS12, JKS, JCEKS).

**Go.** Five declared differences. The first three are about what Go reads. The last two are about
what Go writes, and in both a Java reader reads what Go writes:

- **Every store reads a prefixed record, whatever it writes with** (a read-side extension).
  `FDBRecordStore.readStoredRecord` (`pkg/recordlayer/store.go:2293`) decodes the prefix at every
  stored-record read site. A store with no serializer reads a record that Java's relational layer
  or a compressing Java application wrote, where Java's default serializer would fail it. An
  encrypted record still needs the key: a store whose serializer has no key manager fails it with
  Java's "this serializer cannot decrypt" (`pkg/recordlayer/transformed_record_serializer.go:676`).
- **The one cipher is `AES/CBC/PKCS5Padding`**, Java's default (`CipherPool.DEFAULT_CIPHER`,
  `CipherPool.java:32`; Go's `DefaultCipher`, `transformed_record_serializer.go:41`), with 16-, 24-
  and 32-byte keys. The name matches case-insensitively, as `Cipher.getInstance` matches it
  (`transformed_record_serializer.go:731`). A key manager naming any other cipher fails the record
  with Java's "encryption error" or "decryption error", its cause "unsupported cipher X: Go
  implements AES/CBC/PKCS5Padding only" (`transformed_record_serializer.go:732`). Java encrypts
  under any cipher its JCE provides. A key the key manager reports as another algorithm is refused
  as Java's AES cipher refuses it ("Wrong algorithm: AES or Rijndael required",
  `transformed_record_serializer.go:743-744`; measured against the JVM for an HmacSHA256 key).
- **A key store must be PKCS12**, the JDK's default type and what `keytool` writes. A JCEKS file is
  refused as "Key store loading failed", its cause naming the conversion (`keytool -importkeystore
  -deststoretype PKCS12`, `pkg/recordlayer/keystore_key_manager.go:381`). A JKS file cannot hold a
  secret key, so neither engine serves a key from one: Go refuses it at load, Java when a key is
  read. Within PKCS12, Go reads the protections the JDK writes (`pbeDecrypt`,
  `keystore_key_manager.go:500`): PBES2 (PBKDF2 with an HMAC-SHA PRF, AES-128/192/256-CBC), the
  JDK 12+ default, and the older pbeWithSHAAnd3-KeyTripleDES-CBC, under a MAC over SHA-1, SHA-224,
  SHA-256, SHA-384 or SHA-512. As in the JDK, a key's password and the store's password must be
  printable ASCII ("Password is not ASCII", `checkPBEPassword`, `keystore_key_manager.go:229-243`).
  The JDK derives the key-entry and the MAC keys through a `PBEKey`, and was measured refusing to
  store either.
- **Compressed bytes are Go's deflate, not the JDK's zlib.** Both write a zlib stream (Java's
  `Deflater.finish()` then `FULL_FLUSH`, `TransformedRecordSerializer.java:133-134`; Go's
  `compress/zlib`, `transformed_record_serializer.go:439-471`). Each engine inflates the other's,
  and the prefix and length header are Java's. But `compress/zlib` (over `compress/flate`) is a
  different encoder from the zlib the JDK links, so the compressed bytes differ from Java's for the
  same record and level. Each engine keeps the compression only when its own output is shorter
  than the record less the 5-byte header, so a record near that threshold can be stored compressed
  by one engine and clear by the other. Byte equality of deflate output is not a property Java has
  either: the JDK's zlib is the platform's (system zlib on some builds, bundled on others), and its
  output differs between zlib versions.
- **The write-time encryption validation decrypts under the key the record was written with.**
  Java's `validateEncryption` decrypts through a fresh `TransformedRecordSerializerState`
  (`TransformedRecordSerializer.java:184-185`), whose key number is 0. So a sampled write under any
  other key fails ("encryption validation error: decryption failed", or a mismatch if the padding
  happens to hold). Go validates under the record's own key and accepts that write
  (`transformed_record_serializer.go:298-316`). The stored bytes are the ones Java would have
  written, and Java reads them. `TestTransformedSerializer_WriteValidation`
  (`pkg/recordlayer/transformed_record_serializer_test.go:691`) pins Go's answer and, mutated to
  Java's key number 0, reddens with Java's error. Both validations are otherwise Java's: a
  `RecordSerializationValidationError` (`pkg/recordlayer/errors.go:409`; Java's
  `RecordSerializationValidationException`, not a `RecordSerializationException`) naming the record
  type and primary key. The encryption one catches only a cipher failure, so a key manager's own
  refusal propagates as it is.

Also, not a divergence of the result: Java refuses an option value of the wrong type when the option
is set (`TypeContract.validate`, `TypeContract.java:72`, INVALID_PARAMETER). Go's `api.Options` take
any value, so `serializerFromOptions` (`pkg/relational/core/embedded/serializer_options.go:35`,
`:113-118`) refuses a mistyped `ENCRYPTION_KEY_*` option when a store opens, with Java's message
and code.

Pinned by `pkg/recordlayer/transformed_record_serializer_test.go`,
`pkg/recordlayer/keystore_key_manager_test.go`, `pkg/recordlayer/transformed_serializer_store_test.go`,
and the JVM spec "RFC-257 TransformedRecordSerializer records are read and written as Java's"
(`conformance/transformed_serializer_conformance_test.go:34`): the no-serializer read of each
transformation and the "cannot decrypt" arm, Go reading each kind of key store the JDK writes
(PBES2, the empty password, a separate key password, the pre-12 3DES/HmacPBESHA1 protection) and
refusing the JCEKS one, the JDK refusing a non-ASCII store password, and both engines refusing an
HmacSHA256 key.

## Assignment and arithmetic where the target crashes, and where Go's types run out (RFC-257 WS-J)

**Where the target fails on an internal error, Go answers as the target was designed to.** Five
statements the WS-J specs run make the target throw something no refusal was designed around. Go
answers each by the same rule the target applies to the neighbouring cases. Each is pinned with both
engines' answers, so the specs redden if the target is fixed or if Go moves. The first four are in
`targetBugs` (`conformance/ws_j_enum_conformance_test.go:444`). The fifth is in `pinned`
(`conformance/ws_j_update_column_conformance_test.go:144`).

- An enum into a string column (`UPDATE t SET str = m`): the target's `computePromotionsTrie`
  reaches a bare `Verify.verify` past its primitive arm (`PromoteValue.java:382`; XX000 "null").
  Go refuses it as INCOMPATIBLE_TYPE, 22000, as the target refuses every other unpromotable
  assignment (`values.CheckPromotionsTrie`,
  `pkg/recordlayer/query/plan/cascades/values/promotions_trie.go:47`).
- A string array into an enum array, once a row matches (`UPDATE t SET ms = ['SAD']`): the target
  admits it while planning, then casts with a null enum descriptor (XXXXX "null"). Go stores the
  names.
- A string into a UUID column, once a row matches: the target sets a `java.util.UUID` into the
  column's message field unconverted (XXXXX, protobuf reflection). Go stores the UUID.
- A NULL projected by an INSERT … SELECT into a UUID column: the target throws XXXXX "should not be
  called". Go answers what the target's own UPDATE answers for a NULL into a UUID (there is no
  NULL_TO_UUID promotion): 22000.
- The table itself as an UPDATE's SET column (`UPDATE w SET w = 5`): the target resolves `w` to
  the whole row and then casts it to a field (QueryVisitor's `castUnchecked`), XX000 "expected
  FieldValue but got QuantifiedObjectValue". Go answers as for any name that is not a column of
  the target: 42703.

**Where Go's type derivation leaves an operand UNKNOWN, two checks fail open.** Java types every
Value it plans. Go's derivation does not yet reach every Value, and an unresolved operand is
`UNKNOWN`. Then `values.NewArithmeticValue`
(`pkg/recordlayer/query/plan/cascades/values/values.go:3732-3740`) resolves no lane: the value
types by promotion and takes its arithmetic from the runtime operands. And
`values.CheckPromotionsTrie` admits the assignment (`promotions_trie.go:56-57`): the converter
checks the value when it is written. Neither is a verdict Java would give on a typed operand. A
value the column cannot hold is refused when it is converted. But one it can hold is stored, where
Java, refusing by type, may refuse the whole statement. A BIGINT into an INTEGER column is 22000 on
Java whatever the value, and a fitting value from an UNKNOWN operand is written by Go. Which SQL
shapes reach UNKNOWN is not enumerated (no census counts them). The WS-J lane and assignment specs
run with every operand typed.

## A table's qualifier is its schema template's name (RFC-257 WS-J v28)

**Aligned.** A table name carries at most one qualifier, and it is the connection's schema
TEMPLATE's name compared exactly. Java's `SemanticAnalyzer.tableExists` and `getTable` compare it
with `metadataCatalog.getName()` (`SemanticAnalyzer.java:264-318`). The schema's own name
qualifies nothing. A statement's target refuses any other qualifier with 42F00 "Unknown schema
template <q>" (`SemanticAnalyzer.java:318`) and two qualifiers with XX000 "Unknown table <path>".
A FROM source so qualified is no table, and the correlated reading then refuses the path, 42703
"Unknown reference <path>". An unaliased qualified table is named by its whole identifier
(`T.W.col`; `W.col` names nothing). Java's qualified lookup prepends the operator's name to an
attribute's table-qualified name, so it also reads `w.w.col` and `W.T.W.col` as a top-level
column. Every shape is measured in `conformance/ws_f_table_qualifier_conformance_test.go`.

**Where the engines still differ**, pinned there with both answers (`declared`,
`ws_f_table_qualifier_conformance_test.go:464`):

- A GROUP BY over a derived table (`SELECT d.f, COUNT(*) FROM (SELECT f FROM w WHERE id = 1) AS d
  GROUP BY d.f`) or over a struct column (`GROUP BY ss.ss`): the target cannot plan it (0AF00). Go
  answers, a read-side reach the target lacks. Derived-table grouping is also covered by
  `pkg/relational/conformance/yamsql/testdata/group_by_derived_expr.yaml`.
- An explicit JOIN whose right side is a correlated array (`FROM w JOIN w.arr AS v ON p`) is, in
  both engines, the comma form `FROM w, w.arr AS v WHERE p` (aligned in WS-J v29). Where it still
  differs, it is pinned with both answers in `declared` in
  `conformance/ws_f_join_unnest_conformance_test.go:557`. Without an ON (`JOIN` or `CROSS JOIN`)
  the target crashes (XXXXX, a null join expression) and Go answers the comma form's rows. For an
  OUTER join the target crashes (XXXXX "quantifier does not flow records") and Go reads the path
  as a table, which it is not (42703). An unnest leg BEFORE an outer join (`FROM w JOIN w.arr AS v
  ON … LEFT JOIN h …`, or the comma spelling) is the target's rows. Go refuses the JOIN spelling at
  translation (0AF00) and the comma spelling at parse (0A000, a JOIN after comma sources).

## A clause read through an unnamed operator (RFC-257 WS-J v29)

**Aligned.** Java resolves some clauses against an operator it builds with preserved expression
names and no name (`LogicalOperator.newOperatorWithPreservedExpressionNames`). An aggregate query
block's select list, GROUP BY, HAVING and ORDER BY resolve against `generateSelectWhere`'s operator
(`QueryVisitor.visitSimpleTable`, `QueryVisitor.java:258-285`). Every clause after an outer join
resolves against the join's operator, which replaces the sources it joins and every source before
it (`wrapOperandsForOuterJoin`, `QueryVisitor.java:614`). `collapseLeftSideOperators`
(`QueryVisitor.java:562`) collapses two or more preceding sources for the outer join's own ON.
Through such an operator `SemanticAnalyzer.lookup` (`SemanticAnalyzer.java:565`) takes no doubled
qualifier, since it prepends the operator's name. `lookupNestedField` (`SemanticAnalyzer.java:661`)
clears the qualifier, so a struct column's field is reached by the column's bare name in the
qualified pass too. So `x.f` over table x with a struct column x holding f is ambiguous there
(42702), where the FROM scope reads x's f. `w.w.f` names nothing (42703). A path beginning with a
column's bare name is read from that column (`x.x.g`: 42703). A GROUP BY-less aggregate block's
select list is read against the named sources first (`hasAggregations`, `QueryVisitor.java:970`),
whose error wins (`MAX(x.x.f)`: 42702). Go marks such sources `semantic.ScopeSource.Unnamed`
(`pkg/relational/core/query/semantic/scope.go:84`), set by `aggregateClauseResolver`,
`unnamedFromLegs` and `unnamedBeforeOn` (`pkg/relational/core/embedded/logical_predicate.go:846`,
`:788`, `:827`). Every shape is measured in `conformance/ws_f_table_qualifier_conformance_test.go`.

A lateral unnest's alias names its operator. `x.x.x` over a scalar element is the doubled reading
(rows). `item.item.b` over a record element is ambiguous (42702): the doubled member and the path
through the whole element are two names. Through an unnamed operator Java stops at an attribute's
direct match (`lookup`'s `continue`, `SemanticAnalyzer.java:579-589`), so `ss.ss` after an outer
join is the struct column, not ambiguous. It also names an unnest's whole element by its bare
alias, so `item.item` reads nothing there (`COUNT(item.item) … GROUP BY` is 42703; `COUNT(item)`
is the element) (WS-J v30).

## A FROM item's correlated path (RFC-257 WS-J v30)

**Aligned.** Java's `generateAccess` (`LogicalOperator.java:179`) reads a FROM item that names no
CTE, table, view or function as a correlated field. That is `resolveCorrelatedIdentifier`
(`SemanticAnalyzer.java:375`, called at `LogicalOperator.java:218`): the column lookup over the
operators of the query block and every enclosing one as one list
(`getLogicalOperatorsIncludingOuter`, `LogicalPlanFragment.java:111`). Go reads it the same way.
Any dotted FROM item that is not a table is a lateral unnest candidate (`unnestCandidateShape`,
`pkg/relational/core/embedded/select_parser.go:2608`), bound with the scope's lookup
(`bindLateralCollections`, `pkg/relational/core/embedded/lateral_collection_binding.go:23`). That
covers a table named by its qualified identifier (`FROM T.W, T.W.arr AS x`), a prior source's
struct column by its bare name (`FROM t2, n.arr AS x`), the doubled qualifier, and an outer query's
source (`EXISTS (SELECT 1 FROM h, w.arr AS v …)`). A path naming nothing is 42703 "Unknown
reference <path>".

The lookup is Java's two passes over one list (WS-J v31). The qualified readings of every level
come first, so two of them are 42702 (`EXISTS (SELECT 1 FROM w, w.arr AS v)` under an outer w). A
struct-relative reading is taken only when no level has a qualified one, so one never competes
with a qualified reading at another level (`Scope.ResolvePathAcrossLevels`,
`pkg/relational/core/query/semantic/scope.go:925`). A block's FIRST FROM item is read the same way
(WS-J v31): an EXISTS or scalar subquery's, and a derived table's, whose enclosing scope is the
FROM's prior sources (`FROM w, (SELECT v AS k FROM w.arr AS v) AS d`). It joins the block's other
sources, comma or INNER JOIN.

An AT is refused only where Java's generateAccess refuses it: on a CTE, table or prior FROM source
(42809). An unqualified name that is none of them is 42F01. A non-array path is 42F10 with or
without it. An array path takes it at any level (`FROM t2, n.arr AS x AT p`, `EXISTS (SELECT p
FROM w.arr AT p)`). A single name is decided in generateAccess order wherever it stands, a
subquery's first FROM item included. A WITH CTE, an operator of this or ANY enclosing block, or a
table is 42809: `findCteMaybe` (`SemanticAnalyzer.java:247`) walks every fragment, so `EXISTS
(SELECT 1 FROM w AT p)` under an outer w is "'W' is a common table expression". Anything else is
42F01 (`EXISTS (SELECT p FROM arr AT p)`, measured). A USING beside an unnest leg resolves each
column on every left operator, then on the right one, with the unnest described by its own
operator. `kk JOIN h ON … JOIN kk.items AS i USING (k)` answers kk.k, and a column two left
sources carry is 42702 (both measured). Measured in
`conformance/ws_f_table_qualifier_conformance_test.go` and
`conformance/ws_f_join_unnest_conformance_test.go`. A collection an enclosing query owns is
exploded as it is (`LogicalUnnest.EnclosingOwner`,
`pkg/relational/core/query/logical/operators.go:149`), as the inner of a FlatMap over the
subquery's other sources. (WS-J v28 recorded the target's answer for the first two paths as 0AF00;
that was the ORDER BY those probes carried.)

A chained unnest over a block's first FROM item (`EXISTS (SELECT t FROM q.bs AS b, b.tags AS t
…)`) is a FlatMap whose outer is the first Explode, Java's two ForEach quantifiers. Sibling unnests
of one row (`w, w.arr AS v, w.arr AS v2`, also over a two-table bottom `w, h, w.arr AS v, w.arr
AS v2`) are two links of one spine. Legs correlated to other legs of one FROM (`FROM w, (…
w.arr …) AS d, (SELECT d.k …) AS e`) plan through a partition whose lower depends on an upper leg,
as Java's PartitionSelectRule allows. A LEFT JOIN's null-supplying leg read by a later leg
(`w LEFT JOIN h …, (SELECT h.f …) AS d`) plans because a leg's provided aliases include its
physical join's own. All measured in the join spec, executed on FDB.

A lateral derived table reading a chain's LAST element (`FROM q, q.bs AS b AT o, b.tags AS t AT p,
(SELECT … t + o + p …) AS d`) and a WHERE [NOT] EXISTS over a spine with a table at its bottom
(`FROM q, q.bs AS b, b.tags AS t WHERE NOT EXISTS (… t …)`) now answer the target's rows; both are
compared, not declared, in the join spec (`ws_f_join_unnest_conformance_test.go:175`, `:321`).

The existential lowering in the nested-loop join rule declines when the inner quantifier is
null-on-empty (`pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:123-127`):
it wraps only a null-on-empty outer in DefaultOnEmpty, and an inner one would lose its
null-extended row. Java's `planPartitionToPhysical` (`ImplementNestedLoopJoinRule.java:311-320`)
wraps any null-on-empty quantifier in DefaultOnEmpty and then lowers. The FlatMap route Go takes
instead carries the wrap. Stricter than Java by construction, and measured in the join spec to
refuse nothing there: `w LEFT JOIN h … WHERE NOT EXISTS (… h.id …)` without an IS NULL, its twin
projecting h.f, and the IS NULL form all plan and match the target
(`ws_f_join_unnest_conformance_test.go:363-365`).

An unnest behind a later table inside a derived table that is itself a join leg (`FROM w, (SELECT
… FROM q, q.bs AS b, h) AS d`) plans: the leg's gate reads the cluster the body rotates to. A
lateral derived table reading an AT ordinal or a mid-link element (`FROM w, w.arr AS v AT p,
(SELECT … p …) AS d`, `FROM q, q.bs AS b AT o, b.tags AS t, (SELECT … o …) AS d`) answers the
target's rows.

An enclosing block's value in a post-aggregate expression (`(SELECT COUNT(*) + w.f AS c FROM h)
AS d`, an AT ordinal, a HAVING, a grouped block) is a constant across the aggregated rows. Java's
`SemanticAnalyzer.isComposableFrom` treats it so through its `constantCorrelations` set
(`SemanticAnalyzer.java:960-973`). A local non-grouping reference, including one whose source
shadows the outer name, is GROUPING_ERROR (42803) in both. Go goes further on three shapes the
target refuses, a read-side extension: a grouped block whose key shares the outer field's name
(`GROUP BY h.f` beside `w.f`; the target cannot plan it, 0AF00), a grouping key reading the
enclosing value (`GROUP BY h.f + w.f`; the target crashes, XX000), and a scalar subquery in the
select list (no such grammar in the target, 42601). Their rows are asserted literally and executed
on FDB (`TestFDB_PostAggregateOuterReference`,
`pkg/relational/sqldriver/post_aggregate_outer_reference_fdb_test.go:30`).

**Where the engines still differ**, pinned with both answers in `declared` in the join spec
(`ws_f_join_unnest_conformance_test.go:557`):

- A derived table reading a prior source beside a later one (`FROM w, (SELECT w.f AS x FROM h WHERE
  h.f = w.f) AS d, h`) is the target's internal crash (XXXXX) and Go's empty result, which is what
  the statement defines.
- A prior derived column typed NULL is the target's crash (XXXXX "should not be called") and Go's
  refusal: a later lateral body reading it is 42703, one not reading it 0AF00.
- An enclosing value in a grouped derived body's ORDER BY (with or without LIMIT) is Go's rows and
  the target's refusal, since the target has neither ORDER BY nor LIMIT in a subquery (0A000,
  0AF00). This is a Go read-side extension, executed on FDB over several groups
  (`TestFDB_PostAggregateOuterReferenceInOrderBy`, `post_aggregate_outer_reference_fdb_test.go:137`).
- An ON reading an alias buried in a preserved join (`a1` under `a1 JOIN a2 … LEFT JOIN … ON b.k =
  a1.id + 1` inside a lateral derived table) is the target's planning crash (XXXXX "is not an
  element of this graph") and Go's rows.
- An outer join whose preserved side is a first FROM item's scalar unnest (`FROM w.arr AS v LEFT
  JOIN h …` in an EXISTS or a derived table) is the target's crash (XXXXX; FULL is 42601) and Go's
  0AF00. Both refuse.

**SelectMergeRule declines a merge its reference would reject.** A merged member must state the
leg table its reference's members already state, or the reference has no flowed row and the
whole plan fails (XX000). Two merges re-tile a row. One dissolves a derived table whose body is a
star join (the join row's leg `A` becomes the body's `W`, `G`). The other dissolves a single-source
body by renaming (`FROM (SELECT * FROM w WHERE w.f > 1) AS a, h AS d, g AS e`: the lower {a, d}
states legs A, D; the merge states W, D). The rule asks the reference itself
(`expressions.LegTableConflictsWith`,
`pkg/recordlayer/query/plan/cascades/expressions/quantifier.go:552`, called at
`rule_select_merge.go:355`), the member-agreement scan's own derivation. Java's merge meets no such
refusal, since its member types carry no leg table, so Go's search is narrower here: the join
orders Java reaches only through the merged select (the dissolved body's table placed between the
parent's other legs) are not enumerated. The answers do not change. Every such shape was an XX000
or an execution error before and now matches the target in the join spec. The one arm that
planned before keeps its plan: `TestFDB_FilteredDerivedTableInsideAJoin` and
`TestFDB_DerivedStarJoinBesideAnotherSource`
(`pkg/relational/sqldriver/derived_star_join_beside_source_fdb_test.go:126`, `:22`) pin the GROUP
BY arms' EXPLAIN.

### Go does not reproduce Java's record_types order or anonymous type names (RFC-257 WS-J)

Java lists a schema template's `record_types` in `HashMap` iteration order. It names an anonymous
type (a struct column's unnamed type) `__type__<uuid>`, with a random UUID drawn per build
(`ProtoUtils.java:96-98`). It then emits a table's types from a `TreeSet` keyed by those names
(`TypeRepository.java:269-271`), so two anonymous types swap places between two builds of one
template. Go sorts `record_types` by name and names an anonymous type by the path that reaches it.
None of this is identity: union field numbers, record-type keys, field numbers and every named
message's position agree. A loader of either engine reads the other's template. The conformance
comparison (`wsjCanonical`, `conformance/ws_j_index_fidelity_conformance_test.go`) removes exactly
these three differences and nothing else.

### Arithmetic overflow is 22003 in Go; the target's escapes unmapped (RFC-257 WS-J/WS-E)

An INT or LONG overflow in an expression (`Math.addExact` and its kin) raises Java's
`ArithmeticException`. Java's relational layer has no mapping for it, so the client sees the
unmapped XXXXX. Go reports SQLSTATE 22003 (numeric value out of range), the standard's code,
listed among the Go-only SQLSTATEs above. The message text is Java's ("integer overflow" in an INT
lane, "long overflow" in a LONG lane). The overflowing rows and the statements refused are the
same in both engines. Pinned by the plandiff corpus's `DivergenceBothErrorMessagesDrift` rows
(`corpus_rfc082_divergences.go`, e.g. `arith_bigint_add_overflow`).

### A schema rebind runs the evolution validator; Java's catalog runs none (RFC-257 WS-J)

Java's relational catalog writes a schema row over a stored one without comparing the two templates
(`RecordLayerStoreCatalog.saveSchema`). Go's `validateSchemaRebind` (`fdb_store_catalog.go`) runs the
ported `MetaDataEvolutionValidator` from the bound metadata to the new one, with
`allowNoVersionChange` and `allowIndexRebuilds`, on every save that writes over a stored row (only
UPGRADE does, onto a strictly greater version of the same template) and on `RepairSchema`. A
record-type key change would make the store read old type A's rows as new type B's, so Go refuses it
with 42F59 "cannot rebind schema <db>/<s> from template <t>@<v> to <t>@<w>: metadata evolution
rejected", the validator's message as its cause. Pinned by
`TestRepairSchema_UpgradesThroughTheValidator` and `TestInMemory_RepairSchemaRunsTheRebindValidator`
(`schema_exists_behavior_test.go`).

The ported validator's messages carry the names and versions inline ("former index key used for new
index in meta-data (subspace key=…, index=…)"), where Java's `MetaDataException` carries them as log
keys beside a fixed message. The fixed part of each message is Java's.
