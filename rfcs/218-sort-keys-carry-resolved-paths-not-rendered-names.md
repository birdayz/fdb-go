# RFC-218 — The projected-EXISTS fold must read the sort key's resolved path, not a re-derived name

**Status:** DRAFT rev 3 — rev 2 ACKed; rev 3 adds the YY1 measurement, which changes §2's decision from "carry the value" to "carry the path, re-anchor the ordinal". Awaiting Graefe + Torvalds on the reworked §2b.
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

> **This section was reworked.** An earlier draft confined the fix to the `fv.Child == nil`
> arm and justified it by analogy to `bakeFlatRefsAgainstColumns`. Measurement killed both the
> confinement and the analogy; what follows is the measured version. The narrow design is
> recorded as rejected at the end of this section, because *why* it was wrong is the useful
> part.

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
layout the fold actually evaluates against**, failing closed when that layout cannot be stated.
It does not carry the value's ordinals verbatim. Concretely, in both arms: when `k.Value` is a
`*FieldValue` whose `Resolved` carries more than one accessor, take the accessor *path* and
re-state the root ordinal against the fold's flowed row (the same re-anchoring the NLJ rule's
`rebaseOuterLegValue` performs for leg references), rather than re-minting from a rendered
string **or** trusting a source-relative ordinal.

The machinery for the check already exists and is the right shape: `FieldPath.Domain` records
the layout the root ordinal indexes, and `FieldPath.OrdinalIn(frontier)` is documented to
**fail closed** — including, explicitly, on a multi-accessor fused path whose root addresses an
outer layout. The design must consult it rather than assume; a path whose domain cannot be
reconciled with the fold's frontier must decline the fold, not guess.

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

**Rejected: declining the fold for nested keys.** A capability regression — Java plans this
shape, and §1a shows Go plans it too on every path except this one. Declining would convert a
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
   - single-table fold (`fv.Child == nil`): `SELECT id, EXISTS(...) FROM t1 ORDER BY n.sk`
   - JOIN fold (`fv.Child != nil`, two segments): the same with `JOIN t3 ON …`

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

## 6. BLOCKER — the corpus plan-shape run is OWED and UNRUN

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
| `LogicalOperator.java:394-399` | `output.concat(remaining)` → `generateSort` → `output.expanded().rewireQov(…).rewireQov(…)` |
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
