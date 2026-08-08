# RFC-223 — A bare column reference keeps its ordinal, exactly as a qualified one does

**Status:** rev 1 — awaiting Graefe + Torvalds ACK. NOT implemented; the one-line
change below has been applied and reverted as a measurement probe only.
**Scope:** one guard in `pkg/relational/core/embedded/plan_visitor.go`.
**Origin:** an escalation from the leg-reference RFC's §4, whose premise this
document refutes.

> **Numbering — checked, because guessing it is how two branches both minted a
> 220.** `221` is merged on master (`rfcs/221-retire-the-compiled-key-evaluator-twin.md`)
> and `220` is claimed by TWO open PRs at once — #682
> (`rfcs/220-coveringness-is-a-plan-type-built-at-the-access-path.md`) and #684
> (`rfcs/220-a-leg-reference-is-nested-or-flat-before-it-is-pinned-or-lazy.md`,
> renumbering to 222). Verified by sweeping every remote branch, not just master:
>
> ```
> for b in $(git branch -r --format='%(refname:short)' | grep -v HEAD); do
>   git ls-tree -r --name-only $b rfcs/ | grep -E "rfcs/22[0-9]" | sed "s|^|$b: |"
> done | sort -u
> ```
>
> `222` is taken by the renumbered leg-reference RFC and `223` is free. A check
> against `origin/master` alone would have reported `220` free and repeated the
> collision, so the sweep is the point.

---

## 1. THE ESCALATED PREMISE IS WRONG — read this first

This shape was escalated twice, and both framings are refuted by measurement.

**Refutation 1 — there is no silent wrong-rows defect.** The escalation reported
that `SELECT t1.id, n FROM t1 JOIN t3 ON t3.t1_id = t1.id` "returns rows, with
the FIRST column 0 instead of the ids". It does not. It returns ids `1,2,3` with
the struct payload intact, identical to its unjoined control. Pinned, with the
mutation to `{0,0,0}` reddening all three arms, in
`TestFDB_ProjectedStructColumnThroughAJoin`.

**Refutation 2 — the struct type is not involved.** The remaining failure was
booked as "the positional merge does not model a struct-typed column", to be
fixed by carrying a real record type where Go erases one. **A plain `BIGINT`
fails identically, and the struct column's own qualified twin returns it
intact.** A fix aimed at the type would have left the defect standing while
looking principled.

`FieldTypeForProtoField` really does map every non-UUID message field to
`UnknownType` (`values/proto_field_type.go:70-73`), and that really is a
divergence from Java, which carries a full `Type.Record` recursively via
`Type.fromProtoType`. **It is not this defect.** Nothing on the failing path
branches on that type; the only nearby type-consuming code is census
classification of an already-made decision
(`leg_local_bake_census.go:645-660`, `:797-806`). Un-erasing it would buy a
large divergence for no fix, and this RFC does not touch it.

### The differential that establishes both

Schema `nst(sk BIGINT, co BIGINT)`, `t1(id, n nst)`, `t3(id, t1_id)`; `t3` holds
exactly one row per `t1` id, so the join is row-preserving. Every case asserts
COLUMN VALUES, never a row count — the escalation reported a wrong column, and a
count reads identically whether the column is right or wrong.

| second projected column | join | projected `EXISTS` | result |
|---|---|---|---|
| `t1_id` — **BIGINT, bare** | yes | yes | **FAILS** `field "T1_ID" … ordinal -1` |
| `n` — **struct root, bare** | yes | yes | **FAILS** `field "N" … ordinal -1` |
| `t3.t1_id` — BIGINT, **qualified** | yes | yes | OK `[1 1 true] [2 2 false] [3 3 true]` |
| `t1.n` — struct, **qualified** | yes | yes | OK `[1 struct[50 1] true] …` |
| `n` bare | yes | **no** | OK |
| `n` bare | **no** | yes | OK |
| `n` bare, `EXISTS` in `WHERE` | yes | **not projected** | OK |
| `n.sk` bare, **multi-accessor** | yes | yes | OK `[1 50 true] …` |

Four factors, each moved independently against a passing control: the failure
needs **all** of an unqualified reference, a **single** accessor, a **projected**
`EXISTS`, and a **join**. Drop any one and it goes away.

The runtime row is reported as `[ID T1_ID ID N]` — **it contains the column the
reference names**. This is not a missing column and not a failed name lookup. The
reference arrived with no baked ordinal, and `evaluateOrdinal` has no name
fallback by design (`values/values.go:862-877`).

---

## 2. The mechanism

`plan_visitor.go:703-713` fills `ProjectedValues[i]` for a bare reference only
when the resolved value is **childless**:

```go
if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child == nil && fv.SourceRelativeBaked() {
```

Over a two-source `FROM`, `needsQualification := len(r.scope.Sources()) > 1`
(`expr.go:292`) routes bare resolution through the **correlated** arm, which
returns a QOV-child-bearing value. The guard rejects it, and the slot stays
`nil` — **even though the resolver had a good ordinal in hand**.

