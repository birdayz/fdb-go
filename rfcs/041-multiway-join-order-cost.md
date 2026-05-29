# RFC-041: Recursive total-cost join ordering + multi-way ordering proof (RFC-038 PR-C/PR-D, collapsed)

**Status:** Draft (v2). Supersedes the separate PR-C (associativity rule) + PR-D (cost
selection) split in RFC-038 — see "Reframing" below. Graefe advised (this shift)
collapsing them: the enumeration already exists; the gap is the cost model. v2 addresses
review round 1: Graefe ACK-conditional (made the best-member recursion *recursive*, not
top-Reference-only) + Torvalds NAK (strict landing order — best-member lands and proves
green *before* the shallow criterion is neutered; member-order-stability invariant;
dropped the memo-scope "OR" waffle — the cost memo is already per-call).

**Epic:** RFC-038 PR-C/PR-D. This is where the goal's "multi-way join ordering
proven" box is checked.

## Problem

Multi-way join ordering is **not** cost-based today. Decisive probe (real FDB,
3-table chain `t1=1, t2=20, t3=200`, chain `t1←t2←t3`), planning the SAME join
under two FROM-orders and comparing EXPLAIN:

```
FROM t3,t2,t1 → NLJ(INNER, Scan(T1), NLJ(INNER, Scan(T3), Scan(T2)))
FROM t1,t2,t3 → NLJ(INNER, NLJ(INNER, Scan(T2), Scan(T1)), Scan(T3))
```

The plans **differ** (FROM-order-dependent, not cost-driven) and **neither is
cost-optimal**: the first has a correct 1-row top driver (T1) but its inner join
drives from T3 (200 rows!). A correct cost model would converge both FROM-orders
to the same drive-from-smallest left-deep tree.

## Investigation (Java + Go)

* **Java does not enumerate join orders** (`SplitSelectExtractIndependentQuantifiersRule`
  comments "we don't want to interfere with join enumeration (TBD)"). So multi-way
  ordering is a **wire-compat-neutral Go-only read-path extension** — allowed under
  "query reach may exceed Java" given deep tests. The quantifiers/predicates/executed
  plan are unchanged; only the planner's order *search* widens.
* **Enumeration already exists in Go and is on by default:**
  * `SelectMergeRule` flattens nested binary joins into a flat N-quantifier `SelectExpression`.
  * `PartitionSelectRule` (rule_partition_select.go) enumerates all 2^N−2 non-trivial
    bipartitions for ≥3-quantifier selects, MemoizeExpression-ing each lower sub-product
    as its own Reference and yielding each upper SELECT. `ShouldJoinRightDeep` defaults
    **false** (DefaultPlannerConfiguration), so full enumeration runs.
  * `PartitionBinarySelectRule` fires both operand orders (2-way commutativity).
  * PR-A (just landed) makes equal sub-products across orders **intern/merge** into one
    Reference — so `(t1⋈t2)` is costed once and shared.
* **Recursive total-cost already exists and is correct in shape.** `EstimateCostWith`
  → `estimateCostMemoised` → `localCost` recurses through child References; `localCost`'s
  default case delegates to `CostHinter.HintCost` (cost.go:158), and the physical FlatMap
  wrapper (physical_flat_map_wrapper.go:74) computes `CPU = child[0].CPU + outerCard ×
  innerCPU` — the join multiplication. So a whole join tree's total cost is computable.
* **The bug (Graefe-confirmed):** the winner comparator `compareExpressions`
  (planning_cost_model.go) runs the **shallow** `compareFlatMapJoinOrdering` at line 282
  — which compares two whole multi-way plans by **only the top FlatMap's outer
  cardinality** (`outerCardinality`, :754-767) — **before** the recursive total-cost
  fallback (`EstimateCostWith`) at line 299. A plan with a great top-driver but a pessimal
  inner subtree wins. The inner group's winner is already available (`outerCardinality`
  calls `ref.GetBest(...)`); the shallow criterion throws it away. This is Cascades §3.1
  violated: cost must be **combined cost with inputs**, recursively — never a single node.
* **Cardinality must flow.** Under `DefaultStatistics` every table = `LeafScanCardinality`
  (1e6) → every comparison ties → FROM-order tie-break leaks. The embedded planner
  fetches real per-type counts (`fetchTableStatistics` → `MapStatistics`) when the schema
  tracks per-record-type counts; the proof must use counted tables.

## Reframing vs RFC-038

