# RFC-218 — The projected-EXISTS fold must read the sort key's resolved path, not a re-derived name

**Status:** rev 9 — PARTIALLY IMPLEMENTED. The single-table arm is fixed and mutation-verified in four directions. **§5 deliverable 2 is NOT fully met:** it requires both arms to assert row order and mutation-red independently, and the JOIN arm asserts a REFUSAL, not an order — the leg-window re-anchor is unbuilt and booked in TODO.md. Awaiting the implementation review lap.
**Origin:** RFC-197 ratchet entry `cascades_translator.go # sortKeyFieldRef`, whose stated
mechanisms were measured false; the live bug behind it is this one
**Scope:** `pkg/relational/core/query/cascades_translator.go` (the `sortSource` helpers), plus
one negative pin outside it

> **Numbering.** This was drafted as RFC-216 and renumbered. **216 and 217 are both taken by
> PR #658**, which is open and not yet on master, so `ls rfcs/` showed them free. The
> directory lists only what has MERGED — check `gh pr list --state open` and the RFC files in
> each open PR's diff before claiming a number.

---

## 0. What this RFC is not

It is not the conversion the ratchet entry prescribed. That entry ruled CONVERT — correctly —
but justified it with two mechanisms, and both are false:

- **"Two keys that render alike collapse to one appended column."** They do, and it is
  harmless. `sortKeySourceValue` depends on the key *only* through `sortKeyFieldRef(k)`, so
  alike-rendering keys carry an identical source value by construction. Measured over five key
  shapes against both source classifications.
- **"`pullUpSortKeyValue` recovers them by scanning the folded projection's field names."** It
  does not. It recovers them at the **value** match. Measured by renaming an appended column to
  a string no name scan could find; resolution succeeded unchanged.

Carrying the append index `len(fields)+i` fixes neither, because neither is broken. The real
defect is one level down and this RFC is about that.

---

## 1. The bug, in two shapes, both user-facing

Schema: `CREATE TYPE AS STRUCT nst (sk BIGINT)`, `CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id))`.

```
SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY n.sk
  -> ERROR values: no ordering defined between *dynamicpb.Message and *dynamicpb.Message

SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.n.sk
  -> ERROR ordinal resolution: field "SK" not resolvable in the runtime row
     (ordinal -1, row columns [ID N]) — malformed plan
```

Both reach `sortKeySourceValue` — confirmed by instrumenting it and first verifying the
instrument was live (7 hits on `TestFDB_ProjectedExists_Round4`). The printed state:

```
field="N"       Expr="N.SK"    Bare="SK" Qual="N"    Qualified=true Value=&FieldValue{Field:"N", Resolved:<non-nil>}
field="T1.N.SK" Expr="T1.N.SK" Bare="SK" Qual="T1.N" Qualified=true Value=<nil>
```

And a THIRD failure mode, which an earlier revision missed entirely — the crossing breaks
differently depending only on what ELSE the SELECT list projects:

```
SELECT id, EXISTS(...)          ORDER BY n.sk -> no ordering defined between *dynamicpb.Message
SELECT id, n, EXISTS(...)       ORDER BY n.sk -> 0AF00: Cascades planner could not plan query
SELECT id, n AS nn, EXISTS(...) ORDER BY n.sk -> 0AF00: Cascades planner could not plan query
```

Omitting it from the deliverable list was **§2a's error one layer out**: a target population
enumerated short. Each shape now has its own arm and its own mutation-red.

These are **two different bugs** that happen to share a symptom, and conflating them is how the
ratchet entry went wrong. They are separated in §2 and §3.

### 1a. What is NOT broken — read this before the failing queries above

A structurally identical query plans and sorts correctly today:

```
SELECT id FROM t1 ORDER BY n.sk                                  -> ids [5 4 3 2 1]  CORRECT
SELECT t1.id FROM t1 JOIN t3 ON t3.t1_id = t1.id ORDER BY n.sk   -> ids [5 4 3 2 1]  CORRECT
```

`ORDER BY n.sk` is **not** broken in general, and a reviewer holding it next to the failing
query cannot tell them apart from the ORDER BY clause alone. **The discriminator is the
projected `EXISTS` in the SELECT list.** That is what routes the query into
`translateProjectOverExistsFilter` — the RFC-141 fold — and only the fold re-derives the sort
key from a rendered name. Every failing query in this RFC has a projected `EXISTS`; every
passing one does not.

This is also the correction to an earlier draft's claim that the testing axis was "absent". It
is not. `pkg/relational/sqldriver/struct_ordering_gate_fdb_test.go:176-178` asserts exactly the
non-fold shape, in a subtest named `scalar_and_leaf_orderings_still_plan`, and it passes:

```go
if err := drain("SELECT id FROM T_S ORDER BY home.city"); err != nil {
    t.Fatalf("ORDER BY on a struct LEAF field must still plan: %v", err)
}
```

**The axis exists and is guarded. What was unprobed is a DIMENSION within it — the fold arm.**
That distinction is the finding: "add a test" would be wrong, because the test is there and
correct; what is missing is the crossing of nested-ORDER-BY with the projected-EXISTS fold. The
earlier "absent, not thin" phrasing was asserted over a population of 8,364 matches for
`grep -rniE "order by [a-z_0-9]+\.[a-z_0-9]+" pkg/relational` without enumerating it, which is
precisely the unscoped negative this repo's rules forbid.

