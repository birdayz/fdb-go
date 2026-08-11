# RFC-231: one fused shape has one name, and no consumer may key on it

**Status:** IMPLEMENTED (rev 2 — rev 1's "nothing moved" evidence is RETRACTED, see §5)
**Scope:** the mint of a fused nested `FieldValue`, and the name-keyed reads downstream of it.
**Relates to:** RFC-225 (nested descent reads by ordinal), RFC-227 (the hidden sort column is named by the path it reads), RFC-228 (a leg column lookup declines on an ambiguous name), RFC-229 (a column states its own name), RFC-230 (nested grouping keys — shares the arity census).

## 1. The divergence

`field(field(x, p1), p2)` fuses into ONE `FieldValue` carrying the whole path. Go minted that shape in two places and they named it differently:

| mint | `Field` |
|---|---|
| `values/simplifier_value.go` `composeFieldOverField` | `fused.Last().Field` — the leaf |
| `expr/expr.go` `fuseNestedAccessors` | `out := *fv` — the struct root |

So `fv.Field` on a fused value answered differently depending on which mint produced it. This is the defect class RFC-227/228/229 removed from the naming *authorities*, surviving in the *mint*.

**Java is unambiguous, and it is the simplifier that was right.** Java's fused `FieldValue` has no root-name accessor at all. It carries a `FieldPath`, and the one single-name question askable of it is `getLastFieldName` (`FieldValue.java:134-135`), delegating to `FieldPath.getLastFieldName` (`:463-466` — `getOptionalFieldNames().get(size()-1)`). Its complement `getFieldPrefix` (`:450-454`) returns everything *but* the last, which is what makes "last" the distinguished accessor rather than an arbitrary pick. Verified against the pinned 4.12.11.0 checkout (`git describe --tags` → `4.12.11.0`, HEAD `257aa83ca`).

`fuseNestedAccessors` now names the fused value after its leaf.

## 2. The half rev 1 got wrong

Naming the value after its leaf is correct in isolation and Java-backed. It also **moved a defect rather than fixing one**, and the argument that concealed the move is the subject of §5.

`cascades_generator.go#operandTypeNameViaDesc` took a `FieldValue`'s `Field` straight into a top-level descriptor lookup with **no arity test at all**:

```go
case *values.FieldValue:
    if desc != nil {
        if n := protoFieldTypeName(desc, t.Field); n != "UNKNOWN" { return n }
    }
```

With `CREATE TYPE AS STRUCT nn (sk BIGINT, co STRING)` and `CREATE TABLE t (id BIGINT, n nn, sk DOUBLE, PRIMARY KEY (id))`, `SELECT n.sk + 1 FROM t`:

```
before this RFC : type="BIGINT"   PASS
rev 1           : type="DOUBLE"   FAIL   (want BIGINT)
rev 2           : type="BIGINT"   PASS
```

Master was right **by accident**. The struct root `N` is a `MessageKind`, so `protoFieldTypeName` answered `UNKNOWN`, the guard skipped, and the value's own leaf type won. A leaf name is not safe that way: `SK` names a real flat column of a different type and wins the lookup.

**Java settles the direction, and it is not the descriptor.** `FieldValue.computeResultType` (`FieldValue.java:143-148`) is `fieldPath.getLastFieldType()`. There is no name-keyed re-derivation upstream of it anywhere — `operandTypeNameViaDesc` is a Go-only invention. So the fix is not a heuristic: a multi-accessor reference states its own type, and the descriptor is not consulted.

## 3. The structural fact that makes this a class

`fv.Child == nil` is read across this tree as "this is a flat reference", and used as the gate that lets a name-keyed lookup proceed. **It is not an arity gate.** `expr.sourceRelativeColumnRef` builds its root with `NewFieldValueWithResolvedOrdinalInDomain`, which sets no `Child` — a reference needing no correlation has no quantifier object to hang one on — and `fuseNestedAccessors` then appends the descent to `Resolved`. The result is a reference of arbitrary depth with a nil `Child`.

Childlessness answers a question about **correlation**. Arity answers a question about **how many segments the reference spelled**. A consumer wanting the second must test `len(Resolved.Accessors)`. Pinned by `TestFusedNestedReferenceCanBeChildless`, which asserts both arms — the childless fused value and the correlated fused value that does carry a QOV child — so neither shape can be relied on to report the arity.

## 4. The audit, and what it actually found

Five sites were nominated as the same class. Every one was measured by instrumenting it and running the full `//pkg/relational/...` suite including the FDB corpus, rather than re-read.

| site | nominated as | MEASURED verdict |
|---|---|---|
| `groupByOutputBaker` (aggOrds) | unguarded, highest risk | **already arity-gated** 27 lines above the name read |
| `clusterProjectionsResolvable` | hit→miss | **already arity-gated** — `!SourceRelativeBaked()` declines a multi-accessor value before the name read |
| `rewriteUnnestPredicate` | struct member vs UNNEST alias | **reached, and rev 1 FIXED a live wrong read here** — see below |
| `rebaseUnnestOuterLegPredicateOrdinal` | drops the suffix | unguarded; **0 hits** across the whole suite |
| `deriveColumnsFromProjection` name arms | same collision class | unguarded; **0 hits** across the whole suite |

Two of the five were already correct. That is worth recording because the nomination was confident about both.

**The unnest arm is the one that mattered, and it moves in rev 1's favour.** The element-substitution arm replaces a reference that IS the bound element with the element's QOV, selecting that arm by comparing the reference's single display name against the binding's alias. The root of a descent through an unnest binding **is the alias**, so under the old mint the comparison matched for *every* member reference:

```
old mint: ZZPROBE Field="I" accs=2 path=/I/SKU asAlias="I"  MATCHED=true
rev 2   : ZZPROBE Field="SKU" accs=2 path=/I/SKU asAlias="I" MATCHED=false
```

`WHERE i.sku = 'x'` over `orders.items AS i` was silently reading the whole element. The suite did not notice because the only test covering the shape asserts that the query *plans*, never what it returns.

**State this precisely, because the loose version is worse than useless.** What is removed is a **measured wrong READ at the arm**, whose end-to-end manifestation is **masked by the separate §7 zero-rows defect** — not a closed wrong-rows bug. No query's rows are demonstrably corrected: the emblematic shape returns zero rows identically on `origin/master`, rev 1 and rev 2. Writing this up as "fixes wrong rows" would be stronger than the evidence and would invite whoever reads the merge record to conclude §7 is already handled.

Nor does the row-level coverage in this change reach the gate. The `[x y]` control asserts `SELECT i.sku FROM orders, orders.items AS i` — a **projection** — while every caller of `rewriteUnnestPredicate` is a **predicate** path (`unnest_gather.go:283`; `cascades_translator.go:3086, 3101, 3264, 3477, 3531`). So this arm sits in the same epistemic class as the two 0-hit sites: gated fail-closed, argued rather than corpus-proven, **no reproducer**. The census entry says so in those words.

Naming the value after its leaf narrows the collision to the shapes where a member is spelled like the alias (`AS sku` … `sku.sku`), which the resolver currently refuses `42702` before any consumer sees it. That refusal is now **pinned as a negative result** (`TestFDB_NestedMemberSpelledLikeItsUnnestAliasIsRefused`), because a gate's reachability resting on an unasserted refusal is how a fence gets removed by someone who does not know it is load-bearing. The arm is gated on arity regardless, so it is correct without depending on the refusal.

The two 0-hit sites are likewise gated in the fail-closed direction. **That is a statement about the corpus's coverage, not a proof of unreachability**, and it is recorded that way in the census rather than dressed up as coverage.

## 5. RETRACTED: "nothing moved" was a tautology, not evidence

Rev 1 argued the change was safe because the corpus was unchanged (`pass=69 fail=0 skip=169 queries=1775`) and no plan-golden line moved. **That argument cannot fail**, and it is what let §2 through review.

The channels it observed are structurally incapable of carrying `Field` for a fused value:

- **EXPLAIN** renders a multi-accessor node from `Resolved.Accessors`, dot-joined, and never from `Field` (`values.go:1994-2010`).
- **The semantic hash** folds ORDINALS only for a resolved node; `Field` is hashed exclusively on the lazy branch, and a fused value always has `Resolved != nil` (`semantic_hash.go:124-131`).
- **Structural equality** is ordinal-only — `FieldPath.Equals` explicitly does not compare the per-step `Field`, matching Java `FieldValue.java:411-420`, whose `ResolvedAccessor.equals` is `getOrdinal()`-only (`:675-689`).

So "no golden line moved" was a restatement of the goldens' construction. A green there is compatible with any `Field` whatsoever, including a wrong one — and there was a wrong one.

What the corpus's silence *does* say is that **the corpus lacks the colliding shape**: no query in it puts a struct member and a top-level column of a different type under one spelling. That is a fact about coverage. Every test added by this RFC is built around that collision precisely because a fixture without it is green either way — which is stated in each fixture's header, in the imperative, so the collision is not "simplified away" by a later reader who cannot see what it is for.

**The dimension that was unprobed** was not nesting and not naming. It was *a consumer keying on the name of something that has a path* — pinned now by `TestFDB_FusedNestedReferenceSurvivesNameKeyedConsumers`, which drives a fused reference through sort, predicate and aggregate-operand consumers with the collision live, and which goes red when the path is flattened at the mint.

## 6. What the census can and cannot see

`pkg/docscheck/accessor_arity_census_test.go` swept for `len(...Accessors)`, and its header already warned that a site discarding a chain **without testing its length** is invisible to that sweep.

That blind spot is no longer hypothetical: `operandTypeNameViaDesc` is exactly such a site, in a file the census already covers, and it is where the live defect turned out to be. The sweep could not see it *because* it was unguarded — the census finds sites that ask about arity, so a site that never asks is absent by the same property that makes it wrong.

Consequently the population **grows** when an unguarded read is fixed: 36 symbols → 39, and 52 grep lines → 56. A rising count here is the instrument working. The complement sweep — reads of `.Field` on a value that may be fused — is the one that would have found this, and it has not been done; that is recorded in the header rather than left as an implication.

### 6.1 Reconciliation with RFC-230

RFC-230 (#719) landed first, so this change carries the reconciliation and is rebased onto it. Both changes edit the census; the conflict was the design, and it is resolved as arithmetic rather than by taking a side.

RFC-230's entry for `deriveColumnsFromProjection` recorded `(d)` LIVE DEFECT and instructed that it be **re-read rather than carried** if the fix landed afterwards. It did. The entry moves to `(c)` and is **kept, not deleted** — a `(d)` outliving its defect and a silently-vanished entry are the same unwatched-revival failure, and the class moved because the *code* moved, not because the verdict was revised. RFC-230's `q.s.label` scalar control and its top-level `top BIGINT ARRAY` / `topbin BYTES` controls are preserved verbatim: they are what identify the mechanism as nesting-specific rather than an array- or bytes-typing gap.

Two independent movements apply:

```
RFC-230 base   (a) 13  (b) 0  (c) 22  (d) 1  (?) 0   total 36
  the FIX                            (c) 22->23, (d) 1->0
  the GATES    (a) 13->15            (c) 23->24
settled        (a) 15  (b) 0  (c) 24  (d) 0  (?) 0   total 39
```

RFC-230 moved all three former blockers to `(a)` — not two to `(a)` and one to `(c)`, which an earlier draft of this section predicted from reading rather than from the merged file.

The `(d) == 0` floor RFC-230 deleted is **restored, with a growth-direction message**. Deleting it was correct at the time: zero had stopped being the steady state, so the floor was unsatisfiable. That premise expired when the fix landed. A floor left pointing at an old expectation is a build break; a floor deleted outright is an unwatched revival — so it is reconciled rather than either. It now also records that its zero means **found and fixed, twice**, and states what the zero does not cover: a `(d)` can only be counted once someone has written the gate that makes the site visible. RFC-230's `MEASURED`-content check on any `(d)` is retained over the now-empty population rather than deleted along with the last entry.

## 7. Known-adjacent, NOT fixed here

`SELECT order_id FROM orders, orders.items AS i WHERE i.sku = 'x'` returns **zero rows** where it must return one. The projection of the same reference is correct (`SELECT i.sku …` returns `[x y z]`), so the descent resolves and the element flows; the predicate path drops the rows, silently and without error. It is a different mechanism with a different fix, so it is booked rather than folded in (TODO.md, Phase 12), and the existing coverage of that shape asserts only that the query plans.

**The three-way measurement is the load-bearing part, and it is recorded in the booking as well as here.** This defect and §4's unnest element-substitution wrong read sit at the *same binding* and are **observationally identical**: on master the substitution arm would also have yielded zero rows, because it replaced `i.sku` with the whole element and no struct equals `'x'`.

```
origin/master (root mint, no arity gate)   rows=0
RFC-231 rev 1 (leaf mint, no arity gate)   rows=0     <-- THE DISCRIMINATOR
RFC-231 rev 2 (leaf mint, arity gate)      rows=0
```

Rev 1 is what separates them: there the substitution no longer fires (`Field` became `SKU`, not the alias `I` — measured `MATCHED=false`) and the rows are *still* zero. So the substitution arm is not the cause and the two findings are independent.

Without that row on the record, the likely failure mode is concrete: someone re-diagnoses this as §4's wrong read and "fixes" it by reverting the mint to name a fused value after its struct root — restoring a wrong read, re-breaking the §2 arithmetic-operand metadata, and still returning zero rows.
