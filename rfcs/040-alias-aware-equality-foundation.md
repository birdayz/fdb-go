# RFC-040: Alias-aware expression/predicate equality + alias-invariant hashing

**Status:** Draft
**Epic:** RFC-038 — the **foundation** PR. Empirically-confirmed prerequisite for PR-A
(RFC-039 `memoEqual`) and thus the whole multi-way-join-ordering goal.
**Framing:** closes a **Java divergence**. Java threads an `AliasMap` (via `ValueEquivalence`)
through `equalsWithoutChildren` and makes `semanticHashCode` alias-invariant; Go does neither at
the predicate/expression level. "Java is the reference."

## Problem

Go's relational equality and hashing are **alias-sensitive**, so two sub-expressions identical
except for quantifier alias names compare **unequal** and **hash differently**:

* `LogicalFilterExpression.EqualsWithoutChildren` (`logical_filter.go:94`) does
  `_ = aliases` — it **discards the alias map** and compares predicates via reference-comparable
  `PredicateEquals` (no alias translation). Proven: two filters identical but for their
  quantifier alias (referenced in the predicate) return `false` even under the correct
  `{q_a↦q_b}` map.
* `HashCodeWithoutChildren` folds alias-bearing predicate text → alias-variants hash
  differently. Since the dedup path gates equality on hash equality
  (`Reference.Insert`: `hash==hash && SemanticEquals`), alias-variants are never even compared.

Consequence: rule-rewritten equivalents (fresh aliases) never intern together → RFC-037
cross-group merge fires ~once → no broad merging → no multi-way-join sub-product sharing. This
RFC removes that floor. It is the deepest, highest-blast-radius change in the codebase: it
alters the equality + hashing contract **every** memoized expression depends on.

## Investigation

**Java (the design to port).** `RelationalExpression.equalsWithoutChildren(other, AliasMap)`
threads the map into predicate/Value comparison via
`QueryPredicate.semanticEquals(other, ValueEquivalence.fromAliasMap(aliasMap))`;
`Value.semanticEquals` translates `QuantifiedObjectValue`/`FieldValue` alias references through
the map. Crucially, `semanticHashCode` is **alias-invariant** — `QuantifiedObjectValue.
hashCodeWithoutChildren` returns only `BASE_HASH` (excludes the alias), `FieldValue` hashes the
field path not the child, `ValuePredicate` hashes `value.semanticHashCode()` (alias-invariant).
The contract (`Correlated.java:186`): `a.semanticEquals(b, m) ⟹ a.semanticHashCode() ==
b.semanticHashCode()` — hash is identical regardless of alias bindings.

**Go already has the Value-layer machinery** (this narrows the work a lot):
* `value_equivalence.go`: `ValueEquivalence` interface, `ConstrainedBoolean`,
  `EmptyValueEquivalence`, and **`AliasMapValueEquivalence`** (maps `QuantifiedObjectValue`s
  through an `AliasMap`) — Java's `fromAliasMap`.
* `value_semantic_equals.go`: `ValueSemanticEquals(a, b, veq)` — alias-aware Value comparison.

So the Value layer is alias-aware **already**; it's simply **not used** by predicate or
relational equality, and hashing is not alias-invariant.

## Fix (three coordinated changes)

### 1. Alias-aware predicate equality
Add `PredicateSemanticEquals(a, b QueryPredicate, veq ValueEquivalence) ConstrainedBoolean`
(Java `QueryPredicate.semanticEquals` via `ValueEquivalence`): structural class/op match, then
compare operand/child **Values** via `ValueSemanticEquals(.., veq)` and recurse on child
predicates. Cover the predicate types: `ValuePredicate`, `ComparisonPredicate` (operand + RHS
Value), `AndPredicate`/`OrPredicate`/`NotPredicate` (recurse), `ConstantPredicate`,
`ExistsPredicate` (alias via `veq.IsDefinedEqualAlias`). `PredicateEquals` (the existing
alias-blind helper) is kept as `PredicateSemanticEquals(a, b, EmptyValueEquivalence())` for
callers that want identity semantics, or migrated.