---

## 2. Shape A — `ORDER BY n.sk`: fold-local, and BOTH arms are affected

> **THIS SECTION COST THREE REVISIONS, AND IMPLEMENTATION REVIEW SHOULD LOOK HARDEST HERE.**
> §2 has been NAK'd three times running — **never on its direction, always on its mechanism**:
> rev 1 confined the fix to one arm and justified it by analogy; rev 2's "carry the value"
> ignored that the carried ordinals are stated in a pre-merge domain; rev 3 named two guards
> that both refuse the entire target population. Every other section of this RFC has survived
> review unchanged. That asymmetry is evidence, not bad luck: **the fold's re-anchoring surface
> is genuinely less understood than the rest of this document**, and the reviewer's prior on any
> claim made about it should be correspondingly lower.
>
> Two rules were bought with those three revisions, and they govern the rest of this RFC:
>
> 1. **A cited mechanism is not evidence until it has been run against the shape in question.**
>    This series went 0-for-5 on "the other path already does this" — two accessors that
>    differed, a predicate whose polarity inverted, a guard collapsing two cases, and a rebase
>    excluding the shape entirely.
> 2. **A decline is uninterpretable until a control succeeds.** A harness defect and a genuine
>    refusal are the same observation. §2c's first run had two failing controls and its declines
>    proved nothing until they were fixed.
>
> Accordingly §2's mechanism is no longer specified — it is built and exercised (§2f).

The resolver does the right thing. `ResolveIdentifier` recognises `N` as a struct column
(`semantic/scope.go` Rule 5) and `fuseNestedAccessors` (`query/expr/expr.go`) fuses the
reference into **one** `FieldValue` whose `Field` is the struct root `N` and whose
`Resolved.Accessors` is the full `[N, SK]` path, with `Typ` the leaf type. The `.SK` is present
throughout — in `Resolved`, not in `Field`.

The ordinary, non-fold sort path is correct — **measured, not inferred**: `SELECT id FROM t1
ORDER BY n.sk` and the same query over a JOIN both return `[5 4 3 2 1]` (§1a). An earlier draft
explained this by citing `bakeFlatRefsAgainstColumns`' early-out as "the same early-out, for the
same reason". **That citation is withdrawn.** It is not the same operation (a skip-this-node
guard inside a tree walk that returns the node and continues over siblings, versus a terminal
answer for the whole key), and not the same predicate — it collapses `Child != nil` and
`Resolved != nil` into one disjunction with one outcome:

```go
// cascades_translator.go:6357-6361 — bakeFlatRefsAgainstColumns
if !ok || fv.Child != nil || fv.Resolved != nil { return node }
```

Read honestly, that precedent does not support treating the two arms differently; if it governs
at all it says they should be treated **alike**. The measurement below says the same thing, and
the design follows the measurement.

The fold throws the path away at `cascades_translator.go:4947`:

```go
if fv, ok := k.Value.(*values.FieldValue); ok {
    if fv.Child == nil {
        return strings.ToUpper(fv.Field)   // <-- "N". fv.Resolved.Accessors ignored.
    }
    return strings.ToUpper(values.ColumnNameValue(fv))
}
```

`fv.Child == nil` holds for a single-table nested reference, so the function returns the struct
root. Every fold decision flows from that one string — `sortKeyName`, `sortKeySourceValue`,
`collectExtraSortColumns` — so the fold appends the whole struct as the hidden column and sorts
by it. `sortKeySourceValue` compounds it by re-minting a **fresh, path-less** `FieldValue` from
the flat name, discarding the resolver's work a second time.

### 2a. The JOIN arm is reachable at TWO segments — measured

The earlier draft assumed the `fv.Child != nil` arm was safe because `ColumnNameValue` renders a
multi-accessor path dot-joined. **It renders it and then the same last-dot split destroys it.**
A JOIN whose nested column is unambiguous needs no leg qualifier, so it reaches the fold in two
segments. Instrumenting the arm selection directly (`fv.Child == nil`, `fv.Resolved == nil`,
`len(Resolved.Accessors)`):

```
WW2  fold, single-table, ORDER BY n.sk
     ZZARM childNil=true  resolvedNil=false accessors=2 field="N"
     -> ERROR values: no ordering defined between *dynamicpb.Message and *dynamicpb.Message

WW3  fold, JOIN, ORDER BY n.sk  (2 segments, `n` exists only on t1)
     ZZARM childNil=FALSE resolvedNil=false accessors=2 field="N"
     -> ERROR ordinal resolution: field "SK" not resolvable in the runtime row
        (ordinal -1, row columns [ID T1_ID ID N]) — malformed plan

WW3  control: same JOIN, no projected EXISTS
     -> ids [5 4 3 2 1]  CORRECT
```

Both arms carry a **multi-accessor `Resolved`** and both discard it. The JOIN case is **not**
Shape B — it is two segments, fully resolved, and the resolver made no refusal. The
justification that would have earned the narrow fix (that the JOIN arm's nested case is only
reachable at three segments, hence out of scope under §3) is therefore **false**, and it was
available to check.

