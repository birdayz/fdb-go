# RFC-188 — Cost/cardinality-model soundness: align with Java, kill the estimate-as-correctness-gate

**Status:** DRAFT — awaiting Graefe (Cascades alignment) + Torvalds (code quality) ACK before implementation.
**Tracks:** TODO.md "FINDINGS 2026-07-22" systemic problem **B** — the cost/cardinality subsystem is a
regression-tuned re-derivation, not a port. Covers findings 2 (HIGH wrong results), 3 (HIGH worse
plan), 6 (MED sign flip), 10 (MED missing rungs / property divergences).
**Baseline:** master at `c1e3c0d2f` (RFC-187 merged). Java source at `fdb-record-layer/` tag 4.12.11.0.
**Work branch** `feat/rfc188-cost-model-soundness`.

**Why this matters:** plan choice is read-side, but it IS wire-visible for cross-engine continuation
resumption, and finding 2 is a **wrong-rows** correctness bug. The directional cost rungs are correct
(verified in the quality review — no sign flips in the physical comparator); the defects are *missing*
Java rungs, one *wrong-iteration* rung, two under-derived properties, and one correctness transform
gated on a heuristic estimate.

---

## 1. Finding 2 (HIGH, wrong results) — RemoveRangeOne deletes LIMIT 1 on an unfloored estimate

**Go:** `rule_remove_range_one.go` deletes `LIMIT 1 OFFSET 0` when `isAtMostOneRow(inner)` — which
returns true when `properties.EstimateCardinality(inner) <= 1.0` (`:68`). `EstimateCardinality` is the
heuristic Cost-walk: `LogicalFilterExpression` = `in * 0.5^numPreds` and `SelectExpression` =
`product * 0.5^numPreds`, **unfloored**, over `LeafScanCardinality = 1e6` (`properties/cost.go:520,609`).
So `1e6 * 0.5^20 ≈ 0.95 < 1.0`: a ~20-conjunct `SELECT * FROM t WHERE a1=? AND … AND a20=? LIMIT 1`
deletes the LIMIT and returns **all** matching rows.

**Java:** `RemoveRangeOneRule.java:45-102` removes an **unreferenced `RANGE(0,1)` table-function
quantifier** from a `SelectExpression` — nothing to do with LIMIT. There is **no** Cascades rule that
eliminates a `RecordQueryLimitPlan` based on a proven at-most-one-row input (grep-confirmed: only
`RemoveProjectionRule`, `RemoveRangeOneRule`, `RemoveSortRule`; no `RecordQueryLimitPlan` reference).
Max-cardinality (`CardinalitiesProperty`) is a **statically PROVEN** property, never a heuristic
estimate, and is never used to drop a limit.

**Root cause:** a correctness transform (deleting a semantic `LIMIT`) is gated on a made-up selectivity
constant. Cost estimates decide *between correct plans*; they must NEVER decide *whether a rewrite
preserves semantics*. And the Go rule wears Java's `RemoveRangeOneRule` name for a completely different
(Go-invented) behavior — a false-parity trap.

**Fix:**
- `isAtMostOneRow` drops the `EstimateCardinality <= 1.0` gate entirely and returns true only for
  **structurally-provable** single-row inners — today `LogicalValuesExpression` (one row of constants,
  the sole case the existing tests rely on). This keeps the sound optimization (a redundant `LIMIT 1`
  over `VALUES` is removed) and makes the wrong-rows path unreachable: a 20-conjunct filter is not a
  `LogicalValuesExpression`, so the LIMIT stays.
- **Rename** the Go rule to name what it does (proposed `EliminateLimitOverSingleRowRule`) so it no
  longer collides with Java's unrelated `RemoveRangeOneRule`. Update its registration
  (`default_rules.go`) and any `%T`-keyed `DisabledRules` reference.
- **Follow-up (booked, not A/B scope):** Java's *actual* `RemoveRangeOneRule` (drop an unreferenced
  `RANGE(0,1)` quantifier) is UNPORTED — a distinct missing-rule item, filed separately.

**Test:** `TestRemoveRangeOne_ManyPredicateFilterKeepsLimit` — a 20-equality-conjunct `LIMIT 1` over a
scan must NOT have its LIMIT removed (red on baseline: the estimate deletes it). Plus the existing
`LimitOverValues` positive stays green. Plus an FDB row-count pin: the 20-conjunct query returns the
correct single row, and a multi-match variant returns exactly one, not all.

---

## 2. Finding 3 (HIGH, worse plan) — comparePrimaryScanVsIndexScan drops Java's SARG sub-case + config

**Go:** `planning_cost_model.go:878` `comparePrimaryScanVsIndexScan(opsA, opsB)` checks only operator
SHAPE (primary-scan vs singular-index-scan-with-fetch) and unconditionally returns the `PREFER_SCAN`
direction (`-1`/`+1`). Its comment falsely claims "matches Java's check."

