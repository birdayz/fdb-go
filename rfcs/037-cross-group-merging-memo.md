# RFC-037: Full Graefe Memo — Cross-Reference Equivalence-Class Merging

**Status:** Accepted (v2). Graefe ACK, Torvalds ACK (conditions on `Get()` + `traversal.go`
citation folded in).
**Item:** TODO "Beyond Java (Go-only improvements) → Full Graefe Memo with cross-group merging" (the `B3` follow-on).

## Problem

The Cascades memo is supposed to hold **equivalence classes of logically-equivalent
expressions** (groups). The paper (Graefe 1995, §2 "Integration into memo" and §3.5
"Pattern Memory") is explicit that when two groups are *discovered to be the same*, the
optimizer must **merge** them — "the most complex pattern memory method is merging two
pattern memories when two groups of equivalent expressions are discovered to be actually
one" (§3.5), and reduction rules whose substitute is a bare leaf reference cause "two
groups in memo [to] be merged" (§3.6).

Neither Go nor Java does this today. Both do *find-or-create interning at insertion time*
(Go: `Memo.MemoizeExpression`; Java: `CascadesRuleCall.memoizeExploratoryExpressions`) and
*per-Reference dedup* (Go: `Reference.Insert`; Java: `Reference.isMemoizedExpression`).
Interning only catches duplicates that are equivalent **at the moment of insertion**. It
does **not** catch the case that actually produces redundant groups:

> Two References `A` and `B` are created **distinct** (they were not equivalent when
> created). Later, rules transform their members until `A` and `B` hold logically-equivalent
> member sets. Nothing ever notices; they stay two pointers forever.

Concretely, rule outputs are inserted via `t.Ref.Insert(expr)` into the *firing* Reference
(`unified_tasks.go` `TransformExprTask.yieldFn`) — they are **never** checked against other
References. So:

* A shared subexpression reachable by two derivation paths (e.g. the same scan+filter under
  both arms of a self-join, or a common sub-product in a multi-way join) lives in two
  separate Reference objects. It is **explored and optimized twice**, and the two parents
  that range over it are not recognized as equivalent, so *their* parents aren't either —
  the redundancy compounds upward.
* Multi-way join ordering (5+ tables) is the canonical victim: distinct join sub-orders
  share common sub-products, but without group merging each sub-product is a fresh group, so
  the planner cannot reuse the best plan for a shared sub-product across orders.

This is the difference between a *tree of References that happen to share leaves* and a true
*Cascades Memo DAG*. We have the former.

## Investigation

### Go (`pkg/recordlayer/query/plan/cascades/`)

* **`Reference`** (`expressions/reference.go:33`): holds `members` + `finalMembers`,
  `plannerStage`, exploration state (`explState`/`explRounds`/`explMemberCount`),
  `winners map[any]RelationalExpression`, `partialMatchMap map[any][]any`,
  `correlatedToCache`. No identity/ID field. No forwarding.
* **Child references are held by pointer** in `Quantifier.rangesOver`
  (`expressions/quantifier.go:54`), read **only** through `GetRangesOver()`
  (`quantifier.go:164`). Grep confirms: **444** call sites use `GetRangesOver()`; the raw
  field is touched in exactly **one** place (the accessor). A key lever — but, as v1 of this
  RFC learned the hard way, not the *only* identity-bearing path (see Fix §2).
