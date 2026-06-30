# RFC-164 — Port-fidelity drift: why the Cascades bugs happened, and how to make them un-shippable

Status: Draft (proposal + tracked workstream)
Origin: the `hunt/cascades-bug-hunt` batch (RFC-163) surfaced 9 confirmed Cascades
correctness/quality bugs. This RFC is the post-mortem + the systemic fixes so the
*class* stops recurring instead of being hunted one PR at a time.
Scope: query-engine + test infrastructure (Graefe-gated for engine changes).

---

## 1. The root pattern: port-fidelity drift

This is a Java→Go port where wire/behavior compat is the whole point. Every one of
these bugs is a spot where someone wrote a *reasonable-looking Go version* instead of
a faithful 1:1 port of the Java — and silently dropped an invariant the Java carries.
CLAUDE.md already screams about this ("1:1 port is king", "no Go-only shortcuts",
"Read Java first"). These bugs are evidence the principle was violated in specific
places. The flavors:

| Flavor | Bugs | What got dropped |
|---|---|---|
| Hand-rolled rule bypassing the framework | AGG-RESIDUAL | Java runs `Compensation::intersect → isImpossible()`; Go wrote a shortcut with no impossibility check |
| Simplified a Java data model → lost a whole dimension | NULLS-ORDER | `RequestedSortOrder` enum is `{Any,Asc,Desc}` — Java has the `NULLS_FIRST/LAST` axis; it's literally unrepresentable in Go |
| Reimplemented an algorithm instead of porting | CAST-ROUND | `floor(x+0.5)` instead of `Math.round` |
| Go-only path with no Java oracle to check against | COST-SELECTIVITY, HAVING-PUSHDOWN | Java has no scalar cost model and refuses to push through GROUP BY — Go invented both and got the guard/constants wrong |
| Go language trap | NONDETERMINISM | plain `map` where Java uses insertion-ordered `LinkedHashMultimap` |
| Duplicated truth that drifted | COUNT-COL, DISTINCT-UNIONALL, AGG (guard≠consumer) | planner's "count star" ≠ executor's `isCountStar`; a hand-maintained distinctness type-switch + a naming divergence (`RecordQueryUnionPlan` = no-dedup in Go, dedup in Java) |
| Invariant reimplemented per-component, fix not propagated | IN-LIMIT-NIL | the nil-child relink contract lives in ~20 wrappers; RFC-070 fixed it in one (fetch), left it latent in limit |

## 2. Why CI was green anyway

1. **The test gap is dimensional, not volumetric.** Each feature was tested in
   isolation but never in the *combination* that breaks it: aggregate-index but never
   *with a residual WHERE*; `g > 5` but never `g > SUM(v)`; `COUNT(*)` but never
   `COUNT(col)` over an index lacking col; UNION and DISTINCT but never
   DISTINCT-over-UNION ALL; IN and LIMIT but never IN+LIMIT-without-ORDER-BY. The bug
   lives in the negative space *between* two tested features. (Textbook instances of
   the failure mode CLAUDE.md already cites: non-correlated EXISTS, secondary-UNIQUE→23505.)
2. **One test actively pinned the bug.** `plan_properties_test.go` asserted "union
   plan should produce distinct records" — a test locking in wrong behavior. Worst kind.

## 3. The deepest issue

Port fidelity isn't enforced by anything automated — it relies on each author reading
Java carefully, and the one safety net that *could* catch drift (the differential
harness) is **hand-fed**, so it only catches what someone already thought to write.
Fix the net (generative differential) + add structural invariants (make whole bug
classes un-shippable) + kill the duplicated truth, and these stop happening.

---

## 4. Workstream (tracked)

Ranked by leverage. Each item: deliverable, acceptance criteria, gate. Engine changes
are Graefe-gated; harness/test changes are Torvalds + `/code-review`.

### [ ] WS-1 — Generative Go-vs-Java row-level differential (biggest single win)
The harness *exists* (`pkg/relational/conformance/plandiff`, the `*_java.yaml` pairs,
the live `conformance/conformance_server.java`) but is hand-fed. Make it a DST-style
generator: emit random valid SQL over a fixed schema across the feature × condition
matrix (WHERE × GROUP BY × HAVING × DISTINCT × UNION[ ALL] × INTERSECT × LIMIT/OFFSET ×
ORDER BY [NULLS FIRST/LAST] × JOIN), run each through the Go embedded engine **and**
the Java conformance server, diff **rows** (then plans, advisory). Seeded/reproducible;
mismatch = failing case minimized to a repro.
- **Catches:** 7 of the 9 (all wrong-rows bugs, incl. the AGG non-leading-key hole
  Graefe found — which no human would hand-write a YAML for).