`translateProjectOverExistsFilter` then mints a lazy carrier for the nil slot
(`cascades_translator.go:4630`):

```go
values.NoteFieldValueMint(strings.ToUpper(col), false)
v = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
```

and it is **the one projection path with no bake pass to recover it**.
`translateProject` (`:7046-7062`) and `translateProjectWithCorrelatedScalar`
(`:7305-7317`) both run `bakeSegmentedColumnRef` / `bakeFlatRefsAgainstColumns`
over their minted carriers. This path runs none, and the `gatedSeedStep1` arm
passes the projection through verbatim un-rebased
(`rule_implement_nested_loop_join.go:4174-4184`). Nothing ever bakes it.

**Why the qualified twin works.** `resolveQualifiedBaked`
(`plan_visitor.go:1853-1865`) accepts precisely the shape the bare arm rejects:

```go
if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child != nil && fv.RootIsLegRelativeUnpinned() {
```

The two guards are **disjoint and complementary**. The bare arm accepts exactly
what the qualified arm rejects and vice versa, and the qualified arm is never
consulted for an unqualified reference. That gap is the defect.

**Why the other three factors matter.** No join → one source → `needsQualification`
is false → the childless arm hits and bakes. No projected `EXISTS` → a `Project`
operator survives with its own bake pass (`Project([T1.ID#0, N#1], FlatMap(…))`).
Multi-accessor → a different arm. Each is a way of never reaching the gap, not a
second defect.

---

## 3. Java is the spec, and it has ONE resolution path

Java does not fork on qualification. `SemanticAnalyzer.resolveCorrelatedIdentifier`
(`SemanticAnalyzer.java:301-305`) asserts the identifier is qualified and then
**delegates to the very same `resolveIdentifier`**:

```java
public Expression resolveCorrelatedIdentifier(@Nonnull Identifier identifier,
                                              @Nonnull LogicalOperators operators) {
    Assert.thatUnchecked(identifier.isQualified(), ErrorCode.UNDEFINED_TABLE, ...);
    return resolveIdentifier(identifier, operators);
}
```

Qualification is a **precondition check**, not a different algorithm. A resolved
reference freezes into an ordinal `FieldPath` once, and Java's runtime never sees
a column name (`FieldValue.java:164-169`). Go's two disjoint guards are a
divergence in structure, and converging them removes it.

---

## 4. The decision: relax the bare guard. One guard, one rule.

**Chosen — accept at the source, in `plan_visitor.go:706`,** the union of the two
shapes, so a bare reference and a qualified one resolve by the same rule:

```go
isFV && ((fv.Child == nil && fv.SourceRelativeBaked()) ||
         (fv.Child != nil && fv.RootIsLegRelativeUnpinned()))
```

The added disjunct is **`resolveQualifiedBaked`'s existing predicate, unchanged**.
It is not a new trust boundary: `resolveQualifiedBaked`'s own doc comment already
states why this shape is the safe one over a merged row — the executor binds the
leg's window off the merged row's leg boundaries (`rowLegsBinder`), so a
leg-relative ordinal reads positionally over the composed row, while a
**childless** bake carries an ordinal relative to its own source row and would
misread another leg's slot. The bare arm accepts only the childless shape, which
is right for a single source and simply has no arm for the multi-source case.

**Rejected — add a bake pass to `translateProjectOverExistsFilter`.** It loses on
three counts, none of which is diff size:

1. **It re-derives downstream what was already known upstream.** The resolver
   held the correct ordinal and the guard discarded it; recovering it later by
   re-matching names against a derived layout is strictly less information.
2. **It leaves the asymmetry standing.** The next consumer of a nil
   `ProjectedValues` slot over a multi-source `FROM` hits the identical gap. The
   defect is that bare and qualified *disagree*; a third bake pass does not make
   them agree.
3. **It is a parallel pipeline, which Java does not have and this repo forbids.**
   Java resolves both spellings through one function. Go would gain a third
   bake path whose only reason to exist is that the first one declines.

### Measured blast radius — the reason this is safe, not an assertion that it is

`ProjectedValues` has **116 non-test mentions across 19 files**
(`git grep -n "ProjectedValues" -- '*.go' ':!*_test.go' | wc -l`), of which
**24 branch on nil-ness across 5 files**
(`… | grep -E "== nil|!= nil"`). That is the surface the change can move, so it
was measured rather than reasoned about.

With the one-line change applied as a probe, the whole real-FDB `sqldriver`
corpus reports **exactly 4 failures, and all 4 are this workstream's own
tripwires asserting the OLD broken behaviour**. No other test moves, and **no
census fails** — including `foldStep1Seed`, whose equalities are the instrument
most sensitive to bake-decision changes.

