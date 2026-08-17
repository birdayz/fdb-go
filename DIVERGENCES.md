# Divergences from Java fdb-record-layer-core 4.12.11.0

Comprehensive list of Go vs Java differences. All Cascades planner subsystems
fully ported: ~65 PlanningRuleSet rule instances, 5/5 RewritingRuleSet rules,
34/34 physical plan types, 48/48 value types, 19/19 properties, 12/12 match
candidate types, 24/24 comparison operators, 9/9 predicates. Remaining items
are execution-layer, wire-format, or intentional architectural choices.

Validated against a live Java **4.12.11.0** conformance run (the cross-engine corpus runs against
live 4.12 in `just test` with a stale-annotation guard, and the suite is green).

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
4.12.11.0 `MAX_SUPPORTED_VERSION` is computed as the maximum enum value (`:182`) and
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
pinned spec — Java 4.12.11.0 reads 14 — and it is the strongest argument that exists
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
**Go:** Decomposes into `LogicalFilterExpression`, `LogicalProjectionExpression`, `LogicalSortExpression`, etc.

Go needs ~25 extra rewrite rules (Push/Pull/Merge per operator). Same functional behavior. Go's decomposition makes each operator's semantics explicit and simplifies rule correctness verification.

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

### Sibling leg aliases stay bound at runtime (`bindMergedOuterLegs`) — Go-only namespace widening, INTERIM

**Java:** `RecordQueryFlatMapPlan.executePlan` binds exactly TWO correlations, chained: the outer quantifier's alias over the incoming context, then the inner quantifier's alias over a context derived from that one (`RecordQueryFlatMapPlan.java:135-140`, resolved through the parent chain at `Bindings.java:116-134`). That is a PAIR, not a set of siblings. Java never has a merged row with sibling legs to serve, because where its planner collapses several sources into one quantifier it RE-ANCHORS every reference to a sibling **by ordinal**: `PartitionSelectRule.java:296-303` builds one translation entry per lower alias, `when(lowerAlias).then(FieldValue.ofOrdinalNumber(QuantifiedObjectValue.of(newUpperQuantifier), index))`, and `translateCorrelations` replays it. After that rule the sibling alias does not exist — it is not kept bound, it is *gone*.

**Go:** `executor.bindMergedOuterLegs` (`flat_map_cursor.go`) binds EVERY leg of a merged outer row under its own correlation, over that leg's window. Go's two-level NLJ→FlatMap lowering collapses a multi-source outer into one join whose row is a merged concat, so binding only the join's own alias leaves the source aliases the inner still references unbound — they evaluate to NULL and an EXISTS correlation that never matches silently drops rows. Widening the namespace is what makes those references resolvable without a name.

