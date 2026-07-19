# RFC-183 — Rules yield fully-linked plans: delete the nil-inner shell architecture

**Status:** ACK-WITH-CHANGES from Graefe and Torvalds (2026-07-19); changes
folded. **P0 and P1 are implemented and landed** (76d6d06a6) — see §9 for
what P0 actually reported, including a wrong-rows bug it surfaced on
master. P2/P3 follow.
**Tracks:** RFC-167's deeper layer (the nil-inner-shell finding), promoted to its own design at the owner's request after RFC-182 P2 patched a fourth repair site.
**Relates to:** RFC-070 (deferred child linkage — the origin), RFC-076 (`findBestPhysicalPlan`), RFC-182 (the harness that keeps surfacing this class).

## 1. Problem

Go's Cascades has a state Java cannot represent: a physical plan node whose
child is **nil**, to be filled in later by a `WithChildren` relink at
extraction. These are "shells", and they are the root cause of a recurring
family of defects. RFC-182's grammar extension alone surfaced four:

| Symptom | Repair that shipped |
|---|---|
| `PredicatesFilter(<nil>)`; before compensation existed, the same queries silently **dropped a residual** (wrong rows) | set ops added to `isLeafReplaceable` |
| `InUnion(PredicatesFilter(<nil>))` | a relink added to a wrapper that had none |
| nil-inner shells selected as valid plans | shell guard generalized from Fetch-only |
| `InJoin(<nil>)` on nested `IN`s | `shouldRelinkInner` + recursive `completeShellPlan` |

Each fix is local and correct; none removes the category. The pattern is
diagnostic: **we are repairing plans after the fact instead of never
building broken ones.**

## 2. Java: the property we lack, and why it holds

Java has no shell window. Verified against 4.12.11.0 — **all citations in
this section were independently re-checked during the RFC gate and stand
without correction.**

1. **A plan IS an expression, and its child is a Quantifier.**
   `RecordQueryFetchFromPartialRecordPlan.java:84` holds
   `private final Quantifier.Physical inner`; `:142` `getChild()` returns
   `inner.getRangesOverPlan()`. `Quantifier.java:467` declares
   `@Nonnull private final Reference rangesOver`, assigned in a private
   constructor; the only factories (`:611-622`) all require a `Reference`.
   There is no `setRangesOver` anywhere in `plan/`. A child is a
   *dereference through a non-null final reference*, not a stored pointer
   that can be nil.
2. **Rules memoize the child BEFORE constructing the parent.**
   `PushFilterThroughFetchRule.java:197-225`:
   `Quantifier.physical(call.memoizePlan(innerPlan))` is evaluated as a
   constructor *argument*. No window exists in which the parent lacks its
   child.
3. **Every yield asserts it.** `CascadesRuleCall.java:221-241`
   `verifyChildrenMemoized` runs an unconditional `Verify` (not a debug
   check) that each quantifier's reference is already in the traversal.
4. **Copy-on-write everywhere else.** `withChild(Reference)` returns a new
   node (`RecordQueryPredicatesFilterPlan.java:161-164`); the only mutable
   state is the member set *inside* a group, never the parent→child edge.

So Java's guarantee is structural, then asserted, then preserved. Go has
none of the three.

## 3. What the shells cost us today

The gate review established that the draft's original "~600 lines" figure
conflated **three separate machines**. Measured total is 759 lines, but only
the first block below is shell-specific. This split is load-bearing for the
plan: it is the difference between deletion and triage.

### 3a. Shell-specific (~380 lines) — deletable once shells cannot exist

| Construct | Location | Callers |
|---|---|---|
| `isNilInnerShell` / `isNilInnerFetch` | `physical_fetch_from_partial_record_wrapper.go:175,192` | 3 / 5 |
| `completeShellPlan` / `resolveInnerPlan` / `planWithInner` | `physical_wrapper.go:332,361,389` | 1 / 2 / 1 |
| `shouldRelinkInner` / `planIsShell` / `isOrderDestroying` | `physical_wrapper.go:472,502,513` | 2 / 1 / 1 |
| template-aware costing & hashing (`planNodeIsStub`, `exprConcreteHash`, …) | `planning_cost_model.go` | ~250 lines |
| the winner-clearing exemption for shells | `unified_tasks.go:503-521` | — |

### 3b. General infrastructure — survives, and must not be swept up in P2

- `findPhysicalPlan` (`physical_wrapper.go:313`) — **20 callers.** The
  general Reference→plan primitive every wrapper needs regardless of shells.
- `isLeafReplaceable` (`physical_wrapper.go:557`) — **13 callers.**
  Substitution-safety, an independent concern.

### 3c. Ordering-pin family (~197 lines) — NOT shell machinery

`pinOrderedSpine` (`winner_lookup.go:90`, 5 callers), `planHasDirectChild` /
`planEmbedsDirectChild`, `rebuildOrderedSpine`. **All five callers are
implement rules** (`rule_implement_distinct_union.go:230`,
`rule_implement_sort.go:113,132`, `rule_implement_in_union.go:313`,
`rule_implement_streaming_agg.go:172`); none is on the shell path. This is
sort-elision correctness. The draft's P4 rationale — "they exist only
because a wrapper's quantifier and its embedded plan can disagree" — is not
what these functions do. **P4 is re-scoped accordingly (see §6).**

