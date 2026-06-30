# RFC-164 — Port-fidelity drift: why the Cascades bugs happened, and how to make them un-shippable

Status: Draft (proposal + tracked workstream). v2 incorporates Graefe + Torvalds review.
Origin: the `hunt/cascades-bug-hunt` batch (RFC-163) surfaced 9 confirmed Cascades
correctness/quality bugs (a 10th — an aggregate equality whose RHS is another column,
`region = status` — was caught by codex during PR review and folded into the AGG fix).
This RFC is the post-mortem + the systemic fixes so the *class* stops recurring instead
of being hunted one PR at a time.
Scope: query-engine + test infrastructure (Graefe-gated for engine changes).

---

## 1. The root pattern: port-fidelity drift

This is a Java→Go port where wire/behavior compat is the whole point. Every one of
these bugs is a spot where someone wrote a *reasonable-looking Go version* instead of
a faithful 1:1 port of the Java — and silently dropped an invariant the Java carries.
CLAUDE.md already screams about this ("1:1 port is king", "no Go-only shortcuts",
"Read Java first"). The flavors:

| Flavor | Bugs | What got dropped |
|---|---|---|
| Hand-rolled rule bypassing the framework | AGG-RESIDUAL | Java runs `Compensation::intersect → isImpossible()`; Go wrote a shortcut with no impossibility check |
| Simplified a Java data model → lost a whole dimension | NULLS-ORDER | `RequestedSortOrder` is `{Any,Asc,Desc}` — Java's `OrderingPart.RequestedSortOrder` carries the `NULLS_FIRST/LAST` axis; it's literally unrepresentable in Go |
| Reimplemented an algorithm instead of porting | CAST-ROUND | `floor(x+0.5)` instead of `Math.round` |
| Go-only extension with an incomplete soundness guard (behavior IS Java-row-checkable) | HAVING-PUSHDOWN | Java's `PredicatePushDownRule.visitGroupByExpression` pushes nothing through a GroupBy; Go's pushdown guard checked only the LHS |
| Go-only path with genuinely NO Java oracle (no row difference) | COST-SELECTIVITY | Java has no scalar cost model — the wrong index returns the *same rows*, so no differential can see it |
| Go language trap | NONDETERMINISM | plain `map` where Java uses insertion-ordered `LinkedHashMultimap` |
| Duplicated truth that drifted | COUNT-COL, DISTINCT-UNIONALL, AGG (guard≠consumer; RHS-constness) | planner's "count star" ≠ executor's `isCountStar`; a hand-maintained distinctness type-switch + a naming divergence (`RecordQueryUnionPlan` = no-dedup in Go, dedup in Java); the AGG guard accepting predicates `ToScanPlan` then drops |
| Invariant reimplemented per-component, fix not propagated | IN-LIMIT-NIL | the nil-child relink contract lives in ~20 wrappers; RFC-070 fixed it in one (fetch), left it latent in limit |

## 2. Why CI was green anyway

1. **The test gap is dimensional, not volumetric.** Each feature was tested in
   isolation but never in the *combination* that breaks it: aggregate-index but never
   *with a residual WHERE* (and never with a non-leading-key, a gap, or a column-valued
   RHS); `g > 5` but never `g > SUM(v)`; `COUNT(*)` but never `COUNT(col)`; UNION and
   DISTINCT but never DISTINCT-over-UNION ALL; IN and LIMIT but never IN+LIMIT-without-
   ORDER-BY. The bug lives in the negative space *between* tested features.
2. **One test actively pinned the bug.** `plan_properties_test.go` asserted "union plan
   should produce distinct records" — locking in wrong behavior. Worst kind.

## 3. The deepest issue, and how to rank the fixes

Port fidelity isn't enforced by anything automated — it relies on each author reading
Java carefully, and the one net that *could* catch drift (the differential harness) is
**hand-fed**, so it only catches what someone already thought to write.