* **`Memo`** (`memo.go`): `refs` set, `childToParents` reverse index (parent edges per child
  Reference, the Go analog of Java's `Traversal`), `leafRefs` slice (deterministic order).
  `MemoizeExpression` does topological find-or-create; `sameChildRefs`/`sameChildReferences`
  compare child References **by pointer**.
* **No merging anywhere.** Grep for merge/union/canonical across the package: nothing. The
  `Reference.Insert` doc comment (`reference.go:179`) and `memo.go:8` both name this as the
  unbuilt "B3 follow-on": *"The full Memo (B3 follow-on) generalises this further to merge
  equivalence classes across the whole memo."*

### Java (4.11.1.0)

* `Reference` uses `LinkedIdentitySet` members + `isMemoizedExpression` (identity →
  class → correlation → recursive `containsAllInMemo` on children → `equalsWithoutChildren`).
  Per-Reference only.
* `Traversal` (`MutableNetwork<Reference, ReferencePath>`) is a full reverse-index that
  *would* support redirecting edges during a merge — but Java never calls a merge. Confirmed:
  Java stops at find-or-create interning, exactly like Go.

**Framing (required by CLAUDE.md "verify before treating as parity").** Java does **not**
implement this. This is a sanctioned **Go-only read-path extension** ("Beyond Java" in
TODO.md). It is faithful to the Cascades *paper* (which Graefe authored), so it is
"more-Cascades-than-Java," not a divergence from Java's wire/record behavior. **Wire compat
is untouched**: this only changes how the planner shares/optimizes sub-expressions; the
records read/written and the chosen physical plan's semantics are unchanged. Deep test
coverage is therefore the bar (per the "query reach may exceed Java" rule).

## Fix

Implement union-find group merging. The v1 of this RFC was NAK'd by both reviewers: its
premise — "reads resolve only at `GetRangesOver()`, leave everything else alone" — was
false. Grep shows the planner keys on raw `*Reference` identity far beyond `GetRangesOver()`:
in-flight tasks hold `t.Ref` raw and gate on `ContainsExactly` (pointer); long-lived maps
`planner.exploreCount` and `Memo.{refs,childToParents,leafRefs}` are pointer-keyed;
`expression_rule_call.go:115` does `ref == c.Reference`. v2 addresses every flagged hole.

### 0. Scope: merge during the REWRITING phase only

The merge fires only while References are at `StageInitial`/`StageCanonical` (REWRITING).
Rationale, not punt:

* The redundant-subexpression / shared-sub-product / multi-way-join-order value the item
  names is produced by **logical** rules (join reorder, filter/projection pushdown) — all
  REWRITING. PLANNING is logical→physical implementation selection; it creates no new
  *logical* equivalences to merge.
* `partialMatchMap` and `winners` are **PLANNING-phase artifacts** (verified: index match
  candidates / requested-ordering winners; both empty during REWRITING). Confining merges to
  REWRITING means the merge never has to canonicalize a `PartialMatch` (which holds raw
  `queryRef`/`candidateRef` fields) or a `winners` key — **dissolving Graefe's "pattern
  memory merge is the hard part" concern**: in REWRITING the only per-group bookkeeping is
  exploration state, which the merge folds explicitly.
* A debug assertion guards it: merging a Reference that already has partial matches or
  winners panics — a tripwire that forces a deliberate scope extension rather than a silent
  bug. PLANNING-phase merging is Future Work.

### 1. Reference identity + forwarding (union-find, path-compressed)

Add to `Reference`: `id uint64` (monotonic, assigned by `InitialOf` from a **per-Memo**
counter — not a global, which would break test isolation/determinism) and
`forwardedTo *Reference` (nil ⇒ canonical). Add `Canonical()` (follows the chain, compresses
the path), `ID()`, `IsForwarded()`.

### 2. The loser is a transparent forwarder, NOT an emptied husk

This is the core correction over v1. After `merge`, the loser is **not** cleared. Instead
**every state-bearing `Reference` method resolves `self` to `Canonical()` at entry**:

```go
func (r *Reference) Members() []RelationalExpression { r = r.Canonical(); return r.members }
func (r *Reference) ContainsExactly(e RelationalExpression) bool { r = r.Canonical(); ... }
// …Get, Insert, InsertFinal, FinalMembers, AllMembers, GetBest, NeedsExploration,
//   StartExploration, CommitExploration, ExplRounds, ExplMemberCount, GetCorrelatedTo,
//   Winner/SetWinner/HasWinner/GetWinners, partialMatch accessors, Stage/SetStage,
//   AdvancePlannerStage, GetPlanProperties/SetPlanProperties — EVERY state-bearing method.
```

The canonicalize-at-entry prologue must cover **every** state-bearing method, including
`Get()` (used by `InitialOf` seeds and equivalence walks) — a single un-canonicalized
state method is a latent two-state bug. `GetCorrelatedTo` recurses into
`childRef.GetCorrelatedTo()`; that is safe (each child resolves once at its own entry, no
method→`Canonical`→method cycle). Audit: the only allowed non-canonicalizing methods are
`Canonical()`, `ID()`, `IsForwarded()`, and the `absorb` primitive.

`Canonical()` itself, `ID()`, and the merge primitive do **not** recurse. Consequence:
in-flight `TransformExprTask`/`TransformImplTask` holding `t.Ref = loser` keep working —
`loser.ContainsExactly(expr)` and `loser.Members()` transparently operate on the survivor's
state, so **no exploration work is stranded** (Torvalds #1). Expression *pointers* are moved,
not copied, so `isFinalMember`/`isAlreadyExploratoryMember`'s pointer scan still finds them.

`GetRangesOver()` also resolves (`q.rangesOver.Canonical()`), so the 444 consumers,
`sameChildRefs`/`sameChildReferences`, and `collectReferences` (walks via `GetRangesOver`)
all see the survivor. `expression_rule_call.go`'s `ref == c.Reference` becomes
`ref.Canonical() == c.Reference.Canonical()`. `planner.exploreCount` is canonicalized at its
two sites (`planner.go:528,637`: `p.exploreCount[t.Ref.Canonical()]`).

### 3. The merge operation (`Memo.merge`)

Deterministic winner = **lower `id`** (older group; independent of map order). Steps:

1. **Guard:** panic if either ref has partial matches or winners (scope tripwire, §0).
2. Fold loser→winner via an `expressions`-package primitive (`winner.absorb(loser)`, touches
   private fields): `Insert` loser's exploratory members, `InsertFinal` its finals (dedup
   handles overlap); fold exploration state so the survivor re-explores genuinely-new members
   (`explState=explorationNever` iff members grew). This re-explore is bounded by the existing
   `maxRoundsPerRef` backstop so a merge-induced re-explore cannot relitigate a rule cycle
   indefinitely — test plan #1 asserts a merge that adds members bumps `explRounds` but stays
   under the cap (Graefe watch-item).
3. `loser.forwardedTo = winner` (loser now forwards; its fields are inert but readable).
4. **Memo index repoint (eager, Torvalds #3):** rewrite `childToParents` edges naming `loser`
   to `winner` (dedup); delete `loser` from `refs`, `leafRefs`/`leafRefsSet`; if
   `loser == m.root`, `m.root = winner`. Other long-lived `*Reference`-keyed structures need
   no repoint: `planner.exploreCount` is canonicalized at its two access sites
   (`planner.go:528,637`); `Traversal` (`traversal.go` `refToExprs`/`childToParents`) is a
   PLANNING-phase artifact rebuilt per match-candidate **after** REWRITING-only merges settle,
   and `collectNetwork` descends via `GetRangesOver()`, so it never indexes a forwarded
   Reference; the per-call `visited` maps in `fixpoint`/`cost`/`extract`/`planning_cost_model`/
   `rule_adjust_match` likewise walk via `GetRangesOver()`. The load-bearing invariant: the raw
   `rangesOver` field is read **only** by the `GetRangesOver()` accessor (grep-verified), which
   canonicalizes — so resolution at that one point covers all 444 sites plus those traversals.
5. **Cache invalidation across the DAG (Torvalds #2):** equivalent groups have equal
   correlated-to sets, so the survivor's set is unchanged *in value* — but defensively walk
   **up** `childToParents` from the survivor invalidating each ancestor's `correlatedToCache`
   (exported `InvalidateCorrelatedToCache()`), plus the survivor's. Bounded; uses the
   existing reverse index. A test asserts merged-group `GetCorrelatedTo()` == survivor's
   pre-merge value (pins the "equivalence ⇒ equal correlation" invariant).

### 4. Merge trigger + recursive upward re-merge (§2, Graefe #1)

At memo integration during REWRITING (where rule output enters the memo —
`MemoizeExpression` / the yield path), when placing an expression into Reference `G`, the
existing topological lookup (`childToParents` ∩ + hash + `EqualsWithoutChildren` + canonical
child match) already finds a structurally-equal member. If that member lives in a **different**
Reference `H`, `merge(min_id(G,H), max_id(G,H))` instead of inserting a duplicate.

The paper's integration is **recursively bottom-up**: discovering `G≡H` can make their
parents duplicates. So `merge` feeds the survivor onto a **worklist**; after each merge we
re-run duplicate detection on the parents of the merged group (via `childToParents`),
cascading merges upward until the worklist drains. Termination: each merge strictly reduces
the live-Reference count, which is finite.

Reduction-rule-triggered merges (§3.6, bare-leaf substitutes) reuse this same `merge`
primitive and are deferred to Future Work — they are a *trigger*, not a new mechanism.

### Ordering invariant (Graefe #3)

Merges are applied synchronously at the integration call, never mid-binding-iteration of a
rule. The transparent-forwarder design (§2) makes this safe even for bindings extracted
*before* a merge: such a binding holds a `loser` pointer, and every subsequent read through
that pointer resolves to the one survivor — there is no window in which the same group is
observed in two states.

### Determinism (the #1 risk per the query-engine skill)

Every merge decision keys on the monotonic per-Memo `id` (winner = lower id), never on map
iteration order; the candidate scan uses insertion-ordered `childToParents` edges and the
`leafRefs` slice; `Canonical()` is a pure function of the chain. Merge is a deterministic
function of the (deterministic) task schedule. Pinned by a 10×-repeat plan-hash test.

## Performance

* `Canonical()` is O(1) amortized (path compression). Added to the hot `GetRangesOver()`
  path: one pointer-nil check + (rarely) a short chain walk. Negligible vs. the existing
  per-call work in correlation/dedup walks.
* Merge detection reuses the existing `childToParents` topological narrowing already run
  during memoization — no new global scan. Merges *reduce* total References, so exploration
  and optimization do **less** work on join-heavy queries (the whole point).
* Simple CRUD/single-table queries create no duplicate groups → zero merges → cost is the
  one nil-check in `Canonical()`. Verified by the 1M stress baseline (must not regress).

## Test plan

1. **Unit (`memo_merge_test.go`):** `Canonical()` chain + path compression + acyclicity;
   `absorb` folds exploratory+final members (pointer-preserving) and exploration state; winner
   = lower id; idempotent re-merge; root re-pointing; `childToParents`/`leafRefs`/`refs`
   repointed and loser removed.
2. **In-flight task not stranded (Torvalds #1 regression):** construct loser + winner, queue a
   transform task on the loser, merge, then run the task — assert it operates on the survivor
   (its `ContainsExactly`/`Members` see merged state; the rule still fires). Guards the exact
   bug v1 would have shipped.
3. **Recursive upward re-merge (Graefe #1 regression):** build G≡H whose parents P(G), P(H)
   become equal once G,H merge; assert the parents also merge (worklist cascades up).
4. **correlatedToCache (Torvalds #2):** merged-group `GetCorrelatedTo()` == survivor's
   pre-merge value; an ancestor's cache is invalidated (recomputes, not stale).
5. **Scope tripwire (§0):** merging a Reference carrying a partial match / winner panics.
6. **Merge fires e2e (optimization proof, not just rows):** a query whose REWRITING yields the
   *same* scan+filter subexpression in two independently-derived References (self-join / shared
   sub-product). Assert a `Memo` stats hook (`MergeCount() > 0`, post-plan live-`refs` strictly
   below the no-merge baseline) **and** correct rows. A BAD test checks rows only (passes via
   the un-merged path).
7. **Determinism:** the e2e query planned 10× with `--nocache_test_results`, identical plan
   hash each run (per skill: non-deterministic plan = bug).
8. **Multi-way join (5 tables):** assert live-group count is bounded (shared sub-products
   merged) and the plan is correct — the headline capability the item names.
9. **No-regression:** existing 46 targets green; 1M stress baseline within thresholds (no
   merges on simple/single-table queries ⇒ no perf delta; `Canonical()` is one nil-check).
10. **Fuzz:** merge invariant — after any sequence of merges, `Canonical()` is acyclic, no live
    Reference forwards, and `GetCorrelatedTo()` of a merged group equals the survivor's.

## Out of scope (Future Work, tracked in TODO.md)

* Reduction-rule-triggered merges (§3.6 bare-leaf substitutes).
* Cost-model exploitation of shared sub-products for full N-way join-order optimality (the
  merge makes the *sharing* correct; squeezing the last bit of join-order quality is a
  separate cost/winner-propagation effort).