### Relink surface

**18 relink sites: 11 wrapper files + 7 inline in `physical_wrapper.go`**
(plus 2 in implement rules). The draft's "16 sites plus 5 inline" was a grep
artifact. Separately there are **35 `WithChildren` implementations** across
23 files, but most are leaf wrappers with no child to relink — that is the
structural-rebuild surface, not the shell surface. Actual `.WithChildren(`
*call* sites number 6, of which 4 are `values.WithChildren` (value-tree
rebuilds, unrelated) and 1 is a test; the sole genuine relink call is
`winner_lookup.go:126`.

## 4. Only 5 rules create shells — corrected inventory

Verified by direct scan, superseding the draft's list (which was off by one
throughout and included a rule that does not qualify):

| Rule | nil-passing sites |
|---|---|
| `rule_push_filter_through_fetch.go` | 91, 102, 121 |
| `rule_push_map_through_fetch.go` | 94 |
| `rule_push_distinct_through_fetch.go` | 66, 77 |
| `rule_push_distinct_below_filter.go` | 68, 86 |
| `rule_push_in_join_through_fetch.go` | 81, 101 |

**5 rules, 10 sites.** All are push-through rules; all have a Java
counterpart that memoizes the child first.

`rule_push_set_operation_through_fetch.go` is **not** a shell source and is
removed from the list — see §5.

The data-access path is also not a shell source:
`abstract_data_access_rule.go:476-486` already builds a fully-linked fetch.
The cost model's claim that "the data-access path builds chains of these
shells" (`planning_cost_model.go:1227`) is stale and should be corrected
regardless of this RFC's outcome.

## 5. Evidence: the experiment has already been run, on the hardest case

This section replaces the draft's framing of P0 as exploratory, and
dissolves the draft's unknown #4.

`rule_push_set_operation_through_fetch.go` **already does what P0 proposes.**
It passes a concrete `newSetOpPlan` from `rebuildPlan(innerPlans)` (`:370`);
every leg carries a non-nil `innerPlan` (`:246-257`); each child quantifier
is built as `FinalOf(leg.innerExpr)` over the exact expression whose plan is
baked (`:368`). Its own comment (`:354-363`) records *why*: passing the
stale plan was the bug, and baking the concrete plan was the fix — "a
cost-model lie."

This matters disproportionately because set-op is the **N-ary** case, and
`completeShellPlan` refuses non-unary outright (`physical_wrapper.go:348-350`).
There was no recovery path, which is likely why it was converted first. So
the conversion has already landed clean on the hardest shape — direct
evidence for Outcome A that the draft did not know it had.

It also makes the `physical_fetch_from_partial_record_wrapper.go:156-174`
comment stale on its own terms: it names `PushSetOperationThroughFetchRule`
as a shell creator, and that rule no longer creates shells. Two comments in
one package now assert opposite conclusions.

### Why baking is provably behavior-neutral at these sites

The draft feared, and the gate initially flagged, that baking at rule time
would freeze the child choice and re-create RFC-076's "first physical
member" bug. It does not. The rules collapse the child to a singleton one
line after selecting it:
`MemoizeFinalExpressionsFromOther(fetchInnerRef, []{fetchInnerExpr})`
(`rule_push_filter_through_fetch.go:93`) builds a **brand-new Reference
holding only that expression** (`implementation_rule.go:106-125`,
`ref = expressions.InitialOf(e)`) — it does not alias the original group. At
extraction, `findPhysicalPlan` resolves a singleton and can only return that
same plan. **The freeze already exists**, expressed via the quantifier
instead of via the plan. Baking makes it explicit; it does not narrow the
search space.

### The real risk the draft misidentified

The draft blamed a comment claiming staleness, and called that folklore. The
comment is quoted accurately but the mechanism is different, and worse:
`findPhysicalExpr` (`physical_wrapper.go:435-453`) prefers a non-shell
member but **falls back to returning a shell** (`:452`) when every physical
member is a nil-inner fetch. So "the rules already hold the concrete child"
is false in the fallback case — a naive P0 would bake shell-of-shell. This
is the actual constraint, and it drives P0's design in §6.

## 6. Plan

**P0 — confirmatory, with a real exit gate.** Convert the 10 sites in the 5
rules to pass the concrete child; leave quantifiers untouched. Three
requirements the draft lacked:

- **Bake bottom-up in one pass.** For the three stacked shells
  (`filter:102`, `distinct_through_fetch:77`, `in_join:101`) the child is
  itself a shell at that instant. Follow the set-op rule's shape.
- **`rule_push_map_through_fetch.go` must bake the post-covering-rewrite
  plan** (`:81-90`), not the pre-rewrite one.
- **Build the EXPLAIN-differ first** (see below). It is a P0 deliverable,
  not a P2 proof obligation.

