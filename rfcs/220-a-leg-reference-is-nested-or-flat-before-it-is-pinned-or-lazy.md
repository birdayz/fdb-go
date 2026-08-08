# RFC-220 — A leg reference is NESTED or FLAT before it is PINNED or LAZY

**Status:** rev 2 — IMPLEMENTED and exercised; rev 1 was NAK'd on mechanism and record. §2a answered the suffix question about the ORDINAL when the executor reads the NAME; §2b understated where the emitted value goes; §3's fourth mutation conflated two fields; §4's silent variant was a probe artifact and is RETRACTED. No production behaviour changed as a result — the wrong-rows fix stands and is unaltered — but one real coverage hole (§3c) was found and closed. Awaiting the delta lap.
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

### 2a. The root is DERIVED; the suffix is CARRIED — and the suffix's IDENTITY IS ITS NAME

A root ordinal is stated in the layout the reference was resolved against, and a merge
RESTATES that layout. That is the whole reason RFC-218 insisted on derive-and-assert, and
it is why the root is derived here.

**A suffix step is a different question, and rev 1 answered it about the wrong field.**
Rev 1 argued that the suffix ORDINAL is safe to carry because it indexes a struct's own
declared field order, which no merge restates. That is true and it is nearly irrelevant,
because **on every path this code reaches, the executor does not read the suffix ordinal
at all.** `FieldValue.descendResolvedPath` dispatches on what the value materialises as:

| the nested step lands on | what is consumed |
|---|---|
| `proto.Message` — a STRUCT column, the shape every case here produces | the per-step **NAME** (`protoFieldByName`) |
| `OrdinalRow` — a positional row | the **ordinal** |

A struct column materialises as its raw proto message, so the descent is by name. That is
not an accident of this change: `values.go` documents it as a standing divergence from
Java (Java descends by ordinal via `MessageHelpers.getFieldValueForFieldOrdinals`,
`FieldValue.java:169`) and pins it with
`TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal`, whose whole point is that
the ordinal must NOT be consulted there.

**Measured, four mutations, one per (site × field), every run with its `=== RUN` count
checked:**

```
values.go FuseNestedSuffix   suffix ORDINAL forced   -> GREEN, plan unchanged
values.go FuseNestedSuffix   suffix NAME swapped     -> RED: the wrong-rows pin
translator sortKeySourceValue suffix ORDINAL zeroed  -> GREEN, plan unchanged
translator sortKeySourceValue suffix NAME swapped    -> RED: BOTH sort arms, opposite directions
```

So the field that carries correctness end-to-end is the **name**, and the ordinal is inert
on the reachable paths. Both are carried; `FuseNestedSuffix` validates whichever it can —
when a record type is available it derives the ordinal FROM the name and declines on a
disagreement, an absent name, or a duplicate one, which is a check on the load-bearing
field and not merely on the inert one.

**The ordinal is carried unvalidated into a path that COULD consume it**, and that is the
honest residual rather than a solved problem: the `OrdinalRow` arm is live for a
`LegKindNested` window's first step. No corpus query reaches it with a struct suffix
today, so no end-to-end pin can be written for it — a fabricated one would assert a
reachability that does not exist. It is guarded where it is observable, in
`FuseNestedSuffix`'s unit pins (`fused path [{N 11} {CO 0}], want [{N 11} {CO 1}]`), and
the inertness is recorded as a negative result rather than left as folklore. What re-arms
it: any producer that makes a struct column materialise as an `OrdinalRow`, or the
conversion of the proto descent to ordinals that `values.go`'s two `protoFieldByName` debt
entries retire on.

**The type story, which rev 1 got right.** Neither the merged row nor the leg window
carries a struct column's type — both state `UNKNOWN`:

```
ZZREBASE field="N" accs={N@1}{SK@0} kind=flatRun off=0
         legfields=[ID:LONG NULL, N:UNKNOWN NULL]   ok=true
```

A first implementation descended against the merged slot's type and failed on **every real
nested reference** while looking careful:

```
ordinal bake: cannot resolve ordinal 0: a nested suffix step descends
into a non-record type (child type UNKNOWN NULL)
```

The leaf TYPE therefore comes from the reference itself, which the resolver computed
against the real schema — strictly better information than either layout holds — promoted
nullable if the merged slot is. Taking the root SLOT's type instead is the bug
`NewFusedFieldValueOfNestedOrdinal`'s own doc records: it reports the whole struct as the
type of a single-column read.

