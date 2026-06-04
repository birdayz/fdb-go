# RFC-074: Collapse the dual join-merge values to one canonical value

**Status:** Implemented (Graefe ACK + ruling (A), re-ruled after measurement; Torvalds ACK)
**Area:** Cascades query planner — join result values + memo interning
**Reviewers:** Graefe (Cascades alignment — mandatory), Torvalds (code quality), codex, @claude

## Problem

The Go port carries TWO result-value types for ONE concept — "merge these join legs'
columns into one row":

- `JoinMergeResultValue{OuterAlias, InnerAlias}` — binary, the translator's join seeds
  (`cascades_translator.go:396/657/767`).
- `JoinMergeAllValue{Aliases []}` — N-ary, `PartitionSelectRule`'s re-enumerated merges.

They are distinct Go types with disjoint arms in `SemanticEqualsUnderAliasMap`
(`semantic_equals.go`) and `SemanticHashCode`, so the same join sub-product reached as a
translator binary seed vs an N-ary re-enumeration never compares equal. This is a Go-only
divergence (Java has a single `RecordConstructorValue`-based path), dead-weight duplication,
and it blocks any single-type interning.

## What this RFC is NOT (corrected after measurement)

An earlier draft claimed the dual-type split *causes* the ≥5-way join task-count blowup
(`ErrPlannerCapHit`) and that collapsing the types fixes it. **Measurement falsified that.**
`distinctRefs`/`tasksRun` on the chain join are IDENTICAL before and after the collapse
(N=5: 127k tasks / 171 refs, unchanged; N=7: 3.08M / 2563). The duality is a ~2× constant,
not the exponential.

The real blowup: logically-equivalent join sub-products land in SEPARATE memo `References`
and never coalesce — even identical single-table scans fork into 3 References, and a
correctly-merged `{T1,T2,T3}` group coexists with duplicate standalone bushy regroupings.
Go's cross-group merge (`memo_merge.go`, RFC-037 — a **Go-only extension**; Java has no such
primitive) is disabled once a group has a winner (`mergeable` → false on
`HasWinnersOrMatches`, `memo_merge.go:84`), and `OptimizeGroupTask` interleaves `SetWinner`
(`unified_tasks.go:332`) with `Integrate` yields (`expression_rule_call.go:116`) on one LIFO
stack — so a group routinely holds a winner before its equivalent twin is born → merge
refuses → fork → exponential. The acceptance test's own note agrees
(`multiway_join_order_nway_test.go:36-40`: "ties into RFC-039 broad memo merging").

**Graefe's re-ruling (the decisive part):** the Java-faithful budget fix is NOT "make merge
work under winners" — that doubles down on a Go invention Java lacks. Java bounds the
bipartition lattice at the source via `shouldDeferCrossProducts` + `shouldJoinRightDeep`
(`PartitionSelectRule.java:92,122`), and builds legs in a canonical interning-friendly form.
Porting that pruning is the ≥5-way fix — tracked as a SEPARATE RFC (see Follow-up). This RFC
ships ONLY the collapse: a correct divergence-removal and a prerequisite for single-type
interning. Do NOT decouple exploration from optimization (Java interleaves identically — not
the divergence).

## Fix — one canonical merge value (`JoinMergeResultValue` ≡ `JoinMergeAllValue{Seed:true}`)

Retire `JoinMergeResultValue`; always emit the single N-ary `JoinMergeAllValue`. The two retired
Go types encoded a PROVENANCE distinction the memo relied on: the **translator seed** (binary,
names only its two source legs but hides the real projection) vs the **re-enumeration merge**
(names exactly the live legs). The collapse must preserve that distinction EXACTLY at every site
that read it, or it stops being behavior-preserving. So the new struct carries a `Seed bool`, and
the faithful mapping is `JoinMergeResultValue` ≡ `JoinMergeAllValue{Seed:true}`, `JoinMergeAllValue`
(old) ≡ `JoinMergeAllValue{Seed:false}`. The translator seeds use `NewJoinMergeSeedValue` (Seed=true);
re-enumeration uses `NewJoinMergeAllValue` (Seed=false).

The behavioral parity, site by site (each verified against master's task counts — chain 64957,
star 98465 — which the collapse now reproduces exactly):

1. **`Seed` IS part of equality + hash** (`semantic_equals`, `semantic_hash`, `map_field_values`):
   the two old types never compared equal, so a translator seed and a re-enumeration of the same
   leg-set never interned. Preserved — else they'd suddenly intern and fire the RFC-037 cross-group
   merge the two-type design never did (measured to blow the STAR past budget). `Seed` is excluded
   ONLY from `Evaluate` (merged-row semantics are identical).
2. **`GetCorrelatedToOfValue` skips `Seed=true`** (`value_correlation`): the old
   `JoinMergeResultValue` stored its aliases as plain fields this walk never read, so a seed
   reported NO correlations — load-bearing (a seed's named legs are sources, not a correlation
   set). Reporting them inflates every enclosing select's correlation set and exploration (measured
   +~32% planner tasks, tipping the STAR). A re-enumeration merge (Seed=false) still reports its
   aliases — the live set the partition rule's exact branch reads.
3. **`composeFieldOverJoinMerge` / `joinResultValueIsReversed` gate on `Seed=true`** (the exact set
   the binary `JoinMergeResultValue` covered): a re-enumeration merge must NOT be rewritten /
   reversal-checked (it wasn't, as a distinct type). `composeFieldOverJoinMerge` reads the binary
   inner = `Aliases[1]`; `joinResultValueIsReversed` reads SQL-first = `Aliases[0]`.
4. **`PartitionSelectRule` keep-all-vs-exact keys off `Seed`** (was the binary type): a seed keeps
   ALL lower aliases live (it hides its projection); a re-enumeration keeps only the named live set.
5. **Set-based (canonical) equality/hash** among same-provenance merges (Graefe condition 1 —
   commutative/associative): the `Aliases` field keeps INSERTION order for the order-dependent
   consumers in (3); equality/hash are order-independent.

Behavior-preserving: same plans, same task counts (master-identical). Cleanup + prerequisite,
nothing more.

## Performance

No change — the collapse is behavior-preserving. The ≥5-way budget is addressed by the separate
pruning RFC.

## Test plan

- **Canonical-value property** (values unit): `JoinMergeAllValue[A,B]` ≡ `JoinMergeAllValue[B,A]`
  under `SemanticEqualsUnderAliasMap` with equal `SemanticHashCode` (commutative — Graefe
  condition 1); a multi-leg permutation test that every ordering of a leg-set yields one
  canonical value (and identical `Evaluate` output).
- **Seed re-stamp regression** (PartitionSelect unit, updated): a flattened seed naming an
  unbound alias keeps all lowers live, and the merge-case upper is re-stamped to a
  `JoinMergeAllValue` naming only bound aliases (no stale unbound alias).
- **Zero regression**: full `just test` green — every FDB join test, the cross-engine harness,
  plandiff (no plan-shape change at any arity), determinism 10×. The collapse must change no
  result and no plan.
- NO `distinctRefs`/task-count polynomial assertion — those claims were false and are removed.

## Follow-up (separate RFC) — the actual ≥5-way budget fix

Port Java's bipartition-lattice pruning into `PartitionSelectRule`: `shouldDeferCrossProducts`
+ `shouldJoinRightDeep` (`PartitionSelectRule.java:92,122`), bounding enumeration at the source
— the 1:1 Java-aligned fix. Do NOT extend broad-merge-under-winners (a Go-only invention Java
lacks) and do NOT decouple exploration from optimization. Tracked in TODO.md.
