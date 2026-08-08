# RFC-220 — A leg reference is NESTED or FLAT before it is PINNED or LAZY

**Status:** rev 1 — IMPLEMENTED and exercised; awaiting Graefe + Torvalds review.
**Origin:** TODO.md's "RFC-218 remainder — the leg-window re-anchor". That booking
described a capability gap. **It is a wrong-rows defect**, and the correction is §1.
**Scope:** `rebaseOuterLegValueOrdinal` (`left_outer_existential.go`), one new values
primitive, and the join arm of `sortKeySourceValue` (`cascades_translator.go`).

> **Numbering.** 216 and 217 are taken by open PR #658 and do not appear in `rfcs/`.
> 218 and 219 are merged. 220 is the next free number.

---

## 1. THE BOOKING IS WRONG IN THE DIRECTION THAT MATTERS — read this first

The booking, and the task that carried it, state that `rebaseOuterLegValueOrdinal`
"refuses multi-accessor paths outright (`fv.Resolved.Single()`, `!single -> failed`)",
and that the resulting `0AF00` is a clean, loud, capability-only gap.

**That is true of ONE of its two arms.** The function dispatches on a reference's
frontier PIN, and arity is orthogonal to the pin, so a multi-accessor reference takes
whichever arm its pin selects:

| form | arm taken | what happened |
|---|---|---|
| pinned | the baked arm | `Resolved.Single()` fails → **declines (loud)** — the booked behaviour |
| unpinned | the NAME arm | looks up `fv.Field`, which for a nested reference is the struct **ROOT**, finds it, bakes the merged address of the whole struct and **DROPS the descent** |

Driven against the production symbol by name, with the single-accessor control
passing in all four combinations (`nested_leg_window_rebase_test.go`):

```
flatRun/unpinned  multi-accessor -> ok=true  path [11]      want [11 0]   TRUNCATED
flatRun/pinned    multi-accessor -> ok=false                              declined
nested/unpinned   multi-accessor -> ok=true  path [10 1]    want [10 1 0] TRUNCATED
nested/pinned     multi-accessor -> ok=false                              declined
flatRun/unpinned  single-accessor -> ok=true path [10]                    [CONTROL]
flatRun/pinned    single-accessor -> ok=true path [10]                    [CONTROL]
nested/unpinned   single-accessor -> ok=true path [10 0]                  [CONTROL]
nested/pinned     single-accessor -> ok=true path [10 0]                  [CONTROL]
```

A truncated path is not a decline. It is a **real merged column of the wrong type**,
so nothing downstream rejects it. Reached from SQL, with no `ORDER BY` and no
projected `EXISTS` anywhere — measured on master, same data, same predicate:

```
SELECT id     FROM t1                          WHERE EXISTS(...t2.t1_id = n.co) -> [1 3]    CORRECT
SELECT id     FROM t1                      WHERE NOT EXISTS(...t2.t1_id = n.co) -> [2]      CORRECT
SELECT t1.id  FROM t1 JOIN t3 ON t3.t1_id=t1.id WHERE EXISTS(...)               -> []       WRONG
SELECT t1.id  FROM t1 JOIN t3 ON t3.t1_id=t1.id WHERE NOT EXISTS(...)           -> [1 2 3]  WRONG
```

`t3` holds exactly one row per `t1` row, so the join is row-preserving and cannot
change which `t1` rows qualify. Adding it flips the answer anyway: the predicate
`t2.t1_id = n.co` lost its `.co` and compared a `BIGINT` against a whole struct,
matching nothing. **`EXISTS` collapsed to empty and `NOT EXISTS` inflated to
universal, silently.**

This is why the work is an RFC rather than a contained extension of RFC-218's ACK'd
design. RFC-218 reasoned about a decline; the thing being fixed is a wrong answer,
and the guard the booking called "load-bearing and verified by construction" was
load-bearing only on the arm that was already loud.

---

## 2. The design: dispatch on ARITY first, and fuse

A leg reference is *nested or flat* before it is *pinned or lazy*. Arity is the
property that decides what the address must LOOK like; the pin decides only where
the root ordinal came from. The old dispatch had them the other way round, which is
how one shape got two different wrong treatments.

So a single arm, above the pin dispatch, handles every multi-accessor reference
under both leg kinds:

1. **Re-anchor the ROOT in the leg's own layout**, via `FieldPath.ReAnchorRootInto` —
   the primitive RFC-218 §2e/§2f built and exercised. Derived from the layout by the
   root accessor's name; the carried ordinal is a **tripwire**, never a source; absent
   or DUPLICATED root names decline (a first match is indistinguishable from a correct
   answer, and disambiguating needs a leg identity this site does not carry).
