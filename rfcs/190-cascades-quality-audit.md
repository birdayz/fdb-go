# RFC-190 — Cascades quality audit v2

Status: **Reviewed — implementing** (Graefe ACK + Torvalds ACK on the RFC).
190.1 was **materially re-designed** after its original delete premise proved false (the arm's
"never produced an executable plan" gravestone is a lie — deleting it regresses working FDB tests).
The new 190.1 (converge on Java's two-rule architecture via a guarded `PartitionSelectRule`) carries
a fresh **Graefe ACK** from a two-round adversarial design dialogue (round 1 NAK: a wrong-rows
merge-case hole; round 2 ACK: the live-existential guard verified airtight) **and a Torvalds ACK**
(three impl-discipline conditions folded: Step 1+2 atomic, Step 3 helper-reference proof, Step 5 1M
stress gate; LOC corrected to arm 388 / wrap 477). §190.1 is fully gated — ready to implement.
Branch: `feat/rfc190-cascades-quality-audit` · one branch, one PR.
Reviewers: Graefe (Cascades alignment) + Torvalds (code quality) on RFC and impl.

## Problem

A 2026-07-23 full-engine quality assessment (5 subsystem review agents cross-verified against
Java 4.12.11.0) graded the engine architecture A−, cost B, rule fidelity B+, tests B+, code B.
The wire-compat hard line is clean — every finding is read/optimization-path. This RFC grinds the
concrete residuals in one milestone, severity order (correctness first). Items are independent;
each lands its own commit(s) + regression test. Grouped for one review lap + one PR.

## Items

### 190.1 (HIGH) — EXISTS over an N-way join: converge on Java's two-rule architecture

**Premise correction (the original delete plan was WRONG).** The first RFC-190 draft (Graefe+Torvalds
ACK) rested on the arm's gravestone comment: *"HAS NEVER PRODUCED AN EXECUTABLE PLAN."* **That comment
is FALSE.** Implementing the delete broke two FDB tests (`TestFDB_BuriedInnerJoinProjectedExists` +
`_Discriminating` → `0AF00: could not plan query`): the arm `implementNWayJoinWithExistential`
(`rule_implement_nested_loop_join.go:~2607`) DOES produce correct working plans (`[[10 true]]`,
Java-verified in-test) for the **explicit-`JOIN…ON`** projected-EXISTS-over-3-way shape. It only
crashes for the **comma-join** shape (RFC-173 ordinal tripwire). Deleting it is a net regression.
So 190.1 is not a dead-code delete; it is a real architecture fix — re-designed principles-first and
re-reviewed (this design carries a fresh **Graefe ACK** via a two-round adversarial design dialogue
that caught and closed a wrong-rows hole before any code; needs Torvalds ACK on the revised RFC).

**The functionality (form × projection × correlation).** EXISTS over a ≥3-way join, comma-join AND
explicit-JOIN surface forms, projected (`SELECT …, EXISTS(…)`) AND WHERE (`… WHERE EXISTS(…)`),
correlated AND uncorrelated. Java plans EVERY such shape through exactly two rules — `PartitionSelectRule`
(reduce an ≥3-quantifier `SelectExpression` to binary sub-selects; `PartitionSelectRule.java:63` admits
ANY quantifier via `all(anyQuantifier())`, existential included) and `ImplementNestedLoopJoinRule`
(matches EXACTLY 2 quantifiers; existential inner → `FirstOrDefault(NULL)` + existential-flag `FlatMap`,
`.java:187,313-316`). Java has **no N-way arm and no positional-merged-row join construct**.

**Root diagnosis.** Go carries a Go-only positional-ordinal join pipeline — the N-way arm AND a second
mechanism `translateExistsOverGatheredCluster` (`exists_gathered_cluster_wrap.go`, the WHERE-EXISTS
path) — that exists ONLY because Go's `PartitionSelectRule` (`rule_partition_select.go`) **refuses to
partition existential selects**: the `existentialCount==1` bail (`:67-69`) and the projected-multi
decline (`:70-86`). Both Go-only constructs are workarounds for that refusal. The code's own comment
admits it (`:57-60`): *"subsuming that Go-only arm into partitioning is separable … that migration is
its own slice."* This is that slice.

**Fix — Option A: make `PartitionSelectRule` partition existential selects the Java way (guarded).**
Replace the over-broad `existentialCount==1` bail with a **targeted per-bipartition guard**, so the flat
`[ForEach×N, Existential]` select decomposes into binary sub-selects Go's existing existential-FlatMap
path (`implementExistentialSelect`) already implements — SARG-preserving (no cross-product), projected
`ExistsValue` riding through unchanged — **retiring both the N-way arm and (follow-on) the wrap**.

The guard (design dialogue, Graefe-verified airtight): **reject any bipartition whose LIVE set
(`lowersCorrelatedToByUppers`, computed at `rule_partition_select.go:~415`) contains an existential
quantifier.** Rationale: that live set is exactly what `positionalMergeCase` collapses into positional
ordinals (`:550`, ≥2 live) and what Case-2 flows as the lower's own row (`:559`, ==1 live) — and Go's
merge/Case-2 machinery **cannot represent a projected existential as a positional ordinal** (a real Go
constraint Java lacks — Graefe's NAK, sustained). A **projected** δ in a lower is necessarily live
(`ExistsValue.GetCorrelatedTo`→δ, `value_exists.go:107`) → caught. A **WHERE** δ is never live (its
`ExistsValue` is a predicate classified to `lowerPredicates` at `:403`, never in the live set) →
admitted as a correct Case-1/Case-2 semi-join filter — so the guard **preserves the working WHERE
multi-EXISTS peel (cell 8)**. Case-1 (`:537`) flows only `LiteralValue(1)`, never a live alias, so it
needs no guarding. Graefe verified `:550`/`:559` are the ONLY sites that flow/collapse a lower alias,
that the guard's key is complete, and that no correct plan is lost (the complementary δ-upper split is
always co-enumerated, chaining to the terminal binary `[ForEach, Existential]`).

**Hazard proof (Graefe-ACK'd):** under the guard, no existential is ever a member of the live set at the
case dispatch → none is collapsed by `:550` or flowed by `:559`; a projected δ is always upper →
survives to the terminal binary select → `ImplementNestedLoopJoinRule` implements it as
`FirstOrDefault(NULL)` + existential flag. Graefe's `{a,δ}` counterexample (which reached
`positionalMergeCase` under the naive delete) is now rejected before `:550`. No wrong-rows path exists.

**Migration — CORRECTED after implementation probing (the RFC's original "Step 1 = stop the
un-enclosure, near-flag-flip" estimate was WRONG; proven empirically).** Go ordinalizes N-way
existential clusters at TRANSLATION time (positional seeds via the cluster-gate machinery,
`cluster_gate.go:399-419`, porting Java `QueryVisitor.java:429-434`). Two probes established the
truth: (i) the guard alone, on the existing ORDINAL seed, converts the comma-join crash into SILENT
WRONG ROWS (always-true EXISTS — δ→a mis-wires as an ordinal) → **the guard is UNSAFE on an ordinal
seed and CANNOT ship as a standalone safety net**; (ii) merely flipping `inInnerCluster=true` marks
the box name-model-enclosed but nothing ordinalizes it → `0AF00: join did not ordinalize`
(`cluster_gate.go:399`), breaking the WORKING explicit-JOIN shape too. So the emit, the guard, and
the arm-retirement are **ONE ATOMIC commit** — they cannot be separated.

The correct Step 1 is **direct-emit**: a Java-faithful port of `QueryVisitor.java:429-434` — dissolve
the ≥3-way INNER cluster into a flat NAME-model `[ForEach×N, Existential]` select (each leg a plain
alias-bound ForEach), **bypassing** `translateRef`-on-the-box and thus the ordinalization machinery
(which stays intact for every other shape). Java has no ordinal seed; its flat select is alias-bound —
direct-emit is that, ported. `PartitionSelectRule` then decomposes it BY ALIAS (its entire logic is
alias-keyed: `aliasToQ`, `computeTransitiveCorrelationOrder` from `GetCorrelatedTo`), so δ→a stays a
name correlation and the wrong-rows mis-wire is gone; the dup-column discriminator is *trivially* safe
(distinct QOVs, no positional last-leg-wins hazard).

- **Step 0** — correct the false gravestone (`rule_implement_nested_loop_join.go:2907`). No behavior
  change. (The RED comma-join FDB test lands in the atomic commit below, not as a separate red commit —
  no-red-commit rule.)
- **Step 1 (ATOMIC: direct-emit + guard + arm-retirement, ~250-400 lines, three call sites):**
  1. Add `gatherInnerClusterOnPredicates(j)` beside `gatherInnerClusterLegs` (`ordinal_seed.go`) —
     walk the inner-join tree collecting each node's `OnPredicate` (`gatherInnerClusterLegs` discards
     nested ONs — for `(p JOIN q ON q.qid=p.id) JOIN r ON r.rid=p.id` only the top ON is gathered; the
     buried `q.qid=p.id` must be recovered). Comma-joins carry predicates in the WHERE
     (`splitNonExistsPredicates`) — already gathered.
  2. In `buildExistentialJoinSelect` (`cascades_translator.go:4084`), add the `clusterHasBoxLeg(j)`
     branch BEFORE the AXIS-1 block: gather legs, decline (`return nil`) on `mintedBindingLeg` (dup-alias,
     mirroring the wrap `exists_gathered_cluster_wrap.go:71`); translate each `leg.op` (with
     `inInnerCluster=false`) as `NamedForEachQuantifier(NamedCorrelationIdentifier(leg.binding), ref)`,
     `sourceAliases=leg.binding`; predicates = nested ONs + `splitNonExistsPredicates(f.Predicate)` +
     `extractExistsPredicates`; append each `esq` as `NamedExistentialQuantifier` with
     `existsInnerCorrelation`'s joinPred; `return NewSelectExpressionWithAliases(resultValue, quants,
     preds, sourceAliases)`.
  3. Remove the `existentialCount==1` bail (`rule_partition_select.go:67-69`) + add the live-existential
     guard after `:415` (`continue` if any live-set member is existential). KEEP the `>=2` projected
     decline (`:70-86`) as the scoped cell-7 reach-gap; keep `applyExistentialSourceAliases`.
  4. Delete the arm's `≥3` dispatch (`rule_implement_nested_loop_join.go:53-58`) + `implementNWay­JoinWithExistential`
     (~388 lines) **in the SAME commit** — its matcher also matches `[ForEach×N, Existential]`, so on the
     name-model select BOTH it and PartitionSelectRule fire; the arm would re-ordinalize into a competing
     crash. **Torvalds condition A:** grep-prove `planBuriedLegConcat`/`reconstructFoldStep1Seed`/
     `legIsOrdinalSafe`/`collectInnerLegAliases` are still referenced by retained paths (kept); delete
     only `existInnerIsScanSafe`/`nwayOuterProbe` (arm-only).
  *Validate: comma AND explicit projected EXISTS → `[[10 true]]`; `_Discriminating` → `[[7 true],[8
  false]]`; `FourLegJoinDiscriminating`; cells 3/4/8 (WHERE) still green via the wrap; a static unit
  test asserting no yielded lower carries a live existential.*
- **Step 2 — SARG + stress gate (same commit or immediate follow):** `EXPLAIN` asserting each
  decomposed binary join SARGs its inner (index/PK probe, NOT a merged-row `PredicatesFilter` over a
  cross-product) + **1M stress before/after (Torvalds condition C)** — the arm's cross-product was
  O(N³); the SARG'd decomposition must not resurface as a regression (point-lookup <5ms, index-equality
  <10ms thresholds hold) + full-corpus explain-diff against the committed golden (all flips Java-verified).
- **Step 3 (separable follow-on, same PR or later)** — retire `translateExistsOverGatheredCluster`
  (477 lines, the WHERE-EXISTS wrap) once cells 3/4/8 are demonstrably served by the partitioned
  name-model WHERE-EXISTS select. NOT urgent — WHERE rides the wrap, untouched by the arm-retirement.
  Gated by full-corpus explain-diff + preservation of scalar-subquery pre-eval registration.

**Cell 7 (multi projected EXISTS, `SELECT …, EXISTS(…) x, EXISTS(…) y`) — honest scoped decline, NOT
claimed fixed.** Keep Go's `existentialCount>=2` projected decline: it strands cleanly (0AF00, never
wrong rows), backed by the guard. **Java parity is asymmetric (Graefe correction):** for a *same-leg*
sibling correlation Java's `PartitionSelectRule.java:234-243` also rejects, but for a *cross-leg*
correlation (δ1→a, δ2→b) Java falls into its Case-3 merge and *does* attempt the reduction (correctness
unverified). So Go's decline is **conservative**, not symmetric-with-Java; it never ships wrong rows.
Book cell 7's full support (nested, not flat, translation) as a follow-on.

**Why Option A over the alternatives (Graefe's terms):** (B) fix-the-crash-in-the-arm keeps a Go-only
rule matching ≥3 quantifiers that builds a SARG-destroying cross-product Java never emits; (C) extend
the wrap to correlate projected EXISTS reinvents the FlatMap existential path over a positional box
(already tried → wrong rows, correlation dropped). Only (A) restores logical/physical separation (the
join is *explored* as logical sub-selects, SARG/join-order emergent), rides EXISTS as the emergent
`FirstOrDefault`+flag property rather than bolted-on positional plumbing, and deletes the ~388-line arm
(+ the 477-line wrap in Step 4) to converge Go onto Java's exact two-rule architecture.

**Test plan:** the full form×projection×correlation matrix as FDB rows tests (Java-4.12.11.0 semantics),
the buried-leg discriminators (all legs share column `k`; a mis-bind flips value AND boolean), an
explicit hazard-bipartition probe on Graefe's `{a,δ}` shape (correct rows + plan shape shows
`FlatMap`/`FirstOrDefault`, not a merged-row δ projection), a NOT-EXISTS/below-FOD-filtered variant
that proves the *guard* (not the coincidental positive-case fallback) carries correctness, SARG
plan-shape assertions, and full-corpus explain-diff gating Steps 3-4.

### 190.2 (MED, unpinned) — cost-comparator transitivity

`planning_cost_model.go` gates five structural rungs on `opsA.inMemorySortCount == 0 &&
opsB.inMemorySortCount == 0` (`:286` typeFilterDepth, `:302` fetchDepth, `:315` distinctDepth,
`:320` unmatchedField, `:337` mapFilter). A conditionally-skipped rung breaks transitivity.
**Concrete cycle** (constructed, will be the red test): plans A, B both sort-free and tied on
rungs 1–5; A beats B on the gated `typeFilterDepth` rung (`:286`); B beats A on the ungated
`fetchCount` rung (`:306`) — which never evaluates for the A,B pair because `:286` short-circuits.
Introduce C carrying a sort with `fetchCount == fetchB < fetchA`, tied to both on rungs 1–5:
`compare(A,C)` skips `:286`, so fetchCount fires → C<A; `compare(B,C)` ties fetchCount, falls to
`inMemorySortCount` (`:365`) → B<C. Result: **A<B, B<C, C<A** — a cycle. Winner selection is a
linear min-scan (`winner_lookup.go`), so an intransitive relation has no well-defined minimum and
the winner depends on member iteration order — the exact nondeterminism the hash tie-break exists
to kill.

**Fix (scoped to the Go-invented sort-gate class — Graefe condition):** the five per-rung sort
gates are a Go-specific band-aid for "a sorted plan winning a structural rung before criterion #12
(fewer sorts) can drop it" — the real intent is *fewer sorts is high-priority*. Hoist the
`inMemorySortCount` rung ABOVE the structural depth rungs (but below rungs 1–5) — partition sorted
vs sort-free first, then apply the structural rungs uniformly within each class — and drop the five
per-rung sort gates. **This is a real reprioritization, not a safe no-op** (Torvalds): sort-count
moves from rung ~12 (deliberately placed *before* the phantom-prone scalar fallback, `:351-364`) to
rung ~1, so it now dominates the 7 structural rungs; and between two plans that BOTH carry a sort,
the de-gated structural rungs newly apply where they previously abstained. For sort-free pairs
nothing Java-relevant reorders (the gate was already `true`); the sorted-pair cases are the behavior
change. Graefe owns this on merit (ACKed) — every resulting corpus flip is Java-verified via
per-commit explain-diff, none waved through as "obviously safe". **Do NOT touch the `both-index-scans` conditional rung
(`:291`)** — it is Java-faithful (Java `PlanningCostModel.java:208`, the fetch rungs only apply when
both plans are index-based), so it stays for parity even though it is technically also conditional.
The claim is therefore "removes the Go-specific sort-gate intransitivity," **not** "provably total"
(the Java-inherited index-scan conditional is retained by design). Verify no unexplained corpus flip
via explain-diff; Java-verify any flip.
Regression: `TestCostModel_ComparatorTransitivity` — a property test over generated op-profiles
asserting antisymmetry + transitivity, **scoped to the sort-class fix** (the A/B/C sort-gate cycle
above), RED on current code. The test must not flag a cycle arising solely from the Java-faithful
`:291` index-scan conditional as a Go defect.

### 190.3 (MED, narrow) — partial-PK prefix scan mispriced as a point probe

`plans/cost.go:34` prices a scan as cardinality=1 when `numBound > 0 && allEquality && numBound ==
len(comps)`. But `comps = GetScanComparisons()` is the **bound prefix only** — a composite-PK scan
bound on just the first column has `numBound == len(comps) == 1` and is priced as a 1-row point
probe. The ctx-aware path (`planning_cost_model.go pkFullyEqualityBound`) correctly checks
`numBound >= pkLen`; `HintCost` has no ctx so it can't. Criterion #2 abstains for two-unbounded
comparisons, leaving a scalar-fallback window where a prefix scan beats a genuinely cheaper plan.

**Fix:** stamp the primary scan plan with its PK column count (or a `fullPKEqualityBound` bool) at
construction (where metadata is available, like `WithIndexMetadata`/`WithDistinctRecordsSignal`),
and gate the `HintCost` point-probe on full-PK coverage. When PK coverage is unknown/partial, fall
back to selectivity pricing, never cardinality=1.
Regression: `TestScanHintCost_PartialPKPrefixNotPointProbe`.

### 190.x-bundled (cheap correctness latents, already booked 2026-07-22)

- **finding 5** scalar signed-zero: `values/map_field_values.go:254` scalar fallthrough `a == b`
  (−0.0==+0.0) vs `semantic_hash.go:156` `%v` ("−0"≠"0") → memo dedup miss. Bitwise-compare scalar
  floats (the []float arms already do, RFC-176 §2). Pin `TestScalarConstant_SignedZeroHashConsistent`.
- **finding 8** merge cycle guard: `memo_merge.go:103` `reachable` recurses `Members()` only;
  correct only by the undocumented "every final is also an exploratory member" invariant. One
  distinct-final `InsertFinal` from approving a cycle → planner hang. Walk `AllMembers()`. Pin a
  distinct-final-with-cycle regression.

### 190.4 (MED) — MatchIntermediateRule subset subsumption

`rule_match_intermediate.go:225,459` requires equal quantifier count (`matchIntermediateStructural`)
and hard-gates `matchSingleSourceAgainstSelect` to one quantifier per side — no non-exact/subset
subsumption (Java's existential Example-2 multi-match, `MatchIntermediateRule.java:112-149`).
Bijection enumeration landed (RFC-189); subset did not. **Fix:** port Java's subset subsumption —
a query SelectExpression with fewer quantifiers subsumed by a candidate with more, binding the
extra as compensation. Graefe-gated (matching-infra). Regression: an index-match test on a
multi-quantifier candidate that Go currently misses.

### 190.5 (MED) — index-intersection reach

`intersector_primary_key.go:94` caps k-way at 3 (`planner.go:662,709` caps ≤4 candidates/≤8
matches); PK-only comparison key; `plans/intersection.go:90` carries no reverse flag → no
reverse-ordered (`DESC`) intersections. Java (`AbstractDataAccessRule.java:434-524`): unbounded
k-way via `ChooseK` + ordering-compat sieve, any common ordering, `isReversed`. **Fix:** lift the
k-way cap to Java's ChooseK enumeration (bounded by candidate count, not a magic 3); carry the
comparison-key direction + reverse flag into `RecordQueryIntersectionPlan`. Graefe-gated.
Regression: a 4-way intersection + a `DESC` intersection plan-shape test.

### 190.6 (architectural — Graefe ruling) — NLJ ordering-obliviousness (localized, not systemic)

Investigation corrected the premise: the join/set-op rules are **not** systemically
ordering-oblivious. `ImplementDistinctUnionRule` (`:67,93`), `ImplementInJoinRule` (`:102`),
`ImplementInUnionRule` (`:233`) and the unary Filter/Projection/Limit rules all consult
`GetRequestedOrderings()` / pick the ordered child via `getWinnerForOrdering(ordering)`. The gap is
**one rule**: `ImplementNestedLoopJoinRule` is the lone ordering-sensitive Cascades rule that never
calls `GetRequestedOrderings()` — it picks both legs by cost only (`:98-107`, `:697,707`
`PreserveOrdering`). It ports none of Java's `ImplementNestedLoopJoinRule.java:180-217` Case 1/2a/2b
outer-ordering enumeration.

Two disjoint input sets, two conclusions:
- **(A) `ORDER BY` with no satisfying index anywhere** — Java's Cascades declines (`RemoveSortRule.java:108`
  yields nothing → `UnableToPlan`; `RecordQuerySortPlan` exists but is **legacy-planner-only**,
  `RecordQueryPlanner.java:315` — the *only* construction site). Go's `ImplementInMemorySortRule`
  (RFC-001) produces correct sorted rows. This is a clean sanctioned read-side SUPERSET — keep it.
  **Doc fix:** CLAUDE.md's "Java has no physical sort operator" is imprecise — Java's *Cascades*
  never emits one; the class is legacy-only. Correct the wording (CLAUDE.md + DIVERGENCES.md).
- **(B) `JOIN … ORDER BY <outer-indexed-col>` where the ordered index is not the cheapest outer scan**
  — BOTH engines handle it. Java Case 2a picks the outer partition that *satisfies* the ordering
  (even if pricier) → sort-free streamed `FlatMap`. Go picks the cheapest outer → `ImplementSortRule`
  finds no satisfying partition → declines → in-memory sort wins by default → materialized O(n log n)
  sort. Same rows, strictly worse plan. The physical sort **masks** this (correct rows ship green) —
  the exact latent-quality failure mode.

**Fix (Graefe-gated, additive):** make the Go NLJ rule `RequestedOrdering`-sensitive and port Java's
Case 1/2a/2b — for each requested ordering, also yield an ordered `FlatMap` variant whose outer/inner
leg *satisfies* the order (Java `partitionOuterBySatisfyingAndDistinct`/`rollUpIfSatisfyOrdering`),
alongside the cost-best variant. `ImplementSortRule` then drops the sort against the ordered join and
the in-memory sort prunes on cost. Does NOT remove the physical sort (still the sole survivor for set
A). Book the (B) divergence into DIVERGENCES.md immediately as the regression sentinel.
Regression: a yamsql `JOIN … ORDER BY <outer-indexed-col>` with `plan_not_contains: InMemorySort`
(sort-free), PLUS the paired index-less `ORDER BY` asserting `plan_contains: InMemorySort` — the
unprobed dimension that let (B) ship green.

### 190.7 (MED-HIGH) — de-duplicate the existential-join family

The 37-line compensating-operator chain appears 4× in `rule_implement_nested_loop_join.go`
(`:986`, `:2436`, `:2851`, `:3189`); `:2436`≡`:2851` byte-identical (comments included), `:986`
already reworded → hand-drift underway. `tryExistsFlatMap`≡`buildExistsFlatMap`. NLJ file is 3417
LOC vs Java 331. **Fix:** extract `buildExistsCompensationChain(preserveAlias bool, corr, …)` (the
fresh-vs-preserved alias fork is a param, not a copy) and collapse the yield helpers. Pure
refactor — behavior-preserving; the existing NLJ tests + explain-diff are the safety net (zero
plan change required). Torvalds-gated.

### 190.8 (LOW) — scalar-function semantics: one source of truth

`values/values.go:1971` `evalScalarFunction` (36 fns) overlaps `embedded/scalar_functions.go` and
`query/expr/walk.go`, synced by prose comments ("matches embedded…"). A missed edge (rounding,
`ABS(MinInt64)`, LENGTH-on-bytes) makes plan-time constant-fold silently disagree with runtime
eval — a wrong-results class. **Fix:** a shared function table both layers dispatch through, or a
cross-layer equivalence test that fails on divergence. Start with the test (cheaper, proves
current agreement); table extraction is the durable fix. Torvalds-gated.

### 190.9 (LOW) — NLJ cursor twin inner-loops

`executor/streaming_cursors.go:1561` (hash-probe) ≈ `:1609` (linear-scan) — near-identical 45-line
bodies differing only in candidate source. A join-semantics fix in one misses the other, in the
hot path. **Fix:** unify via a candidate-index slice (`allInnerIndices()` exists at `:1528`).
Behavior-preserving; existing NLJ execution tests guard it.

### 190.10 (MED) — behavioral tests for the 12 untested rules

113/125 rules have direct coverage; 12 don't (8 zero-reference, all ordering/projection/fetch
optimizers whose effect is invisible to row tests): `SplitSelectExtractIndependentQuantifiersRule`,
`OrderedPrimaryScanRule`, `RemoveProjectionRule`, `PushUnorderedUnionThroughFetchRule`,
`PushRequestedOrderingThrough{Select,SelectExistential,InLikeSelect,RecursiveUnion}Rule`,
+ 4 with indirect e2e only. **Fix:** a unit test per rule that seeds the matching expression and
asserts the transformation fires (plan-shape, not rows). Adds a completeness test: every registered
rule has a behavioral test.

### 190.11 (MED) — cost-model test suite

The component that picks the winner has 3 tests + 1 fuzz-sanity. **Fix:** add selection-flip tests
(which plan wins for representative op-profiles), rung-order tests, and the 190.2 transitivity
property test. Target: every rung has a test that flips the winner when that rung changes.

### 190.12 (MED) — committed plan-shape golden

`explaindiff` renders every corpus query's plan but its baselines are explicitly NOT committed
(`explaindiff_test.go:166`), so cross-commit plan drift is caught only by manual before/after.
**Fix:** commit the rendered baseline as a checked-in golden + a CI test that diffs against it and
fails on any un-blessed plan change (re-bless intentionally, like a snapshot test). This is the
standing net that would have caught every silent plan-quality regression this audit found.

### 190.13 (LOW) — doc rot

Fix stale comments that actively mislead: `plandiff.go:10`/`runsql.go:84` "Java engine is stubbed"
(it's LIVE in CI); `abstract_data_access_rule.go:~29` "no containment pruning yet" (it DOES prune
via `findContainingAccess`); `plans/distinct.go:38` + 3 sites "cross-page-buggy" (fixed 2026-07-20,
TODO C5). No code change — comments only; but they send the next reader down phantom paths.

### 190.14 (LOW) — cost-model diagnostics + library stderr

`countConcreteNode`/`concreteResidualPredicates` are plain type-switches with no unpriced-type
detector (unlike the cost path's `warnUnpricedPlanType`) → a future plan type is silently uncounted
for criterion #3 with zero diagnostic. And `warnUnpricedPlanType` writes to `os.Stderr` from
library code (`planning_cost_model.go:1452`). **Fix:** add an unpriced-type fallthrough warning to
both walks; route the warning through an injected logger, not `os.Stderr`.

## Commit sequencing & the one-PR decision

Owner directive: **one branch, one PR** for the whole audit. Torvalds's NAK concern — "one
explain-diff over the whole PR shows flips from many causes, un-bisectable" — is valid and is
resolved by **commit discipline within the single PR**, not by splitting it:

1. **190.12 (plan-shape golden) lands as the FIRST commit.** It is the standing net every later
   commit is measured against — and the safety net for the 190.7/190.9 refactors (whose "behavior
   preserving" claim is otherwise only manual before/after).
2. Then the **zero-intended-flip** group (190.13 docs, 190.10/190.11 tests, 190.7/190.8/190.9
   refactors) — each commit must show the golden UNCHANGED. Any flip here is a bug in the "refactor".
3. Then **correctness** (190.1, 190.2, 190.3, the bundled 5/8), one logical change per commit.
4. Then the **Graefe-gated reach** items (190.4, 190.5, 190.6), one per commit.

**Per-flip gate at COMMIT granularity, not PR granularity:** every plan-affecting commit is
explain-diff'd against its OWN PARENT (not the whole PR vs master), so each flip is attributable to
exactly one change and Java-verified before the next commit. `git bisect` works at commit level, so
one PR with this ordering is as attributable as three PRs. If the owner later prefers a physical
split, the commit boundaries above are already the cut lines. (Surface this reconciliation to the
owner; proceed with one PR unless they direct otherwise.)

## Performance

No item regresses steady-state planning. 190.2/190.3 change cost *ordering* (not cost of
computing it) — validated by explain-diff + the 1M stress test (row counts + latencies unchanged
except where a flip is Java-verified). 190.7/190.9 are behavior-preserving refactors. 190.12 adds a
CI test, no runtime cost. 190.5 enlarges intersection enumeration under the existing candidate cap.

## Test plan

Every item lands with a regression test that is RED before the fix (correctness items) or pins the
new behavior (fidelity/refactor items). Milestone gate: full 56-target suite green on every commit,
1M stress no-regression for 190.2/190.3/190.5, explain-diff reviewed for every plan flip.