### 2b. The translator emits the key leg-relative, and that value IS the sorted data

`sortKeySourceValue`'s single-table arm re-anchors the root itself, because there the
fold's flowed row IS the source row. The join arm must not: the merged layout is decided
by the NLJ rule, downstream, and a root ordinal derived in the translator would be stated
against a layout it is guessing at — RFC-218 §2b's silent-wrong-column hazard, arriving by
a third route. So the join arm emits the key **leg-relative** and lets the merged half
happen downstream.

**WHERE THAT VALUE GOES, because it is not obvious and rev 1 did not say.** It is not only
a membership probe. `collectExtraSortColumns` takes `sortKeySourceValue(k)` as the hidden
sort column's **value** (`cascades_translator.go:5206`) and appends it to the fold's
`RecordConstructorValue`; the re-applied sort then resolves its key to that appended
column. So the emitted value is what the executor EVALUATES to produce the column being
sorted on — it is the sorted data, not a gate.

That is measurable and was measured: swapping the suffix NAME on the value this arm
returns reds **both** join sort arms, in opposite directions —

```
ORDER BY n.co -> got ids [3 2 1], want [1 2 3]
ORDER BY n.sk -> got ids [1 2 3], want [3 2 1]
```

— which is the wrong-COLUMN signal, not a decline. A reading that this arm is "a capability
gate whose value never reaches the sort" follows from mutating the ORDINAL, which §2a shows
is inert everywhere; the value itself is load-bearing.

**The two halves are still independent, and that is the point of the disjoint reds.** The
sort path does NOT go through the ordinal rebase: the hidden column's leg-relative
reference is multi-accessor, so the LAZY rebase declines to rewrite it and the executor
binds each leg under its own correlation. Reverting the entire arity arm in
`rebaseOuterLegValueOrdinal` leaves both sort arms green and reds only the wrong-rows pin.
Reverting the translator arm reds only the sort arms. Two fixes, two paths, one shared
primitive.
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

Every mutation below was run with the `=== RUN` count checked, because a narrowed filter
that matches nothing reports green. (One run during this work did exactly that: reverting
the whole diff also reverted `BUILD.bazel`, the filter matched no function, and Bazel
printed a pass over **zero** `=== RUN` lines.)

### 3a. The two halves, and why their reds must be disjoint

| direction reverted | what reds | verbatim |
|---|---|---|
| the rebase's fusion arm | the wrong-rows pin ONLY | `adding a row-preserving inner JOIN changed the answer: [] vs the single-table [1 3]` and `[1 2 3] vs the single-table [2]` |
| `sortKeySourceValue`'s join arm | both sort arms ONLY | `0AF00: projected EXISTS in this query shape is not yet supported` |
| the identity-not-rendering `continue` | the census hard zero | `FAIL: existsSortSplit manufactured a qualifier that CONTRADICTS the structured identity` |

The first two red **disjoint** sets, which is the evidence that these are two fixes on two
paths and not one fix tested twice (§2b explains why the paths differ).

### 3b. The fused suffix: which FIELD each pin actually guards

**Rev 1 claimed a fourth direction — "the suffix forced to member 0 reds the `n.co` arm" —
and that claim conflated two fields.** The mutation it ran replaced the accessor wholesale
with `{Field:"SK", Ordinal:0}`, changing the NAME as well as the ordinal. It does red, but
not for the stated reason, and forcing the ordinal ALONE reds nothing anywhere. Split
properly, one mutation per (site × field):

| site | field | result |
|---|---|---|
| `FuseNestedSuffix` | suffix ordinal forced | **GREEN**, plan byte-identical |
| `FuseNestedSuffix` | suffix name swapped | **RED** — the wrong-rows pin |
| `sortKeySourceValue` | suffix ordinal zeroed | **GREEN**, plan byte-identical |
| `sortKeySourceValue` | suffix name swapped | **RED** — both sort arms, opposite directions |

The name is load-bearing and the ordinal is inert, for the structural reason in §2a: a
struct column materialises as a `proto.Message` and the descent reads the per-step name.
The ordinal is guarded one layer down, by `FuseNestedSuffix`'s unit pins
(`fused path [{N 11} {CO 0}], want [{N 11} {CO 1}]`), which is the only layer at which it
is observable.

**The wrong-COLUMN claim is delivered, on the field that carries it.** `sk` is
anti-correlated with `id` (50,40,30) and `co` correlated (1,2,3), so a wrong member yields
plausibly-ordered output rather than an error:

