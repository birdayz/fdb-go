# RFC-038: Multi-way Join Ordering via the Memo (Epic)

**Status:** Accepted v2 (umbrella / epic). Graefe ACK, Torvalds ACK (v1 NAK on B-before-A
ordering, PR-A gate, PR-C cap contract, PR-C-gated-on-D — all folded into v2).
**Goal:** make *multi-way join ordering* genuinely provable — a Go-only read-path extension
beyond Java, built on the RFC-037 cross-group merge infrastructure.

## Why this is an epic, not a single PR

Investigation (Java 4.11.1.0 + Go) established:

* **Java does not enumerate join orders.** `SplitSelectExtractIndependentQuantifiersRule.java:62`
  literally comments *"we don't want to interfere with join enumeration (TBD)"* — it's
  unbuilt. Java uses `PartitionSelectRule` (≥3 quantifiers → a fixed **right-deep** partition:
  `shouldJoinRightDeep` forces N−1 lower / 1 upper), `PartitionBinarySelectRule` (2 quantifiers
  → predicate partition, fired for **both** operand assignments via `exactlyInAnyOrder`),
  index/match-candidate selection, and a cost model that *selects* among produced operand
  assignments — but **no rule ever re-orders or re-associates a join**.
* Multi-table joins are a **flat `SelectExpression` with N quantifiers** (not a binary tree).
* So join-order enumeration is a **wire-compat-neutral Go-only extension** (the quantifiers and
  predicates are unchanged; only the *search space* widens). It is sanctioned by the "query
  reach may exceed Java" rule — provided deep test coverage.

Proving "multi-way join ordering" therefore requires several independent capabilities that do
not exist today and that have hard ordering dependencies. Each is its own RFC + Graefe/Torvalds
review + implementation + PR + @claude cycle. This umbrella RFC fixes the sequence and the
contracts between them; **the goal's "multi-way join ordering proven" box is checked only at
PR-D**, never before.

## The blocker chain (why this order)

Today, even if we enumerated orders, the memo would not benefit:

1. **Equivalent sub-products don't share.** `MemoizeExpression` interns with
   `EqualsWithoutChildren` (alias-sensitive) + pointer-identical children. Rule-rewritten
   sub-expressions carry **fresh quantifier aliases**, so two equivalent sub-products intern to
   *different* References; RFC-037 cross-group merge then can't surface them as candidates
   either (its candidate narrowing is pointer-keyed on children). Measured: ~1 merge regardless
   of branch count. **Without sharing, enumerated orders just multiply work — no win.**
2. **Multi-quantifier joins crash.** A 3-quantifier `SelectExpression` panics during dedup:
   `AliasMap.Compose: conflict on source q$N` in `SemanticEquals` (`expression.go:188`) via
   `SelectMergeRule`. Pre-existing, independent of RFC-037, but it blocks any multi-way work.
3. **Enumeration doesn't exist.** `PartitionSelectRule` forces one right-deep shape; nothing
   explores alternative lower-subsets / orders.
4. **The cost model must select** the best order and cost shared sub-products once.

## Sub-PRs (each independently reviewed + merged)

Order (v2, per Torvalds): **#213 → PR-B → PR-A → PR-C → PR-D.** PR-B moves first so the
multi-way test vehicle exists *before* PR-A perturbs core interning equality.

### PR-#213 — Cross-group merge infrastructure *(done, @claude-approved; prerequisite)*
Union-find merge, transparent forwarding, recursive re-merge. RFC-037. Redundant-subexpression
elimination proven. **Reach is alias-gated** — which PR-A fixes.

### PR-B — Fix multi-way `SemanticEquals`/`AliasMap.Compose` crash *(first — test vehicle)*
Root-cause the `q$N` conflict in `matchChildrenPositional` → `AliasMap.Compose`
(`expression.go:188`, `alias_map.go:119`) for ≥3-quantifier selects. Pin with a regression
test on the exact shape that crashed RFC-037's multi-way probe.
*Proves:* multi-way joins plan without panicking — and, crucially, gives PR-A a multi-way
exercise in-tree to catch equality-change fallout. Independently valuable (fixes a real crash)
even if the epic stalls.