### 2b. The ordinals are stated in the WRONG DOMAIN — so "carry the value" is not the fix

Every failure in §1 and §2a is **loud**: a type error or a `malformed plan`. The fix's own
residual risk is **silent**, and it had to be measured before designing around it. Carrying
`k.Value` carries its `Resolved` *ordinals*, and an ordinal resolved against the wrong layout
does not error — it returns the wrong column.

Instrumenting the resolved path and its stated domain at the fold:

```
WW2  fold, single-table   accessors={N@1}{SK@0}  domain=domain(2;2:ID;1:N)
     fold's flowed row:   [ID N]              -> 2 columns, MATCHES the domain

WW3  fold, JOIN           accessors={N@1}{SK@0}  domain=domain(2;2:ID;1:N)
     fold's flowed row:   [ID T1_ID ID N]     -> 4 columns, DOES NOT MATCH
```

The join key's root ordinal is `N@1`, stated in a **2-column `[ID, N]` domain — the pre-merge
`t1` source row**. The fold evaluates against the merged row, where **slot 1 is `T1_ID`**. So a
naive "return `k.Value`" would, in the join arm, read a `BIGINT` foreign key where the sort
expects a struct — and the sort would order by that column with no error at all. The bug it
replaces at least announced itself.

**This is the same trap as §2a, one layer down**, and it is worth naming as such: the
single-table domain *matches*, so "carry the value" passes the single-table reproducer and is
silently wrong on the join. A design validated on the shape you happen to have is how this
defect has now nearly survived three laps.

**Decision.** The fold carries the resolved **path**, and **re-anchors its root ordinal in the
layout the fold actually evaluates against**, deriving that ordinal rather than trusting the
carried one. What follows is the exercised version; rev 3 named two mechanisms and both were
wrong.

### 2c. Neither guard I cited can perform the operation — exercised

Rev 3 cited `OrdinalIn` for the domain check and `rebaseOuterLegValue` for the re-anchor. Both
were read, not run. Run, on the exact WW3 shape (`{N@1}{SK@0}`, root stated in the `t1` domain):

```
AAA1  OrdinalIn  multi  vs MATCHING   domain -> ordinal=0 ok=false   (accessors=2)
      OrdinalIn  multi  vs MISMATCHED domain -> ordinal=0 ok=false
      OrdinalIn  single vs MATCHING   domain -> ordinal=1 ok=true    [CONTROL]

AAA2  rebaseOuterLegValueOrdinal[FlatRun] multi-accessor -> ok=false (unchanged)
      rebaseOuterLegValueOrdinal[Nested]  multi-accessor -> ok=false (unchanged)
      rebaseOuterLegValueOrdinal[FlatRun] single-accessor -> ok=true  [CONTROL]
```

`OrdinalIn` refuses on **arity before it ever reads the domain** (`values.go:487-489`,
`len(p.Accessors) != 1` precedes `p.Domain != frontier`). So it returns `false` for the entire
target population and **cannot distinguish WW2 — where the domain matches and the answer should
succeed — from WW3, where it must decline.** The rev 3 citation was true and vacuously so.

`rebaseOuterLegValueOrdinal` — the *ordinal* twin, which is the one the lazy rebase's own panic
points to for pinned references, and which rev 3 did not cite at all — **also declines every
multi-accessor path**, under both leg kinds, for a naming-collision reason unrelated to domains.
Both controls pass, so the harness is exercising the real path and the declines are the
functions' own.

That is now **five** instances in this series of "the other path already does this" failing on
contact. The rule I am adopting for the rest of this RFC: **a cited mechanism is not evidence
until it has been run against the shape in question, with a control that passes.**

**Consequence, and it is disqualifying for rev 3 as written:** since both mechanisms refuse the
whole population unconditionally, "fail closed when the layout cannot be stated" degenerates to
**fail closed always** — which is §3's rejected alternative, applied to every input, arrived at
by accident.

### 2d. What fail-closed produces — measured, not assumed

Three outcomes were possible and they are not interchangeable. Measured against the two declines
that exist today (LEFT-join-with-ORDER-BY, and a computed non-projected key):

```
DECLINE -> ERROR: 0AF00: projected EXISTS in this query shape is not yet supported
DECLINE -> ERROR: 0AF00: projected EXISTS in this query shape is not yet supported
```

A **clean error**, not a silently-dropped `ORDER BY` and not rows in arbitrary order. §3's
objection to declining therefore rests on the outcome it assumed, and that assumption holds: a
decline is loud. Had it been a dropped ordering, declining would have been disqualifying
outright — a silently-wrong order is worse than the wrong-column hazard §2b exists to avoid,
since both are quiet and the wrong column at least yields a stable answer.

### 2e. The re-anchor: derived and asserted, not carried

The capability does exist; what does not exist is an entry path to it, and that is code to
write, not a wall. `NewFusedFieldValueOfNestedOrdinal` builds exactly the required shape —
exercised:

```
AAA3  NewFusedFieldValueOfNestedOrdinal(mergedQOV, slot=3, t1Type, legOrd=1)
      -> accessors={N@3}{N@1}  domain=domain(4;2:ID;5:T1_ID;3:ID2;1:N)
```