Every previously-failing arm returns **exactly the values its qualified twin
already returns**, including the `structLast` ordering
`[[1 true struct[50 1]] [2 false struct[40 2]] [3 true struct[30 3]]]`.

---

## 5. The regression pins the ASYMMETRY, not the failing query

A test carrying only the failing case passes the moment someone makes bare
references fail *differently*. The defect is a **disagreement**, so the pin
covers both arms of the same shape:

- bare **and** qualified, for **both** a `BIGINT` and a struct root — the pair
  that shows qualification is the discriminator and type is not;
- each failing case against a control differing in **exactly one** factor, so
  the localisation is a measurement and not a story;
- **column values**, never row counts;
- a vacuity guard on every control (zero rows fails loudly rather than
  satisfying the assertion).

That is `TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin`, already on the
branch and green against the CURRENT (broken) behaviour, with both mutation
directions red: driving a control's expected values wrong reds it with the real
values quoted, and converting a failing arm into a success expectation reds it
with the live unresolvable error.

**On implementation** the three tripwire arms convert to the value assertions
their own failure messages already name, and the leg-reference RFC's
`projected_struct_root_over_a_join_still_fails_for_a_reason_that_predates_this`
tripwire reddens — that is expected and is the signal the gap closed, not a
regression. Its replacement assertion is named in its own message.

### Census bookkeeping, so a reader does not re-derive it

`foldStep1Seed`'s equalities moved twice, each time proven by moving the fixture
file out of the package and back rather than asserted: `572/162/202 → 574/162/202`,
then `→ 588/174/204`. **The second round split +14 across TWO arms** — ACCEPT +12
and `rv-no-exist-ref` +2 — because two of the new controls project no reference
to the exists alias at all. The first round landed wholly in ACCEPT. The first
commit's "other equalities untouched" claim was true then and is not true now, so
it is restated at the gate rather than left standing.

---

## 6. Gates

- (a) Every arm of `TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin` green
      with the three tripwires converted to value assertions matching their
      qualified twins.
- (b) `just test` green, with the only expected movements being (a) and the leg-reference
      RFC's named tripwire.
- (c) `foldStep1Seed` and every other census green with no equality relaxed; any
      movement attributed by the move-out/move-back control.
- (d) No change to `FieldTypeForProtoField`. The type erasure is out of scope and
      stays a separate, documented divergence.

### (e) The ONE golden record that moves, justified individually

**A first draft of this section predicted no golden would move. That prediction
was wrong, and the measurement is recorded here rather than quietly corrected.**
Under the probe, `//pkg/relational/conformance/explaindiff:explaindiff_test`
fails `TestPlanShapeGolden` with **3 lines differing, all in one record**
(`cte.yaml#13`):

```
 sql:   WITH lo(li) AS (SELECT id FROM t WHERE v < 20),
             hi(hi_id) AS (SELECT id FROM t WHERE v >= 30)
        SELECT li, hi_id FROM lo, hi ORDER BY li, hi_id
-plan:  Project([LI#0, HI_ID#1], InMemorySort(...))
+plan:  Project([LO.LI#0, HI.HI_ID#0], InMemorySort(...))
```

**The operator tree is byte-identical** — same `Project`, same
`InMemorySort`, same `NestedLoopJoin`, same two filtered scans. Only the two
projected references re-render, and this is precisely the change under review
reaching its intended case: `FROM lo, hi` is a two-source `FROM`, so `li` and
`hi_id` are exactly the bare references that previously stayed lazy. There is no
`EXISTS` here, so this record shows the guard's effect **isolated from the fold**.

**The `#1 → #0` is the load-bearing part and it is not a lost column.** The old
`HI_ID#1` was a FLAT ordinal into the concatenated row; the new `HI.HI_ID#0` is
LEG-RELATIVE — slot 0 within leg `HI` — which is what `rowLegsBinder` binds and
what `resolveQualifiedBaked`'s contract requires. A qualified spelling of the
same query already rendered this way; the bare spelling now agrees.

**Rows are unchanged, verified independently of the plan text.** `cte.yaml#13`
carries row assertions (`[1, 3]`, `[1, 4]`), and
`//pkg/relational/conformance/yamsql:yamsql_test` **passes under the probe** —
751 RUN/PASS lines, `TestYamsqlConformance/cte` among them. An ordinal that had
moved to the wrong slot would have changed those rows. That check is the reason
this is a re-bless and not a defect, and it is why the golden is not re-blessed
on the strength of "the shape looks the same".

Re-blessing is therefore **one record, named, with its diff in the commit** —
not a bulk `dump`. No other golden line moves.

Costing is untouched: the change moves a plan-time ordinal from absent to
present and constructs no new operator, and no cost formula reads it. The 1M
stress gate is consequently not claimed as required; the golden movement above is
a rendering/addressing change with no operator or cardinality effect. Should
review disagree that this is cost-neutral, the stress comparison runs in a
sibling worktree on the same filesystem before merge.
