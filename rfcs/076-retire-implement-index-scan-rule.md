# RFC-076: Retire the Go-only `ImplementIndexScanRule` — make the data-access path the sole scan producer (TODO 7.7)

**Status:** **IMPLEMENTED** (v5, 2026-06-05) — `ImplementIndexScanRule` retired; the data-access/`Compensation` match path is the sole scan producer. Sequence: 3b template-aware costing (committed), 3a constraint-pass activation + stub-chain costing (committed), then deletion + data-access compensation materialization (this change). Full suite green (48/48), plandiff byte-identical, determinism 5×, `validateNoIndexOnlyResidual` kept. The retirement diff still needs its own Graefe + Torvalds + codex + @claude ACK before merge. v5 amendment (the data-access compensation-materialization finding + fix) at the bottom; v3/v4 history retained below.
**Area:** Cascades query planner — physical index-scan production
**Reviewers:** Graefe (Cascades alignment — mandatory), Torvalds (code quality), codex, @claude

> Citation note: bare Go filenames below (`rule_implement_index_scan.go`, `rule_implement_filter.go`,
> `abstract_data_access_rule.go`, `predicate_multi_map.go`, `compensation.go`, `default_rules.go`,
> `plan_executability.go`) are all under `pkg/recordlayer/query/plan/cascades/`.

## Problem (v3 — corrected by empirical reproduction)

Go has TWO would-be scan producers, but **only one actually produces index scans today**:

1. **Data-access/`Compensation` match path** (`abstract_data_access_rule.go` + `predicate_multi_map.go`
   + `compensation.go`) — **structurally Java-aligned but INCOMPLETE: it emits ZERO index scans.**
   v2 claimed it "emits scans." Reproduction (below) proves otherwise.
2. **`ImplementIndexScanRule`** (`rule_implement_index_scan.go`, 407 lines, **Go-only — no Java
   analog**) — matches `Filter(pred, FullScan)`, iterates `ComparisonPredicate`s DIRECTLY (bypassing
   the `PartialMatch` match infrastructure, `:103-146`), builds scan ranges per candidate, emits
   `RecordQueryIndexPlan` (± `Fetch`, ± residual `FilterPlan`). It re-implements the index-only-residual
   skip with its own guard (`:165-177`). **It is the SOLE index-scan producer — load-bearing, not a
   redundant duplicate.**

### Empirical reproduction (the v2 premise was wrong)

Disabling `ImplementIndexScanRule` (both registrations) and running the planner unit tests: **every**
`Filter(pred, Scan)` shape collapses to `PredicatesFilter(Scan(...))` — `TestPlanChoice_IndexScanChosenOverFullScan`,
`_UniqueIndexPointLookup`, `_MultiColumnIndexPrefix`, `_PartialPrefixMatch`, `_EqualityPlusInequality`,
`_PicksBestIndexAmongMultiple`, … No index scan is produced by the data-access path.

Tracing the data-access path for `SELECT … WHERE CATEGORY='electronics'` (idx on CATEGORY) shows the
match infrastructure fires **completely and correctly**:
- `MatchLeafRule`: query `FullUnorderedScan` ↔ candidate scan leaf — `EqualsWithoutChildren=true` ✓
- `MatchIntermediateRule` → `matchSingleSourceAgainstSelect`: `LogicalFilter` ↔ candidate `SelectExpression`,
  child match found, predicate `CATEGORY='electronics'` binds to the candidate `Placeholder` ✓
- `pushDataAccessTasks` fires on the filter ref with the candidate present ✓