Exit criteria are **EXPLAIN-diff over the yamsql corpus + 1M stress**, not a
green suite. Unknown #2 (cost/hash drift flipping winners) means a
flipped-but-still-correct plan passes every existing test while silently
regressing plan quality; the suite cannot see it. Three outcomes:
*A:* clean → P1-P3 proceed as deletion.
*B:* a shape regresses → concrete reproducer; P5 becomes mandatory.
*C:* the shell-fallback of `findPhysicalExpr` fires → P0 blocks on the
selector fix (WS-S) landing first.
**No further phase starts before P0 reports.**

**P0 also delivers: the EXPLAIN-differ.** It does not exist today.
`plan_contains` (`yamsql/yamsql.go:83`) is a per-case substring assert;
nothing diffs EXPLAIN across a corpus; rowdiff compares rows. Given unknown
\#2 this is the load-bearing check for the entire RFC and it was unbudgeted.

**WS-S (new, independent) — first-vs-cheapest selection.** Surfaced by the
gate as a **live correctness bug that predates and outlives this RFC**:
`findBestPhysicalPlan` (cheapest) is wired to **1** site
(`physical_in_memory_sort_wrapper.go:78`) while `findPhysicalPlan`
(first-member) serves **20**. `findPhysicalExpr` additionally scans
`AllMembers()`, which by its own sibling's doc "interleaves exploratory and
final members in yield order" (`physical_wrapper.go:411-414`); Java
enumerates **final** expressions only (`RecordQueryPlanMatchers.java:115`).
Fix the member set and the selector. This is tracked separately so P2's
deletions cannot bury it.

**P1 — install Java's invariant.** Make `MemoizeFinalExpression` actually
register into the memo/traversal (today it returns a bare `InitialOf` —
`implementation_rule.go:129-133`), then port `verifyChildrenMemoized` into
`Yield`, plus rejection of any yielded shell. Add
`Verify(len(finalMembers)==1)` to `AdvancePlannerStage` and a
`GetOnlyElementAsPlan`, matching `Reference.java:208-212,236-239`.
**`verifyChildrenMemoized` is always-on**, matching Java's unconditional
`Verify` (`CascadesRuleCall.java:239`); `ValidatePlanInvariants`
(DIVERGENCES.md:430) is the existing always-on precedent. This resolves the
draft's unknown #5.

**P2 — delete the shell-specific machinery only.** Per §3a:
`shouldRelinkInner`, `planIsShell`, `isOrderDestroying`, `completeShellPlan`,
`resolveInnerPlan`, `planWithInner`, `isNilInnerShell`, `isNilInnerFetch`.
**`findPhysicalPlan` (20 callers) and `isLeafReplaceable` (13 callers)
survive** — they are general infrastructure per §3b. The 18 relink sites
become pure structural rebuilds; `WithChildren` takes Java's
`withChild(Reference)` shape.

**P3 — delete template-aware costing/hashing** and the winner-clearing
exemption. They exist only to cost and hash a tree with holes.

**P4 — DROPPED as written.** Its stated rationale does not describe what the
ordering-pin family does (§3c): five callers, all implement rules, zero on
the shell path. The pins arbitrate the fact that the parent→child edge is
stored *twice*; they become deletable only when that duplication is removed
— i.e. at P5, not before. Any pin cleanup is folded into P5.

**P5 STATUS: the premise below is FALSE for a measured subset. Read §11
before acting on any of it.**

**P5 — TERMINAL, and its real blocker is NOT size.** Make
`plans.RecordQueryPlan` a `RelationalExpression` holding quantifiers,
deleting the `physical_*_wrapper.go` layer. The edge is stored twice today —
quantifier `Reference` *and* embedded plan pointer — and a shell is precisely
the state where the two disagree. P1-P4 make them agree *at construction*;
nothing structurally prevents later divergence. **Duplication is the disease;
nil is a symptom.**

This draft said "large; touches the executor." Size is not what blocks it.
Investigated, with two findings.

*The stated rationale for the split is factually wrong about Java.*
`plan.go:23-28` justifies a separate package by claiming "physical and logical
plan trees live in different namespaces in Java." They do not:
`QueryPlan<T> extends PlanHashable, RelationalExpression` (`QueryPlan.java:51`)
and `RecordQueryPlan extends QueryPlan<…>` (`RecordQueryPlan.java:73`) — in
Java a plan **is** a RelationalExpression. The comment conflates *package*
separation (which Java has, and Go correctly mirrors) with *hierarchy*
separation (which Java does not have). The whole wrapper layer, and the shell
class with it, descends from that misreading. Note also there is no import
cycle in the way: `plans` already imports `expressions`, and `expressions`
never imports `plans`.

*~~The actual blocker is one-final-member, and it is quantified.~~*
**RETRACTED — this paragraph was wrong, and it stood at HEAD for several
commits after the evidence against it existed.**

It claimed P5 was gated on universal prune-to-one-final-member, citing
"1186 references hold multiple finals (max 52), 1125 multiple PHYSICAL
finals". That measurement was taken at RULE TIME, while rules were still
firing, where a group legitimately holds alternatives — the whole point of a
memo. It says nothing about the property P5 actually needs, which is
one-final-member AT EXTRACTION, after the stack drains and OptimizeGroup has
pruned. Measured there: ZERO violations across the corpus and the entire
test suite, pinned by `TestOneFinalPlanPerReference`.