2. **Bake the merged address of the root**, dispatching on the leg KIND exactly as
   before — `w.Offset + legOrdinal` for `LegKindFlatRun`, the fused two-step
   `NewFusedFieldValueOfNestedOrdinal` for `LegKindNested`.
3. **Fuse the remaining accessors on** with `FieldPath.WithSuffix`.

Step 3 is Java verbatim. `FieldValue.withNewChild` → `ofFieldsAndFuseIfPossible`
merges accessor lists via `FieldPath.withSuffix` (`FieldValue.java:525-534`) into one
path; composition is a **fusion**, never a flattening — nothing is materialised and
nothing is re-offset. Go already had the two-step form of this and used it for the
nested leg kind; what was missing was the general form.

### 2a. The suffix is CARRIED, the root is DERIVED — and that asymmetry is the design

A root ordinal is stated in the layout the reference was resolved against, and a merge
RESTATES that layout. That is the whole reason RFC-218 insisted on derive-and-assert.

A suffix ordinal indexes a **struct's own declared field order**, which the schema fixes
and which no merge, projection or re-ordering can touch. Re-deriving it would be
re-deriving a constant. `FuseNestedSuffix` therefore ASSERTS the suffix against a record
type when one is available and CARRIES it when one is not.

**That is not a hedge, and the measurement is why it cannot be.** Neither the merged
row nor the leg window carries a struct column's type — both state `UNKNOWN`:

```
ZZREBASE field="N" accs={N@1}{SK@0} kind=flatRun off=0
         legfields=[ID:LONG NULL, N:UNKNOWN NULL]   ok=true
```

A first implementation of this RFC descended against the merged slot's type and failed
on **every real nested reference** while looking careful:

```
ordinal bake: cannot resolve ordinal 0: a nested suffix step descends
into a non-record type (child type UNKNOWN NULL)
```

The leaf TYPE comes from the reference itself, which the resolver computed against the
real schema — strictly better information than either layout holds — promoted nullable
if the merged slot is. Taking the root SLOT's type instead is the bug
`NewFusedFieldValueOfNestedOrdinal`'s own doc records: it reports the whole struct as
the type of a single-column read.

### 2b. The translator does NOT re-anchor for a join, deliberately

`sortKeySourceValue`'s single-table arm re-anchors the root itself, because there the
fold's flowed row IS the source row. The join arm must not: the merged layout is decided
by the NLJ rule, downstream, and a root ordinal derived in the translator would be stated
against a layout it is guessing at — RFC-218 §2b's silent-wrong-column hazard, arriving
by a third route.

So the join arm emits the key **leg-relative** — root re-anchored in its own leg's row,
suffix intact, hung off the leg's correlation — and the leg-window rebase does the merged
half. That is the same division of labour the flat join arm already used, extended to
carry a path instead of a bare column name.

### 2c. The leg is chosen by IDENTITY, and the rendering is not consulted at all

A nested key over a join renders THREE segments (`T1.N.SK`). `sortKeyName`'s split takes
the LAST dot and manufactures the qualifier `T1.N`, which contradicts the `T1` the key is
holding. That is the wrong-answer population the qualifier recovery census asserts at
zero, and making the arm plan is exactly what made it reachable:

```
existsSortSplit  calls 3 | AGREED 0 | DIVERGED 3
    DIVERGED witness: "T1.N.CO" vs identity "T1"
    DIVERGED witness: "T1.N.SK" vs identity "T1"
FAIL: existsSortSplit manufactured a qualifier that CONTRADICTS the structured identity
```

**The zero is not widened.** The census's own remediation text says the fix is to use the
identity, and that is what happens: a multi-accessor key is settled by its source value
and never falls through to the rendered name, and `legIndexOfNestedKey` attributes it by
its correlation. Post-fix the site reports `calls 0` — the population is gone, not
reclassified.

### 2d. Rejected alternatives

- **Keep declining and widen the SQL-layer refusal.** Disqualified by §1: the decline was
  never the whole behaviour, so "keep declining" leaves the wrong-rows path exactly as it
  is. It also diverges from Java in form for no gain — Java has no SQL-layer rejection for
  this shape (`QueryVisitor.visitSimpleTable` routes to the same `generateSelect`; the sole
  `UNSUPPORTED_QUERY` at `LogicalOperator.java:220` is about index hints).
- **Make the merged/leg layouts carry struct types, then descend against them.** This is
  the *other* real defect (§4) and it is genuinely separate: it must be fixed, but making
  the re-anchor DEPEND on it would gate a wrong-rows fix behind a merge-layout rework, and
  the suffix does not need the type (§2a).