Two leverage axes, often conflated — keep them separate when sequencing:
- **Highest coverage:** WS-1 (generative Go-vs-Java row differential) — catches the most
  *wrong-rows* drift (7 of the 10, see WS-1). But it needs the full conformance
  environment (live Java server **and** FDB), so it's a nightly/conformance lane, not
  per-PR, and it only tells you *that* something diverged.
- **Highest ROI / cheapest class-kill:** the **no-`<nil>`-child invariant** (WS-2) and
  the **`isCountStar` dedup** (WS-3) — always-on, no Java/FDB dependency, Go-only CI,
  each makes a whole class structurally impossible, and each lands in days.

**Execution order = ships × leverage, not coverage alone:** land the cheap always-on
class-killers first (WS-2-nil-child, WS-3-isCountStar), bank the win, *then* invest in
WS-1 as a properly-budgeted multi-day/week effort. Do **not** gate the cheap nets behind
the heavy-infra one.

---

## 4. Workstream (tracked)

Engine changes are Graefe-gated; harness/test changes are Torvalds + `/code-review`.
Every "found" bug gets a **committed, minimized seed** — a generative net or fuzzer that
finds a bug once and then runs different inputs *forgets it*; persistence is what turns a
catch into a regression guard.

### [ ] WS-1 — Generative Go-vs-Java row-level differential (highest coverage; heavy-infra)
The harness already exists and is wired on the Go side (`plandiff/go_runner.go`'s
`goSQLRunner` drives the embedded engine and returns real `RowSet`s; the README's
"`ErrGoUnimplemented`/`UnableToPlanException`" caveats are **stale** — GROUP BY / DISTINCT
/ UNION ALL / SUM / COUNT / HAVING / INTERSECT already run against the live Java
conformance server). So this is "make the existing hand-fed harness generative," not a
green-field build. Generate random valid SQL over a fixed schema across the feature ×
condition matrix; run through the Go embedded engine **and** the Java conformance server;
**diff rows**.
- **Scope (state it):** **row-drift only.** A correct-but-different plan (a perf
  regression, covering↔non-covering returning identical rows) is invisible here — that's
  WS-2/WS-4's job. Plan-tree diff is normalization-heavy (different operator names/EXPLAIN
  formats) and stays advisory. The row comparison MUST be **order-sensitive for ORDER BY
  queries** (compare the row *sequence*, not a set) — otherwise NULLS-ORDER, a pure
  ordering bug, is invisible even with NULL data, and the mutation proof in (e) silently
  fails to guard it.
