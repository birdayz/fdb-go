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

**P5 — TERMINAL, not optional.** Make `plans.RecordQueryPlan` a
`RelationalExpression` holding quantifiers, deleting the
`physical_*_wrapper.go` layer. The edge is stored twice today — quantifier
`Reference` *and* embedded plan pointer — and a shell is precisely the state
where the two disagree. P1-P4 make them agree *at construction*; nothing
structurally prevents later divergence, which is exactly what
`shouldRelinkInner` and `pinOrderedSpine` exist to arbitrate. **Duplication
is the disease; nil is a symptom.** Large; touches the executor. Sequenced
last, but no longer contingent.

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

## 8. Success criteria

- `isNilInnerShell` / `isNilInnerFetch` have no production callers, and §3a
  is deleted in full. (The draft's `grep -c "shell"` trending to zero is
  dropped — a vanity metric that rewards renaming.)
- §3b and §3c survive intact and are demonstrably still exercised.
- The EXPLAIN-differ exists and shows no unexplained plan-shape change
  across the yamsql corpus.
- The RFC-182 harness runs clean at ≥50k seeds across the full grammar,
  including the shapes that produced findings 1-4.
- 1M stress within noise of the pre-change baseline.
- DIVERGENCES.md's "repair-at-EXTRACTION" section is **deleted**, not
  amended — the debt is gone rather than re-described.
