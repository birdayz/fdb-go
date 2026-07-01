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
- [x] **No `<nil>` child in the FINAL extracted plan.** LANDED: `ValidatePlanInvariants`
  walks the materialized plan tree (`physPlan`), flagging any non-leaf node with zero
  children — a nil inner that `GetChildren()` masks as childless (the IN-LIMIT shape).
  It walks the *plan* tree, not the expression tree, because the malformed node is an
  eagerly-embedded plan snapshot with no live expression member. Genuine leaves (the 10
  scan-/value-producing plan types) are exempted via a compile-time type set (WS-3's
  visitor would make this exhaustive). Two detectors: empty-children (catches a unary
  inner-drop AND a zero-leg n-ary set op — the n-ary analog, a true positive since the
  planner never emits a legitimate 0-leg set op) + nil-in-slice (fixed-arity
  join/recursive drop). Empirically confirmed: removing nothing, the full embedded +
  sqldriver + fuzz suites stay green, so no legitimate childless non-leaf exists. Wired ALWAYS-ON into the
  `PlanQueryForTest` family AND the PRODUCTION generator (SELECT / scalar-subquery / DML
  extraction in cascades_generator.go) — so a dropped child fails loudly in production,
  not only tests; runs clean across the entire embedded + full sqldriver corpus with ZERO
  skip-lists; mutation-proven (revert the IN-LIMIT relink fix → `non-leaf plan
  *RecordQueryFetchFromPartialRecordPlan has no children ... Fetch(<nil>)`); pinned by
  `TestValidatePlanInvariants_NilInnerChild` + `TestPlanInvariants_ChildlessClassification`
  + `FuzzPlanner_Invariants` (1M+ execs, 0 failures). Catches every future per-wrapper
  relink bug across all ~20 wrappers at once.
- [ ] **`WithChildren(GetQuantifiers())` round-trip identity.** Re-linking a node with its
  own quantifiers must reproduce the node — by **semantic** equality
  (`EqualsWithoutChildren` + same children), NOT pointer identity, so a node that
  legitimately re-derives its result type doesn't false-positive. The most *direct* catch
  for the whole relink class (more so than the nil check — it also catches a relink that
  swaps in a mismatched-alias child).
- [ ] **Correlation / quantifier-binding completeness.** Every `CorrelationIdentifier`
  referenced in the final plan is bound by an enclosing quantifier (no dangling
  correlation). A first-order Cascades invariant; catches relink/translation bugs
  generically. **See the FINDING below — the direct formulation is BLOCKED on the
  same physical-layer fidelity gap as the field-level invariants.**
