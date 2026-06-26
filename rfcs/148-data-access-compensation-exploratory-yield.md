# RFC-148 — Retire the *standalone* `isSimpleResidualCompensation` allowlist via uniform exploratory yield (Phase 1)

**Status:** v3 (Option A) — Graefe **ACK'd** (RFC text + the implementation refinement). v1 bundled two
retirements with very different risk (NAK); v2 split into Phase 1 (this) + Phase 2 (RFC-150). v3 refines
Phase 1 after implementation surfaced two facts: (1) the allowlist's vector/aggregate inner-scan + index-only
guards are **real safety, not rot** — kept as `compensationSafeForYield`; only the **predicate-shape**
restriction (the rot) is retired via `yieldUnknown`. (2) B3 (the `ImplementFilterRule` `!isIndexOnly()` gate)
is the **wrong layer** without match-level index-only consumption (breaks a should-plan vector query) — DEFERRED
to a named follow-up (§3b, TODO §7.7). Phase 1 is a behavior-preserving refactor (plandiff byte-identical).
The join-leg coupling (`!refIsJoinLeg`/`refHasCorrelatedMatch`) is **retained** here and removed separately in
**RFC-150**, which also retires the Go-only `tryFlatMapPlan`.
**Item:** TODO.md §7.7 / RFC-076 v5 Graefe-ACK condition.
**Reviewers:** **Graefe** (Cascades data-access/yield — REQUIRED) + Torvalds + codex + @claude.
**Classification:** query-engine **parity** (Java's uniform yield replacing a Go stand-in). No wire impact
(plan selection only); plan-shape sensitive → **plandiff byte-identical + 1M stress mandatory** (Graefe
condition 1 — `yieldUnknown→Insert→re-explore` fires the FULL rule set, unlike today's surgical arm).

---

## 1. Problem (verified real, narrowed to the standalone case)

When `pushDataAccessTasks` realizes an index match whose residual predicate is not subsumed by the index,
the data-access path produces a **compensation**: a `LogicalFilterExpression` over the physical index scan
(`ForMatchCompensation.ApplyAllNeeded`, `abstract_data_access_rule.go:330`) — a non-physical expression.
Go inserts it via `ref.InsertFinal` (`planner.go:499`); in the final set "physical beats non-physical", so
a logical compensation **loses to the full scan** and the index scan is silently dropped.

Go bolts a stand-in onto that gap (`planner.go:511`): only if
`inserted && !refIsJoinLeg && !isPhysical(expr) && isSimpleResidualCompensation(expr)` does it call
`implementDataAccessCompensation` (`planner.go:682-687`), which **surgically** fires only
`ImplementFilterRule` (a lone `TransformExprTask`, deliberately not an `ExploreExprTask` — comment at
`planner.go:679`) on the compensation. The allowlist `isSimpleResidualCompensation` (`planner.go:590-653`)
admits a compensation only when every predicate is a simple non-`ComparisonIn` `ComparisonPredicate`, not
index-only, and not row-correlated.

**No live bug** — each exclusion is pinned (`TestVectorPlan_QualifyPlansToVectorScan`,
`TestImplementIndexScanRule_SkipsIndexOnlyResidual`, `TestVectorPlan_MetricMismatchDoesNotMatchVector`,
plus IN / join-leg coverage). **But it rots:** a new *standalone* (non-join-leg) compensation shape with
no allowlist arm falls through to `InsertFinal`-only → loses to full scan → **silent no-plan**. The
allowlist is a correctness landmine for future standalone shapes. **This RFC removes that landmine; it
does not touch join-leg correlated access** (see §2).

## 2. Investigation — the allowlist is TWO separable mechanisms

Deep trace (Go file:line + Java) shows the `planner.go:484-516` block conflates two concerns with very
different risk:

**(M1) the standalone rot — `isSimpleResidualCompensation` (`:590-653`).** Governs single-source
(`!refIsJoinLeg`) refs. Java yields standalone data accesses **uniformly** via `yieldMixedUnknownExpressions
→ yieldUnknownExpression` (`AbstractDataAccessRule.java:219-223`, `CascadesRuleCall.java:212-219`): physical
→ `yieldPlan` (final set), logical → `yieldExploratoryExpression`, re-optimized by the normal planning loop
until it yields a `RecordQueryPlan`. **Replacing M1 with that uniform yield for `!refIsJoinLeg` refs is
low-risk and faithful** — it is exactly Java's structure, and the Go primitives exist (two-set memo
`Insert`/`InsertFinal` `reference.go:299,376`; exploratory re-explore `[ExplMemberCount:]` `reference.go:443`;
PLANNING-phase explode/join/filter rules in `PlanningExplorationRules`).

**(M2) the load-bearing join-leg coupling — `refHasCorrelatedMatch`/`!refIsJoinLeg`
(`:490,511,573-582`).** This is **NOT rot** — it is structural. Go has **two** correlated-join-access paths
where Java has one:
- **PATH A — `tryFlatMapPlan`** (`rule_implement_nested_loop_join.go:1009-1289`): fires on the flat
  predicate-bearing `Select([o,t], preds)` member where the inner quantifier ranges over a **bare
  `RecordQueryScanPlan`**, and *manually* re-derives a correlated index/PK scan from `sel.GetPredicates()`
  (type-asserts `rightPlan.(*RecordQueryScanPlan)` at `:1018`). Residual placement: above-FlatMap for INNER
  (`:1272-1283`), inner-pushed for LEFT OUTER (`:1241-1255`). **This is a Go-only divergence — Java has no
  such rule.**
- **PATH B — data-access compensation** on the `SUBSEL` ref that `PartitionBinarySelectRule`
  (`rule_partition_binary_select.go:172-190`) memoizes (a *fresh* ref): the matcher sargs the correlation
  into a `PartialMatch` bound prefix → a correlated index scan, with residuals as a compensation.

**Disjoint memo groups (corrects v1's premise).** `tryFlatMapPlan` only fires when
`findBestPhysicalExpr(rightRef)` is a plain `RecordQueryScanPlan` — a **predicate-free bare-scan ref**. A
predicate-free ref has no sargable predicate → zero-prefix matches dropped (`abstract_data_access_rule.go:82`)
→ **it never carries a compensation**. Compensations land only on the predicate-bearing `SUBSEL`. So PATH A
(bare scan) and PATH B (`SUBSEL`) are **distinct memo groups**; `PruneWith` collapsing `SUBSEL`
(`reference.go:491-494`, per-ref) can never evict PATH A's plain scan. **The plain scan survives
unconditionally** — there is no "scan destruction". The real Phase-2 risks (deferred to RFC-150) are: (a)
**redundancy** (empowered PATH B + PATH A both yield correlated FlatMaps into the top ref with non-identical
encodings → pinned-plan flips) and (b) the **standalone-leg-winner 0-rows** (a `SUBSEL` correlated scan
referencing an unbound outer, stamped as a standalone `OptimizeGroup` winner — there is **no** correlation
guard in `findBestPhysicalExpr`/`getWinnerForOrdering`/`PruneWith`; `!refIsJoinLeg` is the only thing
holding it). **Both are M2's job and stay guarded in Phase 1.**

**Nuance (Graefe, carried to RFC-150):** `!refIsJoinLeg` blocks only `implementDataAccessCompensation` (the
**residual filter**, `:511`); the **bare correlated scan wrapper** is still `InsertFinal`'d unconditionally
(`:499`). So a *no-residual* correlated `SUBSEL` already carries a physical correlated index scan today and
PATH B can already fire for it — PATH A/B competition is **partly pre-existing**. Phase 1 changes none of
this (it only routes `!refIsJoinLeg` refs through `yieldUnknown`).

## 3. Design (Phase 1)

**3a. Replace the surgical arm with `yieldUnknown` — BYTE-IDENTICAL (the predicate-shape retirement, the
"rot-fix", moved to RFC-150).** Add `yieldUnknown(ref, expr)`: `isPhysical(expr) ? ref.InsertFinal(expr) :
ref.Insert(expr)` — Go's `CascadesRuleCall.yieldUnknownExpression` analog. In the `planner.go:484-552` block,
for a `!refIsJoinLeg` ref a **safe** logical compensation routes through `yieldUnknown` (→ exploratory set,
re-optimized by the existing `ExploreGroupTask`/`ExploreExprTask` loop) instead of the Go-only surgical
`implementDataAccessCompensation`. An **unsafe** compensation keeps the OLD `InsertFinal` path.

`compensationSafeForYield(expr)` is the **full** OLD `isSimpleResidualCompensation` predicate set, kept:
unsafe ⇔ inner scan is a vector top-K / aggregate scan, OR any predicate carries an index-only value, OR any
predicate is non-simple (compound/OR or IN), OR any residual is correlated to an OUTER quantifier. So
`yieldUnknown` fires for EXACTLY the shapes the allowlist accepted → **Phase 1 is byte-identical** (plandiff
confirms), a pure Java-aligned mechanism swap (the surgical arm is a Go-only divergence).

**Why the shape gate is RETAINED (not retired) in Phase 1 — the rot-fix is unsafe without RFC-150.**
Implementation proved (codex) that retiring the predicate-shape restriction ships a 0-row plan: a NON-simple
residual on a partition-SUBSEL **join leg** whose join correlation lives in a SIBLING predicate
(`t.fk = o.id`) is NOT flagged by `refIsJoinLeg` (bound-prefix only) nor by the residual-correlation guard
(the residual is local). Materializing it via `yieldUnknown` wins and severs the join feed →
`FlatMap(... inner=Fetch(<nil>))`. Distinguishing "standalone single-table" from "partition-SUBSEL join
leg" needs the PARENT context — RFC-150's winner-selection invariant. (A simple-AND variant of the same
query produces the same `Fetch(<nil>)` on **base** too — a PRE-EXISTING join-leg 0-row bug, the headline
RFC-150 sentinel.) So the rot-fix (compound/IN residuals realizing) lands in RFC-150, after B1.

The vector/aggregate inner-scan + index-only branches of `compensationSafeForYield` are a documented
**stand-in** (§3b). **Retain the entire `!refIsJoinLeg` / `refHasCorrelatedMatch` path unchanged** (M2 —
RFC-150's surface). Net: **fully** behavior-preserving (plandiff byte-identical) — safe standalone
compensations re-optimize through the full rule set instead of the surgical arm, with identical plans. The
predicate-shape rot is NOT closed here (it is RFC-150's, after B1).

**3b. B3 (`ImplementFilterRule` `!isIndexOnly()` gate) — DEFERRED; it is the wrong layer without match-level
consumption.** v2 §3b proposed porting Java's `ImplementFilterRule.java:62` `all(anyCompensatablePredicate())`
gate. **Implementation proved this breaks a should-plan query** (`TestVectorPlan_QualifyPlansToVectorScan`):
Go's vector / aggregate match leaves the index-only value (vector `DistanceRank`,
`vector_index_match_candidate.go:220-234`; aggregate `UnmatchedAggregateValue`) as a **residual** where Java
marks it **consumed** by the index access. So the legit vector query still has a `DistanceRank` predicate
reachable by `ImplementFilterRule`, and a `!isIndexOnly()` gate cannot distinguish "redundant-but-legit"
from "genuine leak" at that layer — it kills the legit query (design-principle #10: the gate is a downstream
observable; the real property is match-level consumption). The gate is sound **only after** the match
consumes consumable index-only values. Therefore B3 + that match-level consumption fix are a **named,
filed follow-up** (TODO.md §7.7), carrying `TestVectorPlan_QualifyPlansToVectorScan` (must still plan) +
`TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual` (must stay unplannable) as red→green
sentinels. Until it lands, `compensationSafeForYield` (3a) is the conservative data-access-boundary proxy and
`validateNoIndexOnlyResidual` (`plan_executability.go:45`) stays as the late net for the original-query path.

**3c. B4 — re-entry / termination guard keyed on match-set GROWTH.** `pushDataAccessTasks` is step 1 of
`ExploreExprTask` (`unified_tasks.go:113-114`); routing a compensation to the exploratory set makes the
enclosing re-pushed `ExploreGroupTask` re-explore it, re-entering `pushDataAccessTasks` on the ref. A blanket
"consumed-ever" sentinel would drop **late-seeded** matches (`pushDataAccessTasks` is re-run across rounds
specifically to pick up mid-exploration matches via `AdjustPartialMatchesForRef`) → silent no-plan. The guard
must **re-run iff the consumed partial-match set GREW** (key on the match-partition set, à la
`hasIntersectionFinal` `:529,689` but growth-aware), preserving both termination and late-match pickup. Add a
chain task-count gate that would **trip the 10-round cap** (`unified_tasks.go:62-66`) on a re-entry
regression — "determinism 5×" is not a convergence proof (the cap *masks* non-convergence).

**3d. Ordering preservation.** A compensation inserted into the exploratory set mid-exploration must still
receive the requested-ordering push (`PushRequestedOrderingThrough{Filter,Select}Rule`) so the inner scan's
matched ordering eliminates the in-memory sort (Go has no physical sort — a missed push = wrong/extra-sort
shape, not just slower). Analyze reachability of a late exploratory member by the constraint pass + an
EXPLAIN test that a re-optimized residual compensation still eliminates the sort.

**3e. Staging — per-shape SWITCH, never both** (Graefe condition 3). For a given non-join-leg shape, route
through `yieldUnknown` *or* the allowlist arm in one build, never both (double materialization). Keep the
allowlist arm for not-yet-switched shapes; delete `isSimpleResidualCompensation` only after every
non-join-leg shape it admits is red→green-proven through `yieldUnknown`.

## 4. Wire / behaviour impact

No wire impact (plan selection only). **Goal: byte-identical plans for every currently-pinned shape**, with
the allowlist's silent-no-plan failure mode removed for future *standalone* shapes. Join-leg plans are
untouched (M2 retained).

## 5. Test plan (Graefe conditions are mandatory, not optional)

- **Plandiff byte-identical across the full corpus** + **1M stress before/after** (CLAUDE.md planner-change
  protocol: point lookups <5ms, full scans ~3s/1M, index equality <10ms). Mandatory even for "low-risk"
  Phase 1 — `yieldUnknown→Insert→re-explore` fires the full rule set and can mint members that move the
  winner; this is the safety net (Graefe condition 1).
- **Simple-residual yieldUnknown realizes correctly**: `TestPlanHarness_ResidualCompensationPreservesOrdering`
  (a simple residual re-optimized via yieldUnknown keeps the IDX_K order — no physical Sort, §3d) +
  `TestPlanHarness_YieldUnknownReentryConverges` (the exploratory re-entry converges to a stable plan, §3c).
- **M2 0-row guard** (the residual-correlation half of the retired allowlist):
  `TestPlanHarness_CorrelatedResidualNotStandaloneLeg` — an unindexed `t.fk=o.id` residual alongside an
  indexed `t.k=5` plans the valid correlated FlatMap, no `Fetch(<nil>)`.
- **codex regression** (the shape gate keeps a non-simple residual on a hidden join leg off yieldUnknown):
  `TestPlanHarness_MultiTableJoinCompoundResidualNotMaterialized` — the 3-way-join OR-residual query has no
  nil inner.
- **Join-leg untouched**: the correlated-join sentinels (`TestFDB_CascadesFlatMapCorrelatedJoin`,
  `zz_join_selpred_repro_test`, `plan_shape_conformance_test`) stay byte-identical.
- **Deferred to RFC-150** (gated on B1, the winner-selection invariant): the rot-fix (compound/IN residuals
  realizing via the index instead of a full scan), with the pre-existing simple-AND-on-hidden-join-leg
  0-row bug as the headline red→green sentinel. **Deferred to the match-consumption follow-up** (§3b): the
  `ImplementFilterRule` `!isIndexOnly()` gate + retiring `compensationSafeForYield`'s index-only branch +
  `validateNoIndexOnlyResidual`, sentinels `TestVectorPlan_QualifyPlansToVectorScan` (plans) +
  `TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual` (unplannable).

## 6. Gate & risk

**Graefe ACK on RFC + impl** + Torvalds + codex + @claude. Risk: bounded to the standalone path; the
PR-#201 surface (join-leg) is explicitly out of scope (RFC-150). The residual risk is a winner move from the
full-rule-set re-exploration (3a) — caught by mandatory plandiff + 1M stress.

## 7. Scope

**In:** the `yieldUnknown` router (non-join-leg only), B3 matcher gate, B4 growth-keyed termination guard,
ordering-preservation, staged removal of `isSimpleResidualCompensation`. **Out (→ RFC-150, Phase 2):**
removing `!refIsJoinLeg`/`refHasCorrelatedMatch`; retiring the Go-only `tryFlatMapPlan`; the B1 structural
no-correlated-standalone-leg-winner invariant; LEFT/FULL OUTER residual-placement reconciliation; the
no-residual-vs-residual PATH-A/B interaction map. **Out (both):** `Compensation` construction;
`matchBoundPrefixIsCorrelated` (retained — it gates the RFC-069 intersection exclusion). Prerequisite
sub-task for the index-only arm: audit `comp.IsImpossible()` vs Java's `Compensation.isImpossible()`.