**Java:** `PlanningCostModel.java:376-415`. When the shape matches, FIRST (`:381-406`) the SARG
sub-case: if `typeFilterCountPrimaryScan > 0 && typeFilterCountIndexScan == 0`, compute
`primaryMinusIndex = Sets.difference(primaryComparisons, indexComparisons)`; if empty, compute
`indexMinusPrimary = Sets.difference(indexComparisons, primaryComparisons)`; if **non-empty**, return
`+1` (the index has an extra SARG the primary lacks and needs no type filter → prefer the index).
THEN (`:408-412`) the config branch: `PREFER_SCAN` → `-1`; else (`PREFER_INDEX` /
`PREFER_PRIMARY_KEY_INDEX`) → `+1`.

**Consequence:** with a multi-record-type table indexed on `(recordType, pk…)` and `WHERE pk=?`, plan A
= `TypeFilter(Scan)` (SARGs pk, needs the type filter), plan B = `Fetch(IndexScan)` (SARGs pk AND
recordType). Java prefers B; Go returns `-1` → a near-full-table scan + high-discard type filter.

**Fix:**
- Pass the two sides' type-filter counts (already in `opsA/opsB.typeFilterCount`) and the two plans
  into the rung; extract each scan's SARG comparison SET (a helper over `ScanComparisons` producing a
  comparable set — column + comparison), compute the two `Sets.difference`, and return `+1` in the
  exact sub-case Java does. The rung's signature grows to carry the concrete `a, b` expressions (the
  comparator already has them).
- **IndexScanPreference config:** honor it if `PlanContext` exposes it (finding says Go "silently
  ignores" `PREFER_INDEX`/`PREFER_PRIMARY_KEY_INDEX`). Implementation checks whether Go models the
  config; if it does, thread it and match Java's branch; if Go has no such config knob today, the
  `PREFER_SCAN` default is Go's only behavior — the SARG sub-case is the wrong-plan fix, and honoring
  the (currently-absent) config is booked as a separate small item. Either way the SARG sub-case lands.

**Test:** `TestFDB_PrimaryVsIndex_SargScanPrefersIndex` (EXPLAIN pin): the multi-type `WHERE pk=?` shape
plans the selective index, not the type-filtered primary scan. Red on baseline.

---

## 3. Finding 6 (MED) — comparePredicateCountByLevel iterates the union; Java iterates the first map

**Go:** `planning_cost_model.go:115-139` iterates `for level := 0; level <= maxLevel` over the UNION of
both maps' levels.

**Java:** `PredicateCountByLevelProperty.java:182-193` iterates ONLY the first map's entries
(`aLevelToPredicateCount.entrySet()`, an `ImmutableSortedMap` → ascending), reading
`bLevelToPredicateCount.getOrDefault(level, 0)`; first differing level returns
`Integer.compare(aCount, bCount)`; final tiebreak `Integer.compare(a.getHighestLevel(),
b.getHighestLevel())`. It does NOT visit levels present only in `b` (except via the highest-level
tiebreak).

**Consequence:** for asymmetric-depth predicate maps (`a={0:1,2:5}`, `b={0:1,1:3}`) Java returns `+1`
(level 2: `compare(5,0)`), Go returns `-1` (level 1: `compare(0,3)`) — opposite. This is the REWRITING
"more predicates deeper wins" rung (`designated_final.go`), so it selects a different logical survivor.
(This comparator is deliberately asymmetric in Java — `compare(a,b) != -compare(b,a)` — and Go must
match that asymmetry, not "fix" it.)

**Fix:** iterate `a`'s levels in ascending order only, `b`'s count via `getOrDefault(level, 0)`; final
tiebreak on the highest level of each. Exactly Java's loop.

**Test:** `TestComparePredicateCountByLevel_AsymmetricLevels` (unit): the `a={0:1,2:5}` vs `b={0:1,1:3}`
pair returns `+1` (was `-1`). Plus the symmetric cases stay unchanged.

---

## 4. Finding 10 (MED) — missing Java rungs / under-derived properties

**M2 — default-on-empty rung (Java `PlanningCostModel.java:312-317`, fewer wins).** `expressionCounts`
has no field for `RecordQueryDefaultOnEmptyPlan` and the walk never counts it, so two plans differing
only in ON-EMPTY-NULL count fall to the scalar-cost/hash tiebreak instead of "fewer wins."
**Fix:** add `numDefaultOnEmpty` to `expressionCounts`, count the plan node in the walk, add the rung
in Java's position (after the existing ordinal rungs, before the scalar-cost fallback).

**M3 — whole-plan cardinality OUTER guard (Java `PlanningCostModel.java:121`).** Java gates the entire
max-data-access-cardinality criterion behind `if (!cardinalitiesA.getMaxCardinality().isUnknown() ||
!cardinalitiesB.getMaxCardinality().isUnknown())` — the WHOLE-PLAN max cardinality, not the data-access
maxima. Go (`planning_cost_model.go:174-191`) has only the inner data-access guard, so when both
whole-plan cardinalities are unknown but a data access is provably bounded (InUnion/Explode over point
lookups) Go decides on data-access cardinality where Java abstains.
**Fix:** compute each side's whole-plan `Cardinalities.GetMaxCardinality()` (the proven property already
in `plan_properties.go`/`cardinality.go`) and add the outer guard. Assess in impl whether the
whole-plan Cardinalities is reachable from the comparator (physical plans carry it); if the property
is not yet computed for the comparator's inputs, that plumbing is part of this fix.