A two-step path whose **root ordinal addresses the merged row** and whose **domain is the merged
domain**. That is the re-anchor, constructed by an existing primitive. The build for
`{N@1}{SK@0}` is the same composition: bake the root against the fold's flowed layout, then
`FieldPath.WithSuffix` the surviving accessors.

**The rule, and it is a design requirement rather than a test obligation:** the re-anchor must
**derive** the root ordinal from the flowed layout by name-or-identity lookup and then **assert
agreement** with the carried one — never accept the carried ordinal because a domain token
compared equal. The coincidence case is why: `[ID N]` puts `N` at 1, and any merged layout that
also happens to put `N` at 1 makes the re-anchor a no-op that is correct **by accident** and
passes a token comparison. Deriving-and-asserting confirms the coincidence instead of assuming
it; a disagreement declines the fold (§2d: loudly).

Since `OrdinalIn` cannot serve (§2c), the design needs a new arity-tolerant
`RootOrdinalIn(frontier)`. Building it is part of this work — and it, and the re-anchor, were
built and exercised before this revision was sent to review (§2f), because a specified-not-run
mechanism is exactly what the previous three revisions were NAK'd for.

### 2f. The mechanism is BUILT and EXERCISED — it is not a specification any more

> **The harness is COMMITTED**, at
> `pkg/recordlayer/query/plan/cascades/values/nested_root_reanchor_test.go`. Revs 2 and 3 were
> NAK'd for *cited-but-unrun*; keeping these numbers in prose over a deleted probe would have
> been *run-but-unkeepable* — the same hole one step later, and a breach of the standing rule
> that every proof gets committed as a test. Every arm below is driven there, including the
> ambiguity arm, and the join case uses the REAL merged row `[ID T1_ID ID N]` (duplicate `ID`
> and all) rather than a synthetic stand-in.

`RootOrdinalIn` differs from `OrdinalIn` in one line — it drops the arity gate and keeps the
domain gate — and that is precisely the separation §2c showed was missing:

```
BBB2  RootOrdinalIn multi vs MATCHING   domain -> ordinal=1 ok=true
      RootOrdinalIn multi vs MISMATCHED domain -> ordinal=0 ok=false
      (OrdinalIn, for contrast)  MATCHING -> 0,false   MISMATCHED -> 0,false
```

The re-anchor, run against the real flowed layouts:

```
BBB1  WW2 single-table flowed [ID N]            -> OK  path={N@1}{SK@0}  slot 1 -> column "N"
BBB1  WW3 JOIN flowed [ID T1_ID ID N]          -> OK  path={N@3}{SK@0}  slot 3 -> column "N"
BBB3  COINCIDENCE flowed [ID N X Y]             -> OK  path={N@1}{SK@0}  slot 1 -> column "N"
BBB3  MISMATCH flowed [ID Q] (root absent)      -> DECLINE (root column absent)
BBB3  carried ordinal disagrees in SAME domain  -> DECLINE (disagrees with the flowed layout)
```

Reading these in the order that matters:

- **WW2 succeeds.** This is the case §2 exists to fix, and it is the one that would have proved
  the design had collapsed back into fail-closed-always. It does not: it derives root `N` at
  slot 1 and yields the struct.
- **WW3 re-anchors** to merged slot 3 — column `N`, the struct. It does **not** read slot 1
  (`T1_ID`). The rev-2 silent-wrong-column hazard is averted by construction, not by luck.
- **The coincidence case is confirmed, not assumed** — and the mechanism deserves a precise
  statement rather than a flattering one. The flowed layout `[ID N X Y]` puts `N` at 1, the
  same slot the carried ordinal names. The result is correct because `FieldIndex` **derived** 1
  from that layout. The *assertion* did not fire here at all: the carried ordinal's domain is
  the 2-column `t1` layout, so it is not comparable and is correctly not compared. Derivation
  is what carries correctness; the assertion is a separate tripwire for the same-domain case,
  and the last line above shows it firing when a carried ordinal contradicts the layout it
  claims to index.

Both declines are loud (§2d: a clean `0AF00`), and both positive controls succeed — which, per
§2c's rule, is what makes the declines interpretable at all rather than possible harness
defects.

**FIRST THING IMPLEMENTATION MUST CONFIRM — a check to perform, not a risk to note.** §2f is
exercised against hand-built layouts: it proves the composition behaves as specified *when handed
a given layout*, never that the fold hands it that one.

**And the fold has TWO layouts, so "the flowed layout" is not yet a well-formed instruction.**
An earlier revision said "the merged row" and that under-specifies by exactly one:

| layout | what it is | who uses it |
|---|---|---|
| outer / merged scan row | `[ID T1_ID ID N]` for WW3 | `collectExtraSortColumns` / `sortKeySourceValue` build the hidden column's **value** against this |
| folded projection | `[ID HAS_T2 N]` for WW2 | `applySortOverRef` resolves the sort **key** against this, via `outputFieldDomain(fields)` (`cascades_translator.go:5199-5222`) |

The re-anchor must name which one it targets and assert it — they differ in width, in order,
and in content, and a root ordinal derived against the wrong one lands on a real column and
returns rows.

