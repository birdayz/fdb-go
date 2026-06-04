# RFC-076: Retire the Go-only `ImplementIndexScanRule` — make the data-access path the sole scan producer (TODO 7.7)

**Status:** Accepted (v2 — Graefe ACK + Torvalds ACK; v1's false "no Java ImplementFilterRule" premise corrected)
**Area:** Cascades query planner — physical index-scan production
**Reviewers:** Graefe (Cascades alignment — mandatory), Torvalds (code quality), codex, @claude

> Citation note: bare Go filenames below (`rule_implement_index_scan.go`, `rule_implement_filter.go`,
> `abstract_data_access_rule.go`, `predicate_multi_map.go`, `compensation.go`, `default_rules.go`,
> `plan_executability.go`) are all under `pkg/recordlayer/query/plan/cascades/`.

## Problem

Go produces a physical index scan via TWO scan producers; one is a Go-only divergence:

1. **Data-access/`Compensation` match path** (`abstract_data_access_rule.go` + `predicate_multi_map.go`
   + `compensation.go`) — Java-aligned. A matched candidate's index-only residual marks the
   compensation `impossible` (`predicate_multi_map.go:198`) and the match is skipped — the bad plan
   is never built. Emits scans (and, via compensation, residual fetch/filter).
2. **`ImplementIndexScanRule`** (`rule_implement_index_scan.go`, 407 lines, **Go-only — no Java
   analog**) — matches `Filter(pred, FullScan)`, iterates `ComparisonPredicate`s DIRECTLY (bypassing
   the `PartialMatch` match infrastructure, `:103-146`), builds scan ranges per candidate, emits
   `RecordQueryIndexPlan` (± `Fetch`, ± residual `FilterPlan`). It re-implements the index-only-residual
   skip with its own guard (`:165-177`).

Because this Go-only rule bypasses `Compensation`, the "index-only value can't be a residual"
property is enforced at THREE layers: `valueContainsUncompensatable` (match path), the
`ImplementIndexScanRule` residual-skip loop, and a final-plan `validateNoIndexOnlyResidual`
(`plan_executability.go`). No live bug — but a triplicated guard whose root is the one duplicated
*scan* producer.

### NOT a producer to retire: `ImplementFilterRule` is Java-faithful (v1 correction)

`rule_implement_filter.go` wraps a physical inner in a `RecordQueryPredicatesFilterPlan`. v1 wrongly
called this Go-only and proposed retiring it. **It is a faithful 1:1 port of Java's
`ImplementFilterRule.java`** (Graefe + Torvalds verified against tag 4.11.1.0; the in-code doc says
so). Java has BOTH the data-access `Compensation` path AND a standalone `ImplementFilterRule` — they
are not redundant: compensation handles residuals from a *matched candidate*; `ImplementFilterRule`
handles the general `Filter(plan)` case. **`ImplementFilterRule` STAYS, unchanged.**

## Fix — retire ONLY `ImplementIndexScanRule`; make the data-access path the sole scan producer

1. **Match-coverage extension (the bulk of the work — gating prerequisite).** Measured: disabling
   `ImplementIndexScanRule` today fails ~20+ planner tests (`TestPlanChoice_*`, `TestPipeline_IndexScan`,
   `TestSortElim_*`, `TestInExplode_*`, …). So the data-access/match path does NOT yet cover the
   `Filter(pred, Scan)` shapes `ImplementIndexScanRule` handles — `ImplementIndexScanRule` produces
   index scans without a `PartialMatch`, whereas the data-access path requires `MatchLeafRule`/
   `MatchIntermediateRule` to produce one first. Closing this gap (so every index-scannable
   `Filter(pred, Scan)` is matched → scanned via `Compensation`) is the real work and must land
   FIRST; a missing match silently yields *no plan*. Pin each shape (equality, range, multi-column
   prefix, covering, IN-explode, sort-providing) as a red-first regression.
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

The match-coverage gap is large (audit-confirmed) — this is a SIGNIFICANT data-access-path extension,
not a clean rule deletion. If extending the match path to cover a shape proves too invasive, that
shape stays on `ImplementIndexScanRule` until covered (partial retirement) rather than risking a
silent no-plan. The match-coverage regression suite is the safety net.
