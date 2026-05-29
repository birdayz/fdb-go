# RFC-037: Full Graefe Memo — Cross-Reference Equivalence-Class Merging

**Status:** Draft
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
  (`quantifier.go:164`). Grep confirms: **472** call sites use `GetRangesOver()`; the raw
  field is touched in exactly **one** place (the accessor). This is the key lever — see Fix.
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

Implement union-find group merging with resolution at a single chokepoint.

### 1. Reference identity + forwarding (union-find with path compression)

Add to `Reference`:

```go
id          uint64     // monotonic creation order; stable, deterministic
forwardedTo *Reference // nil ⇒ canonical; else this group was merged away
```

`InitialOf` assigns `id` from a package-level monotonic counter seeded **per Memo**
(not a global — global counters break test isolation/determinism). Add:

```go
// Canonical follows the forwarding chain to the surviving Reference,
// compressing the path so subsequent lookups are O(1).
func (r *Reference) Canonical() *Reference { ... }
```

### 2. Resolve at the chokepoint

```go
func (q Quantifier) GetRangesOver() *Reference { return q.rangesOver.Canonical() }
```

All 472 consumers (correlation walks, exploration scheduling, executor extraction, dedup
comparisons) now see the canonical Reference automatically. **No in-flight expression or
task is rewritten** — the stored pointer is left intact (Quantifier is an immutable value),
only *reads* resolve. This sidesteps the "redirect every pointer / fix in-flight tasks"
problem the naive approach has. `sameChildRefs`/`sameChildReferences` compare
`GetRangesOver()` results, so they too become merge-aware for free.

### 3. The merge operation (`Memo.merge(winner, loser)`)

Deterministic winner = **lower `id`** (older group wins; stable regardless of map order).
The paper's "pattern memory merge" (§3.5) = fold the loser's state into the winner:

1. `Reference.Insert` each of loser's exploratory `members` and `InsertFinal` each
   `finalMembers` into winner (dedup handles overlap).
2. Merge `partialMatchMap` (candidate → matches) and `winners` (per-properties best);
   on key collision keep winner's (it is the canonical group). Clear `correlatedToCache`.
3. Re-seed exploration state so winner re-explores any genuinely-new members
   (`explState = explorationNever` if new members were added) — bounded by the existing
   `maxRoundsPerRef = 10` backstop.
4. Set `loser.forwardedTo = winner`; clear loser's members so a stale direct pointer can
   never re-introduce the loser as live work (all legitimate reads go through `Canonical()`).
5. Update `Memo` indices: re-point `childToParents` edges that named `loser` to `winner`;
   move `loser` out of `refs`/`leafRefs`; if `loser == m.root`, advance `m.root` to winner.

### 4. Merge trigger — bottom-up duplicate detection at memo integration (§2)

The paper integrates each substitute "with search for and detection of duplicates (a
recursive bottom-up process using hash tables)." Today that search returns a Reference but
never merges. Change the integration point so that when an expression is being placed into
Reference `G`, we look up (via the existing `childToParents` topological index + hash +
`EqualsWithoutChildren` + canonical child match) whether a **structurally-equal** member
already lives in a **different** Reference `H`. If so, `Memo.merge(min(G,H), max(G,H))`
instead of inserting a duplicate. This is wired where rule output enters the memo
(`TransformExprTask.yieldFn` / the `MemoizeExpression` path), so it fires for
rule-derived equivalences, not just initial construction.

Reduction-rule-driven merges (§3.6 — substitute is a bare group reference) are the natural
follow-on and are noted in Future Work; the bottom-up trigger above is the general case that
delivers redundant-subexpression elimination and is what the e2e test pins.

### Determinism (the #1 risk per the query-engine skill)

Every merge decision and every iteration that could affect plan choice is keyed on the
monotonic `id`, never on map iteration order: winner = lower id; candidate scan uses the
existing insertion-ordered `childToParents` edges and `leafRefs` slice; `Canonical()` is a
pure function of the forwarding chain. The merge is therefore a deterministic function of
the (deterministic) task schedule. Pinned by a 10×-repeat determinism test on the e2e query.

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

1. **Unit (`memo_merge_test.go`):** `Canonical()` chain + path compression; `merge` folds
   members/finals/partialMatch/winners; root re-pointing; idempotent re-merge; winner = lower id.
2. **Merge fires e2e (the optimization proof, not just correct rows):** a query whose plan
   construction yields the *same* scan+filter subexpression in two independently-derived
   References (self-join / shared sub-product). Assert via a `Memo` stats hook
   (`MergeCount() > 0` and post-plan `len(refs)` strictly less than the no-merge baseline)
   **and** correct rows. A BAD test would only check rows (could pass via the un-merged path).
3. **Determinism:** the e2e query planned 10× with `--nocache_test_results`, identical plan
   hash each run (per skill: non-deterministic plan = bug).
4. **Multi-way join (5 tables):** assert group count is bounded (shared sub-products merged)
   and the plan is correct; this is the headline capability the item names.
5. **No-regression:** existing 46 targets green; 1M stress baseline within thresholds
   (no merges on simple queries ⇒ no perf delta).
6. **Fuzz:** extend `FuzzSemanticEquals_Properties`-style coverage with a merge invariant:
   after any sequence of merges, `Canonical()` is acyclic and `GetCorrelatedTo()` of a
   merged group equals that of the survivor.

## Out of scope (Future Work, tracked in TODO.md)

* Reduction-rule-triggered merges (§3.6 bare-leaf substitutes).
* Cost-model exploitation of shared sub-products for full N-way join-order optimality (the
  merge makes the *sharing* correct; squeezing the last bit of join-order quality is a
  separate cost/winner-propagation effort).
