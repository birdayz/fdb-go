# RFC-190 — Cascades quality audit v2

Status: **Reviewed — implementing** (Graefe ACK + Torvalds ACK on the RFC)
Branch: `feat/rfc190-cascades-quality-audit` · one branch, one PR.
Reviewers: Graefe (Cascades alignment) + Torvalds (code quality) on RFC and impl.

## Problem

A 2026-07-23 full-engine quality assessment (5 subsystem review agents cross-verified against
Java 4.12.11.0) graded the engine architecture A−, cost B, rule fidelity B+, tests B+, code B.
The wire-compat hard line is clean — every finding is read/optimization-path. This RFC grinds the
concrete residuals in one milestone, severity order (correctness first). Items are independent;
each lands its own commit(s) + regression test. Grouped for one review lap + one PR.

## Items

### 190.1 (HIGH, crash-at-execution) — dead N-way projected-EXISTS join arm

`implementNWayJoinWithExistential` (`rule_implement_nested_loop_join.go:2607`) emits a plan that
plans fine then **dies at execution** for `SELECT a.v, EXISTS(SELECT 1 FROM d WHERE d.id=a.id)
FROM a,b,c WHERE a.id=b.id AND b.id=c.id` (projected EXISTS over >2 ForEach legs). Its own
gravestone (`:2907`) admits: *"HAS NEVER PRODUCED AN EXECUTABLE PLAN"* — the binding failure is a
correlated FieldValue that can't resolve against a multi-leg merged row. Zero corpus firings. By
CLAUDE.md's "E2E or it's not done" rule this is a fake checkbox shipping a broken plan.

**Investigation (confirmed):** the arm is a Go invention with **no Java analog** — Java's
`ImplementNestedLoopJoinRule.java:97` matches exactly two quantifiers; a 3+-quantifier flat select
is illegal in Java and must be partitioned into nested BINARY sub-selects first. Java DOES support
projected EXISTS over a 3-way join, but via left-deep binary `FlatMap` composition correlating by
alias — never a positional "merged-row-with-ordinal-windows" physical construct. The Go arm's root
failure is the deep RFC-173 ordinal tripwire ("multi-leg row cannot serve a source-relative
ordinal", `values.go:869,902`), NOT a localized rebase bug — the obvious RV-rebase fix was tried
and reverted (`:2929`). A delete-probe (isolated worktree, dispatch disabled) empirically confirms
the trigger query then fails **LOUD at plan time** (`best expression is not a physical plan:
*SelectExpression` = UnableToPlan), not a crash, not wrong rows — strictly better than today.

**Fix — DELETE the arm AND route the shape through the working wrap (Java parity):**
1. **Delete (scope corrected per Torvalds — grep-verified):** the arm body
   `implementNWayJoinWithExistential` (`:2607-2988`); the N-way FORWARD inside
   `implementJoinWithExistential` (`:2047-2050` — so a ≥3-ForEach-leg existential select now
   declines instead of hitting the arm; `OnMatch` and the 2-leg path are untouched);
   `existInnerIsScanSafe` (`:2551-2562,2706` — genuinely arm-only); the `nwayOuterProbe` global +
   `NWayOuterYieldCount`; the arm's `scanPlanExpression` memo-opacity hack USE at `:2977` (the cost
   pollution); and `nway_exists_memo_shape_test.go`.
   **KEEP (all live in retained paths — deleting them breaks the build):** `collectInnerLegAliases`
   (`:763` `implementExistentialSelect`, `:2225`), `foldStep1Seed`/`reconstructFoldStep1Seed`
   (`:2271` retained 2-leg path), `buriedLegOrdinalLayout`/`planBuriedLegConcat` (`:480`), the
   `scanPlanExpression` TYPE (defined in `abstract_data_access_rule.go:587`, used at NLJ `:499`),
   and the shared ordinal-seed infra (`ordinalSeedLegWindowsOf`
   et al. — used by `left_outer_existential.go`/`ordinal_join.go`/`unnest_gather.go`).
2. **Close the parity gap properly:** the WHERE-EXISTS variant of this exact shape already
   plans+executes SARG-preservingly via `translateExistsOverGatheredCluster`
   (`exists_gathered_cluster_wrap.go`) — it translates the ≥3-way join as its own gathered ordinal
   cluster (a reference `PartitionSelectRule` enumerates) + a 2-quantifier existential select. That
   wrap **explicitly declines projected-EXISTS** (`:308-315`). Remove the decline and fold the
   projection over the box positional output using the existing `rebaseLegRefsToBox`/`wrapRVFullyBaked`
   machinery → the trigger query routes through the proven binary-lowered mechanism, genuine Java
   parity, plans AND executes correctly.
Regression: an FDB integration test running the trigger query end-to-end asserting the correct rows
(parity with Java). If step 2 proves deeper than the wrap-decline removal suggests, land step 1
(fail-loud, strictly correct) in this PR and book step 2 as a tracked reach follow-up — Graefe's
call on scope.

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