**INFERRED, NOT MEASURED — and it is a POST-FIX consequence stated in the present tense by an
earlier revision, which is the exact error §2 itself adopts a rule against.** Read against the
code today it is FALSE: `collectExtraSortColumns` suppresses the hidden column via
`sortKeyInOutput`'s VALUE match (`:5107`), and today's path-less `FieldValue{N}` matches the
projected `N`. The duplicate appears only AFTER the fix, when the source value carries `{N}{SK}`
and stops matching. The arm is justified independently by the Shape-B route above; this is not
load-bearing for it.

What it DOES surface is a real consequence nobody had named: post-fix, `sortKeyInOutput` stops
matching for nested keys, so hidden columns are APPENDED where today they are suppressed. That
is a plan-shape change on Shape A, and `fields` is mutated in place at `:4725` with the SAME
slice reaching `applySortOverRef` at `:4746`, so appended extras are inside the key's
`outputFieldDomain`. Expect golden movement from this and review it record by record. The appended hidden column is named by `sortKeyFieldRef` → `"N"`, so a
folded projection that already carries a column named `N` gets a **second** one. The fold
manufactures the duplicate-root layout itself, out of SQL that is perfectly unambiguous. No
resolver rejection stands between a user and that row, which is the strongest single reason the
arm must exist in code.

This check is first because its failure mode is **silent**: a wrong-but-plausible layout still
re-anchors to *some* column and still returns rows — the same shape as the rev-2 hazard this
section exists to remove.

### 2g. The ambiguity arm is unreachable — measured, and pinned as a test

§2e's re-anchor declines when the flowed layout holds more than one column of the root's name.
A merged join row *can* hold two same-named columns, so the open question was whether that
decline is a capability regression on a shape users write — §3's objection arriving by a third
route. Measured:

```
self-join, BARE n.sk                  -> ERROR 42702: Ambiguous reference N.SK
t1 JOIN t4 (both expose `n`), BARE    -> ERROR 42702: Ambiguous reference N.SK
self-join, QUALIFIED a.n.sk           -> 3 segments = Shape B; row columns [ID N ID N]
CONTROL only t1 exposes `n`           -> reaches the fold (the WW3 failure)
```

For **two-segment** keys, duplicate root names and a resolvable nested key are mutually
exclusive, and they are the same fact: a bare `n.sk` is ambiguous *precisely when* more than one
leg exposes `n`, and SQL rejects it with `42702` before the fold sees a key.

**That licence is CONDITIONAL, and an earlier revision of this section stated it as absolute.
It was wrong, and Shape B is why.** A merged row carrying two `N` columns is reachable today
through the three-segment form:

```
self-join,  ORDER BY a.n.sk    -> row columns [ID N ID N]
t1 JOIN t4, ORDER BY t1.n.sk   -> row columns [ID N T1_ID ID N]
```

Those fail only because `walkColumnRef` refuses a 3-segment reference — and **that refusal is a
divergence, not a rule.** Java has no arity cap anywhere on this path, verified at the pinned
tag: `RelationalParser.g4:747` is `fullId : uid (DOT uid)*`; `SemanticAnalyzer.java:574-597`
walks `remainingPath` with no arity branch; `V2PlanGeneratorTests.java:978` *executes*
`select x.c.m.n.p from (select a.b.c from t1) as x`. §3 is therefore right on **sequencing** and
wrong on **grounds**: Shape B is deferred work, not an unsupported shape, and closing it is
prescribed by this very workstream.

So the instant that divergence closes, duplicate roots and a resolvable nested key coexist —
the exact conjunction this section previously called impossible. And `RecordType.FieldIndex`
first-matches (`type.go:825`), so `b.n.sk` over a self-join would silently read the **left**
leg's `N`: the wrong-column read this whole design exists to prevent.

**Consequence: the ambiguity arm is BUILT, not licensed away.** It counts duplicate root names
explicitly and declines, because `FieldIndex` alone cannot — a first match is indistinguishable
from a correct one. Every arm is unit-driven in
`values/nested_root_reanchor_test.go` (§2f), including this one. The negative result narrows to
what it can support: *today, two-segment keys cannot reach the arm*, which is a statement about
scheduling, not about the design.

**A second escape hatch the negative result did not account for.** Java suppresses
ephemeral-derived duplicates by name *before* its ambiguity assert
(`SemanticAnalyzer.java:491-510`), so a mixed direct/ephemeral pair yields one match and **no**
`AMBIGUOUS_COLUMN`. If Go matches that behaviour, a duplicate root name arrives with no `42702`
at all — and the pinning test stays green while the conjunction becomes reachable. That is the
one re-armer the SQL-level assertions structurally cannot see, which is a further reason the arm
exists in code rather than resting on them.

That is a negative result, so it is committed as a test rather than asserted here —
`nested_sort_key_ambiguity_fdb_test.go` — pinning the `42702` rejection that makes the arm
unreachable, with a failure message naming what re-arms it: any change letting a bare
multi-segment reference resolve when its root is ambiguous (first-match, an implicit left-leg
preference, or scoping that hides a column instead of rejecting). If that happens the arm
becomes reachable and **must disambiguate by leg IDENTITY, never by first name match** — the
RFC-197 principle that a name is display-only and must never decide.

