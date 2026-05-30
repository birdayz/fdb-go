# RFC-044: Codex review follow-ups (REVIEW.md findings)

**Status:** Draft

## Problem

A Codex review pass (REVIEW.md) flagged four latent correctness issues against the
PR #213–#217 merge train. This RFC triages each with the divergence-vs-extension
lens (CLAUDE.md): a finding is either (a) a **Java-parity divergence** — Go behaves
differently than the ported Java reference and must be corrected to match, or (b) a
**bug inside a Go-only extension** — a feature Java lacks entirely, which is allowed
to diverge but must still be pristine and bug-free.

Verdicts (detail below):

| # | Component | Class | Verdict |
|---|-----------|-------|---------|
| 214 | `IndexEntryObjectValue` equality/hash | Java-parity divergence | **REAL — fix** |
| 215 | `SelectExpression.ChildrenAsSet` for outer joins | Bug in Go-only extension | **REAL (latent) — fix** |
| 216 | `composeFieldOverJoinMerge` side resolution | Bug in Go-only extension | **REAL — fix** |
| 213 | `Memo.integrateOne` equivalent-ref iteration | Go-only extension | **FALSE POSITIVE — no change** |

---

## Finding #214 — `IndexEntryObjectValue` drops `Source` (KEY vs VALUE)

### Problem
`IndexEntryObjectValue.Evaluate` reads a **different tuple** per `Source`
(`TupleSourceKey` → `PrimaryKey()`, `TupleSourceValue` → `IndexValues()`;
value_index_entry_object.go:150-157). So `KEY[0]` and `VALUE[0]` (same alias, same
ordinal path) are **different columns** — e.g. the indexed column vs. the covered
column of a `KeyWithValue` covering-index entry. But Go's memo equality
(`EqualsWithoutChildren`, map_field_values.go:323) and `SemanticHashCode`
(semantic_hash.go:54) both compared **only `OrdinalPath`** — collapsing them into one
value.

### Investigation (Java is the reference)
`IndexEntryObjectValue.java`:
- `eval` (L130) branches on `source` — identical to Go.
- `equalsWithoutChildren` (L120-123) compares class + `ordinalPath`, **ignoring source**.
- `hashCodeWithoutChildren` = `planHash` (L140-147), and `planHash` folds
  `(ordinalPath, source)` — **including source**.

So Java's `semanticEquals` is source-blind but its `semanticHashCode` is source-aware
— Java technically violates `equal ⟹ same hash`, but it never manifests because
hash-bucketing separates `KEY[0]` and `VALUE[0]` before equality is ever consulted.
Java's **effective** behavior: the two are **distinct** in the memo.

Go copied the source-blindness into **both** equality and hash, so Go is internally
consistent but **actively collapses** `KEY[0]≡VALUE[0]` — diverging from Java's
effective "distinct" behavior. This is a real divergence (the memo can dedup two
different covering-index columns into one).

### Fix
Fold `Source` into **both** `EqualsWithoutChildren` and `SemanticHashCode`. This
reproduces Java's *emergent* behavior (distinct) while keeping the
`equal ⟹ same hash` contract intact — a well-architected version of Java's
contract-violating literal code (CLAUDE.md principle 10: match the architectural
property, not the downstream observable). The memo hash is alias-invariant and
in-memory only — not a wire/continuation hash — so this does not touch wire compat.

### Test plan
`TestSemanticEquals_IndexEntryObject_Source`: `KEY[0]` and `VALUE[0]` (same alias,
same path) are NOT `SemanticEqualsUnderAliasMap`-equal and hash differently; same
`Source` differing only by alias stays equal + equal-hash (fix does not over-restrict).

---

## Finding #215 — `SelectExpression.ChildrenAsSet()` ignores join type

### Problem
`matchChildrenInMemo` (memo_equal.go:77) permutes children for memo equality whenever
both expressions report `ChildrenAsSet()`. `SelectExpression.ChildrenAsSet()`
(select.go:162) returns `true` **unconditionally** — including for `JoinLeftOuter` /
`JoinFullOuter`. So `MemoEqual(A LEFT JOIN B, B LEFT JOIN A)` can return true and the
two could intern to the same `Reference`, which is not semantics-preserving for left
joins.