```
ORDER BY n.co -> got ids [3 2 1], want [1 2 3]
ORDER BY n.sk -> got ids [1 2 3], want [3 2 1]
```

### 3c. A hole this review found in the correlation pins, now closed

The wrong-rows pin used only `n.sk`, and in its fixture `co` matched no `t2` row. So
swapping the suffix member in ONE direction (`SK`→`CO`) left every pin green — both sides
read empty — and detection required swapping BOTH ways, which is a stronger mutation than
a real defect would be: a producer that emits the wrong member emits it one way. A third
`t2` row now makes the two members answer **disjointly** (`sk`→ids 1,3; `co`→id 2) and the
`co` polarity pair is added, so a one-directional substitution reds whichever arm it lands
away from.

### 3d. The unit pins

The leg-window pin drives the production symbol **by name**
(`rebaseOuterLegValueOrdinal`), not a local copy — the defect that NAK'd RFC-218's
implementation. It covers both leg kinds × both pin states × both type-knownness states,
with four single-accessor controls, because a decline whose control also fails is
uninterpretable.

**Two dimensions this RFC's own work missed, both now covered.** First, all four typed
arms passed while every real query crashed: `into` was a typed-nil `*RecordType` in a
`Type` interface, so the assertion arm succeeded with a nil receiver — the fixture only
ever stated a type. Second, and it is this RFC's own rule landing on its own fixture: the
leg-window pin's reference is `L.N.SK` with `SK` at struct ordinal **0**, so forcing a
suffix ordinal to 0 is a NO-OP against it. It stayed green under a mutation
`values_test` caught. A fixture whose discriminating value is the mutation's target
cannot discriminate.

---

## 4. A SEPARATE PRE-EXISTING DEFECT — one, not two, and the second was MY error

Projecting a struct column alongside a projected `EXISTS` through a join's merged row
fails, with no `ORDER BY` involved. Measured with this entire change **reverted**, so it
is not a consequence of it:

```
SELECT t1.id, n, EXISTS(...) FROM t1 JOIN t3 ON ...   (no ORDER BY at all)
  -> ordinal resolution: field "N" not resolvable in the runtime row
     (ordinal -1, row columns [ID T1_ID ID N]) — malformed plan
```

Root cause: the positional merge does not model a struct-typed column (§2a), so the
reference stays lazy (`ordinal -1`) and resolves by a name the runtime row does not answer
to. It is pinned as a tripwire
(`projected_struct_root_over_a_join_still_fails_for_a_reason_that_predates_this`) that
reds when the merge learns struct columns, with the replacement assertion named.

**RETRACTED — and recorded rather than deleted, because a withdrawn measurement is the
part of this section worth reading.** Rev 1 also reported a SECOND, silent form:

```
SELECT t1.id, n FROM t1 JOIN t3 ON ...   (no EXISTS either)
  -> "returns rows, with the FIRST column 0 instead of the ids"
```

and escalated it as a live silent wrong-rows defect on master. **It does not reproduce.**
Re-measured on `426cbcea1` with the same schema and inserts, it returns `1, 2, 3` —
correct. The zeros came from the throwaway probe that produced them: it scanned a
TWO-column result into ONE destination and ignored the `Scan` error, leaving the `int64`
at its zero value. The engine was never wrong there; the instrument was, and it was a
probe I had already deleted, which is exactly the failure the "every proof gets committed
as a test" rule exists to prevent — a deleted instrument cannot be re-read when its
conclusion is challenged.

There is therefore ONE defect on this shape and it is LOUD. It is booked, and the booking
carries this correction rather than the retracted claim.

---
## 5. Java

`FieldPath.withSuffix` — `FieldValue.java:525-534`. `ofFieldsAndFuseIfPossible` recomputes
the fused node's result type rather than inheriting the first step's. `PartitionSelectRule.java:296-303`
rewrites a reference to a collapsed sibling as `ofOrdinalNumber(QOV(newUpper), index)` and the
alias then ceases to exist; `translateCorrelations`' leaf swap fuses the enclosing accessor onto it.
Java has no SQL-layer rejection for a nested `ORDER BY` key over a join — it attempts the plan and
fails downstream in `RemoveSortRule.onMatch` if no ordering satisfies. Go's `0AF00` was therefore a
divergence in FORM, and closing it removes the divergence rather than adding an extension.