**Why the discriminator is multi-accessor, stated as a principle rather than a threshold:** a
single-accessor `Resolved` is fully expressible by `Field`, so rendering it loses nothing.
Multi-accessor is precisely the case where **the rendered name cannot express the path**. The
rule is therefore *"act when the rendered name is lossy"*, not *"act when there are ≥2
accessors"* — which is why it does not need widening to single-accessor references, and why it
is not a constant fitted to two reproducers.

**Rejected: confining the fix to `fv.Child == nil`.** It would have left the JOIN shape above
failing with an unchanged error message, and — worse for review — it would have looked correct,
because the single-table reproducer passes under it. A fix that repairs the query you probed and
not the one you did not is how this defect survives a second lap.

**Rejected: rendering the full path in both arms** (`ColumnNameValue` everywhere). It fixes the
symptom and keeps the round trip: the name becomes `N.SK`, which `stripSortQualifier`'s last-dot
rule turns straight back into `SK`. That is the defect restated, not removed.

**Note on the JOIN decline vs Java.** Java has no SQL-layer rejection for the join shape: it
attempts the plan and fails downstream in `RemoveSortRule.onMatch`. Go's clean `0AF00` is
therefore a divergence in FORM, not merely a narrower Go behaviour, and a gap vs Java remains
open until the leg-window re-anchor lands.

**Rejected: declining the fold for nested keys.** A capability regression — Java is *inferred*
to plan this shape (no Java test combines a projected EXISTS with an ORDER BY on a
non-projected column, so this is an inference from `generateSelect` accepting arbitrary
`remainingOrderByExpressions`, not an observed plan), and §1a shows Go plans it too on every path except this one. Declining would convert a
bug into a documented narrowing.

---

## 3. Shape B — `ORDER BY t1.n.sk`: not a fold bug, and it does not convert here

`walkColumnRef` rejects a 3-segment FullId outright
(`query/expr/walk.go`, `UnsupportedExpressionShapeError{"FullId with 3 segments"}`), and
`upgradeSortKeyValues` swallows the walk error with `continue`, leaving `Value` nil and only the
text `T1.N.SK`. So a table-qualified nested path is unresolved **everywhere**, not just in the
fold — the ordinary path has the same hole.

**Decision.** Out of scope for this RFC, and it must not be fixed by teaching the fold to split
`T1.N.SK`. The last-dot rule at `stripSortQualifier` yields `SK`; `splitQualifier`'s own doc
already concedes it manufactures qualifier `T1.N` for `A.B.C`. Any fold-side handling of this
shape is guessing at a segmentation the resolver declined to make. The correct fix is in
`walkColumnRef` — resolve the 3-segment FullId — and it is a separate item. What this RFC owes
it is the negative pin in §5 so the hole cannot close silently and re-arm the fold.

---

## 4. The last-dot survey, and what it does and does not license

Every `strings.LastIndex` / `splitQualifier` site in `core/query` and `core/embedded` was
surveyed. Two can demonstrably receive a >2-segment input today, and both are the fold's:
`stripSortQualifier` and `splitQualifier`, reached only from the `sortSource` helpers.