### Investigation
`LEFT/FULL OUTER JOIN` on a `SelectExpression` is a **Go-only read-side extension** —
Java's `SelectExpression` does not model outer joins at all (Java's only outer joins
are synthetic record types, materialized at write time). Java's
`SelectExpression.ChildrenAsSet` is an unconditional marker, which is **sound in
Java** precisely because every Java `SelectExpression` IS commutative. The Go
extension bolted a non-commutative join type onto the class without updating the
`ChildrenAsSet` invariant the permutation logic relies on.

**Reachability:** latent today. The four swap-exploration sites
(expression_matcher.go:86, implementation_rule.go:136, unified_tasks.go:211/274) all
gate `WithSwappedQuantifiers()` on `!= JoinLeftOuter`, so the commuted form is never
memoized through any planning path. But `findCandidateParents` is order-insensitive,
so a future caller that memoized both directions would land on one `Reference` and
`MemoEqual` would falsely merge them. The invariant is wrong; only the absence of an
inserting path keeps it dormant.

### Fix
Make `ChildrenAsSet()` reflect actual commutability — the single source of truth all
permutation sites consult:

```go
func (e *SelectExpression) ChildrenAsSet() bool {
    return e.joinType == JoinInner || e.joinType == JoinCross
}
```

Keep the four `!= JoinLeftOuter` swap-exploration guards — they serve a *different*
concern (don't generate a semantically-invalid commuted plan alternative), not memo
dedup, and remain correct/needed independently.

### Test plan
`TestMemoEqual_LeftOuterJoinNotCommutative` (expressions package, hits `MemoEqual`
directly): build `selAB` and `selBA = selAB.WithSwappedQuantifiers()` with
`JoinLeftOuter`; assert `MemoEqual(selAB, selBA) == false`. Assert the `JoinInner`
swapped pair still returns `true` (fix does not over-restrict). Assert
`MemoizeExpression` of the two left-join directions yields distinct `*Reference`s.

---

## Finding #216 — `composeFieldOverJoinMerge` blindly resolves to the inner side

### Problem
`composeFieldOverJoinMerge` (simplifier_value.go) canonicalizes
`field(join_merge{outer,inner}, f)` → `field(QOV(inner), f)` **unconditionally**.
`JoinMergeResultValue.Evaluate` merges outer-then-inner, so a **bare outer-only**
field resolves to its outer value at eval time but to `nil` after the rewrite (proven
at the value level: 42 before, nil after). The existing comment admits the unsoundness
("the conformance suite is the oracle that this does not arise") — exactly the
unsound-but-hopefully-unreachable paper-over CLAUDE.md forbids.

### Investigation
`join_merge` is a **Go-only construct** (RFC-042/043) — Java has no equivalent (Java's
join result is a typed `RecordConstructorValue` resolved by ordinal). It's an opaque
string-keyed merge with no column schema, so the rule can't resolve ownership by type.

**Empirical reachability probe (FDB testcontainers, every shape):** the bug is **NOT
reachable through real SQL.** 3-way joins projecting/filtering outer-only columns (both
FROM-orders), spanning predicates that force the upper level onto the lower join's outer
table, `SELECT *`, 2-way outer-column WHERE — **all return correct rows.** Instrumenting
the rule showed it fires often but **only ever on inner-side fields**, never on an
outer-only column.

**Why (structural invariant, proven):** a `FieldValue` gets a `JoinMergeResultValue`
child only via `SelectMergeRule` (rule_select_merge.go), which substitutes the captured
merge for a `QOV(parentAlias)` whose alias is the merge's **inner** quantifier — the
merge is re-flowed under the inner side's own alias. So only inner-side references are
ever composed onto it; outer/third-table columns resolve through their own QOV and reach
the merge only as already-qualified `ALIAS.COL` keys (value_join_merge.go), never as a
bare field over the merge. Hence `bare → inner` is **sound by invariant**, and the
existing `bare ID → inner` test is load-bearing, not an artifact.

The real defect is therefore the *lying comment*, not the logic. (Qualifier-routing — an
earlier proposal — was correctly NAK'd: it services a shape that never occurs while
breaking the bare shape that does. Membership-via-column-lists would gold-plate a dead
path: per-side columns aren't cleanly available — scans carry no column list at the
construction sites.)

### Fix
1. Replace the paper-over comment with the precise `SelectMergeRule` re-flow invariant
   that makes `bare → inner` sound.
2. **Fail-safe**: rewrite *bare* fields to inner (the only shape that occurs, sound by
   invariant); **refuse** *qualified* fields (any `.`-carrying name) — return `nil` so
   `JoinMergeResultValue.Evaluate` resolves the qualified key. This removes the silent
   mis-resolution landmine (if the alias convention ever drifts so an outer/foreign
   column reaches the rule) at zero cost to real firings, without phantom routing or
   column-list machinery.

`JoinMergeAllValue` has no compose rule and already resolves by qualified name — left
unchanged.

### Follow-up (Graefe condition)
The Java-aligned root fix is to anchor fields to their source during pull-up —
`FieldAccessValue(QuantifiedObjectValue(alias), field)` — so the opaque-merge ambiguity
never exists and this rule (and the bare/qualified distinction) can be retired. Not
urgent (no live bug); filed in TODO.md as the next join-merge item.

### Test plan
- Value-tree unit: bare inner field → `field(QOV(inner), f)` (existing, kept);
  `TestSimplifyValue_FieldOverJoinMerge_QualifiedOuterPreserved` — a **qualified
  outer-only** reference survives simplification with its value intact via `Evaluate`
  (and demonstrates the `nil` the old blind rewrite produced).
- **E2E sentinel** `TestFDB_JoinMerge_OuterColumn_NotDropped`: projects/filters
  outer-side columns across a 3-way join (both FROM-orders, spanning predicate, outer
  WHERE) and asserts correct, non-NULL rows — the dimensional axis the prior suite
  missed; fails loudly if the invariant ever breaks.

---

## Finding #213 — `Memo.integrateOne` only checks one equivalent ref — FALSE POSITIVE

### Investigation
RFC-037 cross-`Reference` equivalence-class merging is a **Go-only extension**: Java's
`Reference`/`Memoizer` only reuse an existing equivalent ref at insertion time and have
no group `merge`/union-find at all. So there is no Java "iterate siblings, skip cyclic"
behavior to port.

The reviewer's failing scenario — `integrateOne` finds a cyclic descendant as the first
equivalent ref while a mergeable sibling also exists — is **self-contradictory under the
memo's invariants**: two *distinct* References both structurally equal to the yielded
expr would be equal to each other, and interning (`MemoizeExpression`/`memoizeNonLeaf`)
plus the RFC-037 merge collapse equal siblings on insert. Whichever was integrated
second already merged into the first; they cannot stably coexist. The temp test
hand-builds a state real planning never produces.

### Verdict
No fix. Adding "iterate equivalent candidates" would be speculative complexity guarding
an unreachable state and would diverge further from Java (which merges no groups). The
reachable cyclic case is already pinned by `TestMemoMerge_SkipsCyclicMerge`. This RFC
records the verdict so the finding isn't re-litigated.

---

## Performance

All three fixes are O(1)-per-node local changes with no added planner passes:
- #214: one extra discriminator byte in equality/hash — strictly fewer false memo
  merges (better, not worse).
- #215: one boolean expression in `ChildrenAsSet()`; `JoinInner` (the hot path) is
  unchanged.
- #216: replaces an unconditional allocation+recurse with a qualifier check that
  rewrites in strictly fewer (only provably-correct) cases — never more work.

## Overall test plan
- `go test ./pkg/recordlayer/query/plan/cascades/values/...` (value equality/hash/simplify)
- `go test ./pkg/recordlayer/query/plan/cascades/expressions/...` (MemoEqual)
- `just test` full suite, incl. `multiway_join_order_*` and cross-engine harness
- New regressions pin each fixed axis (per-finding test plans above)
