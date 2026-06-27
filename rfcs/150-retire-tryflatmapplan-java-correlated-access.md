# RFC-150 — Retire the Go-only `tryFlatMapPlan`; unify correlated-join access on the data-access path (Phase 2 of RFC-148)

**Status:** Draft — Phase 2 of the RFC-148 split (Graefe direction-ACK'd the 148/150 split; this is the
deep, PR-#201-class half). **Do not start impl until RFC-148 (Phase 1) has landed** and this RFC has its
own Graefe ACK.
**Item:** TODO.md §7.7 (the join-leg half) — full Java alignment of correlated-join access.
**Reviewers:** **Graefe** (data-access / winner-selection / join-leg consumption — REQUIRED, this is the
0-row surface) + Torvalds + codex + @claude.
**Classification:** query-engine **parity + correctness**. No wire impact, but **plan-shape AND row-level**
sensitive (LEFT/FULL OUTER residual placement changes NULL-extension → wrong rows if mishandled). Per-shape
EXPLAIN + **row-count** proofs + 1M stress mandatory.

---

## 1. Problem — Go has two correlated-join-access paths; Java has one

For `SELECT * FROM o, t WHERE t.fk = o.id AND t.amount > 100`, Go can reach a correlated inner index scan
two structurally disjoint ways (full trace in RFC-148 §2):
- **PATH A — `tryFlatMapPlan`** (`rule_implement_nested_loop_join.go:1009-1289`): a **Go-only** rule that
  manually re-derives a correlated index/PK scan from the flat select's `sel.GetPredicates()` over a **bare
  `RecordQueryScanPlan`** inner (type-assert `:1018`), and owns residual placement (above-FlatMap for INNER
  `:1272-1283`; inner-pushed for LEFT OUTER `:1241-1255`). Java has **no equivalent**.
- **PATH B — data-access compensation** on the `PartitionBinarySelectRule` `SUBSEL` ref: the matcher sargs
  the correlation into a `PartialMatch` bound prefix (Java's only mechanism, `AbstractDataAccessRule`).

Maintaining both is a CLAUDE.md "no parallel pipelines" violation: two producers, double maintenance, and a
latent **pinned-plan-flip / 0-row** surface. RFC-076 v5 + RFC-148 (Phase 1) leave the join-leg coupling
(`!refIsJoinLeg`/`refHasCorrelatedMatch`, `planner.go:490,511,573-582`) in place precisely because removing
it without the safeguards below ships a 0-row plan (the PR-#201 class).

**Critical framing (Graefe).** It is NOT true that "PATH B is muzzled on join legs." `!refIsJoinLeg` blocks
only `implementDataAccessCompensation` — the **residual filter** (`:511`). The **bare correlated scan
wrapper** is `InsertFinal`'d unconditionally (`:499`). So a *no-residual* correlated `SUBSEL` already carries
a physical correlated index scan today and PATH B can already fire for it; only the **residual** case is
muzzled. PATH A/B competition is therefore **partly pre-existing** — this RFC must map the
**no-residual-vs-residual** interaction exhaustively, not assume a clean "turn PATH B on".

## 2. Goal

Retire PATH A (`tryFlatMapPlan` + the EXISTS analogs `tryExistsFlatMap`/`buildExistsFlatMap`), remove the
`!refIsJoinLeg`/`refHasCorrelatedMatch` data-access guard, and let the data-access path (PATH B) be the
single producer of correlated-join access, consumed by the join structure — 1:1 with Java. Plans and rows
identical to today for every pinned shape.

## 3. Design — the three load-bearing invariants (Graefe conditions)

**B1 — a STRUCTURAL no-correlated-standalone-leg-winner invariant, ported from Java, not a Go guard.**
Today there is **no** correlation awareness in `findBestPhysicalExpr` / `getWinnerForOrdering` /
`PruneWith` (`physical_wrapper.go:238-252`, `winner_lookup.go:19-76`, `reference.go:491-494`); `!refIsJoinLeg`
is the only thing preventing a correlated `SUBSEL` scan (referencing an unbound outer) from being stamped as
that ref's standalone `OptimizeGroup` winner → 0 rows. The replacement must specify, ported from Java's
`OptimizeInputs` / FlatMap-inner consumption (where the correlated inner is optimized **with the outer
binding live** and consumed only by the driving join): **where** a correlated leg's plan is consumed only by
the driving join, and **why** `OptimizeGroup` structurally never stamps it standalone. "A test verifies it"
is explicitly insufficient for this axis.

**B2 — residual-placement reconciliation is a CORRECTNESS axis, not plan-shape.** When PATH B owns residual
placement (PATH A owns it today), **LEFT/FULL OUTER** placement must be proven with **row-level** tests: a
residual on the wrong side of an outer join changes NULL-extension → **wrong rows**, not merely slower.
Inner-join above-FlatMap vs leg-pushed is plan-shape; outer-join side is correctness. Enumerate INNER /
LEFT OUTER / FULL OUTER × (covering / residual) × (PK-prefix / secondary-index) and pin each.

**B3 — do NOT retire `matchBoundPrefixIsCorrelated`** (`abstract_data_access_rule.go:515`) with
`refHasCorrelatedMatch` — it still gates the RFC-069 intersection exclusion (`planner.go:550`). Only the
**data-access join-leg muzzle** use of the correlated signal is removed; the intersection-exclusion use
stays.

**Plus** RFC-148's `pushDataAccessTasks` re-entry/termination guard (inherited) and the `comp.IsImpossible()`
equivalence audit (prerequisite, shared with RFC-148's index-only arm).

## 4. Method — one shape at a time, behind a switch

Grind shape-by-shape; for each, route correlated access through PATH B, prove EXPLAIN + row-count identical,
then remove the corresponding PATH A branch. Order (least → most NULL-sensitive):
1. INNER join, secondary-index correlation, no residual (PATH B already fires — prove parity + remove PATH A
   branch).
2. INNER join, PK-prefix correlation, no residual.
3. INNER join + residual (un-muzzle the residual filter on join legs; B1 invariant must be in place first).
4. LEFT OUTER (residual placement = correctness, B2 row-level proofs).
5. FULL OUTER (the materialized-NLJ-only path — confirm PATH B's correlated scan is *never* chosen for FULL,
   which `tryFlatMapPlan` guards today at `rule_implement_nested_loop_join.go` FULL-OUTER guard).
6. EXISTS / lateral subquery-in-FROM (`tryExistsFlatMap`).
Only after all six: delete `tryFlatMapPlan` + analogs and the `!refIsJoinLeg` data-access guard.

## 5. Test plan

- **Per-shape EXPLAIN + row-count**, before/after, for every entry in §4's matrix. Row-count is mandatory on
  outer joins (NULL-extension correctness).
- **Full plandiff byte-identical** across the corpus + **1M stress before/after** (per shape and final).
- **B1 regression**: a correlated leg that, without the invariant, would be stamped standalone → assert it
  is consumed by the join and rows are non-zero (the PR-#201 reproduction).
- **Termination**: inherited from RFC-148 (growth-keyed guard) re-validated with PATH B as sole producer.
- Existing sentinels stay green: `correlated_intersection_guard_test`, `TestFDB_CascadesFlatMapCorrelatedJoin`
  (+ LEFT/LIMIT variants), `zz_join_selpred_repro_test` (RFC-069), `plan_shape_conformance_test`,
  EXISTS/`outer_join_parity` suites.

## 6. Gate & risk

**Graefe ACK on RFC + impl**, per shape if needed. This is the highest-0-row-risk change in the item-5
series: it removes the only guard currently preventing a correlated standalone-leg winner. The mitigation is
the B1 structural invariant (not a test), per-shape row-level proofs, and staged removal. **Never delete a
PATH A branch before its PATH B replacement is EXPLAIN+row-count-proven.**

## 7. Scope

**In:** retire `tryFlatMapPlan` + EXISTS analogs; remove the `!refIsJoinLeg`/`refHasCorrelatedMatch`
data-access muzzle; the B1 structural winner-selection invariant; LEFT/FULL OUTER residual-placement
reconciliation; the no-residual-vs-residual interaction map. **Out:** `matchBoundPrefixIsCorrelated`
(retained for intersection exclusion); `Compensation` construction; anything RFC-148 (Phase 1) owns.