- **Widen the pinned arm's `Single()` only.** Fixes the loud half and leaves the silent
  half. It is the shape of fix that passes the reproducer you happen to hold, which is the
  failure RFC-218 §2 spent four revisions on.

---

## 3. What is pinned, and how each pin was proven able to go red

Every mutation below reverted a real hunk and was run with the `=== RUN` count checked,
because a narrowed filter that matches nothing reports green. (One run during this work
did exactly that: reverting the whole diff also reverted `BUILD.bazel`, the filter matched
no function, and Bazel printed a pass over **zero** `=== RUN` lines.)

| direction reverted | what reds | verbatim |
|---|---|---|
| the rebase's fusion arm | the wrong-rows pin only | `adding a row-preserving inner JOIN changed the answer: [] vs the single-table [1 3]` and `[1 2 3] vs the single-table [2]` |
| `sortKeySourceValue`'s join arm | both sort arms only | `0AF00: projected EXISTS in this query shape is not yet supported` |
| the identity-not-rendering `continue` | the census hard zero | `FAIL: existsSortSplit manufactured a qualifier that CONTRADICTS the structured identity` |
| the suffix forced to member 0 | the `n.co` arm only | `got ids [3 2 1], want [1 2 3]` |

The first two red **disjoint** sets, which is the evidence that the two halves are
independent fixes and not one fix tested twice.

The fourth is the one that matters for the "row order cannot catch a wrong column"
rule. `sk` is anti-correlated with `id` (50,40,30) and `co` correlated (1,2,3), so
forcing the suffix to `sk` leaves `ORDER BY n.co` returning a perfectly sorted `[3 2 1]`
— plausible output, wrong column, caught.

The unit pin drives the production symbol **by name** (`rebaseOuterLegValueOrdinal`),
not a local copy — the defect that NAK'd RFC-218's implementation. It covers both leg
kinds × both pin states × both type-knownness states, with four single-accessor controls,
because a decline whose control also fails is uninterpretable.

**A dimension this RFC's own first implementation missed, and the pin now covers.** All
four typed arms passed while every real query crashed: `into` was a typed-nil
`*RecordType` in a `Type` interface, so the assertion arm succeeded with a nil receiver.
The fixture only ever stated a type. The `UNKNOWN leg column type` cases are the arm the
planner actually takes, and they exist because that gap shipped a nil dereference past a
green unit test.

---

## 4. A SEPARATE PRE-EXISTING DEFECT, found here and NOT fixed here

Projecting a struct column through a join's merged row is broken, with no `ORDER BY`
and no `EXISTS` involved. Measured with this entire change **reverted**, so it is not a
consequence of it:

```
SELECT t1.id, n, EXISTS(...) FROM t1 JOIN t3 ON ...   (no ORDER BY at all)
  -> ordinal resolution: field "N" not resolvable in the runtime row
     (ordinal -1, row columns [ID T1_ID ID N]) — malformed plan

SELECT t1.id, n FROM t1 JOIN t3 ON ...                (no EXISTS either)
  -> returns rows, with the FIRST column 0 instead of the ids
```

The second is a **silent wrong answer** and is the more dangerous. Both point at the same
root cause §2a measured: the positional merge does not model a struct-typed column, so the
reference stays lazy (`ordinal -1`) and resolves by a name the runtime row does not answer
to.

It is pinned as a tripwire
(`projected_struct_root_over_a_join_still_fails_for_a_reason_that_predates_this`) that
reds when the merge learns struct columns, with the replacement assertion named. **It is
escalated rather than filed:** it is a live silent wrong-rows defect on master on a
different mechanism (merge layout derivation, not the leg-window re-anchor), and it wants
its own RFC and its own review lap.

---

## 5. Java

`FieldPath.withSuffix` — `FieldValue.java:525-534`. `ofFieldsAndFuseIfPossible` recomputes
the fused node's result type rather than inheriting the first step's. `PartitionSelectRule.java:296-303`
rewrites a reference to a collapsed sibling as `ofOrdinalNumber(QOV(newUpper), index)` and the
alias then ceases to exist; `translateCorrelations`' leaf swap fuses the enclosing accessor onto it.
Java has no SQL-layer rejection for a nested `ORDER BY` key over a join — it attempts the plan and
fails downstream in `RemoveSortRule.onMatch` if no ordering satisfies. Go's `0AF00` was therefore a
divergence in FORM, and closing it removes the divergence rather than adding an extension.