…but the produced `Compensation` is **`ImpossibleCompensation`**, so `DataAccessForMatchPartition`
(`abstract_data_access_rule.go:296`) skips the match and emits no scan. Root cause: every Go
match-seeding path — `matchLeafWithCandidate` (`rule_match_leaf.go:131`), `matchSingleSourceAgainstSelect`
(`rule_match_intermediate.go:445`), `matchIntermediateStructural` (`:288`) — constructs its
`RegularMatchInfo` with **`maxMatchMap = nil`**. `PartialMatch.PullUp` (`partial_match.go:117-122`)
returns nil when the MaxMatchMap is nil → `CompensateCompleteMatch` returns `ImpossibleCompensation`
(`:153`). So the data-access path can NEVER produce a scan: it has no MaxMatchMap, hence no result
pull-up, hence impossible compensation. `ComputeMaxMatchMap` (`max_match_map.go:167`) exists and is
fully implemented — it is simply never called by the seed match paths.

(The v2 claim that the "index-only value can't be a residual" property is *triplicated* still holds —
`valueContainsUncompensatable`, the rule's residual-skip loop, and `validateNoIndexOnlyResidual` — but
the deeper truth is that two of those layers never run, because the data-access path produces nothing.)

### NOT a producer to retire: `ImplementFilterRule` is Java-faithful (v1 correction)

`rule_implement_filter.go` wraps a physical inner in a `RecordQueryPredicatesFilterPlan`. v1 wrongly
called this Go-only and proposed retiring it. **It is a faithful 1:1 port of Java's
`ImplementFilterRule.java`** (Graefe + Torvalds verified against tag 4.11.1.0; the in-code doc says
so). Java has BOTH the data-access `Compensation` path AND a standalone `ImplementFilterRule` — they
are not redundant: compensation handles residuals from a *matched candidate*; `ImplementFilterRule`
handles the general `Filter(plan)` case. **`ImplementFilterRule` STAYS, unchanged.**

## Fix — retire ONLY `ImplementIndexScanRule`; make the data-access path the sole scan producer

1. **Complete the data-access path — build the MaxMatchMap (the bulk of the work; gating
   prerequisite).** Reproduction (above) pinpoints the real gap: matching produces a correct
   `PartialMatch`, but its `MatchInfo` has `maxMatchMap = nil`, so compensation is always
   `ImpossibleCompensation` and no scan is emitted. The fix mirrors Java's `subsumedBy`, which
   **always** computes a `MaxMatchMap`:
   - **(1a) Wire `ComputeMaxMatchMap` into the seed match paths.** `matchSingleSourceAgainstSelect`
     and `matchIntermediateStructural` (`rule_match_intermediate.go`) and the leaf
     `matchLeafWithCandidate` (`rule_match_leaf.go`) must compute the MaxMatchMap between the query
     expression's result value and the candidate expression's result value (the exact values Java's
     `SelectExpression.subsumedBy` and the leaf `subsumedBy` feed to `MaxMatchMap.compute`) and pass
     it into `NewRegularMatchInfo` instead of nil. `ComputeMaxMatchMap` (`max_match_map.go:167`)
     already exists; this is wiring, not new infra.
   - **(1b) LogicalFilter compensation.** `CompensateCompleteMatch`'s predicate-compensation phase
     (`partial_match.go:199`) is gated on `queryExpression.(*SelectExpression)`, so a bare
     `LogicalFilterExpression` query never compensates its predicates. Java has no
     `LogicalFilterExpression` (filters ARE `SelectExpression`s); the Go-aligned fix is either to
     teach this phase about `LogicalFilterExpression` (it already matches one in the matching path)
     OR to normalize bare `LogicalFilter(scan)` → `SelectExpression` before matching. The
     normalization route is strictly more Java-faithful and is preferred if it doesn't regress.
   - Closing this gap (every index-scannable `Filter(pred, Scan)` matched → MaxMatchMap built →
     compensation possible → scanned) must land FIRST; a missing/`impossible` match silently yields
     *no plan*. Pin each shape (equality, range, multi-column prefix, covering, IN-explode,
     sort-providing) as a **red-first** regression: assert the data-access path produces the scan
     **with `ImplementIndexScanRule` already disabled**, so the test fails before the wiring lands.