### 2. Relational `EqualsWithoutChildren` threads the AliasMap
For predicate-bearing expressions (`LogicalFilterExpression`, `SelectExpression`, and any
other with predicates/correlated Values), build `AliasMapValueEquivalence` from the incoming
`aliases` and compare predicates via `PredicateSemanticEquals` (Java
`SelectExpression.equalsWithoutChildren` zips predicates under the alias map). Result-Value
comparison likewise routes through `ValueSemanticEquals`. Delete the `_ = aliases` discards.

### 3. Alias-invariant `HashCodeWithoutChildren`
Make hashes exclude specific alias names so alias-variant-equal nodes hash identically:
* `QuantifiedObjectValue.HashCodeWithoutChildren` (and any `QuantifiedValue`) — exclude the
  alias (hash only the type/structure), mirroring Java's `BASE_HASH`.
* `FieldValue` — hash the field path, not the alias-bearing child.
* predicate/expression `HashCodeWithoutChildren` — fold child **semantic** (alias-invariant)
  hashes (`value.SemanticHashCode()`), not alias-bearing `Explain()` text.

This restores the invariant the dedup path needs: **equal-up-to-alias ⟹ equal hash**.

## The HashConsistency invariant (the linchpin)

The whole memo relies on: `SemanticEquals(a, b, m) true ⟹ HashCodeWithoutChildren` agree (so
hash-gated dedup never misses an equal pair). Today that holds only because both are
alias-sensitive. After this change both become alias-aware/invariant **together** — they must
stay mutually consistent. The existing `FuzzSemanticEquals_Properties` fuzz target is extended
to generate alias-variant expressions and assert: `SemanticEquals(a, b, m) ⟹ equal semantic
hash`, now across non-trivial alias maps. This is the primary correctness gate.

## Test plan (heavy — deepest change in the codebase)

1. **HashConsistency fuzz** (primary): `SemanticEquals true ⟹ equal hash`, generating
   alias-variant expressions + random alias maps. 200k+ execs, 0 violations.
2. **Alias-equivalence unit tests**: `Filter(QOV(q_a)=1)` vs `Filter(QOV(q_b)=1)` now
   `EqualsWithoutChildren` under `{q_a↦q_b}` **and** hash-equal; `FieldValue` over QOV; nested;
   `ComparisonPredicate`; And/Or/Not. Negatives: different field/constant/op stay distinct.
3. **stress-1M before/after**: row results byte-identical; durations within thresholds.
4. **determinism 10×** (`--nocache_test_results`): identical plan hashes.
5. **non-join plan stability — alias/id-canonicalized**: a corpus (single-table, scalar
   subquery, aggregate, IN, group-by) — row results byte-identical and EXPLAIN identical after
   canonicalizing alias/group ids (more interning renumbers ids; real plan-shape changes fail).
6. **interning-improves probe**: two independently-built `Filter(QOV=1, scan)` with different
   quantifier aliases now `MemoizeExpression` to the **same** Reference (was two).
7. full conformance (46 targets), Graefe + Torvalds + @claude.

## Risks / mitigations

* **Over-broad equality → wrong plans.** The danger of alias-aware equality is interning
  genuinely-different expressions. Mitigated by porting Java's exact `ValueEquivalence`
  semantics (alias correspondence must be in the map; non-`QuantifiedValue`s compare by
  structure) + the non-join EXPLAIN-stability corpus + conformance.
* **Hash collisions / inconsistency.** The fuzz target is the gate; a single violation blocks.
* **Blast radius.** Every predicate-bearing expression + predicate + QOV/FieldValue hashing.
  Landed as ONE PR (the contract must change atomically — equality and hashing can't diverge),
  but implemented type-by-type with tests, full gate before merge.
* **Determinism.** Hashing stays a pure structural function (now alias-invariant); no map order.

## Out of scope
PR-A (`memoEqual` wiring into the memo), PR-C (join enumeration), PR-D (cost + proof) — this RFC
only makes equality/hashing alias-aware/invariant so those can build on it.