**Why it is a divergence and not a port.** It reaches the same end (a leg-correlated read resolves to its own leg's slots) by the opposite means: Java removes the alias and rewrites the reference to an ordinal; Go keeps the alias alive and binds a window to it. Keeping N sibling aliases live is strictly more namespace than Java has at that point, and it is what lets a leg-correlated reference survive into a context where Java would already have rewritten it.

**Precedence, which the widening forces Go to define and Java never needs.** `concatLegPositionals` emits the two top-level leg windows first and appends buried sub-windows after, so an alias appearing twice resolves to its FIRST (widest) entry — matching `addBuriedLegLayouts` and every other reader of the leg table. A leg SHADOWS a same-named binding in the enclosing context for the duration of the inner (Java's chained-context lookup), and the enclosing context object is never mutated. All three are pinned in `merged_outer_leg_binding_test.go`.

**Retirement condition.** This retires when the ordinal re-anchor covers every shape that reaches this cursor — i.e. when a leg-correlated read is rewritten to `ofOrdinalNumber` against the merged quantifier before execution, as `PartitionSelectRule` does, and no sibling alias needs to be resolvable at runtime at all.

**One half of the namespace question is now settled, and it is the half that was arguable.** The seed-window layout this path's producers and readers share used to be a `map[string]` keyed by the UPPER FOLD of each leg's correlation, so a leg was addressed by text while carrying an identity. It is now `map[CorrelationIdentifier]`. That was measured before it was changed, per lookup, at every keyed reader over the real-FDB corpus: **1400 lookups, every one holding a correlation, the identity selecting the same window the fold did on every one** — no text-only hit, no identity-only hit, no divergence. The only two readers that held text and no identity (the gathered-EXISTS wrap's dotted splitter and the existential decline probe's mint-from-key) reached **zero** lookups and were unreachable by panic probe across `./pkg/relational/...` and `./pkg/recordlayer/query/...`; both are gone. What remains a divergence is the runtime binding namespace itself — Java has no sibling aliases to bind, keyed by anything — and that is unchanged by this.

Those digits are a **dated point measurement** of a namespace that no longer exists, and the census that produced them retired with it. The standing instrument over the same readers is `values.AssertSeedWindowReaderCensus` (`seed_window_reader_census.go`), wired into the sqldriver `TestMain` beside its siblings. It is narrower on purpose — per read it records only hit/miss plus the two DECLINE classes — because what it is for is not licensing the conversion (that is done) but keeping the five readers from going quiet: every claim on this path has the shape "this class is EMPTY", and an unreached site prints that identically to a site measured clean. It floors each reader (measured 962 / 92 / 184 / 2 / 160, floored an order of magnitude below) and hard-zeros `QUALIFIED-NO-IDENTITY` and `CHILDLESS-BAKED`, the two declines that replaced the deleted text lookups. Verified by mutation: deleting the `gatheredGroupSlot` recorder reds the corpus run with `gatheredGroupSlot reached 0 reads, want >= 16`.

It was framed as blocked on CQ-63, on the grounds that a leg's quantifier states no row type and so there is no domain to state the ordinal in. **That gate is now closed:** `underivableLegs` in the leg-local bakeability census is **0 of 960 leg derivations** over the real-FDB corpus (it read 82 of 846 when this entry was written). Every leg states its row. What remains is not a missing type — it is that the lowering reaches this cursor **without a stated merged layout**, so there is no merged ordinal to re-anchor TO. `MergedReAnchor` in the same census measures that directly: **0 of 174** firings take the ordinal re-anchor; all 174 take the leg-alias pass-through.

**Which paths those are was stated wrong here and is now measured.** This entry said "on the EXISTS-over-join and RFC-153 buried-leg paths". Split by call site over the whole real-FDB corpus, all **174 are the EXISTS site** (`implementJoinWithExistential`) and **0 are the buried-leg site** (`buildCorrelatedFlatMapPlan`) — which not only contributes nothing but *does* state a layout, `buriedLegOrdinalLayout` answering on 314 of 362 firings. No leg-correlated leaf reaches the match arm on the buried path at all. There is no buried-leg work in this retirement.

On the EXISTS site the merged layout is `ordinalSeedLegWindowsOf(step1RV)`, nil exactly when `foldStep1Seed` declined; the residue reaching the pass-through is a folded projection carrying an `ExistsValue`, not a merged row, and the 154 declines behind it are `legIsOrdinalSafe` refusing a **FlatMap leg**. What those legs flow is the finding: 94 flow a bare `QuantifiedObjectValue`, and **60 flow `RC(_i: QOV(leg_i))` — Java's own unnamed positional merge**, `PartitionSelectRule.java:283-291`'s `RecordConstructorValue.ofColumns(… Column::unnamedOf)`, which Go builds in `positional_merge.go`. So Go already produces the exact merged row this divergence's retirement is defined against, and its own layout authority declined it.

**RFC-200 CLOSED THE 60, AND THE RETIREMENT DID NOT ADVANCE. Both halves of that sentence are measured.**

The layout authority now has a NESTED leg kind and a separate opt-in entry (`values.OrdinalSeedLegWindowsAcceptingNested`), `legIsOrdinalSafe` admits a FlatMap leg whose result value is the positional merge, and the keyed readers address it through the FUSED two-step path that is Java's `ofOrdinalNumberAndFuseIfPossible`. Over the real-FDB corpus the `foldStep1Seed` outcome census moved exactly as predicted: ACCEPT 78 → **138**, reconstruct-nil 154 → **94**, and the positional-merge share of reconstruct-nil 60 → **0**. The permanent `correlatedStep1` wall (108) and the `rv-no-exist-ref` pass-throughs (202) did not move.

**What did NOT move is this entry's own number.** `LegLocalBakeCensus` still reports **174 of 174** reads taking the leg-alias pass-through, `MergedReAnchor` still 0. The reason is a measurement RFC-200 did not have when it was written: cut by the *firing* each read happens under — a mapping that did not exist and is now a standing instrument (`legRebaseOrigin`, threaded from the entry points) — **all 174 reads sit under firings whose refused leg is a BARE QOV, and none under a positional-merge firing. That was already true before the change.** RFC-200's gate (d) asserts the positional-merge subset is 0 post-change; it was 0 pre-change too, so the gate holds VACUOUSLY and the nested window removes zero pass-through reads.

So the retirement condition now decomposes cleanly, and the remaining work is named rather than estimated:

- **60 firings — CLOSED** by RFC-200. Their merged row is positioned, not pass-through-bound. They contributed no reads to this census, so closing them moved no digit here.
- **102 firings — OPEN, and the LARGER block. It is a SHAPE residue, not a typing one, and that correction is measured.** A FlatMap leg flowing a bare `QuantifiedObjectValue` states no row layout, so there is nothing to position. **All 174 of this entry's reads belong to this block.**

  The block was booked at 94 and read as "bare *untyped* QOVs, where Java types unconditionally", with the prescribed fix being to type the producers. Both halves of that reading are now refuted by instruments rather than by argument:

  - **The count is 102, not 94** — RFC-200's own gate-(a) fixtures added 8 bare-QOV firings. Live at `sqldriver/embedded_fdb_test.go`'s `ReconstructNilBareQOV`.
  - **The population is 100% TYPED.** The outcome census's witness now spells the flowed type, and every declined leg carries a real `RecordType`: arity 2 ×30, arity 3 ×66, arity 1 ×4, plus arity 4 ×2 on a `RecordQueryNestedLoopJoinPlan` leg — **arity 1-3 across the FlatMap legs, 1-4 across the whole declined population**, and both spellings appear in the code comments because they describe different populations. There is nothing to type. The witness previously printed a boolean derived from `Typ != nil`, which no constructible QOV can make false, so the population read as typed for the wrong reason and would have read as *converted* on the day a typing sweep started.
  - **Typing cannot convert any of them in principle.** `legOrdinalSafety` refuses a FlatMap leg on `values.IsPositionalMergeRC`, which requires a `*RecordConstructorValue`; a `QuantifiedObjectValue` fails that assertion at any typing whatsoever.
  - **The producer is measured, not inferred.** All four non-test `RecordQueryFlatMapPlan` constructions are instrumented and each declined leg is attributed by its result value's own identity: every FlatMap-legged decline originates at `buildCorrelatedFlatMapPlan`, which emits **zero** untyped QOVs (25,406 constructions: 476 typed QOV, 6,732 positional-merge RC, 18,198 other). The structural hypothesis that the residue does *not* come from the minting site is confirmed.

  **A real and separate Java divergence survives, on a different population — and it was attributed to the wrong sites.** Three FlatMap sites carry an untyped QOV: `implementExistentialSelect` **1,609**, `yieldExistsFlatMap` **269**, `implementJoinWithExistential` **249**. Java cannot express any of them: `QuantifiedObjectValue.of` has no untyped overload (`QuantifiedObjectValue.java:187`) and `Quantifier.getFlowedObjectType` is a `Verify.verify` plus `requireNonNull` (`Quantifier.java:801-810`).

  **But the first two BUILD NOTHING.** They flow `sel.GetResultValue()` verbatim — exactly as Java's three `RecordQueryFlatMapPlan` constructions flow `selectExpression.getResultValue()` (`ImplementNestedLoopJoinRule.java:187, 201, 214`) — so their counts are a count of *traffic through a courier*, and the divergence cannot be fixed at either. The author is whatever fills `sel.GetResultValue()`; on the EXISTS path that is the SQL translator's mint, which builds `values.NewQuantifiedObjectValue(outerQ.GetAlias())` where Java builds `overQuantifier.getFlowedObjectValue()` (`GraphExpansion.java:401`). **Measured for the first time: 1,086 mints, 100% untyped, 0 typed**, standing in `values.SelectResultMintCensus` and floored in the sqldriver harness. (The total is a rule-firing count and is not deterministic — a second consecutive run read 1,004. The 100% is.) `implementJoinWithExistential`'s 249 is a genuine third mint. Booked as **CQ-96**.

  None of those values reaches the decline classifier, so typing them advances THIS entry by zero reads. Both censuses are floored so the population stays counted while it stands, and `describeFlatMapResultOrigin` now reports the MINT ahead of the courier so the misattribution cannot recur silently.

  **A second divergence found on the same pass, booked as CQ-97:** `RecordQueryFlatMapPlan.GetResultType()` (`plans/flat_map.go:86`) returns `values.UnknownType` unconditionally where Java derives it from the result value (`RelationalExpression.java:195-196`) — and `planBuriedLegConcat`'s plan walk exists *because of* that stub, which its own comment says outright. A workaround documented at the call site for a capability Java has is a booking, not a footnote.
- **108 firings — PERMANENT.** The `correlatedStep1` wall: a WHERE-EXISTS correlating into a leg buried in an inner join is the canonical semijoin, whose good plan is a SARG'd correlated index scan under the FlatMap, and that requires name binding to flow the sibling comparand into the index. Java binds every correlation by name and has no positional-correlation concept.

**This entry therefore retires with the 102, not before — and NOT by typing them.** The 60 were a necessary structural step — the authority can now express a nested row at all, and the executor twin agrees with it bit-for-bit — but on the read axis this divergence is exactly where it was.

Two further facts came out of probing the 60 end-to-end, and the first correction supersedes an earlier claim here.

**The projected-EXISTS fold over a THREE-source FROM DOES execute** when it carries equijoin predicates, which is the form the corpus produces. An earlier version of this paragraph said the shape "does not execute" — that was measured on a PREDICATE-FREE comma join, which is a *separate* pre-existing defect (booked CQ-70, pinned as its own test, loud rather than silent). The distinction matters here because it changes what is blocking what.

**What is actually latent is the nested READER arm.** Over the whole corpus the seed-window reader census reports `NESTED-HIT 0` at both keyed readers, and mutating the fused two-step address back to flat `Offset + legOrdinal` leaves the end-to-end row probe green. The layout half of RFC-200 is live and exact (ACCEPT 78 → 138; `existentialRebase` 962 → 1086, the RFC's own §6 growth prediction confirmed), but every window those reads select is a FLAT top-level run — a nested SUB-window is only selected by a reference to a leg buried INSIDE the merge, and no corpus query produces one. That is why RFC-200's gate (a) mutation directions are not yet writable, and it is a second, independent reason this entry's retirement does not advance on the read axis.

The corpus totals quoted throughout this entry are POINT measurements and they move whenever a query is added anywhere in the sqldriver suite — the residues (`0`) are the durable claim, the denominators are not. **Demonstrated rather than asserted: the read count quoted as `174 of 174` throughout this entry measures `190 of 190` on the current tree**, with `MergedReAnchor` still 0 and the bare-QOV cut still 100%. The digit moved; nothing the entry claims did. Do not chase the denominators through this file — re-measure. Re-measure with `go test ./pkg/relational/sqldriver/ -count=1 -v | grep "leg-local bakeability"` rather than trusting the digits below.

**Status: the binder is LIVE, it HAS a reader, and that reader is NOT load-bearing. Those are three separate measurements and all three matter.**

An earlier version of this entry said "no non-test consumer today", which read as *dormant*. It is not dormant. It EXECUTES on the real-FDB corpus on every clustered-box gather: over one full `sqldriver` suite run it binds **15,689 leg windows across 303 distinct shapes** (`executor.FormatMergedLegBindingCensus`, reported by the sqldriver `TestMain`). The commonest single shape alone accounts for 6,513 of them. It runs once per OUTER ROW, so it is on the row-rate path, not the planning path.

**It HAS a reader, on a MULTI-LEG merged row.** A previous version of this entry said the corpus's binding lookups were "all for one single-leg unnest alias" and that none reached a multi-leg row. That was wrong, and it was wrong in the direction that mattered: it under-stated the exposure. Over a full run, **3** lookups resolve to a window this binder produced, and all 3 are on a merged row carrying SIBLING legs — the `foldable_colliding_answers` shape in `TestFDB_ExistsInnerShadow`:

```sql
SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(1, ST."C") = 1)
```

`FROM ST, OT` is the two-leg merged outer (ST at `[0,3)`, OT at `[3,5)`); `COALESCE(1, ST."C")` folds constant, so the colliding reference never survives into the join predicate and the inner is planned as its own two-source merge — the arrangement that ends up consulting the outer leg's binding. The near-identical cousin `colliding_plain` (same outer, a genuinely-read correlated predicate) binds the identical windows and reads NONE of them; the reader shape had to be measured, not guessed. Reads are attributed by a flag the BINDER stamps on each window, not by alias: alias names collide across queries — the corpus binds twenty unrelated legs under the outer name `X` — and an alias-keyed reconstruction was measured to mis-attribute. That **3** is the CORPUS's own count; the census reports **12** over a full run, because the two pins below each execute the same query twice more and every one of those reads lands on the same shape — same shape, excused by the same registrations, expected. Only the 3 are the corpus.

**That reader is NOT load-bearing, and BOTH halves of the evidence now stand rather than being re-run by hand.** The two perturbations are the binding's PRESENCE and its AIM, and they fail on different days — a consumer that reads a leg column gets a WRONG VALUE from a misaimed window long before it gets no value at all — so each has its own pin over the same shape, in one process:

- presence: `TestFDB_MergedLegBinding_NothingReadsTheBinder` runs that exact query and asserts the binder BOUND its windows on it and that NOTHING read them, through a sink scoped to that one `EvaluationContext`. It used to be a LICENSE — the same query down both routes, once with the window serving the read and once with `EvaluationContext.WithMergedLegReadBypass` declining it, asserting the rows agree — and the ordinal model removed the reads themselves, so "the binding is redundant on this shape" became "the binding is not looked at" and the exclusion it granted is gone. Both halves of the engagement floor stayed: the rows are checked, so the zero is a zero over a shape that RAN, and the bind count is checked, so windows-neither-bound-nor-read cannot pass as the retirement;
- aim: `TestFDB_MergedLegBinding_WrongWindowsAreUnobservable` runs it with every window rotated onto a SIBLING leg's span (`EvaluationContext.WithMergedLegWrongWindows`) and asserts the same.

Until the second existed this was the entry's weakest sentence: it cited "pointing every window at slot 0 of the merged row" as a mutation somebody had run, gated by nothing, so the day a binding started to matter no test went red. That mutation was also weaker than its description — leg 0 of a merged row already starts at slot 0, so the constant left `ST`, the leg the corpus's reader actually reads, aimed CORRECTLY. Rotation onto the sibling is what the standing form does instead. Both pins carry engagement floors (the read must happen, and under the aim pin it must resolve THROUGH a perturbed window), so neither can go green by ceasing to reach the binder, and both register their result with the census gate only on their passing path. Replacing the whole binder body with `return ec` remains a by-hand check and remains green; it is now the redundant one, since the presence pin performs it per-execution.

**What the declining route actually is, measured.** Not "the reference resolves by a second route": every one of those reads is of a window that shadowed NOTHING (`0` shadowing, `12` unshadowed over a full run), so declining it leaves the alias resolving to **no binding at all**, and the rows are still the ones Java gives. That is the strongest form the perturbation can take, and the pin asserts it took that form rather than assuming it. It also means the structural marker cannot be reasoned from: "the binder is the alias's only binding" is TRUE here and "load-bearing" is false, because on this shape `COALESCE(1, ST."C")` folds before execution and the value never reaches an answer. Redundancy is settled per reader shape by running both routes, never by reading the shadowing flag — and the exclusion is registered under (alias, merged-row leg layout), not under the bare name `ST`, so it excuses the proven shape and not every future read of a common table name.

**The producer is BACK, and the earlier framing of this paragraph is inverted.** It read: "the binder's only planner-side producer is the leg-local bake, that arm was DELETED in this branch, and a channel whose only producer is absent cannot have a consumer that depends on it." Both halves have moved.

What was deleted was the *name-keyed* bake — the arm minted the leg-local ordinal by resolving a display name against the leg's row type, RFC-197's forbidden move — and it was deleted rather than kept, permanently. It was never coming back, because the thing it was for is no longer needed: the resolver's correlated arm now builds `QuantifiedObjectValue.of(alias, getFlowedObjectType())` the way Java's `Quantifier.java:801-803` always has, so a leg-correlated read **arrives** carrying a single accessor at a non-negative ordinal in its own leg's domain. Nothing has to be minted.

So the arm's live disposition is the PASS-THROUGH, and the pass-through is a producer of exactly what this binder consumes: a read that keeps its own leg correlation. Measured over the real-FDB corpus, **174 of 174** firings take it (`LegLocalBakeCensus`: total 174, baked 174, and `identityInLegDomain` 174 of 174 — every one states its own slot with no name consulted anywhere). The activation criterion's `Baked > 0` is therefore satisfied by a real population rather than waiting on one.

The identity number is only as good as its filing, and the two census cuts do not constrain each other: `Baked + MergedReAnchor + Declined == Total` and `IdentityInLegDomain + IdentityOtherDomain + LazyNameOnly == Total` both hold under a WHOLESALE misfiling of the identity class, which would invert this **174 of 174** to **0 of 174** with the gate green. The gate now cross-checks the two (`IdentityInLegDomain == Baked + MergedReAnchor`, `IdentityOtherDomain + LazyNameOnly == Declined`), which is sound because the arm dispatches on that same class: arms 1 and 2 are both guarded on it and arm 3 is reached exactly when the guard fails.

**What has NOT changed is the load-bearing measurement.** The reader is still not load-bearing, and the mutation that shows it is now sharper than "delete the producer": replacing the pass-through with a deliberate bind to the WRONG leg leaves the whole sqldriver suite green, so the candidate carrying whatever this arm emits loses on every covered shape. The absence of a load-bearing reader is no longer explained by the absence of a producer — the producer is live — but by the candidate losing. That is a weaker guarantee than the old one and it is stated here rather than left inferred: a cost-model change that let that candidate win would make this binder load-bearing without touching it.

Scope of the claim: the **sqldriver corpus**, which is where the census gate is enabled; the census itself is process-global and self-corrects the moment another harness turns the gate on.

**The activation criterion**, asserted by the sqldriver `TestMain` (`assertMergedLegBindingCensus`):

> a read on a MULTI-LEG merged row, outside a reader shape PROVEN redundant in this run, implies `LegLocalBakeCensus.Baked > 0`.

A read with no producer fires the alarm; a read while a producer exists is CQ-53 phase 2 working and must not. `Baked` is **174** on the corpus, so this criterion is currently satisfied on its permissive side — which is a change in what it can catch, and worth stating plainly: while `Baked` was 0 the criterion was a sharp alarm on any read at all, and now it only fires if the pass-through itself disappears. The one exclusion is a REGISTRATION, not a list: the two pins above each register the (alias, merged-row layout) THEY measured, only on their passing path, so a divergence under either perturbation fails that pin, drops it from the registry, and turns those same reads into a red gate in the same run — the license cannot outlive its proof. The registry holds a set of proof names per shape and the gate reports all of them, so losing one of the two is visible rather than silently absorbed by the other. A whole-suite floor of ≥1 read guards the other end: a dead read counter would otherwise report "0 READ" beside a full bind tally, which is exactly the shape of the claim this entry quotes.

**Consequence for the shadowing and precedence semantics above.** They remain *unobservable*, but for a narrower reason than "nothing reads them":

- the *join's-own-alias skip* never engages — a clustered box gathers under a MINTED alias (`B$BOX`, `C$BOX`) whose legs keep the user aliases, and a minted box alias is never a leg alias, so every leg binds. The unit fixtures cover only the opposite (degenerate) case, where the join's alias equals a leg's;
- *first-claim-wins* is reachable in principle — the corpus does produce a merged row with a repeated leg alias (`[$m"2, Q, $m"2]` under `R$BOX`) — but no read distinguishes the two claims;
- *shadowing* is never observed either: the corpus's only reads are of windows that displaced NOTHING (`12 multi-leg, 12 of those UNSHADOWED` in the census line), so no lookup ever chooses between a leg and an enclosing binding.

So the justification for these three semantics is currently **consistency with the leg table's other readers**, not any behaviour a test can see. That is a weaker footing than the earliest wording implied, and it is stated here rather than left to be rediscovered. `TestFDB_MergedLegBinding_LiveBoxGatherShape` pins the box-gather half at FDB level: the live shape's bindings are the correct ones, and its read count is zero — coupled, so it fires on a read with no producer and stays silent on one the restored bake produced.

**Why the path is kept and made cheap rather than deleted.** Deleting it is a query-engine change requiring its own RFC and a Graefe ACK, and the hazard it guards is real if a load-bearing consumer arrives (an unbound leg alias evaluates to NULL, and an EXISTS correlation that never matches silently drops rows). What was done instead is to stop paying for it: the per-outer-row allocation storm is gone (13→5 allocations on the dominant shape, 49→11 on a 6-leg one), so the path now costs close to nothing while the retirement question is settled.

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
| 2. Max data access cardinality | CardinalitiesProperty gate + comparison — index arm has TWO criteria: PK-bound-by-equalities, falling through to unique-index | Data-access cardinality gate — implements the uniqueness criterion only | **Deliberate divergence — Go's sargable surface excludes the PK, so Java's criterion 1 has no constructible input (see below)** |
| 3. Residual predicate count | NormalizedResidualPredicateProperty (CNF size) | `countResidualPredicates` using `cnfSize()` | Aligned |
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

Java (`PlanningCostModel.java`, tag 4.12.11.0):

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

Java (`PlanningCostModel.java`, tag 4.12.11.0):

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

Java 4.12.11.0's `RewritingCostModel.compare()` has **six** ordered criteria: (0) `outerJoinCount`, (1) `selectCount`, (2) `tableFunctionCount`, (3) normalized CNF conjuncts, (4) predicate-count-by-level, (5) `semanticHashCode` tie-break. Go ports criteria **1–5** (`selectCount`, `tableFunctionCount`, CNF conjuncts, predicate-count-by-level, deep hash tie-break). `Planner.WithCostModel()` wires the cost model per phase.

**Criterion 0 — `outerJoinCount` — is DELIBERATELY NOT ported. This is an intentional, justified divergence, not a gap.**

In Java, `outerJoinCount` (penalize any surviving `OuterJoinExpression`, checked FIRST) is a **correctness guard**, not a heuristic. Java's `OuterJoinExpression` is a logical-only node with **no physical operator** and exactly one consumer (`RewriteOuterJoinRule`); it *must* be rewritten before planning. Java's single-final-expression prune keeps one survivor per group, and without `outerJoinCount` the un-rewritten `OuterJoinExpression` (0 selects) would beat the rewritten form (2 selects) on the `selectCount` tie-break, survive the prune, and hand the planning phase an **unimplementable** node — the query would fail to plan. `outerJoinCount` forces the implementable rewritten form to win. (Evidence: `OuterJoinExpression.java` is logical-only; `PlanningRuleSet.java` has no rule matching it; `RewritingCostModel.java:60-68` comment states the rationale.)

Go has **no such correctness problem**: Go's outer join is a `SelectExpression{joinType: LEFT/FULL OUTER}` that is **directly implementable** — `ImplementNestedLoopJoinRule` plans it as a materialized `RecordQueryNestedLoopJoinPlan` (RFC-152), a read-side extension Java lacks (Java has only the correlated `RecordQueryFlatMapPlan` re-scan). Go deliberately keeps the un-rewritten outer-join select as the REWRITING prune survivor (it wins on `selectCount`, 1<2), so PLANNING can derive **both** the materialized NLJ (scan the inner once) and the correlated FlatMap (re-scan) and cost-choose (RFC-152). **Porting `outerJoinCount` would force the rewritten form to win the prune, discard the outer-join select, and suppress the materialized-NLJ alternative — a plan regression, proven by `TestFDB_ArrayUnnestOrdinality` (asserts the materialized `NestedLoopJoin(LEFT OUTER, …)` box).** Pinned by `TestRewritingCostModel_KeepsUnrewrittenOuterJoin` / `TestRewritingBoundary_KeepsUnrewrittenOuterJoin`, which go RED if `outerJoinCount` is (re-)introduced.

**Companion divergence — `RewriteOuterJoinRule` runs in Go's PLANNING phase (`PlanningExplorationRules`); Java's `PlanningRuleSet` does not contain it.** Because Go keeps the un-rewritten outer-join select as the prune survivor (above), PLANNING re-fires the canonicalizer to re-derive the rewritten form and keep the correlated-FlatMap alternative available alongside the materialized NLJ. Kept as an intentional divergence (Java can drop it precisely because `outerJoinCount` makes the rewritten form the sole survivor — the guard Go must not adopt). Empirically the full outer-join FDB suite is green whether or not this PLANNING re-fire is present (the correlated FlatMap is also derivable directly by `ImplementNestedLoopJoinRule.yieldGeneralFlatMap`), so a future cleanup could remove it — but only after pinning the correlated-LEFT-OUTER index-nested-loop (RFC-042) path with a dedicated plan-shape test.

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
| NormalizedResidualPredicateProperty | `countResidualPredicates()` + `cnfSize()` inline in cost model | Aligned (inline) |
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
union its temp-table frontier on every emitted row), parks state in the scratch (the two distincts),
or refuses to resume rather than restarting (the buffered union fallback returns
`UnsupportedContinuationError`). The admission filter is therefore **vacuously true today** and is
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

### Go-only SQLSTATEs

`pkg/relational/api/errcode.go` tracks Java's `ErrorCode` enum: **every one of Java's 74 codes exists in Go** — that direction has no gap. Go defines four codes Java has no member for:

| Code | Go constant | Why it exists |
|---|---|---|
| `21000` | `ErrCodeCardinalityViolation` | SQL-standard class 21. Java's enum has no class-21 member at all, and its relational layer has no scalar-subquery cardinality check, so nothing there reports "subquery returned more than one row". Go enforces the cardinality and needed a code to name the violation. |
| `22003` | `ErrCodeNumericValueOutOfRange` | SQL-standard code for arithmetic overflow. Java's enum has no member for it; overflow there escapes as a raw Java exception and reaches `ExceptionUtil`'s final fallthrough, reported as `UNKNOWN`. |
| `22012` | `ErrCodeDivisionByZero` | SQL-standard code for division by zero. Java's enum has no member for it; the operation raises `java.lang.ArithmeticException`, which is not a `RecordCoreException`, so `ExceptionUtil.toRelationalException` reports `UNKNOWN`. |
| `54F02` | `ErrCodePlanComplexityLimitReached` | Go bounds Cascades planning; Java's SQL layer never enables its planner caps, so the condition cannot arise there. Detailed below. |

The first three predate this list; it was written when a claim that there was only one Go-only code turned out to be false. The list is not maintained by prose: `goOnlyErrorCodes` in `errcode.go` carries the same four with their reasons, and `TestErrorCodesMatchJava` diffs the Go enum against a captured snapshot of Java's, failing if the difference and the list disagree in either direction — a new unlisted Go-only code, a stale entry, or a Java code missing from Go. Regenerate the snapshot on a Java version bump using the command in its doc comment.

#### `54F02` in detail

Java's Cascades planner defines three complexity caps — `maxTotalTaskCount`, `maxTaskQueueSize`, `maxNumMatchesPerRuleCall` — and throws `RecordQueryPlanComplexityException` from `CascadesPlanner.java:448`, `:493`, and `:1026` when one trips. **Java's SQL layer never enables any of them.** `PlannerConfiguration.buildRecordQueryPlannerConfiguration` (`fdb-relational-core/.../query/PlannerConfiguration.java:150-161`) sets index scan preference, in-join union size, index fetch method, disabled rules and `setJoinRightDeep`, and none of the three cap setters. All three guards are gated on a positive bound (`CascadesPlanner.java:325-335`) and the default is `0`, documented as "unbound" (`RecordQueryPlannerConfiguration.java:236,244`). So no JDBC user of stock 4.12.11.0 can observe that exception.

Go bounds planning at 100,000 tasks at every production callsite, because unbounded search on a pathological query is a liveness hazard. That makes budget exhaustion a **Go-only condition with no shared surface to conform to** — the cross-engine conformance principle governs inputs Java also attempts, and Java does not attempt this one.

Porting Java's mapping would have been wrong twice over: `ExceptionUtil.recordCoreToRelationalException` matches `RecordQueryPlanComplexityException` against no `instanceof` arm, so it would land on `ErrorCode.UNKNOWN` (`XXXXX`) — a code Java's own javadoc says "shouldn't be used in general" (`ErrorCode.java:168-171`) — reached only through a path Java's SQL layer never walks.

Class 54 ("program limit exceeded") is the honest class: the planner gave up because the query is too complex, and simplifying it is a real remedy. `54F01` (`EXECUTION_LIMIT_REACHED`) is deliberately NOT reused — it means an *execution*-time limit and is tied to a scan/row continuation reason, so conflating plan-time with it would corrupt a meaning Java owns. `54F02` is unused in Java's enum, leaving room for Java to adopt it should it ever enable the caps.

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
| CoveringIndexPlan | `RecordQueryCoveringIndexPlan` holding `*RecordQueryIndexPlan` as a FIELD | Aligned (RFC-220) — no longer collapsed |
| CountValue + NumericAggregationValue | `AggregateValue` | Aligned (no rule distinguishes them) |
| VariadicFunctionValue | `ScalarFunctionValue` | Aligned (COALESCE folding matches Java) |
| 12 Comparison subclasses | Single `Comparison` struct with optional fields | Aligned |

## RFC-220 — the projection is RETAINED over a covering scan where Java drops it

Java's `MergeProjectionAndFetchRule.onMatch`
(`MergeProjectionAndFetchRule.java:62-78`) does exactly one thing when every
projected value pushes through the fetch: `call.yieldPlan(fetchPlan.getChild())`.
The projection is DROPPED. Go yields the fetch's child with the projection
RETAINED above it (`rule_merge_projection_and_fetch.go`,
`rule_implement_projection.go`).

This is deliberate and permanent, and it is not a shortfall — Go is stricter
than Java here.

Java can drop the projection because
`RecordQueryCoveringIndexPlan.getResultValue()` returns
`IndexedValue(indexPlan.getResultType().getInnerType())` — the base record type,
not the projected shape — and it carries a standing TODO in the Java source
admitting exactly that. `pushValue` translates the projection's Values only for
the FEASIBILITY test; Java then discards both and tolerates a wider output type
than the query asked for.

Go's covering plan reports the same full partial-record shape, but Go's callers
do NOT tolerate a wider row: dropping the projection leaks the whole record and
the wrong output schema. So the projection stays.

There is therefore no "result-value rewrite" in Java to port. An earlier draft of
RFC-220 described this as a fallback pending such a port; that was wrong about
Java and is corrected here. The divergence is not effort-gated and should not be
"closed" by making Go match Java — matching Java would be a regression.

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
| **Merge UNIONs (`InUnion`, `MergeSortUnion`) decline a MIXED-direction comparison key** | Java encodes direction and NULL placement into the physical key with `ToOrderedBytesValue` (`ProvidedOrderingPart.comparisonKeyValue`), so it can merge on `(a DESC, b ASC)` | plan-space narrowing only — never wrong rows; the request falls back to an in-memory sort | **TRACKED, fail-closed.** Go's `ToOrderedBytesValue` has no evaluator, so the executable comparison key is the raw Value and a merge runs in exactly one direction. Both merge-union rules now gate on `properties.NaturalComparisonKeyValues(parts, isReverse)` — the same gate the intersection plans already used — and decline any candidate whose parts disagree with the resolved direction, instead of building a plan whose merge front compares one key the wrong way round. Newly REACHABLE rather than newly introduced: until descending merges were enumerated at all, every comparison key was ascending. **Measured:** the IN-union gate declines exactly twice over the 2475-query corpus, both times `parts=[ASC, DESC]` against a forward merge; the merge-sort-union gate declines nothing, because the union merge carries no equality-bound key for a request to give a direction to (`TestDistinctUnionMergedOrderingCarriesNoEqualityBoundKeys` pins that premise, so the fence's dormancy stays a fact rather than an assumption). The reachable half is pinned at RULE level by `TestInUnionRuleRefusesMixedDirectionMerge`, verified red with the gate removed — the corpus scenarios do NOT pin it, since the mixed-direction shapes plan an in-memory sort with or without the gate. Closing it means porting the `ToOrderedBytesValue` evaluator, which also closes the counterflow-NULLS decline in `EnumerateSatisfyingComparisonKeyValues`. |
| **`PrimaryScanMatchCandidate.ComputeMatchedOrderingParts` SKIPS two branches of Java's shared implementation, and softens a third from an assert to a decline** | `ValueIndexLikeMatchCandidate.computeMatchedOrderingParts` (`ValueIndexLikeMatchCandidate.java:63-118`) hard-asserts `Verify.verify(ordinalInCandidate >= 0)` on an unknown sort parameter, branches on `normalizedKeyExpression.createsDuplicates()`, and de-duplicates emitted ordering VALUES through a `normalizedValues` set | plan-space narrowing only, never wrong rows | **TRACKED, all three deliberate.** (a) A primary key is a flat list of scalar columns, so `createsDuplicates()` is false at every position and the fan-out branch has nothing to act on. (b) With no fan-out and distinct key columns, the `normalizedValues` dedup can never fire. (c) Java ASSERTS an unknown sort parameter is impossible; Go ends the reported prefix instead (`primary_scan_match_candidate.go`, pinned by `TestPrimaryScanMatchCandidateStopsAtUnknownParameter`). Java's assert is the stronger statement and Go should eventually match it, but a panic in library code is forbidden by design principle 4, and truncating is the fail-closed reading — it can only cost an access path, never claim an order the records lack. Revisit (a) and (b) together if a primary key ever gains a fan-out key expression. |
| **Cross-candidate PK-intersection pruning retains singleton alternatives** | Java builds compensated singles and all intersections in one shared `IntersectionInfo` map, evicts immediate subpartitions only after a useful replacement, then yields survivors once | safe plan-space widening / possible winner difference, never missing or wrong rows | **TRACKED SAFE RESIDUAL (RFC-190.5b review):** Go first yields candidate-local accesses, then its separate cross-candidate pass builds a private eviction map. It correctly prunes smaller intersection expressions internally but cannot retract already-yielded exact singleton scans. Do not delete memo members post hoc. Closing this requires a partition-level assembler that creates singles once, shares the map with intersection enumeration, and flattens survivors before any yield; regressions must preserve unrelated finals and safe logical compensations. |
| **Per-wrapper relink** (RFC-070 nil-inner shells across ~20 wrappers, vs Java's eager `memoizePlan` to concrete) | Every non-leaf plan has its child; deterministic relink to the cost winner; one final member | dropped/nil child (0 rows); nondeterministic plan/cache | **CLOSED for the shell half (RFC-183).** Rules now bake the concrete child at rule time, matching Java's memoizePlan-as-constructor-argument; `verifyChildrenMemoized` rejects a holed expression at yield, always-on (ports `CascadesRuleCall.verifyChildrenMemoized`); the ~600 lines of repair machinery are deleted, after instrumentation showed ZERO shells across the full suite and all 2407 corpus queries. `ValidatePlanInvariants` remains as the sink-side backstop. **NOW CLOSED (RFC-184 W2):** P5's terminal step landed — every physical wrapper is deleted, so the parent→child edge is no longer stored twice. A physical plan is its own cascades expression holding its children SOLELY as quantifiers (`RecordQueryFlatMapPlan`/`RecordQueryNestedLoopJoinPlan` carry `outerQ`/`innerQ` and no embedded plan-snapshot field; `GetChildren` resolves through them), so the dual-storage state is unrepresentable. The earlier BLOCKER — `rule_implement_nested_loop_join.go` building compensating filters that were never memoized, so the plan pointer held what EXECUTES while the quantifier held what the memo COSTS (9868 rule-time edges, 472 semantically different) — is resolved: those rules now memoize each compensating operator and advance the quantifier over that reference in lockstep (the FlatMap collapse), matching `rule_implement_simple_select.go`'s long-standing shape. The **LIVE DEFECT** that fell out of the same finding (the memo costing expressions that are not the ones that execute) is therefore gone — see the "memo costs an expression that is not the one that executes — CLOSED" section below. **STILL TRACKED (separate concerns):** retiring `findBestPhysicalPlan` — extraction's ad-hoc cost pick outside the cost framework, wired to ONE site against `findPhysicalPlan`'s twenty (do NOT "fix" that asymmetry by propagating the cheapest-selector: it is ordering-blind and turns `ORDER BY … DESC` into ASC, measured, see RFC-183 §10); and the reachability tally still driving a residual population of unreachable edges to zero (`plan_reachability.go` — a completeness/hygiene concern, NOT the wrong-tree-costing defect, which is closed). **RETRACTED (RFC-224):** an earlier revision cited "1186 references hold multiple finals, 1125 multiple PHYSICAL finals" as P5's blocker, then claimed singleton finals held at extraction. The first measurement was taken while rules were firing, and the second assertion mistook Java's mechanism for Go's property. Go deliberately retains one winner per required physical property; `TestExtractionIsUnambiguous` now pins the actual requirement — a total winner/compatible-physical-fallback selection on the path extraction dereferences, with zero dead ends and coherent retained alternatives. Note also the split's stated rationale in `plans/plan.go:23-28` — "physical and logical plan trees live in different namespaces in Java" — is FALSE: `QueryPlan<T> extends RelationalExpression` (`QueryPlan.java:51`), so a Java plan IS a RelationalExpression; the comment conflates package separation (real) with hierarchy separation (not real). |
| **Go-only physical-filter builders** (`ImplementSimpleSelectRule`, NLJ residual — extra paths past `Compensation`; `ImplementIndexScanRule` was the third until RFC-076 retired it) | Index-only predicates never become an executable residual | panic / wrong plan on a vector `DistanceRank` residual | **COVERED** — `ImplementFilterRule` `!isIndexOnly()` gate (RFC-151) + the retained `validateNoIndexOnlyResidual` catch-all backstop (pinned by `TestVectorPlan_*`). **TRACKED end-state:** gate the remaining builders so the net can retire. |

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
mutation both ways). The JAVA half is INFERRED from 4.12.11.0 sources
plus the checked-in plan golden above. That golden is Java's own
recorded planner output, which makes it strong, but **the Java planner
was not run**. What would upgrade it: reproducing
`sql-functions.metrics.yaml:204` from an actual Java run. That was
deliberately not done — nothing is blocked on it, and the cost is not
justified by the claim's current use, which is only to explain why Go's
guard must not be "fixed" back toward Java.

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