2. **Verify `ImplementFilterRule` mirrors Java exactly** (Graefe): Java's filter rule gates on the
   `anyCompensatablePredicate` matcher + the `isTautology` branch. Confirm Go's port matches (not a
   bolt-on guard); fix any drift. No retirement.
3. **Retire `ImplementIndexScanRule`**: delete the file + BOTH registrations (`default_rules.go:191`
   in `PlanningDataAccessRules` AND `:210` in `BatchAExpressionRules` — v1 missed the double-reg).
   Its residual-skip guard (`:165-177`) goes with it.
4. **Delete the final-plan validation** (`validateNoIndexOnlyResidual` + `UnplannableIndexOnlyResidualError`
   + the `Planner.Plan` call). Graefe's reasoning: once the data-access path (with the
   `Compensation.isImpossible()` gate) is the SOLE scan producer, `ImplementFilterRule` never
   receives an index-only residual to leak (it only ever wraps a residual the data-access path chose
   to surface), so the final-plan guard is unreachable. **Verify** this (no non-data-access path
   hands `ImplementFilterRule` an index-only residual) before deleting; if any does, the guard stays.

## Performance

No steady-state change — same physical plans, produced by the one Java-faithful path. Removes the
redundant predicate re-iteration `ImplementIndexScanRule` did. plandiff byte-identical at every arity.

## Test plan

- **Match-coverage regression (red-first against the retired rule)**: a `Filter(pred, Scan)` over
  each index shape (equality, range, multi-column prefix, covering, IN-explode, sort-providing,
  non-sargable residual) produces the expected scan via the data-access path. This is the audit's
  ~20 failing tests, re-confirmed green through the data-access path.
- The three pinned guards stay green, now via `Compensation`:
  `TestImplementIndexScanRule_SkipsIndexOnlyResidual` (re-expressed via the match path),
  `TestVectorPlan_QualifyPlansToVectorScan`, `TestVectorPlan_MetricMismatchDoesNotMatchVector`
  (now fails at the `Compensation` gate, still loud).
- plandiff conformance green (no plan-shape change); determinism 10×; full `just test`; stress-1M.

## Risk

**Bigger than v2 stated.** This is NOT a clean rule deletion — it is *completing* an incomplete
data-access path: the MaxMatchMap was never wired into the seed match paths, so the path produces
zero scans today and `ImplementIndexScanRule` is the sole producer. Building MaxMatchMap construction
into matching (step 1a) is core Cascades-internals work touching the matching seeds and compensation;
getting the exact query/candidate result values fed to `ComputeMaxMatchMap` right is the load-bearing
correctness risk (a wrong MaxMatchMap yields a wrong result pull-up → wrong columns, or a still-impossible
compensation → no plan). Mitigations: (a) red-first per-shape regressions assert the scan is produced
via the data-access path **with `ImplementIndexScanRule` disabled**; (b) plandiff byte-identical at
every arity before vs after; (c) determinism 10× + stress-1M. If wiring a shape's MaxMatchMap proves
too invasive, that shape stays on `ImplementIndexScanRule` until covered (partial retirement) rather
than risking a silent no-plan — but the empirical finding is that ALL `Filter(Scan)` shapes share the
one missing-MaxMatchMap root, so a single fix should unlock them together.

---

## v4 amendment — implementation findings + the path to full retirement

Implementing v3 confirmed the MaxMatchMap wiring premise, but **completing the data-access path took
five distinct correctness fixes**, not one — and **retiring the rule needs two further changes** (an
ordering-constraint pass + template-aware costing) that v3 did not foresee. All filenames below are
under `pkg/recordlayer/query/plan/cascades/` unless noted.

### Step 1 — DONE: the data-access path is now correct for every shape (Graefe + Torvalds ACK'd)

Wiring `ComputeMaxMatchMap` into the seed paths activated the data-access path, which then produced
*wrong* plans for several shapes that `ImplementIndexScanRule` had been silently covering. Each was a
real latent bug, root-caused against Java and fixed (regression-pinned by the FDB suites listed):