- **Catches (row-drift):** AGG-RESIDUAL, HAVING-PUSHDOWN, COUNT-COL, DISTINCT-UNIONALL,
  NULLS-ORDER (only with the order-sensitive comparison above), and the `region=status`
  instance. CAST-ROUND only if value generation is **boundary-aware** (a random generator
  won't emit `0.49999999999999994`) — it's unit-pinned regardless, so not load-bearing
  here. **Misses:** COST-SELECTIVITY and NONDETERMINISM (same rows). **IN-LIMIT-NIL is
  conditional** — see acceptance (c).
- **Acceptance:**
  (a) **Schema engineered to reach each bug surface** — a *multi-key* aggregate index
      (`SUM(v) GROUP BY a,b`), *nullable* sort columns *with NULL data*, and a
      covering/non-covering index *pair*. A generic single schema silently fails to cover
      exactly the bugs this claims.
  (b) **Engine-acceptance skew classification** — both-reject ⇒ skip; one-rejects ⇒
      known-skew/flag; both-accept ⇒ real diff. Without it the generator drowns in
      Err-vs-rows false positives from each engine's distinct unsupported-syntax set.
  (c) **Verify the LIMIT path** — fdb-relational may treat `LIMIT N` as JDBC
      `setMaxRows`, not SQL syntax. If so the runner must drive Java's cap that way (the
      current `Run(querySql)` signature can't), or IN-LIMIT-NIL is **6/10, not 7**. Verify
      and state which.
  (d) **Corpus persistence + lane + budget** — every mismatch is minimized AND committed
      as a permanent seed; a named CI lane (conformance/nightly, needs Java+FDB) with a
      stated runtime budget. A nightly nobody watches is the "grand harness that rots."
  (e) **Mutation proof** — reverting any of AGG-RESIDUAL / DISTINCT-UNIONALL / NULLS-ORDER
      makes the generator (or its committed seeds) go red.
- **Effort (honest):** ~1–2 weeks for a solid version (valid type/scope-correct SQL
  generation is SQLsmith/SQLancer-class, plus a delta-debugging minimizer plus skew
  classification), OR a deliberately **narrow first cut** in 2–3 days (no JOIN/INTERSECT,
  shrink-by-row instead of a real minimizer). The "~1 day" in v1 was wrong. **Gate:**
  Torvalds + `/code-review`.

### [ ] WS-2 — Structural plan invariants in the planner (highest ROI; always-on, Go-only CI)
Cheap post-extraction assertions enabled in `PlanQueryForTest` + debug/test builds (and a
fuzz target). **Hard acceptance for every invariant below:** the pass runs clean across
the ENTIRE existing plan-test + conformance corpus with **zero runtime skip-lists**; any
unavoidable exemption must be a *compile-time* type distinction (a structurally-optional
slot), never a runtime mute — otherwise the first false positive hollows the check out.
- [ ] **No `<nil>` child in the FINAL extracted plan.** Walk the fully-relinked root
  *post-extraction* only — explicitly **not** memo members, where a nil inner is the
  legitimate pre-relink snapshot (the RFC-070 / IN-LIMIT pattern). Catches IN-LIMIT and
  every future per-wrapper relink bug across all ~20 wrappers at once. Highest-ROI single
  check; land it first.
- [ ] **`WithChildren(GetQuantifiers())` round-trip identity.** Re-linking a node with its
  own quantifiers must reproduce the node — by **semantic** equality
  (`EqualsWithoutChildren` + same children), NOT pointer identity, so a node that
  legitimately re-derives its result type doesn't false-positive. The most *direct* catch
  for the whole relink class (more so than the nil check — it also catches a relink that
  swaps in a mismatched-alias child).
- [ ] **Correlation / quantifier-binding completeness.** Every `CorrelationIdentifier`
  referenced in the final plan is bound by an enclosing quantifier (no dangling
  correlation). A first-order Cascades invariant; catches relink/translation bugs
  generically.
- [ ] **Set-op comparison-key correctness.** Every leg of an ordered UNION/INTERSECTION
  provides a compatible comparison key whose columns exist in that leg's output. Guards
  the multi-intersection aggregate path (`RecordQueryMultiIntersectionOnValuesPlan`).
- [ ] **`COVERING ⇒ index contains every referenced field.**` Catches COUNT-COL. Note:
  only as good as the "referenced fields" analysis (COUNT(*)/COUNT(const) reference zero
  fields → covering OK) — the same analysis whose absence caused the bug, so implement it,
  don't assume it.
- [ ] **Result-type / schema consistency** — declared result type == what children +
  projection produce (a `join_projection_coltype` regression already exists).
- **NOT a structural invariant (do not add as one):** "DistinctRecords==true ⇒ has a
  Distinct node" is **unsound** — distinctness legitimately arises with no dedup operator
  (unique-index, PK, aggregate-index, streaming-agg, intersection; Java's own
  `DistinctRecordsProperty` returns true for all of these). The only non-circular form is
  a **runtime no-duplicate-rows** assertion (test builds) — which is WS-1's row diff — or
  it's subsumed by WS-3 making the property un-miscategorizable. Likewise
  "ordering-claim ⇒ provided order incl. NULL placement" is a runtime/semantic check
  (observe actual row order), overlapping WS-1, **not** structural — and it depends on the
  NULLS axis (§5) existing first.
- **Effort:** ~half day for nil-child; the rest incremental. **Gate:** Graefe + Torvalds.

### [ ] WS-3 — Single source of truth (kill duplicated/drifting facts)
- [ ] **Port Java's `ExpressionProperty` / `RecordQueryPlanVisitor<T>` visitor** for plan
  properties (distinctness, ordering, stored-record) instead of the central hand-
  maintained `switch` in `plan_properties.go`. Java's visitor already enforces per-type
  exhaustiveness — porting it is the *faithful 1:1*; inventing a Go-only "method on each
  plan type" scheme is itself the anti-pattern this RFC fights. The per-type function must
  be a **pure combinator over `childProps`** (it cannot recurse the memo — a ref has
  multiple members; orchestration stays central). Reconcile wrapper-held state
  (`unique`/`covering` live on `physicalIndexScanWrapper`, not the plan) — migrate onto
  the plan as Java does (`getMatchCandidateMaybe().createsDuplicates()`), or thread it in.
  **Acceptance: a new plan type added without declaring its property fails to COMPILE.**
- [ ] **One shared `isCountStar`** used by planner + executor (COUNT-COL was two copies);
  and one **`comparandReadsField`/group-key matcher** shared by guard + consumer (the AGG
  guard≠consumer drift, already done in RFC-163 via `groupColEqualityIndex` — audit for
  other guard/consumer pairs). This is the cheap first piece — land it early.
- **Effort:** ~1–2 days (it's a wide refactor across every `RecordQuery*` type + dispatch
  sites + three properties, not one switch). **Gate:** Graefe + Torvalds.

### [ ] WS-4 — Property/metamorphic tests for the Go-only paths (no Java oracle exists)
- [ ] **Cost monotonicity** — encode `equality selectivity ≤ range selectivity` (and "a
  more selective predicate never estimates more rows") as an invariant in
  `FuzzCostMonotonicity` (it exists but never encoded this, so COST-SELECTIVITY slipped).
- [ ] **Determinism under cost-tied access paths** — **commit a deterministic seed** that
  hits an equal-cost index tie; acceptance = that seed goes red on the mutation.
  `FuzzPlanner_Determinism` passed only because it never *randomly* hit a tie; relying on
  random fuzzing to re-hit it is hope, not a test.
- [ ] **Map-iteration lint** — ship the **CI grep** banning bare `range someMap` in
  plan-affecting code first (80% value, 5% cost); defer the nogo analyzer (gold-plating).
- **Effort:** ~1–2 days. **Gate:** Graefe (cost/determinism) + Torvalds (lint).

### [ ] WS-5 — Audit & enumerate the Go-only divergences (process)
Enumerate each place Go left the Java architecture — the simplified `RequestedSortOrder`
enum, the scalar cost fallback, the hand-rolled `AggregateDataAccessRule`, the per-wrapper
relink — in `DIVERGENCES.md` with the question **"what invariant does the Java carry that
this drops?"** **Acceptance:** the known reservoirs are written down, each tagged "covered
by WS-2/4 invariant" or "tracked TODO". This checkbox means *known reservoirs documented*,
**not** "all divergences found" (un-completable by nature). **Gate:** Graefe.

---

## 5. The 3 open hunt bugs — sequencing

- **NULLS-ORDER** is a live wrong-*rows* bug → **fix it DIRECTLY and first**: extend
  `RequestedSortOrder` with the NULLS axis (port Java `OrderingPart.RequestedSortOrder`:
  `ASCENDING=ASC_NULLS_FIRST`, `DESCENDING=DESC_NULLS_LAST`, `ASCENDING_NULLS_LAST`,
  `DESCENDING_NULLS_FIRST`, `ANY`) and thread NULL placement through `Ordering.Satisfies`.
  This is a concrete ordering-model fix — *enumerated* by WS-5 and *pinned* by WS-2's
  ordering check + WS-1's order-sensitive diff, but **not** "fixed through" an audit.
- **COST-SELECTIVITY** and **NONDETERMINISM** are perf/stability (same rows) → ride WS-4
  (pin-then-fix): land the monotonicity invariant / the deterministic-tie seed, then flip
  the constants / make candidate iteration deterministic.

Closing the open bugs *with* the nets that would have caught them is the test that the
workstream works.
