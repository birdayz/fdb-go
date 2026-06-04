# RFC-076: Retire the Go-only `ImplementIndexScanRule` — make the data-access path the sole scan producer (TODO 7.7)

**Status:** Re-review (v3 — empirical reproduction FALSIFIED v2's premise that the data-access path "emits scans"; it emits ZERO. Re-ACK required from Graefe + Torvalds before impl)
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
