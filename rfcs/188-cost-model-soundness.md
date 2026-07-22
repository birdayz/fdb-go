# RFC-188 — Cost/cardinality-model soundness: align with Java, kill the estimate-as-correctness-gate

**Status:** ACKED (rev 2) — Graefe + Torvalds both NAK'd rev 1, both ACK rev 2 delta. Rev 2 adopts
every required change (finding 2 → **delete**, not rename; finding 3 → bare-comparison set + flipFlop
sign + config confirmed-absent; finding 6 → sparse-sorted first-map iteration; finding 10 M4 → booked as
plan-metadata plumbing, ordering premise dropped). Implementation may proceed; final impl HEAD gets one
joint delta review lap.
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

**Disposition (rev 2, Graefe ruling): DELETE the rule and its tests — do not rename-and-keep.** Two
reasons, both grep-confirmed:
1. **Java has no limit-removal rule at all.** A Go rule that drops a `LIMIT` Java *retains* is itself a
   plan-choice divergence — the exact class findings 3/6/10 exist to eliminate. Keeping it (even
   narrowed to a structural check) is internally inconsistent with the rest of this RFC.
2. **The narrowed rule would be production-dead.** `LogicalValuesExpression` has **zero production
   construction sites** — `NewLogicalValuesExpression` is called only from `*_test.go` (grep: 0
   non-test callers). A rule whose only trigger is the fixtures of its own tests is exactly the
   "suspect Go-only rule" the query-engine guidance warns against. Narrowing it to LogicalValues-only
   keeps a rule alive solely to pass its own tests.

**Fix:**
- **Delete** `rule_remove_range_one.go` + `rule_remove_range_one_test.go` + `isAtMostOneRow`, and remove
  the `NewRemoveRangeOneRule()` registration at `default_rules.go:124`. (grep-confirmed: the sole
  reference outside the rule's own files is that one registration; **no** `%T`-keyed `DisabledRules`
  entry, **no** yamsql/plan test asserts limit-removal — deletion is safe end-to-end.) With the rule
  gone the `LIMIT 1` is always retained → the wrong-rows path is unreachable and Go matches Java (which
  keeps the limit).
- **Follow-up (booked, not B scope):** Java's *actual* `RemoveRangeOneRule` (drop an unreferenced
  `RANGE(0,1)` table-function quantifier from a `SelectExpression`) is UNPORTED — a distinct
  missing-rule item, filed in TODO.md. Porting *that* reclaims the `RemoveRangeOne` name cleanly (no
  rename churn), which is why delete beats rename here.

**Test:** the pin moves from a rule-internal unit test (deleted with the rule) to an **FDB e2e row-count
pin** — the right level for a wrong-rows bug. `TestFDB_LimitOneKeepsLimitOverManyPredicateFilter`: a
20-equality-conjunct `SELECT … LIMIT 1` with multiple matching rows returns **exactly one** row (RED on
baseline: the rule deletes the limit and returns all matches). Plus the `EXPLAIN` shows the limit plan
survives.

---

## 2. Finding 3 (HIGH, worse plan) — comparePrimaryScanVsIndexScan drops Java's SARG sub-case + config

**Go:** `planning_cost_model.go:878` `comparePrimaryScanVsIndexScan(opsA, opsB)` checks only operator
SHAPE (primary-scan vs singular-index-scan-with-fetch) and unconditionally returns the `PREFER_SCAN`
direction (`-1`/`+1`). Its comment falsely claims "matches Java's check."

**Java:** `PlanningCostModel.java:comparePrimaryScanToIndexScan` (called via `flipFlop`). The method is
written with the assumption **first arg = primary scan, second arg = index scan**, and `flipFlop` runs
it in both orderings, **negating** the result of the second. When the shape matches, FIRST the SARG
sub-case: if `typeFilterCountPrimaryScan > 0 && typeFilterCountIndexScan == 0`, compute
`primaryMinusIndex = Sets.difference(primaryComparisons, indexComparisons)`; if empty, compute
`indexMinusPrimary = Sets.difference(indexComparisons, primaryComparisons)`; if **non-empty**, return
`+1` (index has an extra SARG the primary lacks and needs no type filter → prefer the index). THEN the
config branch: `PREFER_SCAN` → `-1`; else (`PREFER_INDEX`/`PREFER_PRIMARY_KEY_INDEX`) → `+1`.