### PR-A — Alias-aware memo equivalence *(the lever — highest blast radius)*
Make `MemoizeExpression` interning **and** RFC-037's `findEquivalentRef` candidate-finding
**alias-normalized** (canonical-alias / `SemanticEquals`-style), so equivalent-but-differently-
aliased sub-expressions intern to / merge into one Reference.
*Proves:* RFC-037 merge fires **broadly** (cascades across K equivalent branches; a real
planner-work reduction benchmark, merge-on vs merge-off, now shows a non-trivial delta).
*Blast radius — this changes the equality the WHOLE memo depends on (every memoized
expression, not just joins).* **Mandatory gate before merge (Torvalds):**
1. **stress-1M before/after** — row counts identical, durations within thresholds;
2. **determinism 10×** (`--nocache_test_results`) — identical plan hashes;
3. **non-join plan stability** — a corpus of single-table / scalar-subquery / aggregate queries
   must produce **byte-identical EXPLAIN** vs. pre-PR-A (the real hazard is an *unrelated* plan
   silently re-interning, not joins regressing);
4. full conformance (46 targets) + Graefe + Torvalds + @claude.
Independently valuable: makes RFC-037's merge actually pay off, regardless of enumeration.

### PR-C — Join-order enumeration (commutativity + associativity)
Exploration rules that enumerate orders into the memo (NOT a fatter partition rule — per
Graefe, each sub-order must land as its own Reference to be shareable; generalizing
`PartitionSelectRule` hardcodes one shape per fire and never re-enters the memo):
* 2-way operand-swap is already covered by `PartitionBinarySelectRule`'s dual firing;
* add **associativity** for ≥3 quantifiers — explore *multiple lower-subsets*, each a
  sub-product Reference. With PR-A, equal sub-products across orders **merge** (shared once);
  RFC-037's cycle guard prevents self-referential groups.

**Cap contract (hard invariants, Torvalds):**
* Enumeration is O(K!) in table count. A **stated, deterministic cap** bounds it: enumerate
  full orders only up to `K ≤ joinEnumCap` (initial value **5**, a named const); beyond it,
  fall back to the existing right-deep partition (Java's behaviour).
* **Truncation order is deterministic** (by quantifier id) and **`log()`-ed** — no silent drop.
* **MUST NOT raise `MaxTasks`** to absorb blowup; doing so is a hard-fail anti-pattern. If a
  capped enumeration still hits `MaxTasks`, the cap is wrong — lower it, don't raise the budget.
* The cap is a **guidance hook** (Graefe), not only a count: structured so cost-/promise-driven
  pruning can later replace the flat cap without re-plumbing.

**Gating (Torvalds):** PR-C **must not merge unless PR-D lands in the same epic, or PR-C ships
behind an off-by-default flag** (`enableJoinEnumeration`, default false). Enumerated orders with
no cost model to choose among them are dead infrastructure that only multiplies memo work.

### PR-D — Cost-based order selection + e2e proof
**Primary risk (not a sub-clause):** *cost propagation through union-find-merged groups.* A
shared sub-product lives in one merged Reference; its winner/cost must be computed once and
reused by every order that references it. Getting winner propagation right across merged groups
is where this PR succeeds or dies — it gets its own design section in PR-D's RFC.
**The proof:** a 3–5 table join `yamsql` / planner test where (a) the chosen plan is the
low-cost order, **differing from FROM-order**, EXPLAIN-pinned; (b) the memo's live sub-product
group count is bounded (shared sub-products merged — `MergeCount()`/live-ref hooks); (c) results
correct; (d) determinism 10×; (e) a measurable plan-cost improvement vs. the naive order.
*Proves:* **multi-way join ordering** — goal satisfied. Flips PR-C's flag on (if used).

## Cross-cutting invariants (hold at every PR)

* **Wire compat untouched** — no change to records, keys, indexes, continuations. Enumeration
  only widens the planner's logical search; the executed plan is still a standard plan.
* **Determinism** — every new rule/equality keys on stable ids / insertion order, never map
  iteration (per the query-engine skill's hardest-won lesson).
* **MaxTasks** — enumeration is exponential in table count; PR-C MUST bound it (k-table cap +
  `log()` what's dropped) so a 10-way join doesn't blow the task budget. No silent truncation.
* **Java conformance unaffected** — Java still reads/writes identical records; this is purely
  Go expressing a wider order search on the read side.

## Test plan (per PR + epic-level)

Each PR carries its own unit + FDB integration + determinism tests (see its RFC). Epic-level
acceptance (PR-D): a multi-table join whose optimal order differs from FROM-order is planned to
the optimal order, EXPLAIN-pinned, with shared sub-products merged and a measurable planner /
plan-cost improvement vs. the naive order — the honest "slow-without / fast-with" artifact that
RFC-037 alone could not provide.

## Status progression

Draft → (per-PR RFCs land) → Implemented when PR-D ships and the epic acceptance test is green.