- **FINDING — correlation-completeness on the EXTRACTED plan is not viable as an
  always-on backstop; Go physical plans over-report free correlations.** Implemented and
  probed the cleanest false-positive-proof formulation: `F_out ⊆ F_in` — planning must not
  WIDEN the external free-correlation set (a legitimate `__const`/outer correlation sits in
  BOTH input and output so it's never flagged; only a DROPPED binding widens the set). Wired
  as a chokepoint in `Planner.Plan` (F_in captured cache-free from the pristine input ref so
  it can't prime a stale `correlatedToCache`; F_out from `bestExpr`). Result: PASSES a 20k
  random-plan fuzz corpus (3,987 with non-empty input correlations, zero widening) BUT
  FALSE-POSITIVES across essentially every real join / IN-list / partition query — e.g.
  `SELECT A.id FROM A LEFT JOIN B ON B.a_id = A.id` yields a physical plan whose correctly-
  computed external correlation is `{A}` over a CLOSED input `{}`. Root cause: the physical
  extraction references internally-bound aliases at a level where they aren't quantifier-bound
  — a `Map`/`Fetch` projecting `A.id` sits ABOVE the `FlatMap`/`NLJ` that binds A as its outer
  leg (the NLJ wrapper itself correctly subtracts both legs; A leaks from the projection wrapper
  over it), plus `RecordQueryInJoinPlan`'s `bindingName` and partition-merge source aliases (the
  `$m"…`/leg aliases in the failures) that are bound by EXECUTION-TIME mechanisms (per-row outer
  binding, IN-list binding, merge re-exposure) the correlation walk doesn't model as binding.
  The fuzz corpus missed this because `buildFuzzExpression` uses constant/self-flowed predicates,
  never correlated joins that implement to these operators. Two nuances proven along the way:
  (a) `Reference.GetCorrelatedTo` adds `GetCorrelatedToWithoutChildren` RAW (no own-alias
  subtraction), unlike the correct `expressionCorrelatedTo` — so it reports own-bound aliases as
  free; the correct own-subtracting computation is required or the check is confounded. (b) Even
  with the correct computation the physical over-report remains.
- **RESOLVED against Java (4.12.11.0) — Go's non-closed join plans ARE a real structural
  divergence; the invariant is viable once Go matches Java.** Java's base algorithm
  (`AbstractRelationalExpressionWithChildren.computeCorrelatedTo`, lines 57-77) subtracts own
  quantifier aliases from own-node correlations UNCONDITIONALLY and from children iff
  `canCorrelate()` — identical to the correct Go computation. Java guarantees a fully-planned
  top-level plan is STRICTLY closed (`getCorrelatedTo() == ∅`, *no* `__const` exception:
  `ConstantObjectValue.getCorrelatedToWithoutChildren()` returns `Set.of()`). It achieves this
  for a projected join by FOLDING the SELECT-list projection into the join plan's own result
  value: `ImplementNestedLoopJoinRule` yields `new RecordQueryFlatMapPlan(outer, inner,
  selectExpression.getResultValue(), …)` (line 187) — the FlatMap OWNS `A.id` and `A` is its own
  outer quantifier, so it's subtracted → closed. NO separate Project sits above a join. When Java
  emits a standalone Map (single-quantifier selects only), it REBASES the result value onto the
  Map's own inner quantifier (`ImplementSimpleSelectRule.java:178`
  `resultValue.rebase(AliasMap.ofAliases(alias, beforeMapQuantifier.getAlias()))`), so a Map's
  value can never reference a buried grandchild alias. Go instead produces
  `Project([A.ID], FlatMap(outer=Scan(A), …))` where the Project's result value still references
  the BURIED outer alias `A` (Go keeps the SELECT list as a separate LogicalProjection over the
  join's Select AND does not rebase the Project's value onto its own inner) → non-closed. Go's
  own `NewRecordQueryFlatMapPlan` already carries a `resultValue` and the Go join rule already
  passes `sel.GetResultValue()` to it, so the folding machinery exists. TWO Java-faithful fixes
  (a design choice for the fix RFC): (a) FOLD the projection into the FlatMap result value and
  drop the separate Project — Java's primary path, fully closes the plan AND removes the shape
  divergence, but re-shapes every `Project(…, FlatMap(…))` EXPLAIN assertion (rfc152/rfc153 et al);
  (b) SURGICAL — keep the Project but rebase its result value onto the Project's own inner
  quantifier (Java's `ImplementSimpleSelectRule` pattern), closing the correlation WITHOUT changing
  plan shape (leaves the extra-Project shape divergence, closes the correlation gap). Either is an
  RFC-gated, Graefe-reviewed query-engine change; once Go's plans are closed, this `F_out ⊆ F_in`
  invariant (or the stricter Java "final plan is strictly closed") becomes viable+clean and ships
  alongside the fix. Do NOT add a plan-tree version that breaks the corpus before the fix lands.
- [ ] **Set-op comparison-key correctness.** Every leg of an ordered UNION/INTERSECTION
  provides a compatible comparison key whose columns exist in that leg's output. Guards
  the multi-intersection aggregate path (`RecordQueryMultiIntersectionOnValuesPlan`).
- [ ] **`COVERING ⇒ index contains every referenced field.**` Catches COUNT-COL. Note:
  only as good as the "referenced fields" analysis (COUNT(*)/COUNT(const) reference zero
  fields → covering OK) — the same analysis whose absence caused the bug, so implement it,
  don't assume it.
- [ ] **Result-type / schema consistency** — declared result type == what children +
  projection produce (a `join_projection_coltype` regression already exists).
- **FINDING — the three field-level invariants above (set-op comparison-key columns,
  COVERING⇒referenced-fields, result-type consistency) are NOT checkable on the FINAL
  extracted plan tree as written.** Probed empirically: the plan nodes that would be the
  set-op LEGS — index-scan / value-producing plans — carry `*values.PrimitiveType` /
  `UnknownType`, not a `*RecordType` with named fields (a `RecordQueryIndexPlan` for
  `SELECT id, x … WHERE x = 5` flows `*PrimitiveType`), and `RecordQueryPlan` exposes no
  `WithChildren`. Field-level typing is INCONSISTENT across the tree: some nodes DO carry a
  `*RecordType` (e.g. the field-name recovery at `executor.go:1660` handles exactly that, with
  a scan-metadata fallback for the untyped case) — but the leg types a set-op comparison-key
  check would need to resolve are the untyped ones. So a post-extraction "does the leg's output
  contain column X" check has NO reliable teeth (its resolvable-type precondition fails on the
  index-scan legs that matter; it would be a fake-green checkbox). These invariants need a
  PREREQUISITE — either consistent field-level type propagation into physical plans (a WS-3
  `RecordQueryPlanVisitor`-adjacent effort) OR check
  them on the richer EXPRESSION tree (quantifiers + result values carry the types/correlations)
  at/just-before extraction, where `bestExpr` is in hand. The no-`<nil>`-child invariant
  works precisely because child-PRESENCE is the one structural property fully available on the
  untyped plan tree. Re-scope: implement these three at the expression layer, or gate them on
  type-carrying plans; do not add a toothless plan-tree version.
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
- [x] **One shared `isCountStar`** used by planner + executor (COUNT-COL was two copies).
  LANDED: `expressions.IsCountStar(AggregateSpec)` is the single source of truth, consumed by
  the aggregate-index candidate (`aggregate_index_candidate.go`, 2 sites) AND the executor's
  group cursors (`streaming_cursors.go`). The executor was the OUTLIER — its prior local copy
  narrowly treated only a nil-VALUED constant operand as count-star, disagreeing with the
  planner (and the translator's documented "a constant operand folds into count-star",
  `cascades_translator.go`) on COUNT(1)/COUNT(TRUE). The shared rule (COUNT with no operand OR
  a constant operand) aligns them; result-preserving (a non-null constant counts every row via
  either the count-star total or the per-operand non-null count — full aggregate corpus green),
  and pinned by `TestIsCountStar` (COUNT(*), COUNT(1), COUNT(NULL), COUNT(col) classification).