Two fidelity points the rev-1 draft got wrong:
- **The comparison set is `Set<Comparisons.Comparison>`, keyed by BARE comparison (type + comparand),
  NOT by column.** Java's `ComparisonsProperty implements ExpressionProperty<Set<Comparisons.Comparison>>`;
  the `Comparison` object carries only the comparison type and comparand — the column/field is *implicit*
  in the position within `ScanComparisons` and is **not** part of the object. Keying Go's set by column
  would make `Sets.difference` non-empty where Java's is empty → the sub-case fires (or doesn't) on the
  wrong queries. Build the set from bare comparisons.
- **The sign is relative to which side is the primary scan.** The `+1`/`-1` above are stated in the
  (primary=first, index=second) orientation. In the comparator, whichever of `a`/`b` is the *index*
  scan must flip the sign (Java's `flipFlop` negation). The rung returns "empty/no-opinion" when neither
  ordering matches the shape.

**Consequence:** with a multi-record-type table indexed on `(recordType, pk…)` and `WHERE pk=?`, plan A
= `TypeFilter(Scan)` (SARGs pk, needs the type filter), plan B = `Fetch(IndexScan)` (SARGs pk AND
recordType). Java prefers B; Go returns the `PREFER_SCAN` direction → a near-full-table scan +
high-discard type filter.

**Fix:**
- Pass the two sides' type-filter counts (already in `opsA/opsB.typeFilterCount`) and the concrete `a, b`
  expressions into the rung (the comparator already holds them; this is a signature growth, not a
  refactor). Extract each scan's SARG comparison SET **from bare comparisons over `ScanComparisons`**
  (no column key), compute the two `Sets.difference`, and return the index-favoring sign in the exact
  sub-case Java does, **threading the sign by which side is the primary scan**.
- **IndexScanPreference config — confirmed ABSENT in Go, not "assess in impl."** `grep -rn
  IndexScanPreference|PREFER_ pkg/` returns nothing: Go's `PlanContext` has no such knob. The Cascades
  config default *is* `PREFER_SCAN` (proto enum `PREFER_SCAN = 0`; the `PREFER_INDEX` multi-type
  override lives in the legacy `RecordQueryPlanner` constructor, not the Cascades `PlanningCostModel`),
  so Go's only behavior is the `PREFER_SCAN` branch (`-1` in the primary-first orientation). The SARG
  sub-case is the wrong-plan fix and lands regardless; **modeling the config knob** (`PREFER_INDEX`/
  `PREFER_PRIMARY_KEY_INDEX` + the legacy multi-type-no-PK-prefix default) is **out of scope, booked
  separately in TODO.md** — nobody sets it today.

**Test:** `TestFDB_PrimaryVsIndex_SargScanPrefersIndex` (EXPLAIN pin): the multi-type `WHERE pk=?` shape
plans the selective index, not the type-filtered primary scan. Red on baseline.

---

## 3. Finding 6 (MED) — comparePredicateCountByLevel iterates the union; Java iterates the first map

**Go:** `planning_cost_model.go:115-139` iterates `for level := 0; level <= maxLevel` over the UNION of
both maps' levels (`maxLevel = max(maxLevelA, maxLevelB)`).

**Java:** `PredicateCountByLevelProperty.java:182-193` iterates ONLY the first map's entries
(`aLevelToPredicateCount.entrySet()`, a `SortedMap` → **ascending key order**), reading
`bLevelToPredicateCount.getOrDefault(level, 0)`; first differing level returns
`Integer.compare(aCount, bCount)`; final tiebreak `Integer.compare(a.getHighestLevel(),
b.getHighestLevel())`. It does NOT visit levels present only in `b` (except via the highest-level
tiebreak).

**Consequence:** for asymmetric-depth predicate maps (`a={0:1,2:5}`, `b={0:1,1:3}`) Java returns `+1`
(level 2: `compare(5,0)`), Go's dense loop hits level 1 first (present only in `b`): `compare(a[1]=0,
b[1]=3)` → `-1` — opposite. This is the REWRITING "more predicates deeper wins" rung
(`designated_final.go`), so it selects a different logical survivor. (This comparator is deliberately
asymmetric in Java — `compare(a,b) != -compare(b,a)` — and Go must match that asymmetry, not "fix" it.)

**Fix:** iterate `a`'s **actual keys, sparse and sorted ascending** (collect `a`'s keys, `sort.Ints`,
range over them) — **not** a dense `0..maxLevelA` and **not** Go's random map order (both wrong: a dense
loop still visits `b`-only levels, and unsorted iteration makes the first-difference return
nondeterministic). `b`'s count via map default `0` (= `getOrDefault(level, 0)`); final tiebreak
`intCompare(maxLevelA, maxLevelB)` (already correct). Exactly Java's loop.

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
**Scope (rev 2, Torvalds catch): this is plan-metadata plumbing, same class as M3 — NOT a light
property tweak.** The `createsDuplicates` signal *exists* in Go, but on the **match candidate**
(`ValueIndexScanMatchCandidate.CreatesDuplicates()`, porting `index.getRootExpression().createsDuplicates()`)
— **not** on the plan node the property visitor sees. `RecordQueryIndexPlan` carries only
`GetColumnNames`/`GetPKColumnNames`/`IsUnique`; it has no key expression and no fan-out flag. So the fix
must plumb the fan-out signal onto the plan (or derive it at property-compute time from the index root
expression / metadata), exactly the class of plumbing M3 needs — the rev-1 "M4 is light, order it before
M3" premise is dropped.
**Fix:** carry `createsDuplicates` (or the index root expression) onto `RecordQueryIndexPlan`, and return
`!createsDuplicates()` instead of `IsUnique()`. A fan-out index on a repeated/collection field creates
duplicates; a scalar-field index does not.

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
  operator (finding 2 *removes* a Go-only rule; it adds nothing). Graefe: match-then-implement untouched.
- The Go-only extension rungs the quality review CLEARED (statistics scalar-cost, `compareJoinOrdering`,
  redundant-sort) are NOT touched — they are documented, test-pinned substitutes for Java's early
  `CardinalitiesProperty` join discrimination, and stay.

## 6. Risks & mitigations

- **R1 — plan churn.** Cost changes flip plans on unrelated queries. Full 56-target suite +
  determinism ×10 on affected planner tests + **1M stress before/after** gate every change; any EXPLAIN
  change is reviewed as a correctness/parity improvement or a neutral shape change, never waved through.
- **R2 — M3/M4 plumbing (M5 is light — plan already carries `GetPKColumnNames`).** M3's whole-plan
  `Cardinalities` and M4's `createsDuplicates` both live off the `RecordQueryIndexPlan` the property
  visitor sees (M4's signal is on the match candidate; M3's `Cardinalities` is computed on physical
  plans but not necessarily reachable in the comparator). Plumbing them onto the plan is **in scope** —
  the full fix, not a property tweak. If either proves larger than expected, that sub-item ships behind
  a test with a booked TODO (never a silent skip); default is the full fix.
- **R3 — finding 6 asymmetry.** The corrected comparator is intentionally asymmetric (Java's shape);
  the determinism harness must still pass (the REWRITING designation is deterministic per-side).

## 7. Implementation order (DFS, one finding to completion, green per commit, e2e each)

1. **Finding 2** (wrong results) — **delete** the rule + tests + registration; FDB e2e row-count pin
   (LIMIT retained) + EXPLAIN. File the "port Java's real `RemoveRangeOneRule`" follow-up in TODO.md.
2. **Finding 6** (pure comparator, low risk) — sparse-sorted first-map iteration; unit pin.
3. **Finding 3** (worse plan) — SARG sub-case (bare-comparison set + flipFlop sign, `PREFER_SCAN`
   default; config knob booked separately); EXPLAIN pin + stress.
4. **Finding 10** — light first, plumbing last: **M2** (add count field + rung) → **M5** (plan already
   carries `GetPKColumnNames`) → **M3 & M4** (both need plan-metadata plumbing — whole-plan
   `Cardinalities` outer guard; `createsDuplicates` onto the plan). Unit pins each + stress.
5. Full suite + determinism ×10 + 1M stress; milestone review lap (Graefe + Torvalds), fold, delta.

No finding "done" until a test (unit + FDB where row/plan-visible) pins the corrected behavior. No
`t.Skip`, no "for now", no deferral of B's four findings.