RFC-038 split this into PR-C (build an associativity enumeration rule) + PR-D (cost
selection). Verification (this shift) shows the enumeration **already exists** — building
a new rule would be redundant (Graefe: "the enumeration was never the problem — the cost
model was looking at one node where Cascades demands the whole subtree"). The two PRs
collapse into this one focused change.

## Fix — strict landing order

The two changes are **ordered, not parallel** (Torvalds): the best-member recursion
(step 1) must land and prove green *before* the shallow criterion is neutered (step 2).
Neutering the shallow criterion first, while line 299 still costs unoptimised inner
sub-trees via `members[0]`, reintroduces exactly the broken-plan selection the D-4
scalar-fallback comment (planning_cost_model.go:294-298) guards against.

**Step 1 — Recursive best-member cost (THE load-bearing change; lands first).**
`estimateCostMemoised` recurses via `firstMemberCostMemoised` → `members[0]`, NOT the
winner. `BestRefCostWith` takes the best member only at the *top* Reference; its children
still recurse through `members[0]`. So the line-299 total-cost rung today costs
**unoptimised** inner sub-products. Fix: make the memoised recursion **itself**
best-member — at every Reference pick `GetBest` (deterministic `Cost.Less` order) and
recurse into that winner's children, memoising best cost per Reference so a shared
(merged) sub-product pays the recursion once (else O(N^K) over members × shared children).
Land this and prove the existing suite stays green (the shallow criterion still fires, so
plan shapes are unchanged) **before** step 2.

**Step 2 — Neuter the shallow join-order criterion (after step 1 is green).** The
total-cost rung at line 299 now recurses through best members and includes the FlatMap
term (HintCost: `child[0].CPU + outerCard × innerCPU`). So the surgical fix is to
**delete the shallow `compareFlatMapJoinOrdering` criterion at line 282** and let line 299
decide join order. Rewriting `compareFlatMapJoinOrdering` to call `EstimateCostWith` twice
is redundant with line 299 (Graefe) — prefer deletion. The rest of the comparator ladder
(index/covering preference, `compareFlatMapVsNLJ`, etc.) is unchanged; this RFC touches
only the *join-order* rung.

**Invariants / guards:**
* **Member-order stability (Torvalds).** Best-member selection is deterministic only if
  `Cost.Less` (cost.go:150) has a total tiebreak AND the comparison never depends on
  `members` slice order. PR-A's union-find merge folds a loser's members into the
  survivor, changing slice order. Step 1 must assert the best-member result is invariant
  to member order (test: shuffle a Reference's members, assert identical chosen cost) and
  `Cost.Less` must break ties on a stable key, not position.
* **Memo scope (Torvalds — no waffle).** The cost memo in `estimateCostMemoised` is
  **per-call** (`memo == nil` at `EstimateCostWith` entry, cost.go:272), so it cannot
  outlive a union-find merge — there is no cross-call staleness to invalidate. State this;
  do not add a persistent memo.
* **Acyclic termination.** `reachable`/`mergeable` (memo_merge.go) forbids cycle-creating
  merges, so the child-Reference DAG is acyclic and the best-member recursion terminates;
  a defensive visited-set is included as cheap insurance.
4. **Cardinality plumbing for the proof.** Use counted tables so `RecordTypeCardinality`
   returns real values. Verify `fetchTableStatistics` populates `MapStatistics` for the
   proof schema (per-record-type count key).

## Performance

The join-order decision already calls `GetBest`/cost on child groups; making it a
memoised recursive total cost is the same asymptotic work (one pass per Reference,
cached), and PR-A's merge keeps the sub-product count bounded. No `MaxTasks` change.
Stress-1M before/after must stay within thresholds (the cost change can only re-rank
join orders, not alter scan/index costs). Determinism: cost is a deterministic function
of stable ids + cardinalities — no map iteration in the comparison.

## Test plan

* **The proof (acceptance, FDB):** `TestFDB_MultiwayJoinOrder_Probe` — a 3-(then 5-)table
  counted join; assert (a) **order-invariance**: the same join under multiple FROM-orders
  produces byte-identical EXPLAIN; (b) **cost-optimal**: the chosen tree drives from the
  smallest table (left-deep), EXPLAIN-pinned, differing from FROM-order; (c) results
  correct; (d) determinism 10×; (e) shared sub-products merged (`MergeCount`/live-ref hook).
* **Cost-monotonicity (Graefe — proves cost actually drove it).** Perturb one table's row
  count and assert the chosen drive-order **flips accordingly**. Order-invariance +
  "drives smallest" can both pass on a degenerate tie; the perturbation test proves the
  cost model — not an incidental tie-break — selected the order.
* **Costed-once (Graefe).** Pin that a merged shared sub-product is costed **once** via a
  recursion-count hook on the best-member cost walk, not merely that `MergeCount > 0`.
* **Cost-model unit tests:** recursive total cost ranks a known 3-way join's orders;
  best-member recursion picks the winner; merged-group cost computed once.
* **No regression:** 46/46 targets incl. plandiff conformance; stress-1M before/after;
  determinism 10×; non-join plan stability (single-table/aggregate EXPLAIN unchanged).

## Status progression

Draft → Implemented when the proof is green and the no-regression gate passes.