- [ ] One **`comparandReadsField`/group-key matcher** shared by guard + consumer (the AGG
  guard≠consumer drift, already done in RFC-163 via `groupColEqualityIndex` — audit for
  other guard/consumer pairs).
- **FOLLOW-UP (surfaced by the isCountStar dedup review, Graefe) — `COUNT(NULL)` folds to
  all-rows, a possible port-fidelity bug.** The translator's "a constant operand folds into
  count-star" doctrine classifies `COUNT(NULL)` (a resolved `ConstantValue{Value:nil}` operand)
  as count-star → the group's TOTAL row count. Standard SQL (and Postgres/MySQL) return
  `COUNT(NULL) = 0` (it counts non-NULL values of a NULL expression). This is INVARIANT under
  the dedup (old and new classifiers agree on `ConstantValue{nil}`), so out of scope there, but
  it is a live port-fidelity question: verify Java's behavior (does it fold the NULL *literal*
  to `COUNT(*)`, or return 0?) — Java is the spec. If Java returns 0, `IsCountStar` must exclude
  a nil-valued constant (`Operand` is `*ConstantValue` with `Value != nil`), and the translator's
  fold must not apply to the NULL literal. Pin with a `COUNT(NULL)` rows test once the Java
  oracle is confirmed. (WS-1's Go-vs-Java differential would catch this class automatically.)
- **Effort:** ~1–2 days (it's a wide refactor across every `RecordQuery*` type + dispatch
  sites + three properties, not one switch). **Gate:** Graefe + Torvalds.

### [ ] WS-4 — Property/metamorphic tests for the Go-only paths (no Java oracle exists)
- [x] **Cost monotonicity** — encode `equality selectivity ≤ range selectivity` (and "a
  more selective predicate never estimates more rows"). LANDED as `TestBoundSelectivity_
  CostMonotonicity`, a focused property test on `boundSelectivity` — the SINGLE-SOURCE
  equality-vs-range costing function (shared by both HintCost sites + scanLikeCost) where
  COST-SELECTIVITY (#405) actually lived. It pins (1) the constant ordering
  `EqualityBoundSelectivity < RangeSelectivity < FilterSelectivity` and (2) `boundSelectivity`
  monotonicity: an equality out-selects a range, adding ANY bound only lowers selectivity, and
  empty/nil bounds are inert. Chosen over encoding it inside `FuzzCostMonotonicity` because that
  fuzz checks a DIFFERENT property (the optimiser's best cost never GROWS across iterations, not
  the selectivity ordering) — a direct property test on the exact function the bug inverted is a
  stronger, clearer pin than a fuzz assertion over random plans. LAYERED protection (Graefe): it
  pins the in-`boundSelectivity` invariant; the actual index MIS-PICK #405 caused is guarded at
  the plan level by `TestCostSelectivity_PrefersEqualityIndex`. FOLLOW-UP: `scanLikeCost`'s
  `fullBindUnique` 1-row short-circuit (the low-NDV secondary-index hazard — a whole-index
  equality bind that selects a large bucket must NOT be costed as a 1-row point probe) is the one
  COST-SELECTIVITY-adjacent path still unpinned.
- [~] **Determinism under cost-tied access paths** — **commit a deterministic seed** that
  hits an equal-cost index tie; acceptance = that seed goes red on the mutation.
  `FuzzPlanner_Determinism` passed only because it never *randomly* hit a tie; relying on
  random fuzzing to re-hit it is hope, not a test. DONE for the EQUALITY tie:
  `TestPlanDeterminism_EqualCostIndexTie` + `_MultiEqualityShellTie` + `_CrossProcess`
  (committed deterministic seeds, cross-process mutation-proven, #409). FINDING while probing
  the IN-list tie: `a IN (1,2,3)` over two identical indexes is NOT plan-deterministic, from
  TWO sources — (#1) the `RecordQueryInJoinPlan` binding correlation alias (a process-global
  `UniqueCorrelationIdentifier` counter) was folded into Explain + Equals + Hash, so every
  replan produced a non-equal / differently-hashed plan → plan-cache churn — **FIXED** for BOTH
  the InJoin AND the InUnion (sorted/merge IN) path (`RecordQueryInUnionPlan` had the identical
  churn, Graefe follow-up): identity is now alias-invariant (only the binding COUNT is
  structural); real aliases retained for execution; pinned by
  `TestRecordQuery{InJoin,InUnion}Plan_BindingAliasInvariant`; and (#2 — **OPEN**) the InJoin's INNER
  index-scan selection is itself a cost-tie (idx1↔idx2) that the Phase-1a `exprConcreteHash`
  tie-break resolves for the plain `a = 5` equality case but NOT through the InJoin inner path
  (~27/200 runs flip). Repro: `SELECT id FROM t WHERE a IN (1,2,3)` over two identical indexes
  on `a`. Fix direction: extend the deterministic winner tie-break to the InJoin inner selection
  (`physical_in_join_wrapper.go` / the InJoin implement rule) — same class as RFC-167 Phase 1b
  comprehensive tie-resolution; the full IN-list Explain-stability seed is re-added once it lands.
  PRE-EXISTING LOOSENESS (flagged by Graefe + Torvalds, NOT introduced here, orthogonal): neither
  InJoin nor InUnion folds `inValues` into identity, the inner index scan compares only its
  `RangeType`, and InUnion additionally omits `comparisonKeys` from Equals/Hash — so `a IN (1,2,3)`
  and `a IN (4,5,6)` over the same index are Equals/Hash-equal, and same-count InUnions differing
  only in merge-key (`ORDER BY x` vs `ORDER BY y`) collide too. Harmless for replan-determinism
  (same query → same values/keys), but a latent WRONG-CACHE-HIT if literal IN-lists reach a
  `PlanHash`-keyed plan cache unparameterized — confirm IN-lists are parameterized
  (ConstantObjectValue) before the cache, or fold `inValues` + `comparisonKeys` into identity.
- [ ] **FOLLOW-UP pin (Graefe + Torvalds, deferred from the InJoin/InUnion alias fix):** a focused
  memo-dedup safety pin — real SQL `a IN (…) AND b IN (…)` → extracted plan → assert ALL binding
  aliases are bound (no double-bind) AND `PlanHash` is stable across replans. This pins the most
  subtle load-bearing invariant of the alias-invariance change (the memo only dedups complete,
  internally-consistent InJoin chains; the broken double-bind shape is never constructed).
- [ ] **Map-iteration lint** — ship the **CI grep** banning bare `range someMap` in
  plan-affecting code first (80% value, 5% cost); defer the nogo analyzer (gold-plating).
- **Effort:** ~1–2 days. **Gate:** Graefe (cost/determinism) + Torvalds (lint).

### [x] WS-5 — Audit & enumerate the Go-only divergences (process)
Enumerate each place Go left the Java architecture — the simplified `RequestedSortOrder`
enum, the scalar cost fallback, the hand-rolled `AggregateDataAccessRule`, the per-wrapper
relink — in `DIVERGENCES.md` with the question **"what invariant does the Java carry that
this drops?"** **Acceptance:** the known reservoirs are written down, each tagged "covered
by WS-2/4 invariant" or "tracked TODO". This checkbox means *known reservoirs documented*,
**not** "all divergences found" (un-completable by nature). **Gate:** Graefe.
DONE: `DIVERGENCES.md` "RFC-164 WS-5 — Go-only divergence reservoir audit" — a table of the
six reservoirs (simplified `RequestedSortOrder`; scalar cost fallback + Go-only tiebreakers;
hand-rolled `AggregateDataAccessRule`; per-wrapper relink / RFC-070 shells; Go-only physical-
filter builders; `WithPrimaryKeyIntersector` discards `requestedOrderings`), each with the Java
invariant it drops, the risk class (wrong-rows / nondeterminism / panic), and a COVERED (naming
the WS-2/4 invariant + pin) or TRACKED tag.
These are the concrete reservoirs this RFC's own §1 flavor table + the DIVERGENCES.md
"Intentional Architectural Decisions" section already surface; each is now tied to its
structural net or a tracked follow-up. New reservoirs get the same treatment (method noted).

---

## 5. The 3 open hunt bugs — sequencing

- **[x] NULLS-ORDER — FIXED.** Restored the NULLS axis to `RequestedSortOrder`
  (`AscendingNullsLast`, `DescendingNullsFirst` added; the existing `Ascending`/`Descending`
  are the natural placements — Java `OrderingPart.RequestedSortOrder`), populated it from the
  SQL `SortKey.NullsFirst`, and made the satisfaction path null-aware:
  `IsCompatibleWithRequestedSortOrder` and the data-access `SatisfiesRequestedOrdering` now
  require NULL placement to match, and the direction-reading sites use `IsAscending()`/
  `IsDescending()`. An explicit non-natural `ORDER BY x NULLS LAST/FIRST` now RETAINS the
  InMemorySort instead of being elided against an opposite-null-placement index. Surgical:
  natural placements still elide. **Committed regressions:** `TestNullsOrder_ExplicitPlacementRetainsSort`
  (plan: single- AND multi-key non-natural placements retain; natural placements elide) +
  `TestFDB_OrderByNullsLast` (rows for BOTH non-natural directions — ASC NULLS LAST → NULL
  last, DESC NULLS FIRST → NULL first — plus a multi-key case). Full embedded + sqldriver
  green. Additionally, an *ad-hoc adversarial review sweep* (ephemeral agents over multi-key,
  IN-join, set-ops, GROUP BY, reverse-scan, over-fix, and a completeness audit of every
  RequestedSortOrder branch site) surfaced no defect — that was a review activity, not
  committed regressions; the committed pins above are what guard the fix.
- **[x] COST-SELECTIVITY — FIXED.** The inverted equality/range selectivity is corrected
  via a distinct `EqualityBoundSelectivity=0.1` (< `RangeSelectivity=0.33`, kept separate
  from the generic residual `FilterSelectivity=0.5`) at the 3 scan-cost sites. Pinned by a
  constant-invariant sentinel (`TestCostSelectivity_EqualityBeatsRange`) + a plan proof
  (`TestCostSelectivity_PrefersEqualityIndex`: master picks the wrong range index, the fix
  picks the equality index). Validated by 1M stress before/after and `FuzzCostMonotonicity`
  (1.3M execs). The general cost-monotonicity net (`FuzzCostMonotonicity`) already existed;
  the missing dimension was the equality<range *ordering* invariant, now pinned. The three
  scan-cost sites were de-duplicated into one `boundSelectivity` helper (the copy-paste was
  how a dead inverted twin, `IndexColumnSelectivity`, survived the original fix — now deleted).
  **Open follow-ups (Graefe, deferred — not blocking the fix, but tracked so they don't evaporate):**
  - **Covering-vs-non-covering crossover pin.** Dropping equality to 0.1 shifts the boundary
    where a non-covering equality index (0.1 leaf + Fetch I/O) beats a covering range index
    (0.33 leaf, no Fetch). Outcome depends on `FetchCPU` magnitudes; add a targeted plan pin
    once the crossover is characterised (a fragile hand-built one now would be worse).
  - **Low-NDV equality.** Statless, `EqualityBoundSelectivity=0.1` assumes a high-cardinality
    point (NDV≈10); a low-NDV equality (`status = ?`, a boolean) retains far more and is
    under-costed. Fixing it correctly needs per-column NDV statistics (not yet available);
    documented at `boundSelectivity`.
- **NONDETERMINISM** is perf/stability (same rows) → ride WS-4 (pin-then-fix): land the
  deterministic-tie seed, then make candidate iteration deterministic.

Closing the open bugs *with* the nets that would have caught them is the test that the
workstream works.