1. **Compensation base-quantifier alias** (`compensation.go`). `ForMatchCompensation.Apply`/`ApplyFinal`
   created the realized quantifier with a *fresh* alias and a rebase band-aid; Java creates it with the
   matched query-side ForEach alias (`Quantifier.forEach(ref, matchedForEachAlias)`) and rebases
   predicates via `realizedAlias → ofAliases(candidateTopAlias, realizedAlias)`. The fresh alias
   orphaned the access from outer correlations → 0-row dual-correlation joins. Ported the Java
   `TranslationMapFunc` signature exactly; removed the band-aid. (`TestFDB_JoinMerge_OuterColumn_NotDropped`)
2. **Seed-merge alias classification** (`predicate_correlation.go` + `rule_partition_select.go`).
   `AddMergeSeedAliases`: partition-select classification now sees SEED `JoinMergeAllValue` source
   aliases that `GetCorrelatedToOfValue` deliberately hides (exploration-budget). Without it a predicate
   reading a buried column through a seed merge was misclassified lower-only and pushed below the merge.
   (dual-correlation join 0-row)
3. **`valuesMatchColumn` outer-correlation guard** (`rule_match_intermediate.go`). The candidate-column
   operand must belong to the matched source, not an outer correlation — `valuesMatchColumn` compares
   FieldValues by field name only, so `Customer.id = Order.customer_id` (matching ORDER) spuriously bound
   to Order's same-named PK. (`TestFDB_InnerJoin`, `TestFDB_JoinSameTableTwice`)
4. **Aggregate-index exclusion** (`planner.go`). `pushDataAccessTasks` drops `AggregateIndexMatchCandidate`
   — those are consumed by `AggregateDataAccessRule` (matches the GroupBy, reads the pre-aggregated
   value), never as a regular value-index scan. The regular path was scanning the aggregate index +
   re-aggregating (COUNT → 1, not 4). (Torvalds note: now the 3rd copy of the agg-skip guard; the deeper
   fix is to not *seed* aggregate matches onto non-GroupBy refs — tracked as step-2 cleanup.)
   (`TestFDB_AggregateIndexUsage`)
5. **Sargable-binding reconciliation** (`rule_match_intermediate.go`). A binding matched to a placeholder
   but NOT consumable into the scan prefix (`ComputeBoundParameterPrefixMap` — e.g. a vector partition
   inequality, or a trailing equality with an unbound leading column) becomes a RESIDUAL, not silently
   dropped. (`TestFDB_VectorSearch_MultiPartition_{Inequality,TrailingEquality}Residual`)

### Step 3 — the two changes retirement requires

Disabling `ImplementIndexScanRule` with step 1 in place leaves the whole FDB suite green and **one**
plangen unit test red: `TestEndToEnd_SortElimThroughResidualFilter` — `Sort(date, Filter([status='active'
AND amount>50], Scan))` over an index `(status, date)` must eliminate the sort (the status-equality run
makes the scan date-ordered; `amount>50` is a residual). The data-access path can satisfy this, but only
if the requested ordering reaches the scan. It does not, for two reasons that compound:

- **(3a) Activate the dormant ordering-constraint pass.** `PushRequestedOrderingThrough{Sort,Filter,
  Select,...}Rule` and `PushReferencedFields*Rule` are all `preOrderMarker` rules that gate on
  `call.IsConstraintOnly()` — but **`constraintOnly` is never set `true` anywhere in production**
  (`unified_tasks.go` schedules preorder rules via `TransformImplTask` with the default `false`), so the
  entire Java-faithful top-down constraint-propagation phase is wired yet inert. The fix sets
  `constraintOnly = isPreOrderRule(t.Rule)` when scheduling a preorder rule, so the ordering reaches the
  scan through the residual filter and the data-access path emits a date-ordered scan (sort eliminated).
  This is the Java mechanism (`PushRequestedOrderingThroughSortRule` et al.); Go merely never turned it on.