Several others (`expressionOutputColumns`, `stripColumnQualifier` for GROUP BY,
`derived_unnest.go`'s projection-output naming) carry the *same* last-dot assumption over
projection/group-key renderings, and I could not establish from reading whether a nested
projection reaches them. **That is recorded as unknown, not as safe.** They are not touched by
this RFC and no claim is made about them; a `SELECT n.sk` / `GROUP BY n.sk` probe is the way to
settle it and is not a precondition for this change, because this change does not alter what
those sites receive.

---

## 5. Deliverables

1. The fix in §2, in `cascades_translator.go` only.
2. **The earning test, on an unprobed DIMENSION of an existing axis.** The axis exists and is
   guarded — `struct_ordering_gate_fdb_test.go:176-178` asserts nested `ORDER BY` plans, and
   passes (§1a). What was never probed is the **crossing** of that axis with the projected-
   EXISTS fold. So the finding is not "add a test"; it is "this axis has an unprobed dimension",
   and the two have different consequences: the first tops up a file, the second says the
   existing guard is correct and cannot see the defect no matter how many cases are added
   along it.

   The test must cover **both arms**, because §2a shows a fix validated on one of them looks
   correct while leaving the other broken:
   - single-table, key NOT projected: `SELECT id, EXISTS(...) FROM t1 ORDER BY n.sk`
   - single-table, struct ROOT also projected: `SELECT id, n, EXISTS(...) … ORDER BY n.sk`
   - single-table, struct root projected under an ALIAS: `SELECT id, n AS nn, EXISTS(...) …`
   - JOIN fold (`fv.Child != nil`, two segments): the same with `JOIN t3 ON …`

   Four, not two. Enumerating this list short is the same error §1 names one section earlier,
   and it was committed here after §1 was corrected.

   Each asserts row order and mutation-verifies red **independently** — a mutation that only
   reds one arm has not earned the other. **This deliverable is SPECIFIED, NOT WRITTEN**, and
   the distinction matters at implementation review: "each arm mutation-reds independently" is
   currently a property of an artifact that does not exist. A single fixture exercising both
   arms would satisfy the sentence and *not* the property — the arms must be separately
   mutable and separately red, and that gets checked against the code, not against this line.

   It must also cover the §2b hazard, which no row-order assertion on a *correct* fix will
   catch by itself: a test that pins the join key resolving to the right **column** (not merely
   a sorted result) is what would fail if the root ordinal were carried un-re-anchored. Ordering
   by the wrong column can still produce a plausibly-ordered result set.
3. **A companion test on the non-fold path** asserting `SELECT id FROM t1 ORDER BY n.sk` is
   correct. This is currently a code-path *reading*, not a measurement, and an unmeasured
   "the other path is fine" is exactly the corollary-inherits-authority error.
4. **A negative pin for §3** asserting the 3-segment shape is refused. Its failure message must
   say **what a PASS would mean**, not merely that something changed: that `walkColumnRef` has
   grown 3-segment support, that the resolver therefore no longer declines to segment
   `T1.N.SK`, and that the fold's decline is now the *wrong* answer and must be revisited under
   §2's rule rather than left to fall through. A negative pin whose message only says "this
   changed" makes the next reader re-derive why anyone cared.
5. The corpus plan-shape comparison — see §6.

---

## 5-RESULT. IMPLEMENTED — single-table arm lands, JOIN arm declines

```
SELECT id, EXISTS(...)          ORDER BY n.sk -> ids [3 2 1]   CORRECT
SELECT id, n, EXISTS(...)       ORDER BY n.sk -> ids [3 2 1]   CORRECT
SELECT id, n AS nn, EXISTS(...) ORDER BY n.sk -> ids [3 2 1]   CORRECT
JOIN + nested key                             -> 0AF00 clean refusal (was: malformed plan)
```

`RootOrdinalIn` and `ReAnchorRootInto` are production methods on `FieldPath`;
`sortKeySourceValue` routes a multi-accessor key through the re-anchor instead of the rendered
name. **The constraint held**, and the reading that shows it must be the UNFILTERED one. Full
`sqldriver_test`, `--nocache_test_results`, at HEAD:

```
existsSortSplit  calls 44 | carried 0 | AGREED 44 | DIVERGED 0 | MANUFACTURED 0 | bare 0
```

`DIVERGED 0` over a real 44-call population, identical to the committed pre-fix baseline: the
zero was **not widened**. An earlier revision quoted `calls 0 | DIVERGED 0` from a NARROWED
`--test.run` run, where every site read 0 — including `displayLabelStrip`, documented at 750
calls — and where the census printed its own warning that at zero the check holds VACUOUSLY.
The conclusion was right and the measurement proved nothing; that is the corollary error this
document keeps catching, committed by this document.

**The JOIN arm declines rather than shipping.** The leg-window re-anchor
(`rebaseOuterLegValueOrdinal`) refuses multi-accessor paths (§2c AAA2), so building it is
remaining work on a different surface. Declining is loud (§2d) and strictly better than today's
`malformed plan`; it is NOT the fail-closed-always degeneration §2c warned about, because the
single-table arm demonstrably succeeds.

**Mutation, four directions, each independent.** With the production change reverted and the
tests kept, every fold arm reds on its own:

```
nested_key_not_projected             -> no ordering defined between *dynamicpb.Message ...
struct_root_also_projected           -> 0AF00: Cascades planner could not plan query
struct_root_projected_under_an_alias -> 0AF00: Cascades planner could not plan query
nested_key_over_a_join               -> ordinal resolution: field "SK" not resolvable in the
                                        runtime row (ordinal -1, row columns [ID T1_ID ID N])
```

The two ambiguity assertions stay GREEN under the same mutation, correctly — they pin a
resolver fact the fold fix does not touch.

---

## 6-RESULT. The run happened, and the corpus CANNOT verify this fix

Disk was cleared (93%, 68G free), so §6's precondition was satisfied and the run was performed.

**`TestPlanShapeGolden` passes.** Nothing moved. And that green is **vacuous with respect to
this fix**, for a reason the fifth gate condition was written to catch — measured rather than
assumed:

```
source: pkg/relational/conformance/explaindiff/testdata/plan_shape.golden
  (NOT the yaml corpus -- re-deriving these from yamsql/testdata gives different
   numbers, e.g. 46 rather than 108, because the golden is the PLANNED set)

corpus:                             348 files, 2506 queries
EXISTS + ORDER BY (107 WHERE + 1 HAVING):  108 lines
SELECT-list (projected) EXISTS:       0     <- the fold this RFC modifies
JOIN + projected EXISTS + ORDER BY:   0
```

The single grep hit for `EXISTS(...) AS` is `) AS sq` — a derived-table alias, not a projected
EXISTS. **The projected-EXISTS fold is exercised by zero corpus queries.** Not thinly: at all.
So the golden could not have moved no matter what this fix does to the fold, and reporting
"nothing moved" as evidence of safety would be precisely the corollary error this document keeps
catching — invariance is not absence.

**Consequence: the fifth gate condition is currently UNSATISFIABLE, and satisfying it is part of
this work.** Before the fix merges, the corpus must gain projected-EXISTS coverage including the
JOIN + fold + nested shape, and the run must show it was *reached* — the gate is not "the corpus
contains it". Extending the corpus is the deliverable; a re-run against today's corpus would
produce the same uninformative green.

This also independently explains §1a's dimension gap from the other side: the shape that broke
is absent from the plan-shape corpus *and* from the ordering tests, which is why a defect this
loud survived.

**And an existing instrument already detects it.** Running an unambiguous nested key over a join
trips a hard zero that is currently green only because nothing exercises the shape:

```
# NARROWED PROBE on a MODIFIED tree (-test.run scoped to one new test), 5 of the
# report line's 8 fields elided here for width. NOT a whole-suite reading:
# the unfiltered production population is 44 calls, and an independent reviewer
# probe of the same shape reported calls 20 | AGREED 12 | DIVERGED 8.
# What is invariant across all three readings is DIVERGED > 0, which is the claim.
existsSortSplit  calls 4 | AGREED 0 | DIVERGED 4   (+carried/MANUFACTURED/leafOnly/bare/heuristicDecline, all 0)
  DIVERGED witness: "T1.N.SK" vs identity "T1"
FAIL: existsSortSplit manufactured a qualifier that CONTRADICTS the structured identity
      ... THE FIX IS TO USE THE IDENTITY, never to widen this zero
```

Its 44 production calls are weaker evidence of health than they look: the AGREED arm there has a **documented tautology** (`embedded_fdb_test.go:982-989`), so "green only because nothing exercises the shape" understates the case rather than overstating it. That is this RFC's conclusion, reached by an instrument that predates it, and it is the
strongest independent corroboration in the document. It also constrains the work: the fix must
make that census green **by using the identity**, and the zero must not be widened. The join
control in `nested_sort_key_ambiguity_fdb_test.go` is deliberately withheld for the same reason
— it would put the suite red against an assertion that is correct — and the implementing commit
owes it.

---

## 6. Precondition as originally stated (retained for the record)

This is translator surgery in the RFC-141 fold and it has plan-shape exposure, so the corpus
comparison is a required deliverable, not an optional one.

**It was not run. It could not be run. It is a precondition on implementation, not a step that
was skipped.**

The numbers, because a precondition without one is a sentence anybody can wave through.
`/home` during this work: **11G free → 8.8G → 5.4G**, then reclaimed to **15G**. That is
**99% utilisation** on a 932G ext4 filesystem. The repo's own rule is that ext4 above **~95%**
degrades point-lookup latency sharply and reports as a planner regression, so a comparison run
at 99% measures the disk, not this change. A green run taken there would be worse than no run:
it would launder a disk artefact into a plan-shape result.

**Gate on implementation, stated so it cannot be misread as done:**

- The comparison MUST be run before any implementation of §2 merges.
- It MUST be run below ~95% utilisation, and the utilisation at run time MUST be recorded
  beside the result.
- If goldens move, **every changed record gets reviewed individually** — no bulk re-blessing.
- **The corpus must contain a JOIN + fold + nested-`ORDER BY` shape**, and the run must confirm
  it was actually exercised. With both arms in scope (§2a) the plan-shape exposure now includes
  join-rooted sorts, so a single-table-only verification would repeat §2 rev 1's error one
  layer up — passing on the reproducer the corpus happens to contain while saying nothing about
  the arm that broke. A corpus run only exercises the arms the corpus reaches.
- **"Nothing moved" is a result to state, not a step to skip.** An un-run precondition and a
  run that found no movement are different outcomes and must never be reported the same way.

---

## 6a. Java citations — re-verified

Every Java citation in this RFC was re-read against the working tree at the pinned
`fdb-record-layer` tag (confirmed via `git describe --tags`) on a second, separate pass, because
the review lap did not independently re-verify them and a citation checked once is not a
citation known current:

| Citation | Verified content |
|---|---|
| `LogicalOperator.java:390` | `orderByExpressions.difference(output, outerCorrelations)` |
| `LogicalOperator.java:394-399` | `output.concat(remaining)` → `generateSort` → `output.expanded().rewireQov(…).rewireQov(…).clearQualifier()` — the trailing `clearQualifier()` is load-bearing and an earlier quote omitted it |
| `Expressions.java:87-96` | `rewireQov`, `int colCount = 0` → `FieldValue.ofOrdinalNumber(value, colCount)` |
| `Expressions.java:124-146` | `difference`, comparing only against `that` — no self-dedup |
| `Expression.java:254-264` | `canBeDerivedFrom` via `simplify` + `pullUp` + `containsKey` |

All five hold as cited. **Status of this table: verified twice by the author, never
independently.** The review lap did not re-check the Java sites and said so rather than
implying otherwise, so this is an unchallenged claim, not a confirmed one. Anyone relying on
the Java argument in §0 should re-read the five sites rather than inherit this table's
authority — it is the author's own measurement restated, and a fact true when observed is not
current merely because it was written down.

---

## 7. Ratchet accounting

Unchanged at **46 escapes / 33 authorities** — `TestFieldNameNeverDecides` and
`fieldDebtAuthorityTotal` green. The `sortKeyFieldRef` entry was rewritten, not retired;
nothing was implemented, so nothing retires. The companion `pullUpSortKeyValue` entry records
that its stated blocker is removable by this conversion (measured) and that it, too, retires
nothing yet.

Implementing §2 is expected to retire neither escape on its own: `sortKeyFieldRef` keeps
returning a string for the naming and guard paths. Retiring it is a follow-on that this RFC
deliberately does not claim.