So prune-to-1 and PLANNING re-derivation parity are NOT P5 blockers. They
remain open items in their own right (DIVERGENCES.md), but they are not on
this critical path.

The real blocker is in §11, and it is a different thing entirely: the two
storage locations do not hold the same fact.

Kept rather than deleted because the failure mode generalises — a
measurement taken at the wrong point in the pipeline produced a confident,
specific, wrong number that then propagated into the authoritative
divergence list. The number was never the problem; the missing question
"measured WHERE?" was.

**What landed here.** The one link that was genuinely unblocked:
`MemoizeFinalExpression` now lands plans in the FINAL set, via a new
`FinalOfAtStage` that separates member-set placement from planner stage. That
conflation was the whole reason the earlier `FinalOf` attempt broke a
continuation pin and moved 18 shapes — it was the STAGE, not the final-set
placement. Zero drift, suite green, and `findPhysicalExpr`'s finals-first
ordering is now live instead of inert.

**A P5 that does not wait for parity is possible** and should be costed
before anyone attempts the terminal form: let plans hold the quantifier and
have `GetChildren()` resolve through the SAME winner-lookup the wrappers use
today, rather than `getOnlyElement`. That stores the edge ONCE — the actual
property — without needing prune-to-1, and re-frames the remaining divergence
honestly as "resolution policy is winner-lookup", a consequence of the
re-derivation gap rather than of the hierarchy split. Surface: 45 plan types,
~506 non-test `GetChildren()` call sites, plus the executor.

## 7. Riskiest unknowns

1. ~~Is "stale plan references" ever true?~~ **Resolved** (§5): staleness is
   not the mechanism. The real constraint is `findPhysicalExpr`'s
   shell-fallback, now P0 outcome C.
2. **Cost/hash coupling.** Shells collapse to identical hashes today
   (`planning_cost_model.go:1971`); removing them changes hashes and costs
   and can flip winners. This is now the **primary** risk and the reason
   the EXPLAIN-differ is a P0 deliverable.
3. **Winner-selection timing.** Largely resolved (§5): the choice is already
   frozen upstream via `MemoizeFinalExpressionsFromOther`, so baking is
   behavior-neutral. Note the draft's use of `ImplementIntersectionRule` as
   the analog was wrong — Java preserves search space through matcher
   fan-out (`AnyMatcher.java:55-59`, one `CascadesRuleCall` per binding at
   `CascadesPlanner.java:1019-1043`), not partition memoization. That
   fan-out gap is real but belongs to WS-S, not P0.
4. ~~Multi-child wrappers.~~ **Resolved affirmatively** (§5): set-op, the
   only N-ary case, is already converted and clean.
5. ~~Always-on vs debug-only invariants.~~ **Resolved** (P1): always-on,
   matching Java.

## 9. P0 report (implemented)

**Outcome A on the shell question, Outcome B on cost.** Both, and the
second is the valuable half.

**Shells are gone, proven rather than assumed.** `isNilInnerShell` was
instrumented to log every observation, then run across the entire Go test
suite and all 2407 corpus queries: **zero observations in both.** The
machinery in §3a is dead code, so P2 deletes on evidence.

**Baking was behaviour-neutral, as §5 predicted.** No plan changed because
a child got frozen earlier.

**But P0 found a wrong-rows bug on master, exactly as an exit gate should.**
Giving the malformed alternative a real (cheaper) cost made it start
WINNING. `PushFilterThroughFetchRule` pushed predicates beneath a fetch
without checking the index covers the columns they read — with `INDEX idx_k
ON t(k)`, `k = 5 AND (a > 1 OR b < 2)` planned as
`Fetch(PredicatesFilter(IndexScan(IDX_K), [(A > 1 OR B < 2)]))`, evaluating
A and B on an index entry carrying only K and the primary key.

The mechanism is a shortcut in `tryTranslateValueRec`: "not correlated to
the source alias" is read as "foreign value, push through unchanged". After
ordinalization a bare column is a `FieldValue` with `Child == nil` and an
EMPTY correlation set, so it took the shortcut and never met the
covered-column check. The final residual-correlation guard cannot catch it
either — an ordinalized field never carries that correlation. A composite
over such fields (`a + b > 3`) has an empty correlation set of its own and
slipped through the same way; that second instance was found by the new
tests, not by the corpus.

Note what this says about the RFC's own framing. §5 argued the "stale plan
references" comment was folklore and that the real risk was
`findPhysicalExpr`'s shell fallback. The folklore claim held. The predicted
risk never materialised — outcome C did not fire. The bug that did surface
was in neither list: it lived in the *pushability* test, not in the linkage
machinery. P0's value was not confirming its own hypothesis but running a
cost perturbation broad enough to shake out a latent defect no hypothesis
had named.

**Corpus: 31 shape flips over 2407 queries, 0 plan regressions.** Dominant
class is the correctness fix. The rest are improvements the real costs
unlocked: the Fetch eliminated outright on covering scans, InJoin pushed
below the fetch, a two-index Intersection chosen over single-index-plus-
residual.

