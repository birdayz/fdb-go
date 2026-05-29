# RFC-039: Alias-aware memo equivalence (PR-A of the RFC-038 epic)

**Status:** IMPLEMENTED. `expressions.MemoEqual` ported and wired into the memo's
three structural-equivalence compare sites (`memoizeNonLeaf`, `refContains`,
`findEquivalentRef`), replacing the alias-sensitive
`EqualsWithoutChildren(empty)+sameChildRefs` pair. Implementation review:
Graefe ACK, Torvalds ACK. Review caught and fixed: an un-Java `seen` cycle
guard (dropped — Java's `containsAllInMemo` is guardless, the memo DAG is
acyclic via the merge `reachable`/`mergeable` guard), exploratory-only child
containment (now matches Java's exploratory↦exploratory **and** final↦final
two-call structure), and a missing external-correlation negative test (added:
two filters correlated to different outer aliases must NOT intern). Activation
proven by `memo_activation_test` (K=6 alias-variant filters → 1 shared
Reference) with zero plan-shape regression (plandiff conformance green) and
no perf regression (stress-1M before/after within noise).

Design accepted v4 (Graefe ACK, Torvalds ACK) — a faithful port of Java
`Reference.isMemoizedExpression` (correlatedTo → canCorrelate → bindIdentities/combine →
directional `containsAllInMemo` with capped `ChildrenAsSet` permutation → `equalsWithoutChildren`
under the built alias map). Review caught, across 4 rounds: dropped `correlatedTo` guard,
over-interning existential child match, missing `bindIdentities`/`combine`, missing
`ChildrenAsSet` branch, absent `AliasMap.With`, and a non-canonicalized EXPLAIN gate.
**Epic:** RFC-038 PR-A. It closes a Java divergence (more correct interning) on its own merits;
it becomes *the lever* that makes RFC-037 cross-group merging fire broadly **only** once the
in-memo recursive child match (Fix #3) lands — pointer-equality alone leaves the bottom-up hole
open (Torvalds #2). Sold as "more dedup + divergence fix" first, "the lever" once #3 is proven.
**Framing:** this **closes a Java divergence** (ports Java's alias-aware `isMemoizedExpression`),
*not* a Go-only invention. "Java is the reference."

## ⚠️ Foundation prerequisite discovered (empirical, post-acceptance)

Implementation surfaced a hard blocker the design (and 4 review rounds) assumed away:
**`memoEqual` requires alias-aware `EqualsWithoutChildren` and alias-invariant
`HashCodeWithoutChildren` — neither exists in Go today.** Proven directly:

* `LogicalFilterExpression.EqualsWithoutChildren` (`logical_filter.go:94`) literally
  `_ = aliases // see doc comment` — it **discards the alias map** and compares predicates via
  alias-*sensitive* `PredicateEquals`. A diagnostic built two filters identical except their
  quantifier alias (referenced in the predicate): `EqualsWithoutChildren` under the *correct*
  `{q_a↦q_b}` map still returned **false**.
* `HashCodeWithoutChildren` folds predicate text including alias names → the two filters hash
  **differently**. Since the whole dedup architecture gates equality on hash equality
  (`Reference.Insert`: `hash==hash && SemanticEquals`), alias-variants are never even compared.

So `memoEqual` (the faithful `isMemoizedExpression` port) is *necessary but not sufficient*:
the load-bearing work is making **every** `RelationalExpression.EqualsWithoutChildren` thread
the `AliasMap` into `PredicateEquals`/Value equality, and making `HashCodeWithoutChildren`
alias-invariant — across all expression, predicate, and Value types. That is the **full
TODO 7.1 "alias-namespace unification"** deep-architectural rebuild, a large multi-PR effort
that must land **before** `memoEqual` is wired in.

**Status of PR-A:** `AliasMap.With` (a clean primitive) shipped; `memoEqual` drafted+reviewed
and held pending the foundation; the foundation (alias-aware equality + alias-invariant hashing)
is now the real first PR of the epic, ahead of this one. Sequence becomes:
**#213 → [Foundation: alias-aware equality/hashing] → PR-A (`memoEqual` wiring) → PR-C → PR-D.**

**Foundation is NARROWER than a from-scratch rebuild — Go already has the Value machinery.**
`value_equivalence.go` defines `ValueEquivalence` (incl. `AliasMapValueEquivalence`, which maps
`QuantifiedObjectValue`s through an `AliasMap`) + `ConstrainedBoolean`; `value_semantic_equals.go`
defines alias-aware `ValueSemanticEquals(a, b, veq)`. So the *Value* layer is alias-aware
already. The foundation's real work is to **use** it where today's code doesn't:
1. **Predicate equality alias-aware** — `PredicateEquals` (reference-comparable) must gain a
   `ValueEquivalence`-threaded variant routing operand/Value comparisons through
   `ValueSemanticEquals` (Java `QueryPredicate.semanticEquals` via `ValueEquivalence.fromAliasMap`).
2. **Relational `EqualsWithoutChildren` thread the AliasMap** — `LogicalFilterExpression`
   (`logical_filter.go:94 _ = aliases`), `SelectExpression`, etc. must build
   `AliasMapValueEquivalence` from the incoming map and compare predicates alias-aware (Java
   `SelectExpression.equalsWithoutChildren` zips predicates via `semanticEquals(.., aliasMap)`).
3. **Alias-invariant `HashCodeWithoutChildren`** — `QuantifiedObjectValue` hash must EXCLUDE the
   alias (Java returns only `BASE_HASH`); predicate/expression hashes fold child *semantic*
   (alias-invariant) hashes. This restores the hash↔equality consistency the dedup path needs.
Still a substantial, high-blast-radius PR (every predicate-bearing expression + predicate + QOV
hashing + the HashConsistency fuzz invariant), but it leverages existing infra rather than
rebuilding it — and is its own RFC.

## Problem

Go's memo interning is **alias-sensitive**; Java's is **alias-aware**. Two sub-expressions
that are identical except for differing **quantifier alias names** are recognized as the same
memo member by Java, but treated as distinct by Go. Because rules mint fresh quantifier aliases
on every rewrite (e.g. `PushFilterThroughDistinctRule` builds its pushed `Filter` with a new
`ForEachQuantifier`), Go interns equivalent rewrites into *separate* References — which is why
RFC-037 cross-group merging fires only ~once regardless of input count (RFC-037 "Reach"), and
why multi-way sub-product sharing (RFC-038 PR-C/D) can't work until this is fixed.

## Investigation (Java vs Go)

**Java — alias-aware (`Reference.java:741–830`, `isMemoizedExpression`).** For each candidate it
builds an `AliasMap` from the two expressions' quantifier aliases and compares the node under it:

```java
// Reference.java ~812-829
final AliasMap.Builder aliasMapBuilder = combinedEquivalenceMap.toBuilder(quantifiers.size());
for (int i = 0; i < quantifiers.size(); i++) {
    if (!quantifier.getRangesOver()
            .containsAllInMemo(otherQuantifier.getRangesOver(), aliasMapBuilder.build())) {
        return false;                                   // child refs must match in-memo
    }
    aliasMapBuilder.put(quantifier.getAlias(), otherQuantifier.getAlias());   // ← THE alias map
}
return member.equalsWithoutChildren(otherExpression, aliasMapBuilder.build()); // ← alias-aware
```

Even when entered with `AliasMap.emptyMap()` from `CascadesRuleCall.memoizeExploratoryExpressions`,
`isMemoizedExpression` builds its **own** map from the quantifier aliases. So a fresh-aliased
rewrite interns into the existing equivalent member.

**Go — alias-sensitive (`memo.go`).** `memoizeNonLeaf` / `refContains` and RFC-037's
`findEquivalentRef` compare with:
```go
member.EqualsWithoutChildren(expr, expressions.EmptyAliasMap()) && sameChildRefs(member, expr)
```
`EmptyAliasMap()` — the node's own quantifier aliases are never mapped, so a node whose
predicate/value references its own quantifier alias (`FieldValue(QOV(q$a), …)` vs `QOV(q$b)`)
compares unequal. (`Reference.Insert`'s `SemanticEquals` *fallback* is closer — it maps child
aliases when recursing — but it still passes the **incoming** map, not the node's **own**
quantifier map, to the top-level `EqualsWithoutChildren`. So it too misses same-level alias
canonicalization. This is the precise gap.)

## Fix

Port Java's `isMemoizedExpression` **faithfully and in full** — including the two pieces v1
dropped, which both reviewers flagged: the `correlatedTo` equivalence guard and the recursive
in-memo child match (`containsAllInMemo`). Pointer-equality on children is **not** sufficient.

```go
// memoEqual reports whether member and expr are the same memo member.
// Faithful port of Java Reference.isMemoizedExpression (4.11.1.0):
//   1. same class / node info,
//   2. same external correlation set (the correlatedTo guard, Java 764–773),
//   3. child References match IN-MEMO (recursive, alias-aware — NOT pointer
//      equality), building the node's own quantifier-alias map as it goes,
//   4. member.EqualsWithoutChildren(expr, builtAliasMap).
func memoEqual(member, expr RelationalExpression, equiv *AliasMap, seen map[[2]*Reference]bool) bool {
    if member.HashCodeWithoutChildren() != expr.HashCodeWithoutChildren() { return false }
    mq, eq := member.GetQuantifiers(), expr.GetQuantifiers()
    if len(mq) != len(eq) { return false }
    // (2) external-correlation guard (Java Reference.java:764–773): see
    // correlatedToMatches below — definesOnlyIdentities fast path else
    // getTargetOrDefault-map member's external correlations and require
    // equality with expr's. Prevents interning exprs that differ only in an
    // outer correlation (…=a.x vs …=b.y).
    if !correlatedToMatches(member, expr, equiv) { return false }
    // (2b) bindIdentities + combine (Java Reference.isMemoizedExpression,
    // before child matching): verify canCorrelate() agrees, then bind the
    // shared external (identity) correlations into the equivalence map so a
    // node correlated to a SIBLING quantifier matches correctly. equiv :=
    // equiv.combine(identitiesFor(member.correlatedTo ∩ expr.correlatedTo)).
    // Without this, sibling-correlated nodes are mis-matched.
    if member.CanCorrelate() != expr.CanCorrelate() { return false }
    equiv = combineIdentities(equiv, member, expr)
    // (3) match children. Order-significant children: positional. Children-
    // as-set (unions / inner joins): permutation match (Quantifiers.findMatches).
    // Either way each child pair is compared via DIRECTIONAL childRefsMatchInMemo,
    // threading `seen` for recursion safety; and the node's own quantifier-alias
    // map `b` is built as pairs are accepted (With returns ok=false on conflict
    // ⇒ that pairing fails). Returns the final `b` used for step (4).
    b, ok := matchChildrenInMemo(mq, eq, equiv, seen)   // positional or set-permuted
    if !ok { return false }
    return member.EqualsWithoutChildren(expr, b)         // (4) alias-aware
}
```
`matchChildrenInMemo` threads `b := b.With(mAlias, eAlias)` per accepted pair (honoring the
`ok` return — a conflicting binding fails that pairing), calls `childRefsMatchInMemo` for each
child, and for `ChildrenAsSet` nodes tries permutations (first consistent one wins), **capped at
`MaxPermutationChildren` (8)** — the same bound `SemanticEquals` already uses; above it, fall
back to positional (no `n!` blow-up, mirroring Java `Quantifiers.findMatches`' bounded fan-out).
The directional-containsAll × permutation nesting only runs under this cap and the
`HashCodeWithoutChildren` early-exit, so the combinatorial worst case is bounded.

**`childRefsMatchInMemo(a, b, equiv)` is DIRECTIONAL `containsAllInMemo`, not existential**
(Graefe v2 catch — "any member matches any member" over-interns refs that share one incidental
equivalent member but otherwise differ):
```go
// Faithful port of Reference.containsAllInMemo: every member of `b` must be
// matched by SOME member of `a` under equiv (directional containsAll), with a
// fast pointer path. NOT symmetric/existential.
func childRefsMatchInMemo(a, b *Reference, equiv *AliasMap, seen map[[2]*Reference]bool) bool {
    a, b = a.Canonical(), b.Canonical()
    if a == b { return true }
    key := [2]*Reference{a, b}
    if seen[key] { return true }      // recursion guard (DAG is acyclic — item 6)
    seen[key] = true
    for _, mb := range b.Members() {
        ok := false
        for _, ma := range a.Members() {
            if memoEqual(ma, mb, equiv, seen) { ok = true; break }
        }
        if !ok { return false }
    }
    return true
}
```
This closes Torvalds #2 (self-cycle-guard fresh-`InitialOf` children) **without** Graefe's
over-interning hole. Recursion is bounded by the `HashCodeWithoutChildren` early-exit in
`memoEqual`, the `seen` pair-set, and the acyclicity the cycle test (item 6) asserts.

**`correlatedToMatches` — port Java exactly** (`Reference.java:764–773`): if both
`definesOnlyIdentities()`, compare correlated-to sets directly; otherwise map each of `member`'s
external correlations via `equiv.getTargetOrDefault(alias, alias)` and require the mapped set to
equal `expr`'s external correlation set. (Not the vague "correspond under equiv".)

**`ChildrenAsSet` branch** (Graefe v2): when both nodes mark children as a set (e.g. unions,
inner joins), match quantifiers by **permutation** (reuse `permute` / the
`composeChildAliasPairs` permuted path) rather than positional pairing — mirrors Java's
`Quantifiers.findMatches`. Positional pairing is only correct for order-significant children.

Sites updated to use `memoEqual` (entered with `EmptyAliasMap()` at the top, like Java):
* `memo.go` `memoizeNonLeaf`, `refContains`;
* `memo_merge.go` `findEquivalentRef`.

`memoizeLeaf` unchanged (no quantifiers). **`Reference.Insert`/`InsertFinal`:** the fast
pointer path stays; the `SemanticEquals` fallback is **augmented** to map the node's own
quantifier aliases (today it seeds `EmptyAliasMap()` at the top node, missing same-level
canonicalization) — or gains a `memoEqual` clause. Final-member dedup uses the same alias-aware
rule (stated explicitly; no divergence between exploratory and final dedup).

**New primitive — `AliasMap.With(s, t)`** (immutable copy-on-write add): does not exist today;
`AliasMapOf` *panics* on the duplicate-alias case that shared References now legitimately
produce, and rebuilding per candidate is wasteful. Add `With` (copies the two small maps, adds
one pair, returns a new map; on a conflicting binding returns the map unchanged + ok=false so
callers treat it as not-equal — consistent with `composeChildAliasPairs`).

## Performance

`memoEqual` is the same cost class as the current check plus building a small `AliasMap` (≤ a
few quantifiers) — O(quantifiers) per candidate, gated by the `HashCodeWithoutChildren` early
exit. Net: **fewer** References (more interning) ⇒ less exploration. Must be verified against
the 1M stress baseline (no regression on simple queries, which have no fresh-alias rewrites).

## Test plan — incl. Torvalds's mandatory blast-radius gate

This changes the equality the **whole memo** depends on (every memoized expression, not just
joins). The hazard is an *unrelated* plan silently re-interning.

1. **stress-1M before/after** — **row results byte-identical**; durations within thresholds.
2. **determinism 10×** (`--nocache_test_results`) — identical plan hashes.
3. **non-join plan stability — alias/id-canonicalized** (Torvalds): EXPLAIN is **not** expected
   byte-identical, because more interning reduces Reference count and `merge` picks the survivor
   by `nextID`, renumbering memo/group ids → cosmetic churn even on unrelated plans. The gate is:
   row results byte-identical **and** EXPLAIN identical **after canonicalizing aliases/group-ids**
   (rename q$N / group-ids to positional canonical form before diffing), across a corpus of
   single-table / scalar-subquery / aggregate / IN / group-by queries. A real plan-shape change
   (different operators/order) fails; pure renumbering does not.
4. **correlatedTo tests** (Graefe): (a) negative — two exprs identical except an external
   correlation (`…=a.x` vs `…=b.y`) must **NOT** intern; (b) positive-under-non-identity-map —
   exprs whose external correlations DO correspond through a non-identity `equiv` MUST still
   intern (exercises the `getTargetOrDefault` translation arm, not just `definesOnlyIdentities`).
   Plus a `ChildrenAsSet` test: a union with swapped-order equivalent branches interns.
5. **`PushFilterThroughDistinct` termination** (Torvalds #4): explicit regression that the
   rule reaches fixpoint under `memoEqual` (more-permissive interning must not change termination).
6. **post-interning cycle test** (Torvalds): broader interning ⇒ more shared children ⇒ feeds
   RFC-037 `reachable`/cycle guard and the self-cycle guard; assert no DAG cycle and the merge
   `mergeable` guard still holds after heavy interning.
7. **broad-merge proof** — the RFC-037 union-of-K-equivalent-branches benchmark now cascades to
   **K−1 merges** (was 1); `tasksRun` merge-on vs off shows a non-trivial delta. This is what
   promotes PR-A from "more dedup" to "the lever" — only valid once Fix #3 (in-memo child match)
   lands and closes the bottom-up hole.
8. **alias-equivalence unit tests** — `Filter(P, →scan)` built with two different quantifier
   aliases intern to one Reference; the self-cycle-guard fresh-`InitialOf` child case; negative
   cases (genuinely different predicates stay distinct).
9. full conformance (46 targets), Graefe + Torvalds + @claude.
10. **fuzz** — `memoEqual` reflexive/symmetric and consistent with `HashCodeWithoutChildren`
    (hash equal whenever `memoEqual` true).

## Risks / mitigations

* **Over-interning** (treating non-equal exprs as equal) → wrong plans. Mitigated by porting
  Java's exact algorithm (children-in-memo + node alias map) and the non-join EXPLAIN-stability
  corpus + fuzz hash-consistency.
* **Determinism** — `memoEqual` is a pure function of structure + built alias map; no map
  iteration. 10× gate.
* **Needs `AliasMap.With`** (immutable add) — add if absent, or build via existing `AliasMapOf`.