**M4 — DistinctRecords for index scans (Java `DistinctRecordsProperty.java:265-272`).**
`plan_properties.go:64` returns `ip.IsUnique()`; Java returns `!matchCandidate.createsDuplicates()`. A
non-unique index on a scalar field does not create duplicates → Java reports distinct, Go reports not,
missing a `SELECT DISTINCT` elision. Safe direction (under-report) but a real divergence.
**Fix:** derive from "index does not create duplicates" (a fan-out index on a repeated/collection field
creates duplicates; a scalar-field index does not) rather than uniqueness. Implementation locates Go's
equivalent of `createsDuplicates` (index type / key-expression fan-out); if absent, add the minimal
predicate on the index metadata.

**M5 — PrimaryKey for index scans (Java `PrimaryKeyProperty.java:293-294`).** `plan_properties.go:216`
returns `nil`; Java returns the translated common primary key (index entries carry the PK). Loses
PK-based dedup/ordering reasoning for any plan over index scans. Safe direction (missed optimization).
**Fix:** return the index's common primary key (Go's index plan carries pk column metadata); translate
to the key/value representation the property uses, matching `commonPKFromChildren`.

**Tests:** M2 — `TestCostModel_FewerDefaultOnEmptyWins` (unit, two plans tie on all else, differ in
default-on-empty count). M3 — a unit pin on the outer guard (both whole-plan unknown, one data access
bounded → abstain). M4 — `TestDistinctRecords_NonUniqueScalarIndexIsDistinct`. M5 —
`TestPrimaryKey_IndexScanCarriesCommonPK`. Each red on baseline.

---

## 5. What this is / is NOT

- Finding 2 is a **wrong-rows correctness** fix. Findings 3/6/10 are **plan-choice** fixes (read-side;
  wire-visible only for cross-engine continuation resumption of the same query). None change key/record/
  index/continuation *format*.
- These are FIDELITY fixes to existing cost rungs/properties — no new Cascades rule, phase, or physical
  operator (except finding 2's rule rename + gate change). Graefe: match-then-implement untouched.
- The Go-only extension rungs the quality review CLEARED (statistics scalar-cost, `compareJoinOrdering`,
  redundant-sort) are NOT touched — they are documented, test-pinned substitutes for Java's early
  `CardinalitiesProperty` join discrimination, and stay.

## 6. Risks & mitigations

- **R1 — plan churn.** Cost changes flip plans on unrelated queries. Full 56-target suite +
  determinism ×10 on affected planner tests + **1M stress before/after** gate every change; any EXPLAIN
  change is reviewed as a correctness/parity improvement or a neutral shape change, never waved through.
- **R2 — M3/M4/M5 plumbing.** If the whole-plan Cardinalities / createsDuplicates / commonPK are not
  reachable where the fix needs them, that plumbing is in scope; if it proves larger than a property
  tweak, the specific sub-item ships behind a test with a booked TODO (never a silent skip). Default is
  the full fix.
- **R3 — finding 6 asymmetry.** The corrected comparator is intentionally asymmetric (Java's shape);
  the determinism harness must still pass (the REWRITING designation is deterministic per-side).

## 7. Implementation order (DFS, one finding to completion, green per commit, e2e each)

1. **Finding 2** (wrong results) — gate on structural provability + rename; unit + FDB row-count pins.
2. **Finding 6** (pure comparator, low risk) — first-map iteration; unit pin.
3. **Finding 3** (worse plan) — SARG sub-case (+ config if modeled); EXPLAIN pin + stress.
4. **Finding 10** M2 → M4 → M5 → M3 (M3 last — most plumbing); unit pins each + stress.
5. Full suite + determinism ×10 + 1M stress; milestone review lap (Graefe + Torvalds), fold, delta.

No finding "done" until a test (unit + FDB where row/plan-visible) pins the corrected behavior. No
`t.Skip`, no "for now", no deferral of B's four findings.