**Why nothing caught it before.** The bug is in which plans are GENERATED;
the malformed alternative carried a stub cost and usually lost. Every test
asserted rows, or a substring of the WINNING plan. None asserted that an
unsound alternative is never generated. The unprobed dimension was "is the
pushed predicate evaluable on the partial record" — now pinned by
`TestPushFilter_NeverPushesUncoveredColumnBelowFetch` (and its
over-declining twin, since a guard that refused everything would also pass).

**P1 landed the ASSERTION but not the memo REGISTRATION — stated plainly so
it is not read as implied-done.** Java's `verifyChildrenMemoized` asserts memo
membership (`traversal.getRefs().contains(rangesOver)`); Go's asserts only
that the child reference is non-nil and non-empty. Three §6 items did not
land: `MemoizeFinalExpression` still returns a bare `InitialOf`
(`implementation_rule.go:129-133`), and neither
`Verify(len(finalMembers)==1)` in `AdvancePlannerStage` nor
`GetOnlyElementAsPlan` exists. `verifyNoShell` — which Java needs no analogue
of — does cover the actual defect class, so this is a shortfall against the
RFC rather than against safety; with repair deleted, the alternative to a
loud abort is `Op(<nil>)` reaching the executor.

The registration item is NOT a rename. `InitialOf` lands the expression in
the EXPLORATORY set, so every reference the push-through rules mint has an
empty `FinalMembers()` — which also means §10's finals-first ordering is a
no-op at exactly those sites and rides entirely on the exploratory fallback.
Swapping to `FinalOf` (Java's `ofFinalExpressions` shape) was tried: it also
sets `StagePlanned`, and it replans `UNION ALL` from UnorderedUnion to Union
— breaking `TestFDB_UnorderedUnion_Continuation_ResumeAcrossPages`, a
continuation pin — shifts alias-aware interning counts, and moves 18 plan
shapes. It needs its own change with those consequences worked through.
Carried to P5, with the hazard documented at `findPhysicalExpr` so a
finals-only tightening cannot land on top of it by accident.

**One test was pinning the bug.** `TestNestedIn_OverIntersection` demanded
`InJoin(` on a query where that shape was only reachable by pushing A and B
onto an `idx_c` entry carrying neither. It now asserts the sound plan;
`TestInJoinBelowFetch_RelinksRealChild` keeps the relink path covered on a
query where the shape is sound.

## 10. WS-S report: the selector item, and why half of it was wrong

WS-S came out of the gate as "`findBestPhysicalPlan` (cheapest) is wired to 1
site while `findPhysicalPlan` (first-member) serves 20 — a live correctness
bug." It is two claims, and they did not survive equally.

**The member-set half was real and is fixed.** `AllMembers()` concatenates
exploratory members BEFORE final ones, so a first-match scan inspected the
exploratory set first; Java enumerates FINAL expressions only
(`RecordQueryPlanMatchers.java:115`), and FinalizeExpressionsRule promotes the
same pointer into both sets, so an expression really can sit in each.
`findPhysicalExpr`/`findPhysicalPlan` now search finals first, with the
exploratory fallback kept because rules call them mid-planning before a group
is finalized. Zero plan drift across 2407 queries — a latent misordering, not
an active one, but a real divergence closed.

**The selector half is wrong, and was falsified by experiment.** Ranking
members with `PlanningCostModelLess` at rule time ignores the REQUESTED
ORDERING. Wiring it in turned `SELECT a, b FROM ab WHERE a = 1 ORDER BY a DESC`
into an ASCENDING result and moved 79 plan shapes. A rule wants *a valid
child*; which alternative wins belongs to OptimizeGroup under the ordering
constraints, and extraction reads that winner through the ordering-aware
lookup.

So the 1-vs-20 asymmetry is not the bug it appears to be — the anomaly is the
ONE site, not the twenty. `findBestPhysicalPlan` is precisely the "ad-hoc
second optimizer running at extraction outside the cost framework" the gate
itself objected to; the correct direction is to retire it into the memo's
winner lookup, not to propagate it. That is a follow-up, not a P2 deletion:
removing it needs the RFC-076 shape it was introduced for
(`TestFDB_JoinSelPred_Repro`) re-verified against ordering-aware winner
selection. The hazard is now documented at `findPhysicalExpr` so the next
person who spots the asymmetry does not "fix" it the obvious way.

**The follow-up covers TWO sites, not one.**
`rule_implement_nested_loop_join.go:97-105` also selects join children through
`findBestValidPhysicalExpr`, and its original justification — skipping
nil-inner shells — evaporated in this very change. So there are now two
ordering-blind ad-hoc selectors, one of which has outlived its stated reason.
Both retire together.

## 8. Success criteria

Status of each, with the evidence rather than an assertion.

- **MET** — `isNilInnerShell`/`isNilInnerFetch` are deleted outright, along
  with the rest of §3a. (The draft's `grep -c "shell"` trending to zero is
  dropped — a vanity metric that rewards renaming.)
- **MET** — §3b and §3c survive: `findPhysicalPlan` (20 callers),
  `isLeafReplaceable` (13), and the ordering-pin family (5, all implement
  rules) are intact and exercised.
- **MET** — the EXPLAIN-differ exists (2407 queries, ~3s, no FDB) and reports
  2407/2407 identical for every change after P0. P0's own 31 flips are
  enumerated and accounted for in §9.
- **MET** — DIVERGENCES.md's "repair-at-EXTRACTION" section is deleted, not
  amended. The adjacent value-range-intersection row was also re-verified:
  its stated reason for staying latent ("a cheaper shell wins") expired with
  this change, and the corpus was re-checked (intersections 7 → 13, none with
  a range leg) plus a direct probe of `a=5 AND b>10` and variants, which
  always keep the range predicate as a residual.
- **MET, with the baseline caveat that predates this work** — 1M stress is
  green: full scans 3.26-3.98s, point lookups and index equality at 0.01s.
  Note the recorded baseline table is *already* failing on its own base
  (TODO.md:1748-1760: master and branch measured noise-identical on every
  violated row, with all 23 EXPLAINs byte-identical). That is an open item
  with its own decision path; this change is not implicated in it.
- **PARTIAL — stated plainly rather than claimed.** The RFC-182 harness at
  ≥50k seeds is a multi-hour sweep (~3.7 seeds/s), so it is not something
  this change can honestly tick. What landed instead is the ability to run
  it: `ROWDIFF_SEEDS`/`ROWDIFF_SEED_START` widen the smoke slice, with
  SEED_START so successive deep runs cover FRESH ranges instead of re-walking
  seeds 1..N. A 3000-seed sweep (56,962 comparisons) was run here; ≥50k
  runs nightly via `.github/workflows/nightly-rowdiff.yml`, which rotates
  SEED_START by day-of-year so successive nights cover FRESH ranges instead of
  re-proving seeds 1..50000. Both reviewers independently made the same point
  on the first draft of this line — "belongs to the nightly" is prose until
  something actually runs it — which is why the workflow exists rather than
  the sentence. The criterion was aspirational as written (a gate nobody can
  run in a shift measures nothing); it is now runnable, wired, and explicitly
  unfinished-at-this-HEAD rather than quietly ticked.

  **That sweep found the harness red on master, and it was a FALSE
  POSITIVE.** 27 of 3000 seeds failed with `22003: long overflow`; master
  fails the same seeds identically, so it predates this work and was hidden
  only because the smoke slice is 25 seeds. The engine was correct: the
  generator plants a 2^62 boundary value, four rows carry it in one group, so
  `SUM(a)` reaches 2^64 and 22003 (`numeric_value_out_of_range`) is the right
  answer. The ORACLE models that with an `aggOverflow` sentinel — but emitted
  it into the row and only THEN applied HAVING, which compares against the
  sentinel, never gets TriTrue, and drops the group. The evidence that an
  error was expected vanished, `AggResultOverflows` returned false, and the
  runner scored correct engine behaviour as a soundness mismatch. The engine
  hits the overflow while FOLDING, before HAVING can discard anything, so the
  sentinel now survives the filter (`oracle.go`). Pinned red→green by
  `TestOracle_SumOverflowSurvivesHaving`, with
  `TestOracle_HavingStillFiltersWithoutOverflow` as the inverse so a bypass
  that turned HAVING into a no-op cannot pass. The unprobed dimension was
  overflow COMBINED WITH having; overflow alone was already covered.

  Worth stating because it generalises: a differential harness that emits
  false positives is worse than no harness, because the next REAL finding
  gets waved off as "that overflow thing again."

## 11. P5 is FALSIFIED as specified — the two edges are not two copies

This RFC's load-bearing claim is that a plan's parent→child edge is stored
twice, that the copies can drift apart, and that a nil-inner shell is
exactly the drifted state — so collapsing to one storage location makes the
bug class unrepresentable. **That is false for a measured subset of edges,
and the measurement is what P5's final step produced instead of a
deletion.**

### The measurement

`TestOneFinalPlanPerReference` pins one-final-member **at extraction**, and
its own doc says mid-planning groups legitimately hold many. Nothing had
ever measured RULE TIME. Instrumenting `ExpressionRuleCall.Yield` — the
single choke point covering every construction path, including the four
composite-literal wrappers a `New*Wrapper` grep misses — over the full
2407-query corpus:

```
total=9868  mismatch=667  semantic_diff=472  multi_final_groups=0  nil_resolve=0
by parent:  FlatMap=392  RecursiveDfsJoin=58  Projection=15  PredicatesFilter=7
```

`multi_final_groups=0` kills the obvious hypothesis: this is not ambiguous
groups. **472 edges resolve, through the quantifier, to a semantically
DIFFERENT plan than the plan pointer holds.** For example the quantifier
reaches `TypeFilter([DEPT], Scan(DEPT,[=]))` while the plan child is
`DefaultOnEmpty(TypeFilter([DEPT], Scan(DEPT,[=])))`.

### Why, in one line of source

`rule_implement_nested_loop_join.go:839` builds a compensating plan LOCALLY
and never memoizes it:

```go
flatMapOuter = plans.NewRecordQueryPredicatesFilterPlanWithAlias(outerPlan, outerOnlyPreds, outerCorr)
flatMapPlan  := plans.NewRecordQueryFlatMapPlan(flatMapOuter, …)          // filter is in the PLAN
outerQuant   := …NamedPhysicalQuantifier(…, call.MemoizeExpression(outerExpr))  // UNFILTERED outer
```

The filter exists only in the plan pointer; no Reference holds it, so no
quantifier can reach it. The code states the split outright at :853 — "the
FlatMap wrapper needs the outer + inner physical quantifiers for Cascades
**bookkeeping and cost**" — and `WithChildren` enforces it by keeping
`plan: w.plan` and swapping only quantifiers. `isLeafReplaceable` says the
same thing from the other side: compound joins "encode predicate semantics
in their structure and must NOT be swapped."

So the two edges are not redundant storage. They are **two different
facts**: the plan pointer is what EXECUTES, the quantifier is what the memo
COSTS. Deleting the wrapper forces the swap those comments forbid, dropping
a `DefaultOnEmpty` (wrong outer-join NULL semantics) and residual filters
(wrong rows) — silently, not as a crash.

### What this does and does not invalidate

Everything P0-P4 and P5 steps 1-2 established still holds and is still
worth having: shells are gone at their source, the hierarchy is unified,
plans carry quantifiers, plans answer all 76 cost/ordering hints, and the
one-final-member property is real AT EXTRACTION. Zero plan drift throughout.

What is invalidated is the terminal claim — that deleting the wrappers is a
deletion. It is not. While a rule deliberately puts different things in the
two slots, collapsing them is a semantic change, not a simplification.

### The actual prerequisite

Not wrapper work. The four rules that build compensating plans must MEMOIZE
them and range the quantifier over THAT reference — which
`rule_implement_simple_select.go:97-117` already does correctly with its
fod/doe/filter chain. Then the two edges agree by construction and
collapsing is safe.

That changes memo contents, therefore costing, therefore plans: it will
move shapes on its own and cannot be verified by the zero-drift gate that
protected every step so far. It needs its own RFC step and a Graefe ACK
before any wrapper deletion is attempted again.

### A note on process

Two independent attempts at this step refused, for two DIFFERENT reasons —
the first on an import cycle it proved with a compiled probe, the second on
this. Both refusals were correct and both were cheaper than the forced
version, which would have compiled, passed the suite, and returned wrong
rows on outer joins. The remaining 31 wrapper types never mismatched across
all 9868 edges and are probably deletable today, but deleting only those
would leave the memo holding a mix of plans and wrappers with every
consumer needing both paths — worse than either endpoint. Prerequisite
first, then all 35 together.

## 12. §11's prerequisite is systematic, not local — re-measured

§11 named "the four rules that build compensating plans" as P5's
prerequisite. That undercounts it. After the FlatMap fix landed
(`3179c0696`), the divergence was re-measured at every yield across the
2407-query corpus:

```
edges=17728  semantic_mismatch=343        (was 472 before the fix)
  FlatMap                233   (was 392)
  RecursiveDfsJoin        58
  InJoin                  20   <- not in §11's list
  Projection              15
  UnorderedUnion          10   <- not in §11's list
  PredicatesFilter         7
```

Two things this establishes.

**The fix works, and it is per-SITE, not per-rule.** FlatMap fell 392 -> 233
because `rule_implement_nested_loop_join.go` contains FIVE
`NewRecordQueryFlatMapPlan` construction sites (`:565`, `:876`, `:2320`,
`:2667`, `:2882`) and seven `newPhysicalFlatMapWrapper` sites. One was
fixed. The remaining 233 are the other four.

**Two parent types §11 never listed also diverge** — InJoin (20) and
UnorderedUnion (10) — so the survey that produced §11's list was itself
incomplete. Example shapes:

```
InJoin           q->PredicatesFilter(Scan(PRODUCTS), [1 preds])
                 plan-child->Scan(PRODUCTS, [=])
UnorderedUnion   q->Project([ID,COL1,COL2], Scan(T1))
                 plan-child->Map(Project(...), {W: ID, X: COL1, Y: COL2})
```

So "build a compensating operator locally and leave the quantifier pointing
at the uncompensated input" is a PATTERN in the rule set, not a defect in
four places. Every instance both mis-prices the plan in the memo and blocks
the wrapper deletion.

Caveat on the measurement: the probe compared `findPhysicalPlan(quantifier)`
against the plan's child by pointer, falling back to `plans.Equals`. At
least one reported class (Projection, 15) renders IDENTICALLY on both sides,
so some of the 343 are equality-predicate false positives rather than true
divergences. The InJoin, UnorderedUnion, RecursiveDfsJoin and
PredicatesFilter examples above are textually different and are real. A
permanent version of this probe should be built as a proper invariant test —
it measures exactly the property P5 needs — but it needs the
false-positive class understood first, which is why it was not landed as a
test here.

**Consequence for scope.** P5's terminal step is gated on roughly ten
construction sites across six parent types, each needing the lockstep
memoization treatment and each capable of moving plans on its own. That is
its own RFC, not a paragraph in this one. RFC-183 delivers P0-P4 and P5
steps 1a-2; the terminal deletion does not land here.

## 13. §12's measurement over-counts — group multiplicity is not divergence

Attempting the InJoin site produced a regression that invalidates part of
§12's count, and the correction matters more than the fix would have.

**What was attempted.** `rule_implement_in_join.go` seeds its memo reference
from `partition.GetExpressions()` (ALL inner expressions) while seeding its
plan chain from one `innerPlan` of `partition.GetPlans()`. That looked like
the same plan-vs-quantifier divergence as the FlatMap and DFS sites, so the
reference was narrowed to the single expression corresponding to the chosen
plan.

**What broke.** `SELECT id, name FROM items WHERE id IN (2,5,7) ORDER BY id`
regressed from `InUnion` (an order-preserving merge of the IN branches) to
`InJoin` under an `InMemorySort`. Narrowing the reference collapsed the
group and destroyed the alternatives a downstream rule needed. Reverted; the
corpus returns to zero drift and yamsql passes.

**Why the measurement was wrong here.** §12's probe compared
`findPhysicalPlan(quantifier)` — the FIRST member of the referenced group —
against the parent plan's child. That conflates two different things:

1. **True divergence**: the plan holds an operator the quantifier's group
   CANNOT produce. FlatMap (compensating filters never memoized) and
   RecursiveDfsJoin (a TempTableInsert stripped from the plan only) are
   this. These are defects — the memo costs an expression that does not
   execute.
2. **Ordinary group multiplicity**: the quantifier ranges over a group
   holding several alternatives and the plan is built on one of them. That
   is Cascades working exactly as designed. A group is *supposed* to hold
   alternatives; "first member != chosen plan" is not a defect, it is the
   memo doing its job.

The InJoin 20 is category 2, and quite possibly Projection (15),
UnorderedUnion (10) and PredicatesFilter (7) are too — all four are shapes
where a rule legitimately builds one plan over a multi-member group. Only
the two already fixed are confirmed category 1.

**So §12's 343 is an upper bound, not a defect count.** A correct probe must
ask whether the plan's child is REACHABLE from the quantifier's group at all
— not whether it equals the group's first member. Until that distinction is
built into the measurement, the remaining sites cannot be triaged, and
"fixing" a category-2 site actively removes plan alternatives.

**A `PlanPartition` contract violation was found and fixed on the way**,
independent of the above. `GetPlans` documented "plans[i] corresponds to
exprs[i]" but filters non-physical members while `GetExpressions` does not,
so a single non-physical member shifts every later index and silently
mispairs. The doc now states the truth and `GetPhysicalExpressions` provides
the genuinely aligned pairing. Also noted: `NewPlanPartition` records
map-iteration order, which would be a real nondeterminism source — but it is
NOT the production path (`toPartitionsFromMap` feeds `addExpression` from an
ordered slice), so an earlier claim that the planner had nondeterministic
partition order was wrong and is retracted here.

## 14. The reachability probe — 343 becomes 158, and InJoin is exonerated

§13 said the remaining sites could not be triaged until the measurement
asked the right question. That probe now exists and has been run.

**The right question** is not "does the quantifier's group have the plan's
child as its FIRST member" but "is the plan's child REACHABLE from the group
at all" — i.e. does any physical member of the group carry that plan (by
pointer or `plans.Equals`). A group holding alternatives is Cascades working;
a group that cannot produce what executes is the defect.