- **(3b) Template-aware costing (the load-bearing fix).** Activating 3a regresses exactly one join test,
  `TestFDB_JoinSelPred_Repro` (`… o.customer_id = c.id AND o.id < 10 ORDER BY o.id`): the planner flips
  off the selective `o.id<10` outer onto a full `Scan(CUSTOMERS)` driving an `idx_customer` inner. Root
  cause, fully traced: a join inner that is a **nil-inner `Fetch` shell** (a push-through template whose
  inner is resolved only at extraction — see `isNilInnerFetch`) **hides its inner data access from the
  cost model**. `concretePlanCost`/`concretePlanCounts` (and criterion #2 max-cardinality via
  `findExpressionsByType`) walk the `GetRecordQueryPlan()` *plan tree*, where the shell's inner is `nil`
  → 0 children → costed as ~free. So the Customers-driven join looks nearly free and wins; the worse join
  order is chosen. (Without 3a this never surfaces because the ordering request that creates the ordered
  shell variant never reaches the join.)

  **Fix (the honest one — NOT a magic cost constant).** Cost the shell as its *real inner* by resolving
  it through the inner Reference (the wrapper's quantifier graph), never as `nil` and never as a
  fabricated `fetchCost(LeafScanCardinality)`-style constant (a constant breaks this one tie by luck but
  under-/over-costs depending on the real inner's selectivity — rejected). The seam already exists:
  `findExpressionsByType` (`planning_cost_model.go:458`) branches on `GetRecordQueryPlan() != nil` to
  choose the flat `concretePlanCounts` walk vs. the ref-descending `walkExpressionTree`
  (`bestPhysicalChild` → cost winner per child Reference). The fix forces the ref-descending branch when
  the wrapper's plan tree contains an unresolved nil-inner `Fetch`, so criterion #2 (max-cardinality)
  sees the real buried data access. The same template-resolution must cover the *total-cost* criterion
  (`concretePlanCost`, used at `:908`), not just criterion #2 — cost the wrapper via the ref-resolving
  path there too. A template thus never costs cheaper than the fully-formed plan it stands in for. (The
  experimental `fetchCost(LeafScanCardinality)` stop-gap has been reverted; it is NOT the landed fix.) The
  guards added alongside are defensive and independently correct: `stampOrderingWinners` and the
  NoProperties/ordered winner-lookup paths now skip nil-inner shells (a shell must never be a standalone
  *winner*), `physicalInMemorySortWrapper.WithChildren` relinks to the inner ref's cost *winner*
  (`findBestPhysicalPlan`) not the first-yielded member, and `physicalFlatMapWrapper.HintOrdering`
  propagates the outer ordering (a nested loop's output is ordered by its outer — a latent
  ordering-propagation gap, the "physical wrappers must propagate ordering" lesson).

### Retirement sequence (only after 3a+3b are green)

1. Land 3b (template-aware costing) + the defensive guards; verify the full suite stays green
   **with the rule still enabled** (no plan-shape regression from the cost change alone).
2. Land 3a (activate the constraint pass); verify `TestFDB_JoinSelPred_Repro` + sort-elim both green and
   nothing else regresses. **Also fix `physicalFlatMapWrapper.HintOrdering` to return the cost WINNER's
   ordering, not the first-known member's** (@claude step-1 finding 4): inert today (single ordering per
   ref), but once 3a adds ordered/unordered variants the first-known member may not be the winner, so a
   join could claim to provide an ordering it doesn't — the same first-vs-winner fix already applied to
   `physicalInMemorySortWrapper.WithChildren`.
3. Delete `ImplementIndexScanRule` + both registrations (`default_rules.go:191`, `:210`) and its
   residual-skip guard. Re-confirm `TestEndToEnd_SortElimThroughResidualFilter` green via the data-access
   path.
4. **Keep `validateNoIndexOnlyResidual`** (v3 step 4 said "verify before deleting; if anything still
   feeds an index-only residual, the guard stays"): the data-access reconciliation (fix #5) now *is* the
   path that surfaces the index-only DistanceRank residual, so the final-plan guard is still load-bearing
   (`TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual` asserts its
   `UnplannableIndexOnlyResidualError`). Do NOT delete it.

### v4 risk

The cost change (3b) ranks **every** plan — high blast radius. Mitigations: it only alters costing for
plan trees that contain an *unresolved nil-inner Fetch* (a pre-extraction template); fully-formed plans
are costed exactly as before. Validation: full `just test` + plandiff byte-identical at every arity for
queries without templates + determinism 10× + stress-1M before/after (point lookups <5ms, join_10_outer
ORDERS-driven ~4ms). The constraint-pass activation (3a) is gated behind 3b so the regression it would
otherwise cause never lands. If 3b proves to destabilize costing broadly, fall back to partial
retirement (rule retained for the sort-elim-through-residual shape only) rather than shipping a cost
regression.

---

## v5 amendment — DONE: rule retired (3b + 3a + deletion + data-access compensation materialization)

Implementing the deletion surfaced one more gap the v3/v4 "step 1 made the data-access path correct"
premise missed: **the data-access path never MATERIALIZED its residual compensation into a physical
plan during PLANNING.** `Compensation.apply` (compensation.go) produces a LOGICAL
`LogicalFilterExpression` over the physical scan (Java-faithful — Java yields it via
`FinalYields.yieldUnknownExpression`, where a non-`RecordQueryPlan` is an *exploratory* member that
re-optimizes). But Go's `pushDataAccessTasks` inserted every result via `InsertFinal`, so a logical
compensation sat as a non-physical final member and lost criterion #1 (physical beats non-physical) to a
full scan — the index scan was silently dropped for the common **indexed-equality + non-indexed
residual** shape (`WHERE status='active' AND amount>50`, status indexed, amount not) and for
sort-elim-through-residual. The retired rule had been masking this (it emitted `Filter(IndexScan)`
directly).

**Fix (planner.go):** when the data-access path yields a logical residual compensation, realize it as a
physical filter by firing the PLANNING expression rules on it (`implementDataAccessCompensation`) —
but ONLY for the unambiguously-safe shape, gated by `isSimpleResidualCompensation` + a join-leg guard
(`refHasCorrelatedMatch`). A compensation is materialized only when it is a `LogicalFilterExpression`
whose predicates are simple, non-IN, non-index-only, non-correlated comparisons, sitting over an
**uncorrelated, narrowable** (value-index / primary / fetch — NOT vector top-K or aggregate) inner
scan, on a ref that is **not a join leg**. Every other compensation stays logical and is handled by the
existing flow, because materializing it standalone is wrong:
- **IN** residual → must take the explode→InJoin path, not a standalone equality filter.
- **correlated** residual (or a residual over a correlated scan) → applied at the join; standalone it
  severs the join's correlation feed → 0 rows.
- **index-only** residual (vector DistanceRank) → must stay unplannable (validateNoIndexOnlyResidual).
- **vector top-K / aggregate** inner → a residual post-filter changes the result; not narrowable.
- **join-leg** ref (has a correlated match) → its compensations are consumed by the join, never a
  standalone leg winner.

Each exclusion is pinned by an existing FDB test that now exercises the data-access path with the rule
gone: `TestFDB_JoinWithInPredicate` (IN), `cte_with_join_filter` (correlated/join-leg),
`TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual` (index-only/vector); the positive case
by `TestPlanHarness_MultiplePredicates` and `TestEndToEnd_SortElimThroughResidualFilter`.

**Result:** `ImplementIndexScanRule` + both registrations + its 3 test files deleted; shared helpers
(`extractIndexPlan`, `findFullScan`, `recordTypesOverlap`, …) extracted to `scan_match_helpers.go`;
`validateNoIndexOnlyResidual` KEPT (still load-bearing). Full `just test` green (48/48), plandiff
byte-identical, determinism 5×. The data-access/`Compensation` match path is now the SOLE scan producer
— Java's single path.