- **Acceptance:** generator + runner committed; a CI target runs N seeds against the
  Java server; a deliberately re-introduced AGG-RESIDUAL / DISTINCT-UNIONALL / NULLS
  bug is caught by the generator (prove the net works); minimizer emits a runnable repro.
- **Gate:** Torvalds + `/code-review`. Effort: ~1 focused day.

### [ ] WS-2 — Structural plan invariants asserted inside the planner (cheapest, kills classes)
Cheap post-extraction assertions, enabled in `PlanQueryForTest` + debug/test builds
(and a fuzz target), failing loudly on violation:
- [ ] **No `<nil>` child in an extracted physical plan** → catches IN-LIMIT *and every
  future per-wrapper relink bug across all ~20 wrappers at once*. Highest-ROI single check.
- [ ] **`COVERING ⇒ the index contains every referenced field`** → catches COUNT-COL.
- [ ] **`DistinctRecords==true ⇒ the plan actually dedups`** → catches DISTINCT-UNIONALL.
- [ ] **`claims-to-satisfy-ordering ⇒ provided ordering matches incl. NULL placement`**
  → catches NULLS-ORDER (depends on WS-3 NULLS axis existing first).
- **Acceptance:** invariant pass wired into the no-FDB plan harness + a `FuzzPlanner_*`
  target; each invariant has a red→green test reproducing the bug it guards.
- **Gate:** Graefe (engine) + Torvalds. Effort: ~half day for the nil-child check; rest incremental.

### [ ] WS-3 — Single source of truth (kill duplicated/drifting facts)
- [ ] Plan properties (distinctness, ordering, stored-record) become **methods on each
  plan type**, not central hand-maintained `switch` truth-tables in `plan_properties.go`
  — so adding/renaming a plan type can't miscategorize it (the `RecordQueryUnionPlan`
  miscategorization becomes structurally impossible).
- [ ] One shared `isCountStar` used by planner + executor (the COUNT-COL bug was two copies).
- [ ] Guard == consumer everywhere (done for AGG in RFC-163 via `groupColEqualityIndex`;
  audit for other guard/consumer pairs).
- **Acceptance:** the central type-switches in `plan_properties.go` are gone or reduced
  to dispatch; a new plan type added without declaring its properties fails to compile / a test.
- **Gate:** Graefe + Torvalds. Effort: ~1 day.

### [ ] WS-4 — Property/metamorphic tests for the Go-only paths (no Java oracle exists)
- [ ] **Cost monotonicity**: encode `equality selectivity ≤ range selectivity` (and "a
  more selective predicate never estimates more rows") as an invariant in
  `FuzzCostMonotonicity` — it exists but never encoded this, so COST-SELECTIVITY slipped.
- [ ] **Determinism under cost-tied access paths**: extend `FuzzPlanner_Determinism` to
  generate equal-cost index ties (it passed only because it never exercised one) → catches NONDETERMINISM.
- [ ] **Lint**: ban bare `for ... range someMap` in plan-affecting code (nogo analyzer or
  CI grep) — Go map iteration order is the NONDETERMINISM root.
- **Acceptance:** re-introducing the inverted selectivity constants / the plain-map
  iteration fails the respective fuzz/lint.
- **Gate:** Graefe (cost/determinism) + Torvalds (lint). Effort: ~1 day.

### [ ] WS-5 — Audit & enumerate the Go-only divergences (process)
The reservoirs are exactly where Go left the Java architecture: the simplified
`RequestedSortOrder` enum, the scalar cost fallback (`physical_wrapper.go` HintCost /
`planning_cost_model.go` scalar path), the hand-rolled `AggregateDataAccessRule`, the
per-wrapper relink contract. `DIVERGENCES.md` should enumerate each with the question:
**"what invariant does the Java carry that this drops?"** — that question is what finds
these before a hunt does.
- **Acceptance:** `DIVERGENCES.md` has a "Go-only paths / Java-model simplifications"
  section listing each, the dropped invariant, and either "covered by WS-2/4 invariant"
  or a tracked TODO to close the divergence (e.g. extend `RequestedSortOrder` with the
  NULLS axis = the NULLS-ORDER fix).
- **Gate:** Graefe review of the divergence list. Effort: ~half day + ongoing.

---

## 5. Relationship to the open RFC-163 bugs

The 3 *unfixed* hunt bugs are the natural first customers of this workstream:
- **NULLS-ORDER** → fixed by WS-5's "extend `RequestedSortOrder` with the NULLS axis",
  pinned by WS-2's ordering invariant.
- **COST-SELECTIVITY** → pinned by WS-4's cost-monotonicity invariant (+ 1M stress).
- **NONDETERMINISM** → pinned by WS-4's determinism-under-ties + map-iteration lint.

Closing them *through* the workstream (not as one-off patches) is the test that the
workstream actually works.