```
edges=17728  UNREACHABLE=158  reachable-in-multi-member-group=1394  avgGroup=1.18
  FlatMap            101
  RecursiveDfsJoin    25
  Projection          15
  UnorderedUnion      10
  PredicatesFilter     7
```

**InJoin disappears from the list.** Its 20 were reachable inside
multi-member groups — precisely the category-2 false positives §13 predicted,
and precisely why narrowing its reference regressed `IN (…) ORDER BY id`.
That prediction being confirmed is the strongest evidence the category split
is the right model.

**1394 edges** are reachable-but-not-first. The §12 probe would have counted
every one of them as a defect. That is the scale of the over-count.

**What the 158 are.** FlatMap 101 comes from the three
`NewRecordQueryFlatMapPlan` sites not yet converted (`:565` is now fixed via
`buildCorrelatedFlatMapPlan`; `:2320`, `:2667`, `:2882` remain).
RecursiveDfsJoin fell 58 -> 25, so that rule has a second path beyond the one
fixed. UnorderedUnion's plan child is a `Map(...)` the group does not hold.
PredicatesFilter's differs in scan comparisons.

**A caveat that has now bitten three times.** Every Projection example
renders IDENTICALLY on both sides — `TypeFilter([T], Scan(T,[=]))` versus the
same string — yet compares unequal. `TypeFilter.EqualsPlanWithoutChildren`
compares only record types and `plans.Equals` recurses, so the difference is
in the child `Scan`, whose `[=]` rendering is LOSSY: two scans with different
comparison operands print the same text. So those 15 cannot be dismissed as
artifacts, and neither can they be confirmed from the dump. Explain has now
hidden the deciding field three separate times on this RFC — the `[2 preds]`
-> `[1 preds]` conjunction, the uncovered-column predicate, and this. Any
triage of the remaining classes must inspect fields, not rendered text.

**This probe should become a ratcheting test** — it measures exactly the
property P5 needs, and 158 is a baseline that must only ever fall. It was
not shipped here because a test asserting a number nobody has triaged is a
tripwire without a diagnosis; the classes above need field-level
confirmation first.
