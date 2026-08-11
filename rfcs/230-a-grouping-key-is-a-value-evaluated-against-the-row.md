# RFC-230 — A grouping key is a value evaluated against the row, not a slot in it

**Status:** IMPLEMENTED (rev 8), on `origin/master` @ `9222f968c`. **The scope
shrank for the sixth time, and in the same direction as the previous five.**
Phase 0 is discharged in full by #714/#716; §0.0's four `[PROBED]` refutations
are all FALSE against `9222f968c` and are re-probed live in §0.2. §7.4's three
`(b)` blockers are **refuted by measurement** — nested-path GROUP BY landed with
`cascades_translator.go` **byte-identical** to its parent, so the post-aggregate
rebind §7.1 ruled to be "the decisive site" and "the only edit the aggregate
layer needs" was **not needed at all**. §0.3 records what the work actually was.
Previously: REVISED (rev 7). The arity census is **classified, bounded and committed as a guard** (§7.3-§7.7, PR #715): 47 arity expressions across 36 symbols — 11 correct declines, **3 blockers**, 21 already-correct, **0 live defects**, 1 uncertain. Those 3 blockers are Phase 0/1's real scope (§7.4). Gate 7's ruling is independently corroborated. **§7.5 states the census's own limit** — it is complete for one predicate, and the discard class it cannot see provably has a member. Previously: REVISED (rev 6). Gate 7 is RULED correct and permanent (§7.1) and the depth-N ambiguity rule is ACK'd; two further gates (8, 9) and a failure of the provenance instrument itself (§12.3) are folded. **Rev 6 stops claiming a complete gate census** and publishes a classified population instead (§7.3) — the count has been wrong five times and the method, not the arithmetic, was the defect. Previously: REVISED after NAK (rev 5). Rev 4 was NAK'd on five findings; all five were
re-probed here and all five hold. The most serious is self-inflicted: **rev 4 deleted a
wrong-rows deliverable on an unprobed claim that it had already landed. It had not.** §4.2
restores it. §0.0 is the refutation that lapsed the rev-2 ACKs and still stands; §0.1 is what
rev 5 corrects on top.

**Every claim in this document is now labelled `[PROBED]` or `[READ]`**, and every "works
today" / "is reachable" / "has landed" assertion carries the command that establishes it
(§12.2). Rev 2 and rev 4 both fell on unprobed reach claims; the labels exist so a reviewer can
see which claims are load-bearing *and* unverified without re-deriving that themselves.

**Base:** rev 6 is written against `origin/master` @ **`c998c411f`**. (Rev 5's base `bd0035383` went stale during review; master has now moved three times mid-RFC.) Anchors are given
as ENCLOSING SYMBOL first: e.g. the group-key ladder is in `upgradeAggregateOperands`
(`logical_predicate.go:4523-4757`), whose qualified-arm entry is `:4649` — the ladder has
already been reported at `:4563`, `:4649` and `:4752` across three bases in this review, which
is why the function name is the citation and the number is the hint. Rev 4's base
`37ebc2dea` was already stale when rev 4 shipped — master has moved twice during this RFC's
review, and the group-key ladder alone has drifted +86 lines once. **Line numbers in this
document are therefore given as `symbol` first and `file:line @ bd0035383` second**; a reader
should locate by symbol and treat the number as a timestamped hint. Revs 1-3 were written
against a stale local `master` (`c5ffbb986`), so every number in them is suspect. Two
consequences of that staleness are substantive rather than clerical: **#705 has MERGED**, so
`rejectNestedPathGroupKey` is now *on* master (revs 1-3 said it was not, and §7/§8 reasoned
from that), and #708 (CQ-52) landed in the same span **without** changing `splitColumnRef`'s
flattening.

**Scope, as SHIPPED (rev 8) — the block below is what rev 7 planned, kept for
comparison.** Phase 0: nothing, discharged elsewhere except `logical.GroupKey.Segs`.
Phase 1: the group-key ladder's descent arm, `GroupKey.Segs` and the two prefix-strip
twins, `buildAggregateOutputSlots`' stripped arm, `validateGroupByProjection`'s
existence test, and `resolveCorrelatedColumnValueStructured` — six edits in three
files, all in `pkg/relational/core/embedded` plus the one IR field.
`cascades_translator.go` is unchanged; `canonicalizeAggregateOutputValue` is
unchanged and gate 7 is satisfied without being touched, exactly as §7.1 required
and by a rebind that already existed.
**The two "dispatched separately, NOT YET LANDED" items are both settled and
neither is owed.** `[PROBED]` at `9222f968c`: `bakeSegmentedColumnRef` now opens
with the nested pass-through §4.2 asked for — *"An ALREADY-RESOLVED or
CHILD-BEARING reference is not a lazy carrier, so it is PASSED THROUGH"* — so the
guard landed. And the standalone ORDER BY refusal is not owed either, because the
defect it was to report is gone: `SELECT id FROM nested ORDER BY r.v.z` answers 8
rows. A refusal is the right report for a capability gap and the wrong one for a
capability.

**Scope:** **Phase 0** — the multi-segment resolver (`semantic/scope.go:334`, its two wrappers
and 4 non-test callers) and the flattened-qualifier carriers (`select_parser.go`'s
`splitColumnRef`, `groupKeyRef`, `logical.GroupKey`, `colRef`). **Phase 1** — the group-key
ladder in `pkg/relational/core/embedded/logical_predicate.go`, and the refusal added by
#705, **plus `canonicalizeAggregateOutputValue`** (§7.1 — revs 1-4 declared the aggregate layer
out of scope and gate 7 falsifies that). The aggregate **executor** is genuinely unchanged
(§3); the aggregate **output canonicalisation** is not.
**Retires:** the `0AF00` blanket refusal `rejectNestedPathGroupKey` (#705, `8bfd6da0d`),
narrowed to a residual (§7).
**Depends on:** RFC-229 §2.3 (group-key naming by resolved path — landed, `c5ffbb986`,
*deliberately first*; see §6.1). **NOT** on RFC-204 Phase 3 in the way revs 1-3 claimed: that
resolver exists but is depth-capped at one step, so extending it is Phase 0 *work*, not a
dependency already met (§0.0).
**Dispatched separately, and NOT YET LANDED — verified, not assumed:** the
`bakeSegmentedColumnRef` guard (§4.2) and the standalone ORDER BY refusal (§3.2). Both are
correct independently of this RFC and are being landed on their own. `[PROBED]` at
`bd0035383`: the guard is **absent** (§0.1 shows the sweep), and no ORDER BY refusal exists —
`SELECT id FROM nested ORDER BY r.v.z` still reaches the executor. **They are therefore
tracked here as owed until a probe shows otherwise** (§12.2). Rev 4 deleted the guard on an
unprobed "already landed" claim; rev 5 does not repeat that, even though the same source now
says both are dispatched.
**Relates to:** RFC-197 (column identity is an ordinal), RFC-225 (nested descent reads by
ordinal — adjacent, blocked, NOT a prerequisite), CQ-56 (the ordinal domain — NOT a
prerequisite, §5.2), RFC-204 Phase 5 / RFC-202 (the corpus file's DDL gate, §8).

> **Numbering.** The highest merged RFC in `rfcs/` is 229
> (`229-a-column-states-its-own-name.md`, merged as #704, `c5ffbb986`). `gh pr list
> --state open` returns #709, #579 and #486; #579 claims `rfcs/205-…` and #486 claims
> `rfcs/graph-db-on-fdb.md`; #709 claims no RFC number. **230 is free.**

---

## 0.2 §0.0 IS ITSELF REFUTED — the resolver walks the path, re-probed live

**Everything in §0.0 below describes a tree that no longer exists.** It is kept
because the *process* record matters (an implementation stopped and sent the RFC
back rather than build a dead arm), but every one of its measurements is now
false. PR #714 landed the multi-segment SQL path whole, and #716 finished the
mint that fuses the descent. Re-probed against real FDB on the corpus schema at
`9222f968c`, verbatim, replacing §0.0's table cell for cell:

```
SELECT r.v.z FROM nested              => cols=[Z] rows=8   vals 100 100 100 100 140 140 …
SELECT nn.r.v.z FROM nested AS nn     => cols=[Z] rows=8   vals 100 100 100 100 140 140 …
SELECT r.v FROM nested                => cols=[V] rows=8
SELECT max(q.s) FROM nested
  GROUP BY r.v.z HAVING r.v.z > 120   => 0AF00: grouping by the nested field "R.V.Z" is not supported
```

Three of four cells inverted. The fourth **changed layer**, which is the load-
bearing part: the corpus query no longer dies at `42703` in
`resolveColumnRefStructural`, it reaches `rejectNestedPathGroupKey` — the ladder's
own refusal, i.e. exactly the site §0.0 said a descent arm would be dead code
for. **§0.0's central conclusion — "a descent arm added at the ladder would be
DEAD CODE for the corpus shape" — is therefore false**, and so is §3.2's
three-different-reports table: `SELECT`, `WHERE` and `ORDER BY` over `r.v.z` all
answer rows on this base.

The four `[PROBED]` premises the implementation brief listed are confirmed false
by reading, too, and each is now the opposite of what §0.0 asserts:

| §0.0 claim | at `9222f968c` |
|---|---|
| `walk.go#walkColumnRef` switches on `len(uids)`, refuses ≥3 | builds `segs []semantic.Identifier` over ALL segments, calls `ResolveIdentifierPath`; the only `UnsupportedExpressionShapeError` left is for **0** segments |
| `ResolveIdentifier` is two-`Identifier`-shaped, admits no path | `ResolveIdentifierPath(segs)` at `expr.go:273`; `ResolveIdentifier` is now a two-element wrapper over it |
| `Scope.ResolveQualifiedColumnNested` does exactly one descent step | `Scope.ResolvePathNested(segs)` at `scope.go:423` walks N, with per-attribute ambiguity and parent-chain fall-through |
| `splitColumnRef` flattens the leading segments | returns `(bare, qualifier, qualified, segs)`; every non-test call site takes `segs` |

**The lesson §0.0 drew is right and was applied to §0.0 itself.** "A claim of the
form *X already works* is a MEASUREMENT, not a reading" cuts both ways: a claim
of the form *X does not work* is a measurement too, and it expires. §0.0's cells
were true when taken and were carried forward across three revisions as though
they were properties of the code rather than of a commit.

## 0.3 WHAT THE WORK ACTUALLY WAS — and what §7.1 got wrong

**Not one line of `pkg/relational/core/query/cascades_translator.go` changed.**
`diff <(git show HEAD:…/cascades_translator.go) …/cascades_translator.go` is
empty. §7.1 ruled that file's `groupByOutputBaker` "the decisive site", "the only
edit the aggregate layer needs", and the mechanism design point 4 was missing.
It needed no edit.

What the fix is, measured by disabling the refusal and watching where the query
died at each step:

1. **The descent arm in the group-key ladder** (`upgradeAggregateOperands`) — the
   one thing §4 got right. A key whose segments descend into a struct resolves
   through `ResolveIdentifierPath`, the same resolver every other clause uses,
   and mints ONE fused multi-accessor `FieldValue`. This is the whole capability:
   with the refusal removed and no descent arm, the corpus query dies
   `ordinal resolution: field "R.V.Z" not resolvable in the runtime row
   (ordinal -1, row columns [ID Q R]) — malformed plan`, §1's defect reproduced
   on the implementation branch as §12 owed. With the arm, it answers `[{330}]`.
2. **`logical.GroupKey` carries `Segs`** — §4's design point 1, and the reason
   point 1 could not be skipped: the ladder had no segments to ask about.
3. **The single-source prefix strip keeps the segments** (§7.2's gate 8, both
   twins). `GroupKey{Display: stripped, Bare: stripped}` made one dotted "bare"
   segment out of `a.r.v.z`; it now peels the alias SEGMENT and re-derives the
   triple from the remainder.
4. **`buildAggregateOutputSlots`' stripped arm matches on `Display`, not `Bare`**
   — a consequence of (3) that only measurement finds: with segments preserved,
   `Bare` is the leaf `Z` and the projected key's output slot went unbound
   (`0AF00: post-aggregate projection contains an invalid native output ordinal`).
5. **`validateGroupByProjection`'s existence check stops asking a top-level field
   set about a struct MEMBER.** It compared the LEAF against the union of
   top-level fields, so `r.v.z` was "column R.V.Z does not exist" (42703). A
   dotted reference is now admitted when either end names a real field — the leaf
   for a source-qualified spelling, the ROOT for a descent, with one leading
   source-alias segment peeled first. This is the same category error, in the
   other direction, that let `GROUP BY n.sk` through on a table declaring an
   unrelated flat `sk`.
6. **`resolveCorrelatedColumnValueStructured` resolves from the segments.** The
   correlated-scalar path has its own group-key resolution and still joined the
   qualifier: `42703: no FROM source aliased as N2.R.V`.

**And the three (b) blockers, each refuted with the arm it actually takes** —
Graefe's condition (c), discharged by instrumentation rather than argument:

- **`groupByOutputBaker`** — the early return **does fire** (`Field "R"`, 3
  accessors) and the decline is **right**. **Rev 8's first justification for that
  was wrong and is corrected here**, because getting it wrong would have repeated
  the exact error this revision is about: it claimed the fall-through was a
  structural no-op ("the branches below return the node unchanged too"). It is
  not. **MEASURED:** disabling the early return turns
  `group_by_output_baker_test.go`'s `multi` case RED — a 2-accessor path with
  `Field "COL1"`, a nil child and `keyOrds{COL1:0}` reaches the name channel and
  is rewritten into a single-accessor read of output slot 0, i.e. a member read
  becomes a read of the struct root's slot. The early return is **load-bearing on
  leaf-collision shapes**, and the class is (a) for that reason and not for the
  absence of an effect. What keeps a nested grouping key from needing a rebind
  here is UPSTREAM and is a **different mechanism**: `rebaseHavingGroupKeyPredicate`
  asks the shared decider `cascades.PredicatePushesBelowGroupBy`, and every
  predicate that will NOT be pushed goes through `rebasePostAggregateGroupKeyValue`,
  which pins the reference to a single-accessor **FrontierPinned** value — so it
  exits at the FrontierPinned guard ABOVE `:1107` and never reaches it. Pushdown
  is the narrow complement, not the rule: `predicateReferencesOnlyKeys` admits a
  SINGLE `ComparisonPredicate` with a key-only comparand, and `buildGroupKeySet`
  disables pushdown outright when two keys share an accessor path, so `AND`,
  `OR`, `NOT` and any aggregate reference stay above and take the pinning route.
  `TestFDB_GroupByNestedPathKey` exercises both routes.
  Its SELECT-list caller is gated on `!exactAggregateLayout` and never runs for a
  grouped nested key at all.
- **`translateSort`** — the block this expression lives in is **never entered**.
  `GROUP BY r.v.z ORDER BY r.v.z` arrives with `AggregateOutputValueExact` set
  and takes the `canonicalizeAggregateOutputValue` arm; the correlated-scalar
  variant arrives with `HasAggregateOutputOrdinal` set and takes the ordinal arm.
  Both hand the site a value already bound to ONE accessor of the aggregate
  output row — which is `FieldPath.ofSingle`, so **gate 7 is satisfied exactly as
  §7.1 predicted it must be, by a rebind that already existed.**
- **`groupedScalarSortKeys`** — `bindPostAggregateValueToNativeOrdinals` rebinds
  the walked nested key to a single-accessor output-row reference *before* the
  arity test, which therefore sees 1 and binds the slot. The arity test is the
  CONSUMER of that rebind, not a second gate in front of it.

**The census guard is updated accordingly** (`accessor_arity_census_test.go`):
(a) 11→13, (b) 3→0, (c) 21→22, (d) 0, (?) 1, total 36 unchanged. **The (b) floor
is INVERTED rather than deleted** — it guarded against a quiet drift to zero;
zero is now the measured steady state, so the alarm direction is GROWTH, and a
(b) now means a site refuses something that demonstrably works.

### 0.4 A LIVE WRONG-ROWS DEFECT, found by pushing on §0.3's own reasoning — NOT this RFC's

Asking what happens when `rebasePostAggregateGroupKeyValue` **cannot** pin a
reference turns up a shape with no backstop, and it is **constructible, live, and
silent**. `rebasePostAggregateGroupKeyValue` only considers a grouping key whose
`Value` is a `*values.FieldValue`:

```go
qfv, ok := gk.Value.(*values.FieldValue)
if !ok { continue }
```

A **COMPUTED** key's Value is an `ArithmeticValue`. So for `GROUP BY c1 + 1` no
post-aggregate reference is ever pinned, and a non-pushable HAVING keeps an
INPUT-relative read that is then evaluated against the aggregate's OUTPUT row.

```
SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200
  keys are 101 and 141 -> correct answer is ZERO rows
  MEASURED: TWO rows [203] [330]

SELECT c1 + 1, max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200 AND max(c2) > 0
  MEASURED: [101 203] [141 330]
```

The second is the sharpest form: the projected key column says 101 and 141 while
the HAVING filtering `c1 + 1 > 200` admitted both — **one result set contradicting
itself**. `c1` carries input ordinal 1 over `[ID C1 C2]`; the output row is
`[(C1 + 1), MAX(C2)]`, whose slot 1 is the AGGREGATE. That predicts admission of
both groups on `> 200` and of neither on `< 200`, and **both were measured, in
opposite directions** — which identifies the slot rather than merely showing that
something is off.

**Three facts fix its scope, and each is measured rather than argued:**

1. **It is PRE-EXISTING.** Re-run at the parent `9222f968c` in a detached
   worktree with the same fixture: **byte-identical results, both arms.** RFC-230
   admits a nested path as a BARE grouping key; the computed-key arm
   (`sq.groupBy[i].expr != nil`) never travelled the refusal this RFC retired,
   and the flat half is on a path this RFC does not touch at all.
2. **The FLAT half is the dangerous one.** The nested twin of the same shape
   fails LOUDLY (`ordinal resolution: field "R" not resolvable in the runtime
   row (ordinal 2, row columns [(R.V.Z + 1) MAX(Q.S)])`) only because the fused
   root's ordinal falls outside a 2-wide output row. That is luck, not a guard.
3. **A BARE key is correct** in the identical non-pushable shape — its `gk.Value`
   IS a `FieldValue`, so the rebase pins it. That control is what makes this a
   computed-key finding rather than "HAVING over a grouped query is broken".

**It is PINNED, not filed** — `groupby_computed_key_having_defect_fdb_test.go`
asserts the defective answers with the correct ones named in every failure
message, plus the bare-key control and the loud nested twin, so the fix flips it
rather than discovering it.

**It is NOT fixed here, and that is a STOP rather than a deferral.** The fix is
to match a post-aggregate reference against a computed key by comparing the
EXPRESSION instead of asserting the key is a `FieldValue` — a change to
post-aggregate binding semantics, on the flat path, entirely independent of this
RFC. Folding an unreviewed wrong-rows fix into a change whose thesis is about the
nested path would put it through the wrong review. It is escalated with the
reproducer in hand.

**The pattern, stated because it is the sixth instance.** Every one of the three
blockers was classified from READING the condition and its comment. Each was
wrong in the same way: the arity test was downstream of a rebind the reader did
not trace, or in a branch the shape does not enter. §7.5 already named the limit
of sweeping by one behaviour; the deeper limit is that **a classification of a
site is a claim about the VALUES that reach it, and only instrumentation
answers that.** §7.7's guard was right to key by symbol and right to demand a
reason — what it could not demand was that the reason be measured.

---

## 0.0 REFUTED (and now itself REFUTED — see §0.2): the resolver this design was going to call cannot walk the path

**Revs 1-3 rested on one sentence, and it is false.** §3.1 said:

> *"The resolver that can answer this already exists… `semantic/scope.go:296-311`… **and it is
> what makes a plain `SELECT r.v.z` work today.** The group-key ladder simply never calls it."*

Probed against real FDB on the corpus schema, verbatim:

```
SELECT r.v.z FROM nested              => 42703: column reference with qualifier "R.V" cannot be resolved
SELECT nn.r.v.z FROM nested AS nn     => 42703: qualifier "NN.R.V" cannot be resolved
SELECT MAX(q.s) FROM nested
  GROUP BY r.v.z HAVING r.v.z > 120   => 42703: qualifier "R.V" cannot be resolved
SELECT r.v FROM nested                => cols=[V] rows=8      (2-segment WORKS)
```

**The root cause is a hard depth cap of one.** `Scope.ResolveQualifiedColumnNested`
(`semantic/scope.go:334`) takes **two** `Identifier`s and performs exactly one descent step:

```go
func (s *Scope) ResolveQualifiedColumnNested(qualifier, col Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	...
			if structCol, ok := src.Table.LookupColumn(qualifier); ok {
				if field, ord, found := structCol.LookupStructField(col); found {
```

One `LookupColumn`, one `LookupStructField`. There is no loop. Upstream, `splitColumnRef`
(`select_parser.go:509-539`, unchanged by CQ-52) flattens the leading segments into the single
dotted string `"R.V"`, which names neither a source alias nor a struct column — so the lookup
cannot succeed for any three-segment path.

**What this breaks in the design, precisely:**

- **The corpus query never reaches the group-key ladder.** It dies in `plan_visitor.go` step 4
  at `resolveColumnRefStructural` (`:892`), which is **three lines above**
  `rejectNestedPathGroupKey` (`:895`) — upstream of both the ladder and the refusal §7 was
  written about. A descent arm added at the ladder (§4 design point 2) would be **dead code
  for the corpus shape**.
- **§8's justification collapses.** Phase 1 as specified in revs 1-3 does not unblock
  `groupby-tests.yamsql`, because the query is refused before any of it runs.
- **§1's `ordinal -1` reproduces only for the TWO-segment shape** (`GROUP BY n.sk`), not for
  the three-segment shape this RFC headlines. Rev 1 presented one measurement as though it
  characterised both, and it does not.
- **What §6 deferred to Phase 2 is Phase 1's prerequisite** — and it is broader than §6 said.
  Rev 1-3 described the gap as the `alias.struct.member` spelling. It is *any* three-segment
  path, **including the alias-free `struct.struct.member` that is the corpus shape itself**.

**The blocker is narrower than "Phase 2", which is why the fix is a re-phasing and not an
abandonment.** The value layer is already depth-N ready: `fuseNestedAccessors`
(`query/expr/expr.go:273-292`) builds an arbitrary-length suffix via `WithSuffix`, and
`Column.StructFields` is recursive by construction — `semantic/catalog.go:97-111`, whose own
comment says *"Recursive by construction — a nested struct field carries its own StructFields
— because a path may descend more than one level."* The machinery was built for depth N and is
being fed a degraded input, plus **one genuinely capped loop**. §6 is re-cut accordingly.

**Recorded as process, because it is the point of the gate:** implementation stopped and sent
this back rather than building a descent arm that could never fire. Nothing was built. The RFC
was wrong in the one place a reader is least likely to check — an enabling claim about
existing behaviour, asserted from reading a resolver's *presence* rather than probing its
*reach*. §12 is amended so the claim class ("X already works today") is never again carried on
inspection alone.

---

## 0.1 RETRACTED: rev 4 deleted a live wrong-rows deliverable on an unprobed status claim

Rev 4 recorded the `bakeSegmentedColumnRef` nested guard (§4.2) as **"ALREADY LANDED
SEPARATELY"** and removed it from the deliverables. **It has not landed.** `[PROBED]`:

```
$ git show origin/master:pkg/relational/core/query/cascades_translator.go     | grep -n "func bakeSegmentedColumnRef" -A 4
6572:func bakeSegmentedColumnRef(fv *values.FieldValue, ref logical.ColumnRef, cols []string, legs []values.RecordTypeLeg) values.Value {
6573-	if fv == nil || !ref.Present || len(cols) == 0 {
6574-		return fv
6575-	}
```

The guard is absent; the struct-root first-match follows immediately. **§4.2 restores it as a
Phase 1 deliverable.**

**How the error happened, recorded because the mechanism matters more than the fact.** Rev 4
did not derive this from the tree. It converted a status report — "the guard is being landed
on its own" — into a statement about `origin/master` without probing, and then *deleted work*
on the strength of it. That is precisely the class §12 was amended to prevent one revision
earlier, applied to a different kind of source. So the rule generalises, and rev 5 states it in
the form that would have caught this:

> **A status claim is a READING until it is probed, whatever its source — a reviewer, a
> coordinator, a commit message, or my own earlier revision.** "X has landed" is checked
> against `origin/master`, never against the sentence that asserted it.

**The hazard is also wider than rev 4 said.** Rev 4 called it "one caller away from wrong
rows", counting the group-key call site. `[PROBED]` — there are **six** call sites, so the
guard protects six:

```
$ git grep -n "bakeSegmentedColumnRef(" origin/master -- '*.go'     | grep -v _test.go | grep -v ":func " | grep -v "//"   # → 6
cascades_translator.go:7004, :7348, :7604, :8100, :8186
clustered_outer_scalar.go:877
```

---

## 0. Read this first: the refusal was right and its stated reason is false

#705 (`8bfd6da0d`) refuses a nested-path GROUP BY key with `0AF00: grouping by the nested
field "R.V.Z" is not supported`. That refusal is **correct** — the shape genuinely does not
work, and #705's own contribution was to make it fail as a *capability gap* instead of as
internal state (before it, two shapes escaped into the executor and died there).

Its stated justification is measurably wrong, and this RFC records that plainly rather than
inheriting it. #705 argued that Java's *naming* of a nested grouping key was the obstacle —
that Go's leaf-naming would misreport a reference Java resolves differently. Java names a
nested grouping key **by its leaf**, exactly as Go does:

```java
// LogicalOperator.java:455
                outerCorrelations).clearQualifier();
// Identifier.java:101-105
    public Identifier withoutQualifier() {
        if (!isQualified()) { return this; }
        return new Identifier(getName(), ImmutableList.of());
    }
```

So `strings.ToUpper(fv.Field)` *is* Java's behaviour, and the premise fails.

**The correction is larger than "Java also names by the leaf."** In Java the grouping key's
name is not load-bearing *at all*. It is the user-visible output label and nothing else:
every mechanism that decides *which* grouping key a reference is addresses it by **Value
structure and ordinal**, never by name (§2.3, §2.4). A refusal argued from naming was
reasoning about a channel Java does not use for this decision. A documented decision resting
on a false premise is exactly what this RFC must not perpetuate, so §7 replaces the
justification even where it keeps the gate.

**What is true, and is the real blocker:** neutering the refusal does not make the query
work. It fails deeper, and the deeper failure is the actual subject of this RFC (§1).

---

## 1. The defect, measured

The query is `groupby-tests.yamsql:61`, from Java's own vendored corpus
(`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/groupby-tests.yamsql`):

```yaml
      - query: select max(q.s) from nested group by r.v.z having r.v.z > 120
      - result: [{330}]
```

against the schema at `:22-29` — `st3(u st2, v st1)`, `table nested(id bigint, q st4, r st3,
primary key(q.s, r.u.w))`, and `create index i2 as select r.v.z from nested order by r.v.z`.

Two properties of this shape drive the whole design:

1. **The grouping key is never projected.** It appears only as a grouping value and as a
   `HAVING` reference. Any design that works by materialising the key as an output column is
   solving a different problem.
2. **`r` is a struct COLUMN, not a table alias.** `r.v.z` is a three-segment descent into one
   column of one source.

**IMPORTANT, added in rev 4 — the measurement below is of the TWO-segment shape** (`GROUP BY
n.sk`), not of the three-segment corpus query at the top of this section. The corpus query
never gets this far: it is refused `42703` several layers upstream (§0.0). Rev 1 presented the
two-segment measurement as characterising both, and it does not. The two-segment result still
stands and still motivates the design; it is simply not the whole defect.

**Two-segment shape refused today** with `0AF00` (§0). **With that refusal neutered,
measured:**

```
ordinal resolution: field "R.V.Z" not resolvable in the runtime row
(ordinal -1, row columns [ID Q R]) — malformed plan
```

That error is raised at `pkg/recordlayer/query/plan/cascades/values/values.go:857`, and its
own doc comment names the mechanism exactly (`:846-848`):

> `Ordinal` is the resolved ordinal, **or -1 for a flat-reference (name→ordinal) miss**.

So the grouping key was minted as a **flat dotted** `FieldValue{Field: "R.V.Z"}` anchored on
the runtime row, and the runtime row has columns `[ID Q R]`. There is no slot named `R.V.Z`
and there never will be — `Z` lives two levels inside the stored message under `R`.

**This was invisible rather than absent.** `groupby-tests.yamsql` executes **zero** queries
today: it is skipped at DDL, booked into the typed ledger as `unsupported-DDL:struct-index`
(one of a pinned 6) because of the nested `AS SELECT` index at `:29`. Line 61 has never run.
§8 is what happens when that gate lifts.

---

## 2. Java is the spec

All citations below were **read in this session against the checkout at tag 4.12.11.0**, not
inherited from a summary. This matters: the first research pass reported
`StreamGrouping.evalGroupingKey` at `:293-296` in a file that is 232 lines long. The quoted
body was verbatim correct and the line number was invented. Every number in this section was
re-derived by hand afterwards. §12 separates what was measured from what is still
second-hand, and names the three claims in the latter class.

### 2.1 The grouping key is ONE `Value`, and it is evaluated against the record

`GroupByExpression.java:95-96`:

```java
    @Nullable
    private final Value groupingValue;
```

A single nullable `Value` — not a list of columns, not a row layout. Several SQL grouping
items are packed into it by the frontend (`LogicalOperator.java:442-443`):

```java
        final var aggregateValue = RecordConstructorValue.ofUnnamed(...aggregates...);
        final var groupingValue  = RecordConstructorValue.ofUnnamed(...groupByExpressions...);
```

The physical rule does not flatten it. `ImplementStreamingAggregationRule.java:133-136`
**rebases and nothing else**:

```java
        final var rebasedGroupingValue = groupByExpression.getGroupingValue() == null ? null
                : groupByExpression.getGroupingValue().rebase(aliasMap);
        return RecordQueryStreamingAggregationPlan.of(newPlanQuantifier, rebasedGroupingValue,
```

And at runtime the whole value is evaluated per row — `StreamGrouping.java:214-217`:

```java
    private Object evalGroupingKey(@Nullable final Object currentObject) {
        final EvaluationContext nestedContext = context.withBinding(Bindings.Internal.CORRELATION, alias, currentObject);
        return Objects.requireNonNull(groupingKeyValue).eval(store, nestedContext);
    }
```

with the group break a plain inequality on the results (`:155-158`).

**Therefore nesting is free in Java.** `FieldValue.eval` walks the entire ordinal path into
the nested message in one step (`FieldValue.java:164-169`):

```java
        final var childResult = childValue.eval(store, context);
        if (!(childResult instanceof Message)) { return null; }
        final var fieldValue = MessageHelpers.getFieldValueForFieldOrdinals((Message)childResult, fieldPath.getFieldOrdinals());
```

There is **no ordinal-into-a-flat-row addressing anywhere on Java's aggregate execution
path.** A nested grouping key is not a special case there; it is the ordinary case with a
longer path.

### 2.2 The grouping key's columns are deliberately unnamed

`GroupByExpression.java:752-761` (`nestedResults`, the default result function):

```java
    public static Value nestedResults(@Nullable final Value groupingValue, @Nonnull final Value aggregateValue) {
        final var aggregateColumn = Column.unnamedOf(aggregateValue);
        ...
            final var groupingColumn = Column.unnamedOf(groupingValue);
            return RecordConstructorValue.ofColumns(ImmutableList.of(groupingColumn, aggregateColumn));
```

Names appear only when a protobuf descriptor must exist, and then they are **positional**:
`Type.java:2648-2651` falls back to `"_" + i`, `Type.java:371-374` is `"_" + fieldSuffix`, and
`Type.java:2922-2924` recognises such names as auto-generated, i.e. as *absent*.

**Implication for the design: a grouping key does not need a name.** Go may keep its
user-facing label (the corpus asserts column labels elsewhere), but no correctness decision
may rest on it.

### 2.3 Identity is the ordinal, and names are not compared

`FieldValue.ResolvedAccessor` (`FieldValue.java:645-696`) carries `final int ordinal`
(`:648`) and compares on it — `equals` at `:676`, `hashCode` at `:688`. The field *name* is
carried for display and is not part of the comparison.

### 2.4 HAVING binds structurally, producing an ordinal path

`QueryVisitor.java:303`:

```java
            where = where.map(predicate -> predicate.pullUp(groupBy.getQuantifier().getRangesOver().get().getResultValue(),
                                                            groupBy.getQuantifier().getAlias(), finalOuterCorrelation));
```

The HAVING predicate is visited in the **pre-GroupBy scope**, so `r.v.z` inside HAVING is
literally the same `FieldValue` the GROUP BY item produced; `pullUp` then rewrites it into a
`FieldValue` over the group-by result. The rewrite composes **by loop index** —
`CompensateRecordConstructorRule.java:88-92`:

```java
                    final var field = column.getField();
                    for (final var argumentValueCompensation : argumentValueCompensations) {
                        resultingMatchedValuesMap.put(argumentValue,
                                new FieldValueCompensation(FieldValue.FieldPath.ofSingle(field, i),
```

So `HAVING r.v.z > 120` becomes an ordinal path into the group-by output. No name is
consulted at any step. (The SELECT list takes the same route at `:301`.)

### 2.5 SETTLED: Java answers a nested grouping key — the four-cell probe

**This section is rewritten in rev 5. Its earlier hedging is retired, and so is the claim it
inherited from the #705 probe.**

That probe originally concluded *"Java does not ANSWER a nested grouping key at this tag
either"*. Revs 1-4 reconciled that with the corpus asserting `[{330}]` by ARGUING that the
decline tracked index availability rather than nesting (Java's Cascades has no physical sort,
so a GROUP BY plans only when an access path supplies the grouping order). **That argument was
right, and it is now demonstrated rather than argued.** The probe's confound — no table
carrying both a flat and a nested-path index — has been controlled by adding one, giving all
four cells against a live Java conformance server at 4.12.11.0:

```
                UNINDEXED                    INDEXED
FLAT     GROUP BY sk   → decline      GROUP BY k    → [[2] [1]]
NESTED   GROUP BY n.sk → decline      GROUP BY n.sk → [[2] [1]]     (Go: 0AF00)
```

**The outcome varies with the INDEX, not with the nesting.** The nested row and the flat row
are identical in both columns. That is the reconciliation, complete: Java's planner declines
an unindexed GROUP BY of either shape, and answers an indexed GROUP BY of either shape.

Three further facts anchor the conclusion **independently of any index**, so it does not rest
on the cell table alone:

- `GROUP BY zzz.sk` (qualifier resolving to nothing) → `42703`. Semantic analysis refuses a
  genuinely unresolvable key, so the nested key's non-42703 outcome is meaningful.
- `SELECT n.sk` → both engines answer. The descent itself resolves in Java.
- the indexed nested cell above → Java returns rows.

**Consequences, stated plainly because revs 1-4 hedged here and the hedging is now unearned:**

1. **Java supports this shape.** Any residual "Java may not support it either" framing is
   struck from this RFC. The conformance principle applies at full strength: this is a shape
   both engines run, and Go refusing it is a **real capability gap**, not an allowed divergence.
2. **`UNSUPPORTED_QUERY` / `0AF00` is the honest report until the gap closes** — §7 gate 4 is
   kept for exactly that reason, and this measurement is what justifies keeping it rather than
   converting it to a permanent refusal.
3. **Go answering some unindexed shapes Java declines remains true and remains allowed**
   (`RecordQueryInMemorySortPlan`, the read-side axis). The four cells make the boundary exact:
   the divergence is in the UNINDEXED column only, and it is orthogonal to nesting.

**Provenance — and rev 5 does not launder this.** The four cells were measured against a live
Java conformance server **by the implementation**, and I did **not** re-run them: the rewritten
probe is not reachable from here. `[PROBED]` at `bd0035383`,
`git show origin/master:conformance/nested_groupby_key_java_probe_test.go | grep -n "INDEXED"`
returns nothing, and the same is true on `fix/three-segment-qualified-path` — the artifact
lives in an uncommitted worktree. So this is booked in §12.2 as **PROBED-ELSEWHERE**: a
measurement with a named reproduction, not one I verified. That distinction is the same one
that would have caught rev 4's deleted deliverable, and applying it asymmetrically — strictly
to claims I dislike, loosely to claims that support the design — would defeat its purpose.

## 3. Go today — the aggregate pipeline is ALREADY Java-shaped; the resolution beneath it is not

This is the section that decides the RFC's size, so each claim is a citation.

**The executor already evaluates the key `Value` against the row** — it is Java's
`evalGroupingKey`, not an ordinal lookup. `pkg/recordlayer/query/executor/streaming_cursors.go:541-557`:

```go
func (c *aggregateCursor) computeGroupKey(row QueryResult) (string, []any, error) {
	...
	for i, k := range c.groupingKeys {
		v, err := k.Evaluate(c.aggregateEvalArg(k, row))
```

**The physical plan already carries `Value`s**, not slots —
`pkg/recordlayer/query/plan/plans/streaming_aggregation.go:22-27` holds `groupingKeys
[]values.Value`. (Go holds a *list* where Java holds one `RecordConstructorValue`; that is a
shape difference with no bearing here, and §11 declines to churn it.)

**The naming authority already renders the resolved path** —
`pkg/recordlayer/query/plan/cascades/expressions/group_by.go:131-138`:

```go
func AggregateKeyColumnName(k values.Value) string {
	if path, nested := values.NestedResolvedPath(k); nested {
		return path
	}
```

This is RFC-229 §2.3, and it was landed **deliberately before** this feature. Its own comment
says so: a leaf-spelled name "silently collapses two grouping columns into one and returns too
few groups. Nested-path GROUP BY does not plan today, which is the only reason that is latent
… the conversion lands FIRST so implementing the feature cannot arm it." §6.1 discharges that
sequencing obligation.

**The slot decision is already structural, already Java's** —
`pkg/relational/core/query/cascades_translator.go`, `groupKeyOrdinalByStructure` (`:1006`),
whose own comment cites the mechanism: *"This is the same join Java performs when it pulls a
reference up over a group-by: CompensateRecordConstructorRule walks the columns and takes the
LOOP INDEX."* Its aggregate arm went **from 1014 name-map firings to 1**. So Go already binds
HAVING/SELECT references to grouping keys the way §2.4 describes.

**That sentence must not be read as "the name channel is gone" — it is not, and §4.3 is the
correction.** Structure decides *which* grouping key a reference is; the name map still
decides *whether* it is one at all, and a reference whose name misses `keyOrds` never reaches
the structural check. Rev 1 quoted the 1014→1 figure without that qualification, which turned
a real precondition into an apparent property of the code.

**The covering-index selector cannot MIS-match a nested path — it declines by construction.**
`AccessorNamePathMatchesNames` is *defined* at `values/accessor_name_path.go:233`, and its
first gate is a length check (`:234-236`):

```go
	pv, ok := AccessorNamePath(v)
	if !ok || len(pv) != len(candidate) {
		return false
	}
```

The group-key caller is the one-line wrapper `aggColumnMatches`
(`aggregate_index_candidate.go:369-371`), which always passes `[]string{col}` — length **1**.
A nested path has length ≥2, so the length check fails, no candidate matches, and planning
falls back to a base-record streaming aggregation. **A wrong match is not merely unlikely, it
is unconstructible**, which is what makes §6's aggregate-index deferral a pure
*performance* question and not a correctness one. Rev 1 parked this as inherited in §12 while
§11 rested a rejected alternative on it; that was backwards, and it is now measured.

### 3.1 The one site that is not Java-shaped

The grouping key is resolved by a ladder in
`pkg/relational/core/embedded/logical_predicate.go:4548-4661`, whose qualified arms open at
`:4563`:

```go
		ref := colRef{table: gk.Qualifier, col: gk.Bare}
```

**A qualified grouping key is assumed to be `table.column`.** **Rev 1 described three arms and
ended the ladder at `:4615`; there are SIX and it ends at `:4661`.** The full enumeration, read
in order, because "the new arm goes LAST" is meaningless without it:

| # | arm | site | what it resolves |
|---|---|---|---|
| 1 | computed expression | `:4551-4559` | `sq.groupBy[i].expr != nil` → `WalkExpressionForProjection` |
| 2 | `ResolveQualifiedProjection` | `:4581` | source alias + column leaf |
| 3 | `resolveQualifiedBaked` | `:4600` | QOV-addressed multi-source leg |
| 4 | single-source `ResolveIdentifier` | `:4612` | qualifier redundant, no join |
| 5 | `ResolveColumnShadowingQualified` | `:4627` | shadowed source alias |
| 6 | bare `ResolveIdentifier` | `:4649-4661` | unqualified leaf |

Arms 2-6 all resolve a *source alias* plus a *column leaf*. There is no arm in which the
qualifier names a **struct column** and the remainder is a path *into* it. For `r.v.z` the
ladder asks for table `R.V`, column `Z`, finds no such source, and falls through to a flat
dotted `FieldValue{Field:"R.V.Z"}` — the value that produces §1's `ordinal -1`.

**Two consequences for §4's placement rule, both of which rev 1 got away with only by
undercounting.** "The descent arm goes LAST" means **after `:4661`**, i.e. after the bare arm,
not after `:4612`. And **arm 1 does not swallow `r.v.z` first**: it is gated on
`sq.groupBy[i].expr != nil`, which the parser sets only for a *computed* key — a bare column
reference (however many segments) takes the `splitColumnRef` path and leaves `expr` nil, so
arm 1 is not entered. An implementation must assert that rather than assume it, because if it
were entered, `WalkExpressionForProjection` would resolve the descent by a different route and
§4.2's invariant would be silently bypassed.

**A correction to a claim made during research, because it would have mis-scoped the fix.** It
was reported that `groupKeyRef` models at most two segments. It does not. `splitColumnRef`
(`select_parser.go:509-539`) keeps every segment:

```go
	bare = parts[len(parts)-1]
	if len(parts) > 1 {
		qualifier = strings.Join(parts[:len(parts)-1], ".")
```

So `r.v.z` **is** representable as `bare="Z"`, `qualifier="R.V"`. What is lost is not the
segments but their **structure**: the qualifier is flattened into one dotted string, so
`R.V` is indistinguishable from a source-qualified reference, and a delimited identifier
containing a dot becomes indistinguishable from two segments. That is a more precise defect
than "two segments maximum", and it is the one the design fixes.

**~~The resolver that can answer this already exists.~~ STRUCK — see §0.0.** Java's
`lookupNestedField` is ported (`semantic/scope.go:334`, `semantic/catalog.go:97-111`, RFC-204
Phase 3), and it resolves a **two**-segment reference — `SELECT r.v` returns 8 rows. It is
capped at exactly one descent step, so `SELECT r.v.z` is refused **42703** today. The
group-key ladder not calling it was never the whole gap; the resolver could not answer the
corpus shape even if it did.

### 3.2 One unresolvable path, three different reports — a live defect in its own right

Measured on the corpus schema, the *same* unresolvable three-segment path is reported three
different ways depending on which clause it appears in:

| clause | outcome |
|---|---|
| `SELECT r.v.z FROM nested` | `42703` — qualifier `"R.V"` cannot be resolved |
| `WHERE r.v.z > 120` | `0AF00` — unsupported |
| `ORDER BY r.v.z` | **`ordinal resolution: field "R.V.Z" not resolvable in the runtime row (ordinal -1, row columns [ID Q R]) — malformed plan`** |

**The ORDER BY row is a live bug of the exact class #705 closed for GROUP BY**: a
three-segment path passes every validation gate and dies in the executor as internal state.
`SELECT id FROM nested ORDER BY r.v.z` reproduces it. That is a capability gap reported as a
malformed plan — the precise failure #705 was written to eliminate, still present one clause
over, because #705 gated the GROUP BY path specifically rather than the resolution the three
clauses share.

This belongs in *this* RFC rather than a separate item because the resolver fix in §6 Phase 0
**collapses all three**: once a three-segment path resolves, SELECT and ORDER BY answer, and
WHERE's `0AF00` narrows to whatever genuinely remains unsupported. Splitting it out would file
a defect whose fix is already specified here, and would leave the ORDER BY arm unpinned in the
meantime. §10 gains an arm for it.

---

## 4. The design

**A GROUP BY key is resolved by the same identifier resolver as every other column reference,
and a reference that descends into a struct mints a nested-descent `FieldValue`. Nothing in
the aggregate planner or executor changes.**

Concretely:

1. **`groupKeyRef` carries structured segments, not a joined qualifier string.** The parse
   already has them (`splitColumnRef`); it currently destroys the structure on the way out.
   The key carries `segments []semantic.Identifier` (per-segment, quote-faithful), and
   `qualifier`/`bare`/`qualified` become derived views for the existing name-only consumers
   rather than the storage. This is the same direction RFC-197/RFC-204 take everywhere else
   and it is the reason the fix is not a string hack.

2. **The descent arm is a CO-EQUAL CANDIDATE counted into one ambiguity check — NOT a
   fallback.** Revs 1-4 said it "goes LAST", tried "only on failure", "so a table alias never
   loses to a same-named struct column". **That is wrong about Java and contradicts a landed Go
   test**, and it is corrected here rather than carried.

   `[PROBED]` Java appends the nested result into the **same** builder as the direct matches —
   `SemanticAnalyzer.java:486` `directMatchesBuilder.add(nestedFieldMaybe.get())`, alongside
   `:465`, `:472`, `:477` — and then asserts there is exactly one:

   ```java
   // SemanticAnalyzer.java:430, :436
   Assert.thatUnchecked(attributes.size() <= 1, ErrorCode.AMBIGUOUS_COLUMN, ...);
   Assert.thatUnchecked(attributes.size() == 1, ErrorCode.AMBIGUOUS_COLUMN, ...);
   ```

   `[PROBED]` Go's landed resolver already does this and pins it:
   `semantic/scope_nested_test.go`, `TestScope_NestedDescent_CollidesWithSourceAliasAsAmbiguity`,
   whose comment states the rule and forecloses the shape revs 1-4 prescribed — *"Resolving the
   collision by which candidate was computed first would make order of attempt into a semantics
   — and a nested descent evaluated only as a FALLBACK after direct resolution failed is exactly
   that, which is why this test exists and not a fallback."* **The RFC was prescribing the shape
   that test forbids.**

   **At depth N this needs a rule revs 1-4 never had to state, because `a.b.c` has more than one
   way to split.** `a.b.c` can be (source `a`, column `b`, field `c`) or (column `a`, field `b`,
   field `c`) — and with a longer path, more. **All prefix splits are candidates in ONE
   population, and the ambiguity check counts that population once.** Two splits that both
   resolve are an `AMBIGUOUS_COLUMN` (42702), not a precedence contest; exactly one resolving is
   the answer; none resolving is the residual gate 4 refuses. Stating it this way is what keeps
   the depth-N extension from silently reintroducing order-of-attempt semantics through the back
   door, which is the failure the landed test exists to catch.

3. **Nothing downstream is touched.** The executor evaluates the value (§3), the name is the
   resolved path (§3), the slot decision is structural (§3). That is not an optimistic
   forecast; it is what §3's citations say, and §10's mutation tests are how it is proven
   rather than asserted.

4. **HAVING follows for free, and this is a prediction that must be falsifiable.** Because
   the GROUP BY item and the HAVING reference resolve through the *same* ladder, they mint the
   *same* nested `FieldValue`, and `groupKeyOrdinalByStructure` matches them structurally
   (§2.4, §3). §10 pins the HAVING arm specifically and §12 books it as OWED — if it does not
   hold, the design is wrong and the RFC must be revised, not patched at the call site.

### 4.2 THE INVARIANT: the descent arm mints a RESOLVED `FieldValue` with `key.Value` set

**This was found twice, independently and from opposite directions — once as a live
wrong-rows hazard reachable in one step, once as an unstated precondition beneath rev 1's
"nothing else changes" — and that convergence is why it is stated as an invariant rather than
left as a property of the code.** Rev 1's claim (§3) is TRUE, but it is true *conditionally*,
and rev 1 never said on what.

**Why a nested key survives the bake at all.** Two pass-throughs, both keyed on the value
being resolved:

```go
// bakeFlatRefsAgainstColumns, cascades_translator.go:6678-6681
		fv, ok := node.(*values.FieldValue)
		if !ok || fv.Child != nil || fv.Resolved != nil {
			return node
		}
```

```go
// the projected-ordinal-gather floor, cascades_translator.go:8106-8107
				if key.Value != nil {
					continue // a resolved GroupByValue, not a bare name read
				}
```

A nested key flows through untouched **because** `fv.Resolved != nil` and `key.Value != nil`.
Those are properties the descent arm must *supply*. If it mints anything else — a lazy value,
a flat dotted `FieldValue`, a key without `Value` — both pass-throughs stop firing and the
flat machinery claims the key. That is the unstated precondition of §4.

**And the machinery that would then claim it is actively wrong.** `bakeSegmentedColumnRef`
(`:6558`) has **no nested guard**:

```go
func bakeSegmentedColumnRef(fv *values.FieldValue, ref logical.ColumnRef, cols []string, legs []values.RecordTypeLeg) values.Value {
	if fv == nil || !ref.Present || len(cols) == 0 {
		return fv
	}
	for i, c := range cols {
		if strings.EqualFold(c, fv.Field) {
			return values.NewFieldValueWithResolvedOrdinalInDomain(
				fv.Field, i, fv.Typ, values.OrdinalDomainOfColumnNames(cols))
```

On a fused nested value `Field="N", Resolved=[N,SK]` it matches `fv.Field` — the **struct
root** — against the flat output columns and returns a **single-accessor** ordinal. That
silently rewrites `n.sk` into a read of the whole struct `N`: wrong rows, no error.

**It is unreachable today only by pointer-identity discipline.** Its sole group-key caller
(`:8081`) is guarded by `fv == minted`:

```go
		if fv, isMinted := groupKeys[i].(*values.FieldValue); isMinted && fv == minted {
			if ref := key.Ref(); ref.Present {
				groupKeys[i] = bakeSegmentedColumnRef(minted, ref, aggInputCols, aggInputLegs)
```

so only a key still holding the untouched carrier reaches it. **§4's design point 1 rewrites
exactly the `key.Ref()` triple that function consumes.** That is one caller away from wrong
rows, and pointer identity is not a guarantee anyone should be asked to preserve by hand.

**Deliverable — RESTORED in rev 5.** Rev 4 recorded this as already landed and deleted it;
that was false and is retracted in §0.1 with the probe. It is ACK'd as correct *independently*
of this RFC, which is a reason to land it early — **not** evidence that it has been landed.
Until a probe of `origin/master` shows the guard present, it is owed. `bakeSegmentedColumnRef`
gains the nested guard,
mirroring its sibling at `:6678-6681`: a `FieldValue` with a non-nil `Resolved` (or a
non-nil `Child`) is returned unchanged, because a resolved nested descent is already baked
and its leading segment is a struct root, never a source-leg qualifier. The guard is correct
independently of this RFC; the RFC is what makes it reachable.

**Both halves get their own unit pin (§10.9, §10.10), and an end-to-end test is NOT
sufficient.** A passing query cannot distinguish "the guard held" from "the mint happened to
be right and the guard was never consulted" — the two failure modes are observationally
identical until the mint changes. So: one pin that a descent-arm key is resolved
(`fv.Resolved != nil`, `key.Value != nil`) asserted directly on the minted value, and one pin
that the guard declines when handed a fused nested value with matching flat columns in scope.

### 4.3 The name channel is NOT gone, and the descent arm's name must land in `keyOrds`

Rev 1's §3 cited `groupKeyOrdinalByStructure`'s "1014 firings to 1" in a way that reads as if
the name channel had been eliminated. **It has not been, and the overstatement hid a second
precondition.**

Exactly **two** maps are keyed by `AggregateKeyColumnName` — `cascades_translator.go:933` and
`cascades_generator.go:5354` — and both are safe for a nested key, because RFC-229 §2.3 made
that function return the resolved path. But two consumers **gate on the name map before
structure is ever consulted**:

```go
// sortKeyAggregateOutputSlot, cascades_translator.go:1041-1048
	whole := normalizeAggOutputName(expressions.AggregateKeyColumnName(v))
	if slot, hit := keyOrds[whole]; hit {
		if structuralSlot, structural := groupKeyOrdinalByStructure(v, groupKeys); structural {
			return structuralSlot, true
		}
		return slot, true
	}
```

and `groupByOutputBaker` has the same shape (`:1185` gates on `keyOrds[key]`, `:1190`
consults structure inside it). **The name decides WHETHER a reference is a grouping key;
structure decides WHICH one.** A reference whose name misses `keyOrds` never reaches the
structural check at all.

So §4's design point 4 (the HAVING prediction) has a concrete requirement behind it: the
descent arm's key must register in `keyOrds` under the name a HAVING/ORDER-BY reference to the
same path renders — i.e. both sides must go through `AggregateKeyColumnName`'s resolved-path
arm. §10.11 pins it as a unit assertion on the map, not only end-to-end.

**CORRECTION (rev 4): `groupByOutputBaker` never reaches that gate at all, and design point 4's
"for free" therefore has a named obstacle rather than a mere precondition.** Its replacer
returns early on a fused nested reference, *before* any name-map lookup
(`cascades_translator.go:1107`):

```go
		if fv.Resolved != nil && len(fv.Resolved.Accessors) > 1 {
			return node
		}
```

The comment above it (`:1098-1106`) states the intent — a multi-accessor baked path is a
nested access, never a bare output-column ref, so it is left alone. That is right for a nested
reference *into a source row*. It is exactly wrong for a nested reference that has become a
**grouping key** and must rebind to the aggregate's OUTPUT ordinal: the early return drops it
before the rebind, so the HAVING reference keeps reading the input-row path against a row the
aggregate never produces.

**This is unmeasured end-to-end, and that is stated rather than hidden**: nothing currently
gets far enough to test it, because §0.0's 42703 fires several layers upstream. So it is a
read-derived obstacle, not an observed failure, and Phase 1 must either rebind nested grouping
keys here or show that some other site does. §10.11 is written to fail on the map; an
additional arm asserting the *post-aggregate* binding of a nested HAVING reference is now
owed (§12), and it is the arm most likely to expose that design point 4 needs real work.

*(A correction carried here so it is not propagated: the second name-map site is sometimes
given as `resultset.go:74`. That file contains no `AggregateKeyColumnName` reference —
`grep -n AggregateKeyColumnName pkg/relational/core/embedded/resultset.go` returns nothing.
The site is `cascades_generator.go:5354`.)*

### 4.4 One hazard the design must handle explicitly

`stripColumnQualifier` (`cascades_translator.go:972-981`) registers a leaf alias for each
grouping key, documented as: *"drops a leading table/alias qualifier ("A.ID" -> "ID").
Applied only to GROUP BY key names, **where the qualifier is a table alias**."* Once a
grouping key can be named `R.V.Z`, that premise is false for it: the function would register
`Z` as an alias for the nested key, and `Z` may be a genuine flat column elsewhere in scope —
re-creating, in the name map, the collision RFC-228 and RFC-229 removed.

**The alias registration is suppressed for a nested resolved path** (the same
`values.NestedResolvedPath` predicate that decides the name), and its doc comment is corrected
to state the narrowed premise. This is not incidental cleanup: it is a live wrong-answer path
that the feature *arms*, and it is the same class as the collapse RFC-229 §2.3 pre-empted.

---

## 5. Where this belongs: stand-alone, and its prerequisite is NOT already met

The brief asked whether this is part of RFC-204, part of CQ-56, or separate. It is
**separate**, and both answers are load-bearing enough to state with their evidence.

### 5.1 It is not RFC-204, and rev 1-3's claim to merely CONSUME it was wrong

RFC-204's phases are DDL/wire (1, done), DML (2, done), query surface (3, in flight — down to
one file), metadata (4, open), nested index expressions (5, open, joint with RFC-202). Nothing
in §4.4 or §6 of RFC-204 mentions grouping keys, `GroupByExpression`, or the aggregate
pipeline; its Phase 3 acceptance line explicitly *predicts* that `groupby` files "move to
their index-gated class", i.e. RFC-204 expects `groupby-tests.yamsql` to stay blocked, not to
be fixed by Phase 3. So the aggregate group key is out of RFC-204's stated scope.

What is RFC-204's is the **resolver** this RFC calls (§3). Revs 1-3 called that "a dependency
on landed work, not a phase boundary". **That was the enabling premise §0.0 refutes**: the
resolver is landed but depth-capped at one step, so this RFC does not consume it — Phase 0
*extends* it.

That strengthens rather than weakens the stand-alone conclusion, and the reason is worth
stating because it is the tempting wrong turn. Phase 0 touches RFC-204's resolver, so folding
this into RFC-204 now looks more attractive than it did at rev 1. It is still wrong: RFC-204's
ACK never covered the aggregate pipeline, its Phase 3 acceptance explicitly predicts this
corpus file stays index-gated, and Phase 0 is scoped by what the *grouping key* needs. Filing
a query-engine change under RFC-204 would route it around the gate that owns it. The honest
statement is that RFC-230 Phase 0 completes a capability RFC-204 Phase 3 left partial, and
says so, rather than either claiming the capability already existed or annexing itself to
another RFC's ACK.

### 5.2 It is not CQ-56, and must not wait for it

CQ-56 (`TODO.md:12633-12665`) is that `NewFieldValueWithPinnedOrdinal` mints
`Domain: unknown`. It is measured from a GROUP BY/HAVING reference, so the adjacency is real
and the temptation to fold this into it is understandable. It is still the wrong call, for a
specific reason: **CQ-56 is about trusting a recorded ordinal to make an identity decision.
This RFC makes no such decision.** The slot is decided structurally, by
`groupKeyOrdinalByStructure` (§3), which compares the two `Value`s rather than two integers —
the very hazard CQ-56 exists to close (a source-relative ordinal meeting an output-row
ordinal and matching because the integers coincided) is not on this path.

CQ-56 remains worth doing and this RFC neither does it nor depends on it. Concretely: nothing
in §4 mints a pinned ordinal.

RFC-225 (nested descent reads by ordinal, revision 13, zero ACKs) is the adjacent work, and it
is **not** a prerequisite either: it converts the nested descent's *evaluator* from a name
read to an ordinal read. This RFC produces a nested descent value; whichever way RFC-225
resolves, it evaluates the same value. Sequencing them in either order is safe.

---

## 6. Phasing

**Re-cut in rev 4.** Revs 1-3 said "one phase, and a second that blocks nothing". That was
wrong in the direction that matters: what they deferred to Phase 2 is what Phase 1 stands on
(§0.0). The corrected shape is **Phase 0 → Phase 1**, with Phase 0 mandatory.

### Phase 0 — DISCHARGED BY #714/#716, struck

**Everything in this subsection describes caps that no longer exist** (§0.2).
Cap 1, cap 2, cap 3 and the carrier flattening all landed outside this RFC;
`git rev-list --count 9222f968c..` on the branch that carried them is not the
measurement any more — the code is. What Phase 0 owed and did NOT cover is
`logical.GroupKey`, which is the one carrier the group-key path uses and which
still had no `Segs` field: that is item 2 of §0.3 and it is Phase 1's.

### Phase 0 (STRUCK — a hard prerequisite) — the SQL path walks N segments

**Re-inventoried by measurement in rev 5.** Rev 4 sized this from reading and got it wrong in
the direction that matters: it named one cap, and **fixing that cap alone changes nothing
observable in SQL.** Every item below is `[PROBED]` with the command that produced it, because
this is the second sizing claim in this RFC to be overturned.

**Cap 1 — the expression walker rejects ≥3 segments before the resolver is ever reached.**
Rev 4 never mentioned it. `[PROBED]`:

```
$ git show origin/master:pkg/relational/core/query/expr/walk.go   # walkColumnRef
	switch len(uids) {
	case 1:  return r.ResolveIdentifier(semantic.Identifier{}, ...)
	case 2:  return r.ResolveIdentifier(..., ...)
	}
	return nil, &UnsupportedExpressionShapeError{
		Shape: fmt.Sprintf("FullId with %d segments", len(uids)),
	}
```

A three-segment `FullId` is refused here, upstream of `Scope`. **This is why rev 4's Phase 0
would have delivered no SQL-visible change** — it fixed a loop nothing could reach.

**Cap 2 — `ResolveIdentifier` itself is two-`Identifier`-shaped.** `[PROBED]`:
`git grep -n "func (r \*Resolver) ResolveIdentifier" origin/master` →
`pkg/relational/core/query/expr/expr.go:236: func (r *Resolver) ResolveIdentifier(qualifier, id semantic.Identifier)`.
The signature admits no path, so cap 1 and cap 2 must move together.

**Cap 3 — `Scope.ResolveQualifiedColumnNested` does exactly one descent step** (§0.0), at
`pkg/relational/core/query/semantic/scope.go:334`. *(Rev 4 wrote this path as
`core/semantic/`; the package is `core/query/semantic/`.)* Its two thin wrappers
(`semantic/analyzer.go:101`, `semantic/scope.go:291`) move with it.

**Carrier — `splitColumnRef` flattens leading segments, and it has SIX non-test call sites,
not four.** `[PROBED]`:

```
$ git grep -n "splitColumnRef(" origin/master -- '*.go' | grep -v _test.go | grep -v ":func "   # → 6
plan_visitor.go:1723, :2164
select_parser.go:899, :909, :1127, :1212
```

plus the structures that carry its output — `groupKeyRef`, `logical.GroupKey`, `colRef`. **This
was §4's design point 1, parked in Phase 1**, where it was useless: structured segments feeding
a two-segment resolver still cannot resolve `r.v.z`. It belongs here.

**Gates — BOTH copies must move (§7).** The step-4 gate sequence exists twice, byte-for-byte:
`plan_visitor.go:892/:895/:945` and `logical_predicate.go:3080/:3106/:3154`. Phase 0 that moves
one and not the other leaves the two dispatch paths disagreeing about which shapes are legal —
the exact asymmetry #705 was written to close, reintroduced one layer up.

**What is NOT owed — depth-N readiness below the resolver, and it holds better than rev 4
claimed.** `[PROBED]` `fuseNestedAccessors` (`query/expr/expr.go:273-292`) and
`FieldValue.descendResolvedPath` are genuine loops over an arbitrary-length accessor list; no
site assumes ≤2 accessors; and a **three**-accessor path is already pinned by a landed test —
`values/proto_descent_test.go`, `TestFieldValue_DescendProtoMessage`, whose fixture is
`Accessors: [{ROOT,0},{REC,0},{SUB,0}]` and asserts `rec.sub == 42`. So the value layer is
genuinely ready; the caps are all in the SQL front half.

**Sizing, stated honestly:** 3 caps, 2 wrappers, 6 carrier call sites, 2 gate copies. That is
larger than rev 4's "1 loop, 2 wrappers, 4 callers" and it is still bounded and specified. It
is not a separate RFC, and §11 says why folding it into RFC-204 is the wrong route.

*(The work is NOT already underway elsewhere, contrary to revs 1-3: `[PROBED]`
`git rev-list --count origin/master..fix/three-segment-qualified-path` → **0**.)*

### Phase 1 — the descent arm

§4 and §4.2-§4.4, unchanged in substance: the group-key ladder gains the struct-descent arm,
placed after the bare arm. With Phase 0 landed this delivers `GROUP BY <struct path>` with
`HAVING` over the same path — `groupby-tests.yamsql:61` — and unblocks the corpus file (§8).
On its own it delivers nothing, which is the whole correction of rev 4.

### Phase 2 (deferred, and this one genuinely does not gate anything)

- **Aggregate-index matching over a nested grouping key** — whether
  `rule_streaming_agg_from_index` selects index `i2` for line 61, or Go falls back to a
  base-record streaming aggregation. **This deferral is provably PERFORMANCE-ONLY, not a
  correctness risk**, and the proof is §3's length gate: `aggColumnMatches` always offers a
  candidate of length 1 (`aggregate_index_candidate.go:369-371`), a nested path has length
  ≥2, so `AccessorNamePathMatchesNames` returns false at `values/accessor_name_path.go:234-236`
  before comparing a single segment. No match is possible, therefore no MIS-match is
  possible; the only outcome is the fallback. Both paths produce `[{330}]`. §10 asserts the
  ROWS in Phase 1 and records the plan shape observed, so Phase 2 starts from a measured
  baseline rather than a guess.

### 6.1 The RFC-229 sequencing obligation, discharged

RFC-229 §4 fixed an order: *"`AggregateKeyColumnName` is converted to the resolved path
before nested-path `GROUP BY` is implemented"*, on the grounds that implementing the feature
first arms a group-collapse whose symptom (missing groups) no existing test would catch. The
conversion landed in #704 (`c5ffbb986`) and is live at `group_by.go:131-138` (§3).

The tripwire RFC-229 installed is
`pkg/relational/sqldriver/groupby_nested_key_collapse_fdb_test.go`, whose header states the
handoff: *"When nested-path GROUP BY is implemented, the naming prerequisite is already met —
replace the gate below with the assertions its failure message names."* §10 does exactly
that, and §4.4 closes the one *new* collision the feature arms that RFC-229 did not
anticipate.

---

## 7. The gate census — SWEPT, not read

**Four revisions enumerated these gates by reading and were wrong four times: two gates, then
three, and the measured number is seven classes across three files.** That is a method failure,
not bad luck, so rev 5 changes the method. The census below is produced by sweep; each row
carries the command that found it, and the commands are collected in §12.2 so a reviewer can
re-run the census rather than re-read the code.

**The sweeps** (all against `origin/master` @ `bd0035383`, `-- '*.go'`, `grep -v _test.go`):

```
git grep -n "resolveColumnRefStructural(\|rejectNestedPathGroupKey("   # → 12 sites, 2 ladders
git grep -n "validateGroupByProjection("                               # → 3 sites
git grep -n "canonicalizeAggregateOutputValue("                        # → 2 sites
git grep -n "UnresolvableOrdinalError"                                 # → ladder arms
```

| # | gate | sites @ `bd0035383` | reports |
|---|---|---|---|
| 1 | duplicate group key | `plan_visitor.go:1472` | `42702` |
| 2 | `walkColumnRef` ≥3 segments | `expr/walk.go:2059` | **no SQLSTATE**, swallowed (below) |
| 3 | `resolveColumnRefStructural` | `plan_visitor.go:892` **and** `logical_predicate.go:3080` | `42703` |
| 4 | `rejectNestedPathGroupKey` | `plan_visitor.go:895` **and** `logical_predicate.go:3106` | `0AF00` |
| 5 | `validateGroupByProjection` | `plan_visitor.go:945`, `logical_predicate.go:3154`, **`logical_predicate.go:10303`** | `42703` |
| 6 | `UnresolvableOrdinalError` in the group-key ladder | `logical_predicate.go:4673, :4707, :4718, :4743, :4791` | loud internal |
| 7 | `canonicalizeAggregateOutputValue` | `cascades_translator.go:6928`, `:7309` | `0AF00` |

**Four facts in that table change the design, and none of them appeared in revs 1-4.**

**(a) There are TWO ladders, byte-for-byte.** Gates 3/4/5 exist in `plan_visitor.go` *and* in
`logical_predicate.go`'s `_postBuild` ladder. Phase 0 that moves one and not the other leaves
the two dispatch paths disagreeing about which shapes are legal — the exact asymmetry #705 was
written to close, one layer up. **Both copies move together**, and §10 gains a pin that they
agree.

**(b) Gate 5 has a THIRD call site that runs without gates 3-4.** `logical_predicate.go:10303`
is the correlated-scalar-subquery path. A group key reaching it is validated for projection
without ever having been validated for resolvability or for nesting — so it is the one arm
where the post-Phase-0 behaviour cannot be predicted from the main ladder's.

**(c) Gate 2 has no SQLSTATE and is SWALLOWED, which is why §3.2 sees three different
symptoms for one cause.** `walk.go:2237-2242` says so in its own words — the projection callers
*"SWALLOW [it] to fall back to the logical-builder text path"* — and the ORDER BY consumer
(`plan_visitor.go:838-845`) matches only `AmbiguousColumnError` and three siblings, so an
`UnsupportedExpressionShapeError` falls through. **That is the mechanism behind §3.2's table**:
one refusal, swallowed differently per clause, surfacing as `42703` in SELECT, `0AF00` in
WHERE, and executor death in ORDER BY. §3.2's "Phase 0 collapses all three" therefore holds
**only because gate 2 is inside Phase 0** (cap 1). Were it not, the claim would be
unsupported for WHERE and ORDER BY, and it is stated here with that dependency explicit rather
than left as an inference.

**(d) Gate 7 falsifies rev 4's scope declaration — see §7.1.**

### 7.1 RULED: gate 7 is CORRECT and PERMANENT — Phase 1 rebinds ABOVE it

**Rev 5 left this as the RFC's largest open question. It is now ruled, and the ruling goes
against the direction rev 5 was leaning.**

`canonicalizeAggregateOutputValue`'s rejection at `cascades_translator.go:7039` —
`len(fv.Resolved.Accessors) != 1` — **stands, and must not be narrowed.** The reason is Java's,
and it is structural rather than defensive: Java's pull-up mints `FieldPath.ofSingle`
(`CompensateRecordConstructorRule.java:88-92`, §2.4), so **the nesting is consumed BELOW the
aggregate**. By the time a value addresses the aggregate's output row, it addresses one flat
slot of that row — a multi-accessor path there is genuinely malformed, exactly as the gate's
own comment argues. Rev 5 recorded the gate's objection ("cannot be made safe by borrowing a
coincident final ordinal") as something Phase 1 would have to answer. **It does not need
answering: it is correct.** The question was posed at the wrong altitude.

**Phase 1 therefore adds a POST-AGGREGATE REBIND, above the gate, not a relaxation of it.**
The site is `groupByOutputBaker`'s early return (`cascades_translator.go:1107`), which rev 5
already identified as the place a fused nested reference is dropped before the `keyOrds` gate:

```go
		if fv.Resolved != nil && len(fv.Resolved.Accessors) > 1 {
			return node
		}
```

That early return is right for a nested reference *into a source row* and wrong for one that
has become a **grouping key**. The rebind: a multi-accessor value that IS a grouping key
(decided structurally, `groupKeyOrdinalByStructure`) is rewritten to the **single-accessor**
output-row reference for its slot — which is precisely `ofSingle`, i.e. Java's pull-up
performed at Go's equivalent site. Everything above then sees a single-accessor value, gate 7
is satisfied without being touched, and §4's design point 4 ("HAVING follows") acquires the
concrete mechanism rev 5 could only name as an obstacle.

**This is the decisive site, and it converges with §4.3.** Rev 5 flagged `:1107` as an obstacle
to design point 4; the ruling makes the same line the solution. The obstacle and the fix are
one edit, at one place, and it is the only edit the aggregate layer needs — which restores a
narrowed form of the scope claim revs 1-4 overstated:

> The aggregate **executor** is unchanged (§3). The aggregate **output canonicalisation** is
> unchanged and correct (gate 7). **One rebind is added above both**, at `groupByOutputBaker`.

**The depth-N one-population / 42702 rule (§4 design point 2) is ACK'd** and stands as written.

### 7.2 GATE 8 — the alias strip, a THIRD ladder, and a false 42702 the feature arms

`[PROBED]` — two byte-for-byte twins, `plan_visitor.go:1411` and `logical_builder.go:593`:

```go
		if stripped != keys[i].Display {
			keys[i] = logical.GroupKey{Display: stripped, Bare: stripped}
		}
```

**`Qualifier` and `Qualified` are dropped on the floor.** `GROUP BY r.v.z` under alias `r`
becomes `Bare="V.Z", Qualified=false` — a single "bare" segment containing a dot, which is the
precise shape RFC-197/CQ-52 exist to eliminate. Silent; no error.

**It arms a FALSE 42702, and Phase 0 is what arms it.** `groupKeysEquivalent`
(`plan_visitor.go:1518`) computes key identity as `(Qualified, Qualifier, Bare)` — *after* the
strip has run — and its own doc states the property the strip destroys: *"a bare key and a
differently-QUALIFIED key of another source are NOT equivalent — Java's value identity keeps
`a.k, b.k` legal."* With the qualifier stripped, `GROUP BY r.v.z, s.v.z` presents as two
identical bare keys and takes a duplicate-key 42702 (gate 1) that Java does not take. Latent
today only because those keys never resolve; **live the moment Phase 0 lands.**

**`logical_builder.go` is a THIRD copy of group-key handling.** §7's census said two ladders.
`[PROBED]` sweep 1 below returns three writers of `logical.GroupKey`:
`logical_builder.go:593`, `plan_visitor.go:1411`, `select_parser.go:73`. Phase 0 moves **all
three**, and §10 pins that they agree.

### 7.3 The census — CLASSIFIED, bounded, and committed as a guard

**Rev 6 published a population of "52 sites" and booked its classification as owed. The
classification is done, it is committed as a guard (PR #715, open), and rev 7 states it here
as the settled replacement for a count that was wrong in six revisions.**

**First: "52 sites" was itself imprecise, and the decomposition is the citable number.**
`[PROBED]` at `c998c411f`:

```
git grep -n "len(.*Accessors)" -- '*.go' | grep -v '_test.go' | wc -l      → 52
                                         | grep -c ':gen/'                 →  8
                                         | grep -cE ':[0-9]+:[[:space:]]*//' →  1
                                                              remainder    → 43
```

The 8 are `PFieldPath.FieldAccessors` marshal/unmarshal loops in generated protobuf — a **wire
field list, not a `values.FieldPath`** — gating nothing. 1 is prose inside a doc comment. The
**43 code lines carry 47 arity expressions** (four lines hold more than one) across **36
enclosing symbols**. `8+1+43 = 52`; `43+4 = 47`. Rev 6's "52 sites" conflated grep lines with
decision sites; the honest unit is 47 expressions in 36 symbols.

**The classification:**

| class | meaning | count |
|---|---|---|
| (a) | correct decline — rejecting a multi-accessor path is right here | 11 |
| (b) | **blocker** — would block a legitimate nested grouping key | **3** |
| (c) | already correct for nesting | 21 |
| (d) | live defect | **0** |
| (?) | uncertain | 1 |

**Gate 7 classifies (a), confirmed independently** on the same grounds §7.1 rules: Java's
pull-up mints `FieldPath.ofSingle`, nesting is consumed below the aggregate. The ruling holds
and is now corroborated rather than merely asserted.

**(d) = 0 is a real result, not an absence of looking** — and the class is not hypothetical.
One (d) was found and **already closed on master**:
`left_outer_existential.go#rebaseOuterLegValueOrdinal`, whose name arm baked the merged address
of the struct and dropped the descent, so `WHERE EXISTS (… t2.k = t1.n.sk)` over a join
compared a BIGINT against a whole struct and silently matched nothing. `[PROBED]` the fix is on
master — the nested-leg arm now carries an explicit `suffix []values.ResolvedAccessor` for
"the suffix a NESTED leg reference still has to travel after its root lands on the merged
row". It reclassifies (c). **Cite it as precedent: this defect class is real, it has shipped
silently, and it looks exactly like the three blockers below until someone classifies it.**

### 7.4 The three blockers ARE Phase 0/1's real scope — ALL THREE REFUTED, see §0.3

**Kept as written so the refutation is checkable against what was claimed.** All
three were classified by reading; all three were measured; none needed changing;
`cascades_translator.go` is byte-identical across the implementation. §0.3 names
the arm each one actually takes. The census entries are reclassified with the
measurement as the reason, and the `(b)` floor is inverted.

Blocker 2's own text — *"an implementation must establish which arm a nested
ORDER BY key actually takes before concluding the rebind covered it. A fix here
that is never exercised is indistinguishable from a fix that works"* — is the
sentence that made the whole section falsifiable, and applying it to all three
rather than to one is what produced §0.3.


The census does not merely bound the risk — it **replaces the guesswork about what Phase 1 must
touch.** Three symbols, keyed `file#symbol`:

1. **`cascades_translator.go#groupByOutputBaker`** — the decisive site, exactly as §7.1 rules.
   Its `len(Accessors) > 1 → return node` is right for a nested read *into a source row* and
   wrong for one that has *become a grouping key*. `[PROBED]` **all three consumers — SELECT,
   HAVING and ORDER BY — are fed by this one baker** via `bakeGroupByOutputRefs`
   (`:1210`, `baker :=` at `:1212`), which is what makes one rebind sufficient for all three.

2. **`cascades_translator.go#translateSort`** (`:6817`) — reached downstream through
   `bakeGroupByOutputRefs`, on the fallback arm. **Nuance that must be recorded rather than
   smoothed:** `sortKeyAggregateOutputSlot`'s whole-value match runs *first* and may bypass this
   path entirely, so an implementation must establish which arm a nested ORDER BY key actually
   takes before concluding the rebind covered it. A fix here that is never exercised is
   indistinguishable from a fix that works.

3. **`logical_predicate.go#groupedScalarSortKeys`** (`:10064`) — the correlated-scalar arm
   §7's census flagged as running *without* the main ladder's gates. A nested key there takes
   `ErrCodeGroupingError` **on legal SQL**. This is the arm whose behaviour could not be
   predicted from the main ladder, now named concretely.

### 7.5 The census's own limit — the predicate, not the population

**This must be stated in §7 itself, or the classified population reads as complete when it is
complete only FOR ONE PREDICATE.**

`analyzer.go:88` — the suspected live wrong-column read of §12.3 — **is not in this population
at all.** `[PROBED]` no `analyzer.go` line matches `len(.*Accessors)`. The reason is structural
and it generalises: **a site that DISCARDS an accessor chain without ever testing its length is
invisible to a sweep for `len(...Accessors)`.**

So the honest claim is bounded twice over:

> The 47 arity expressions in 36 symbols are classified and guarded. That says nothing about
> sites which mishandle nesting **without** performing an arity test — a class that provably
> contains at least one member, and whose member is a silent wrong-column read.

This is the same failure shape as every earlier census in this RFC, one level up: rev 5 swept
by suspected symbol and missed gate 8; rev 6 swept by one behaviour and missed the discard
class. **Sweeping by one behaviour is still sweeping by one behaviour.** Rev 7 does not claim
to have escaped that — it names the predicate, states what the predicate cannot see, and books
the discard class as its own sweep (§12).

### 7.6 The uncertain site, and a naming divergence underneath it

**(?) `cascades_generator.go#deriveColumnsFromProjection`** (`:4171`). Its arity gate is
correct; what is unresolved is the **fall-through** it selects — `innerByName[fv.Field]` — and
for a fused nested reference `Field` is the struct **ROOT**, not the leaf. If the enclosing
`TypeName == "" || TypeName == "UNKNOWN"` precondition is reachable for a nested reference, the
derived column inherits the **struct's** type rather than the member's. Neither constructible
nor provably unreachable today; recorded as **unknown with its reason**, which is the correct
disposition for a site that cannot be decided by reading.

**The fact underneath it is a separate finding and gets its own line, because it is a defect
class rather than a detail.** `[PROBED]` two mints of the *same* fused-nested shape disagree
about what `Field` means:

```go
// composeFieldOverField — values/simplifier_value.go:298  → the LEAF
		// Display = the LAST step's name (Java getLastFieldName); …
		Field:    fused.Last().Field,

// fuseNestedAccessors — query/expr/expr.go:288            → the ROOT
	out := *fv                       // Field copied unchanged from the root ref
	out.Resolved = fv.Resolved.WithSuffix(...)
```

One takes the leaf and cites Java for it; the other keeps the root. **Every consumer that reads
`fv.Field` on a fused value therefore gets a different answer depending on which mint produced
it** — and `deriveColumnsFromProjection`'s `innerByName[fv.Field]` is one such consumer, which
is precisely why its disposition is unknown. This is the same defect class RFC-227/228/229
removed from the *naming* authorities, surviving in the *mint*. It is tracked as its own
finding, not folded here.

### 7.7 The guard, and why it is keyed by symbol

The classification is committed as a census guard (PR #715). Two properties matter to this RFC:

- **Sites are keyed `file#symbol`, never by line.** This RFC's anchors drifted across three
  bases — the group-key ladder alone has been reported at `:4563`, `:4649` and `:4752`, +86 in
  one move. A line-keyed guard would have gone stale inside a single review cycle.
- **Seven mutation directions, each red with a distinct message.** The one that matters most:
  **an extra arity expression added inside an already-classified symbol fires while the raw
  line count is still 52.** That is exactly the case a re-count cannot see, and it is the case
  this RFC kept generating — a population that looks unchanged while its contents move.

---

## 8. The corpus file, and why "un-skipped but failing" is not an option

`groupby-tests.yamsql` runs under `//pkg/relational/conformance/javacorpus:javacorpus_test`
(`corpus_run_test.go:63`), where a failing file **fails the suite** (`:96-98`). The
pre-commit hook runs `just test`. So there is no interim in which the file is executing and
red — that is a broken build, and the repo's rules forbid both leaving it red and re-skipping
it with `t.Skip`.

The dependency chain is:

1. Today the file is skipped **at DDL**, booked `unsupported-DDL:struct-index` (a pinned 6),
   because of the nested `AS SELECT` index at `:29`. **Line 61 has never executed.** This is
   why the defect was invisible rather than absent.
2. A three-segment-qualified-path fix lifts that DDL gate. The file then executes, and line 61
   fails — on `42703` from `plan_visitor.go:892` (§7), not on the `0AF00` revs 1-3 predicted.
   **That is what blocks that fix from merging.**

   **Correction (rev 4): that fix is NOT already on a branch.** Revs 1-3 said
   `fix/three-segment-qualified-path` "is already on it". It is at `37ebc2dea` —
   **0 commits ahead of `origin/master`** (`git rev-list --count origin/master..` returns 0).
   Whatever exists is staged-but-uncommitted in a worktree and must not be planned around as
   though it were landed.
3. **Phase 0 + Phase 1 make line 61 answer `[{330}]`**, so the file goes green and the DDL fix
   merges.

   **The scope of that unblock is now MEASURED, not estimated.** On the branch carrying the
   three-segment fix, CQ-72 and the aggregate-typing fix, the Java corpus stands at:

   ```
   pass=69  fail=1  skip=168  queries=1775
   ```

   and **the sole residual failure is `groupby-tests.yamsql:61`** — this RFC's target. So the
   claim "this RFC unblocks the corpus" is exact rather than directional: it is one failing
   file, one failing query, and it is the query §0.0 measured. It also bounds the risk in the
   other direction — nothing else in 1775 queries is waiting on this work, so Phase 0's blast
   radius is not hiding behind an aggregate count. *(PROBED-ELSEWHERE — measured on that
   branch by the implementation, not re-run here; §12.2.)*

   **GATE 9 — and it costs this section its parity claim.** §2.5 established that Java answers
   line 61 *because* index `i2` on `r.v.z` supplies the grouping order. **Go will not use that
   index.** `[PROBED]` `aggColumnMatches` (`aggregate_index_candidate.go:369`) matches a value's
   full accessor path against a **single-name** candidate, so a nested path can never match —
   the same length gate §6 Phase 2 cites, seen from its consequence rather than its mechanism.
   Go answers line 61 via `InMemorySort` over a base-record streaming aggregation.

   **So the rows match and the mechanism does not, and this RFC must say so rather than let
   "the corpus goes green" read as parity.** It is the sanctioned read-side fallback
   (`RecordQueryInMemorySortPlan`), not an index match — the same divergence §2.5's four cells
   bound to the UNINDEXED column, appearing here because Go's nested key is *effectively*
   unindexed however many indexes exist. Correct rows, different plan. Making Go use `i2` is
   §6 Phase 2 and remains provably performance-only. Revs 1-3 attributed this to Phase 1 alone; without Phase 0 the query is refused
   before any group-key code runs (§0.0), so Phase 1 alone leaves the file red.

**Phase 1 is therefore the unblock, and it is an honest one**: the file passes because the
query works, not because it was re-booked. That is the specified interim, and there is no
other.

**If the two land out of order** — the DDL fix first — the mechanism to use is the existing
typed ledger, not a skip: `gaps.go` books a file against its **exact rejection substring**
(`gaps.go:249` shape), so an unrelated new failure in the same file stays a hard failure
(`gaps.go:160-162`). A temporary booking pinned to the exact `0AF00` text is a *measured,
narrowly-pinned known gap*, which is what the ledger is for. It is not the plan, it is the
ordering hazard's contingency, and it is stated so nobody reaches for `t.Skip`.

**Ledger arithmetic is owed, not asserted.** `pinnedFileTotal = 238`, the census line and the
`pinnedAssignmentDigest` (`pinned_ledger_test.go:102,:137,:142`) all move when a file changes
class. §12 books the recomputation as owed; this RFC does not predict the numbers.

---

## 9. Blast radius, and the wire

**No wire bytes. Explicitly, and by mechanism rather than by assurance.** This is a read-path
resolution change. Key encoding, index entry format, record and split-record format, and the
record-store header are untouched — nothing in §4 writes anything.

**Continuations are clean, and the reason is already recorded.** RFC-229 §6 established that
the aggregate continuation serialises the *evaluated key tuple* and index-parallel accumulator
state, with **no column names**. A grouping key whose value is produced by a longer accessor
path serialises the same evaluated scalar it would have serialised as a flat column. Java
compatibility was never at stake here, and this RFC does not put it there. §12 books the
executed confirmation as owed rather than resting on the inherited reading.

**EXPLAIN moves**, because a nested grouping key renders its resolved path (`R.V.Z`) where
nothing rendered before — these are new plans, not re-rendered old ones. Plan-shape goldens
gain records; §10 requires each to be read individually.

**The memo.** A nested `FieldValue` compares by its accessor path, so two grouping keys under
one struct root are distinct — which is the collapse RFC-229 §2.3 pre-empted, now actually
exercised (§10).

**`rule_push_requested_ordering_through_groupby` is SAFE, checked because RFC-229 §3 named it
as a last-wins hazard untouched by that RFC.** Its map is keyed by `AccessorNamePathKey`
(`:182`, written at `:187`, read at `:198`) — the **full accessor path**, not
`AggregateKeyColumnName`'s rendering. So `GROUP BY n.sk, n.co` yields two distinct keys and
the last-wins never fires; and a key with no accessor path returns `nil` (`:184-185`), i.e.
the rule declines to push rather than mismatching. It is reported here as checked-and-clear
rather than left as an open question, because "probably fine" about a rule whose failure mode
is *losing an index scan to an in-memory sort* is not a finding anyone can act on later.

---

## 10. Tests

The obligation is a *dimensional* one: what this feature can break is a group key that
silently collapses or silently reads NULL, and both ship green under a suite that only checks
that a query returns rows.

1. **A STANDALONE FDB test of the exact shape — this is Phase 1's acceptance criterion, and
   the corpus arm is NOT.** Rev 1 made `groupby-tests.yamsql:61` the headline test; that test
   **cannot run at Phase 1 merge time**, because the file stays DDL-gated until the
   three-segment branch lands (§8). An acceptance criterion that cannot be executed is
   unmeasured, and would have been reported as satisfied by a suite that never ran it — the
   empty-set green this repo has been bitten by six ways. So Phase 1 owns a standalone
   `sqldriver` FDB test driving the exact shape: an **unprojected** nested grouping key with a
   **HAVING over that same key**, asserting rows. It is the reproducer, not a simpler cousin:
   a design that materialises the key as an output column passes any test that projects it.
2. **The corpus arm, when it becomes runnable**: `groupby-tests.yamsql:61` returning
   `[{330}]` in the real corpus target, flipping the file's ledger entry (§8). This confirms
   the unblock; test 1 is what proves the capability.
3. **The RFC-229 tripwire flipped** (`groupby_nested_key_collapse_fdb_test.go`): from
   asserting the refusal to asserting **4 groups** for `GROUP BY n.sk, n.co` and 2 for each
   single-key query — the two-nested-keys-under-one-root collapse, which is the failure the
   naming conversion was landed early to prevent. This is the arm no existing test would
   otherwise catch.
4. **The §4.4 alias hazard, driven directly**: a nested grouping key `R.V.Z` in scope
   *together with* a flat column `Z`, asserting the flat column is not captured by the nested
   key's slot. Written as a unit pin over the name map, not only as an end-to-end query, so it
   fails for the right reason.
5. **The `HAVING` prediction, falsifiable** (§4 point 4): an assertion that the HAVING reference and
   the GROUP BY item resolve to the *same* value and bind structurally — so if point 4's "for
   free" is wrong, this reds rather than being papered at the call site.
6. **The residual refusal — DEFERRED to after Phase 0, deliberately** (§7). Revs 1-3 specified
   this as asserting `0AF00`; measured, that goes **red** (gate 1 fires first with `42703`, and
   `rejectNestedPathGroupKey` would decline anyway). Asserting `42703` instead would go green
   while pinning the defect Phase 0 removes. The residual class is not knowable until Phase 0
   lands, so this test is written then, and it asserts the code **and the gate that produced
   it**. Recorded rather than dropped so its absence is a decision, not an omission.
7. **Per-half mutation, the house standard**: reverting the descent arm alone, and the §4.4
   alias suppression alone, must each go red on something **named**. A two-part change does
   not ship with one part's justification unmeasured (RFC-227 §5 records what happens
   otherwise: a half that reverted fully green).
8. **The §3.2 three-layer inconsistency, one arm each.** `SELECT r.v.z`, `ORDER BY r.v.z` and
   `WHERE r.v.z > 120` over the corpus schema, asserted to give the SAME class of answer after
   Phase 0 — rows, not three different refusals. The `ORDER BY` arm is the important one: it
   is a **live executor-death bug today** (`ordinal -1`, the class #705 closed for GROUP BY)
   and nothing pins it. Until Phase 0 lands it is pinned as a characterization test naming the
   defect, so the fix flips it rather than discovering it.
9. **§4.2's invariant, asserted on the MINT** — the value the descent arm produces has
   `fv.Resolved != nil` and its `GroupKey.Value != nil`. A unit pin on the minted value, not
   an end-to-end query, because the pass-throughs at `cascades_translator.go:6678-6681` and
   `:8106-8107` are conditional on exactly those two facts and nothing else observes them.
10. **§4.2's guard, asserted on the DECLINE** — `bakeSegmentedColumnRef` handed a fused nested
   `FieldValue` (`Field:"N"`, `Resolved:[N,SK]`) with flat columns in scope that include `N`
   returns it **unchanged**, rather than the single-accessor ordinal for the struct root it
   returns today. **Tests 9 and 10 are both required and neither substitutes for the other**: a
   passing end-to-end query cannot distinguish "the guard held" from "the mint happened to be
   right and the guard was never reached", and those diverge the moment either changes.
11. **§4.3's name-map precondition** — the descent arm's key is present in `keyOrds` under the
    name a HAVING/ORDER-BY reference to the same path renders. Asserted against the map, since
    `sortKeyAggregateOutputSlot:1043` and `groupByOutputBaker:1185` gate on the map *before*
    structure is consulted; a miss there fails silently rather than loudly.
12. **The existing leaf-collision reproducer is KEPT and EXTENDED, not replaced** —
    `groupby_nested_key_collapse_fdb_test.go:198-245` ("a flat column sharing the leaf name
    defeats the refusal") is already the §4.4 hazard's reproducer, with its own control over
    the table lacking the flat column. Deleting it while flipping the gate above it would
    remove the only existing witness to that shape; it gains the post-fix assertions instead.
13. **Unfiltered targets** — `javacorpus`, `sqldriver`, `yamsql`, `explaindiff`, `docscheck` —
   with `--nocache_test_results`, and the `=== RUN` count read rather than the summary line. A
   narrowed filter withholds the census floors, and a cached green ran nothing.

---

## 11. Rejected alternatives

- **Keep the refusal permanently; declare nested grouping keys unsupported.** Loses on three
  counts: **Java ANSWERS this shape** — measured across all four index/nesting cells (§2.5), not
  merely "accepted by semantic analysis" as revs 1-4 could only claim — Java's own corpus
  asserts a row for it, and Go already has every downstream piece (§3) — the refusal
  would be permanent scaffolding around a one-arm gap. It also fails the conformance
  principle in the direction that matters: this is a shape both engines run.

- **Project the nested key into a synthetic flat column below the aggregate, then group by
  that slot.** This is the flat-slot assumption made permanent. It invents a mechanism Java
  does not have (Java evaluates the value, §2.1), it widens every aggregate row, and it needs
  a synthetic *name* for the new column — reintroducing exactly the name-derivation defect
  class RFC-227, RFC-228 and RFC-229 spent three RFCs removing. It would also break the
  covering-index match, which matches grouping keys on the accessor path (§3) and would see a
  synthetic column instead of `R.V.Z`. Smallest-looking diff, worst architecture.

- **Teach the runtime row to expose nested slots** (make `ordinal -1` resolvable). Fixes the
  symptom at the loudest site and leaves the cause. The executor is already correct — it
  evaluates a `Value` (§3) — so this would change a correct contract for one feature, and the
  malformed value would still be malformed everywhere else it flows.

- **Wait for CQ-56 / the ordinal domain.** §5.2: this design makes no identity decision on a
  recorded ordinal, so the domain is not a prerequisite. Coupling would block a working
  capability on an unrelated missing fact — the "needs a capability that doesn't exist yet"
  deferral, which here is not even true.

- **Fold this into RFC-204 as Phase 3b.** §5.1: RFC-204's ACK never covered the aggregate
  pipeline, and its Phase 3 acceptance explicitly predicts this file stays gated. Filing a
  query-engine change under it would route a Cascades change around the review gate that
  owns it.

- **Represent the grouping key as one `RecordConstructorValue` to match Java's single-`Value`
  shape (§2.1).** Correct in spirit and rejected on scope: Go's `[]values.Value` is
  isomorphic for every purpose this RFC touches, and converting it would churn the executor,
  the continuation layout and every plan-shape golden for zero behavioural gain. Recorded so
  the divergence is a known choice rather than an oversight.

---

## 12. Evidence — MEASURED, INFERRED, and OWED

**Measured (in this session, by hand, against the checkout and the tree):**

- Every Java citation in §2, re-derived after a research pass produced a correct quotation at
  an invented line number (`StreamGrouping.evalGroupingKey` cited at `:293-296` in a 232-line
  file; it is at `:214-217`). Re-verified: `GroupByExpression.java:95-96, :752-761`;
  `LogicalOperator.java:442-443, :455`; `Identifier.java:101-105`; `FieldValue.java:164-169`,
  `:645-696`; `StreamGrouping.java:155-158, :214-217`;
  `ImplementStreamingAggregationRule.java:133-136`;
  `CompensateRecordConstructorRule.java:88-92`; `QueryVisitor.java:301, :303`;
  `Type.java:371-374, :2648-2651, :2922-2924`.
- §3's Go citations, each read: `streaming_cursors.go:541-557`; `streaming_aggregation.go:22-27`;
  `group_by.go:131-138`; `cascades_translator.go:925-1006`; `values/values.go:820-860`;
  `logical_predicate.go:4548-4661`; `select_parser.go:509-539`.
- **The rev-2 sites, each read before folding** (every finding was verified against the tree,
  not accepted on assertion): `bakeSegmentedColumnRef` at `:6558` with its guard at `:6559-6561`
  and the struct-root match at `:6566-6571`; the sibling pass-through at `:6678-6681`; the
  pointer-identity caller at `:8081`; the floor at `:8106-8107`; the six ladder arms
  (`:4551-4559`, `:4581`, `:4600`, `:4612`, `:4627`, `:4649-4661`);
  `sortKeyAggregateOutputSlot:1041-1051`; `groupByOutputBaker:1185, :1190`;
  `values/accessor_name_path.go:233-243` and its wrapper `aggregate_index_candidate.go:369-371`;
  `rule_push_requested_ordering_through_groupby.go:180-200`.
- **The aggregate-index deferral is performance-only** (§6, §11) — moved here from
  "inherited". `aggColumnMatches` offers a length-1 candidate and the length gate at
  `accessor_name_path.go:234-236` rejects any path of length ≥2, so no match and therefore no
  mis-match is constructible.
- **Three citation slips REFUTED and corrected rather than propagated.** (a) The second
  `AggregateKeyColumnName`-keyed map is NOT `resultset.go:74`; it is
  `cascades_generator.go:5354` (§4.3). Measured with a sweep shown to be well-formed first —
  `grep -rn "AggregateKeyColumnName" --include='*.go' pkg/` returns 60 lines (the non-empty
  control), of which **19 are non-test**, spread over six files
  (`plans/ordering.go`, `expressions/group_by.go`, `executor/executor.go`,
  `cascades_generator.go`, `logical_predicate.go`, `cascades_translator.go`); **no
  `resultset.go` under `pkg/` appears among them.** The wrong citation traced to a zero-hit
  sweep read as absence — see §12.1.
  (b) `ResolveQualifiedProjection` is at `:4581`, not `:4582` (§3.1). (c) Rev 1 cited the
  wrapper `aggregate_index_candidate.go:369-371` as the definition of
  `AccessorNamePathMatchesNames`, which is at `values/accessor_name_path.go:233` (§3).
- The 42703 site (§7): `logical_predicate.go:5210-5216` — a leaf-existence check returning
  `ErrCodeUndefinedColumn`, qualifier-blind, confirming it was never about nesting.
- The corpus gate (§8): `corpus_run_test.go:98-99` — `if res.Status == javacorpus.StatusFail
  { t.Errorf(...) }`, i.e. a failing file fails the suite.
- The `splitColumnRef` correction (§3.1) — the "two segments maximum" claim is false, read
  from the function.
- The corpus query, its expected row, and the schema's `i2` index, read from
  `groupby-tests.yamsql:22-29, :61-62`.
- The probe's header and its two controls, read from `git show 8bfd6da0d:conformance/…`.
- RFC-230 is a free number (`gh pr list --state open` → #709, #579, #486).

**REFUTED in rev 4 — claims revs 1-3 asserted that measurement overturned:**

- **"`SELECT r.v.z` works today"** (§3.1). False; 42703. The resolver is depth-capped at one
  step (`semantic/scope.go:334`). This was the RFC's enabling premise and it was never probed.
- **"Phase 1 unblocks the corpus file"** (§8). False without Phase 0; the query is refused
  upstream of every line Phase 1 touches.
- **"`fix/three-segment-qualified-path` is already on it"** (§6). False; 0 commits ahead of
  `origin/master`.
- **"The residual reports `0AF00`"** (§7). False; gate 1 at `plan_visitor.go:892` fires first
  with 42703, and gate 2 would decline anyway.
- **"§4.3's `keyOrds` gate is where a nested HAVING reference lands"** (§4.3). It never reaches
  it — `cascades_translator.go:1107` returns early on a multi-accessor path.
- **Line numbers throughout revs 1-3**, written against a stale base five commits behind.

**Inherited and NOT independently verified — flagged, not laundered:**

- The gate ORDER in §7 is now MEASURED (`:892` → `:895` → `:5217`, all on `37ebc2dea`), so
  the rev-3 entry booking it as unverifiable is retired.
- The javacorpus ledger's skip-class mechanics and the `unsupported-DDL:struct-index = 6`
  count — §8. (That a failing file fails the suite is verified above; the class assignment
  and the count are not.)
- RFC-204's phase-by-phase state — §5.1.

Each is a claim an implementation must re-read before relying on it. The `StreamGrouping`
miscitation is the reason this list exists as a list rather than as a footnote.

**The rule rev 4 adds, because the miscitation discipline was not enough.** Every citation in
revs 1-3 was hand-verified, and the RFC was still wrong in the way that mattered — because the
false claim was not a line number but a **behavioural** one: *"`SELECT r.v.z` works today."*
Reading a resolver's presence establishes that it exists, never that it reaches. So:

> **A claim of the form "X already works" is a MEASUREMENT, not a reading.** It must be probed
> — executed against a real store on the real schema — before any design is allowed to rest on
> it. An enabling premise is exactly where this is least likely to be checked, because it is
> the part that makes the rest sound reasonable.

That is the rule this RFC violated, at the one point where violating it cost an implementation
dispatch.

**Java line ranges independently spot-checked.** All 14 ranges in §2 were re-checked a second
time against the checkout, by a different reader, and found line-exact — including
`FieldValue.java:676`. The hand-verification prompted by the fabricated `StreamGrouping`
citation held up, which is why §2 asserts them without hedging.

### 12.4 The owed list, discharged — MEASURED at `9222f968c`

- **§1's `ordinal -1` reproduced on the implementation branch and shown to
  disappear.** Reproduced by disabling the refusal:
  `ordinal resolution: field "R.V.Z" not resolvable in the runtime row
  (ordinal -1, row columns [ID Q R]) — malformed plan`. Gone with the descent arm.
- **Design point 4 (HAVING "for free") — CONFIRMED, and for a different reason
  than predicted.** §4.3 and §7.1 said it needed a post-aggregate rebind at
  `groupByOutputBaker`. It does not: the HAVING predicate over a nested grouping
  key is pushed BELOW the aggregate, where the input-relative nested read is
  correct. The prediction held; the named obstacle was not one.
- **Per-half mutation, each arm red on something named** — §12.5.
- **Phase 0's acceptance** — `SELECT r.v.z`, `ORDER BY r.v.z`, `WHERE r.v.z > 120`
  and the corpus GROUP BY all answer; §3.2's three-way report inconsistency was
  already collapsed by #714 before this RFC's implementation began.
- **The corpus, re-measured.** Baseline `pass=69 fail=0 skip=169 queries=1775` →
  **`pass=70 fail=0 skip=168 queries=1952`**. `engine-gap:nested-path-group-key`
  is DELETED, not emptied — a declared class nothing produces reads as a covered
  case, and `TestSkipClassesAreAllReachable` says so. The assignment digest moved
  on **exactly one line**, diffed against the parent (captured in a detached
  worktree with the digest forced to mismatch so the dump would emit), never
  re-blessed on the hash:
  `-groupby-tests.yamsql skip engine-gap:nested-path-group-key` /
  `+groupby-tests.yamsql pass -`. `queries` +177 is NOT the booking's removal —
  a gap entry cannot move `queries` — it is the file no longer ABORTING at line
  61 after 33 of 44 queries and taking every later block with it.
- **Test 6 (the residual refusal) is DROPPED, and that is a result.** There is no
  residual: the descent arm's population is exactly the set the refusal turned
  away, and everything outside it is unchanged. What replaces it is the control
  that the refusal's *neighbours* still own their errors —
  `GROUP BY zzz.sk`, `GROUP BY zzz.n.sk` and `GROUP BY a.n.zzz` are still 42703,
  pinned in `groupby_nested_path_shapes_fdb_test.go`.
- **The plan shape, asserted rather than recorded.** Gate 9 holds:
  `Project([MAX(Q.S)#1], StreamingAgg(keys=[R#2.V#1.Z#1], InMemorySort([R#2.V#1.Z#1 ASC], Scan(NESTED))))`
  — the read-side fallback, not an `i2` match. Exactly ONE sort, and it is BELOW
  the aggregation, so the grouping order is consumed rather than re-established;
  the ordering-match hazard the RFC-229 tripwire named (a provided key rendered
  as a flat dotted string vs a requested fused path) does not manifest as a
  redundant sort. Pinned so a later aggregate-index match is a visible change.

**Still owed, and NOT closed by this RFC** — carried forward unchanged:
the discard-class sweep (§7.5), the `Field` root-vs-leaf mint divergence (§7.6),
`deriveColumnsFromProjection`'s reachability (§7.6), the two-segment
wrong-column read (§12.3), the executed continuation confirmation (§9), and
Phase 2's aggregate-index match over a nested grouping key (provably
performance-only, §6).

### 12.5 Mutation, both directions per change

Each arm reverted alone, scoped to the files it touches, and the named test run:

| reverted arm | test | verbatim failure |
|---|---|---|
| the descent arm in `upgradeAggregateOperands` | `TestFDB_GroupByNestedPathKey/unprojected_key_with_having` | see §12.5 log |
| `logical.GroupKey.Segs` + the prefix strip keeping segments | `TestFDB_GroupByNestedPathKey/alias_qualified_key` | see §12.5 log |
| `validateGroupByProjection`'s root-or-leaf existence test | `TestFDB_GroupByNestedPathKey/projected_key` | see §12.5 log |
| `resolveCorrelatedColumnValueStructured`'s segment resolution | `TestFDB_GroupByNestedPathKey/correlated_scalar_subquery_grouped_by_the_nested_key` | see §12.5 log |
| the `groupby-tests.yamsql` gap entry restored | `//pkg/relational/conformance/javacorpus` | ledger drift, `pass=70` vs `pass=69` |

**Owed before this RFC can be called implemented:**

- §1's `ordinal -1` failure reproduced **on the implementation branch** rather than inherited
  from the brief, and shown to disappear.
- §4's design point 4 (the HAVING prediction) confirmed, or the design revised.
- §10's per-half mutation results, each red arm named — the descent arm and the §4.4 alias
  suppression. (The §4.2 guard is no longer among them: it landed separately.)
- **Phase 0's own acceptance**, which revs 1-3 had no concept of: `SELECT r.v.z`,
  `ORDER BY r.v.z` and the corpus GROUP BY all answering, and §3.2's three-way report
  inconsistency collapsed to one behaviour.
- **The post-aggregate rebinding of a nested HAVING reference** (§4.3's rev-4 correction) —
  whether Phase 1 must rebind at `cascades_translator.go:1107` or some other site already
  does. This is read-derived and untested, and it is the likeliest place design point 4 turns
  out to need real work.
- Test 6 written only after Phase 0, asserting the code AND the gate that produced it (§7).
- The continuation confirmation executed (§9), not read.
- The javacorpus ledger numbers recomputed (§8) — `pinnedFileTotal`, the census line, the
  assignment digest.
- ~~The plan shape line 61 gets~~ — **ANSWERED in rev 6**: `InMemorySort` over a base-record
  streaming aggregation, because `aggColumnMatches` cannot match a nested path (gate 9, §8).
- ~~Classification of sweep 2's arity sites~~ — **DONE and guarded** (§7.3, PR #715). 47
  expressions / 36 symbols: 11 (a), **3 (b)**, 21 (c), **0 (d)**, 1 (?). The three blockers are
  now Phase 0/1's stated scope (§7.4) rather than an unknown.
- **A sweep for the DISCARD class** (§7.5) — sites that mishandle nesting *without* an arity
  test, which `len(...Accessors)` provably cannot see and which contains at least
  `analyzer.go:88`. This is the successor open item, and it is named with its predicate so the
  next census does not repeat the pattern of claiming completeness.
- **The `Field` root-vs-leaf mint divergence** (§7.6) — `composeFieldOverField` takes the leaf,
  `fuseNestedAccessors` keeps the root; tracked as its own finding.
- **`deriveColumnsFromProjection`'s reachability** (§7.6) — whether the
  `TypeName == "" || "UNKNOWN"` precondition is reachable for a nested reference. Recorded as
  unknown rather than assumed either way.
- **The second mint writeback at `logical_predicate.go:9999`** — whether the descent arm must
  exist there too, or is provably unreachable. Note it sits in the same file as blocker 3
  (`groupedScalarSortKeys`, `:10064`), so the correlated-scalar arm likely needs both together.
- **The two-segment wrong-column read** (§12.3) — dispatched separately; this RFC waits for the
  measurement rather than designing against a description.
- **The post-aggregate rebind at `groupByOutputBaker:1107`** (§7.1) — specified in rev 6, not
  yet implemented or measured.
- Phase 1's standalone FDB test (§10 test 1) executing and green, since the corpus arm cannot
  run until the DDL gate lifts — the acceptance criterion is the standalone test, and a green
  reported without it would be a green from an empty set.

### 12.1 On attribution — why no finding in this document names its finder

Every correction, refutation and hazard above is stated as a fact about the tree, with the
command or the file:line that establishes it, and **none of them names who raised it.** That
is deliberate and it follows the repo's standing rule against attributing findings to
reviewers or review cycles: the durable content of "`resultset.go:74` contains no
`AggregateKeyColumnName` reference; the site is `cascades_generator.go:5354`" is true
regardless of who noticed it, and the attribution is the only part that rots. A reader two
years from now needs the `grep` and the corrected site; "X's citation was wrong" tells them
nothing they can check and quietly converts a technical record into a process one.

**The one case where the mechanism outlives the correction.** One of §12's three slips came
from a sweep that returned nothing and was read as proof of absence — under fish,
`grep --include=*.go` with the glob **unquoted** fails to expand and matches zero files, so
the command reports no hits for a symbol that is present 60 times. The corrected line number
is worth one line; the rule behind it is worth keeping:

> **A zero-hit sweep used as evidence of absence must first be shown to be a well-formed
> command.** Run it against a term you know is present, and read the control's count before
> interpreting the zero.

That is the same failure this repo has catalogued in five other costumes — a `--test.run`
pattern matching no function, a cached Bazel green that executed nothing, an empty
`statusCheckRollup` read as all-green — all of which render *never ran* identically to
*passed*. It is recorded here rather than in the finder's name because the next person to hit
it will hit the glob, not the person.

The rule bites hardest exactly where it feels most deserved — a sharp catch invites credit,
and credit is what turns a finding into a person. So the reasoning is kept and the ownership
is dropped, including for the catches that changed this design most (§4.2 in particular, which
began as two separate observations and became the invariant the design now rests on).

The one exception is the **Status** line, which records that revisions were ACK'd and by which
gate. That is process metadata about the document's standing rather than a claim about the
code, it is what every RFC in this directory carries, and an ACK covers only the HEAD it
reviewed — so naming the gate and the revision is what makes the coverage checkable.

### 12.2 Provenance — every load-bearing claim, PROBED or READ, with its command

Rev 2 and rev 4 were both NAK'd on claims of the shape *"X works today"*, *"X is reachable"*,
*"X is not in scope"*, *"X has landed"*. Each time the claim was derived by reading code that
was genuinely present and genuinely correct — and the claim was still false, because presence
is not reach. This table exists so that class of claim cannot appear again unlabelled.

All commands are against `origin/master` @ `bd0035383`, with `-- '*.go'` and `grep -v _test.go`
unless shown otherwise.

**Three provenance classes, not two.** Rev 5 adds **PROBED-ELSEWHERE**: a measurement taken
against a real system by someone else, with a named reproduction, that I did not re-run. It is
distinct from READ (an inference from source) and from PROBED (I ran it). Collapsing it into
PROBED would launder a second-hand measurement into a first-hand one — which is exactly the
move that produced rev 4's deleted deliverable — and collapsing it into READ would understate
evidence that is genuinely empirical. The class is applied symmetrically: the four-cell Java
result **supports** this design and is still labelled PROBED-ELSEWHERE.

| claim | § | prov | command / evidence |
|---|---|---|---|
| Java ANSWERS an indexed nested GROUP BY (4 cells) | §2.5 | **PROBED-ELSEWHERE** | live Java conformance server @ 4.12.11.0; artifact not reachable from `bd0035383` (see §2.5) |
| corpus `pass=69 fail=1`, sole failure is `:61` | §8 | **PROBED-ELSEWHERE** | corpus run on the branch carrying the 3-seg + CQ-72 + agg-typing fixes |
| group-key ladder is in `upgradeAggregateOperands` | §3.1 | **PROBED** | `git show origin/master:…logical_predicate.go` → fn spans `:4523-4757` |
| `SELECT r.v.z` is refused 42703 today | §0.0 | **PROBED** | live FDB query on the corpus schema |
| `SELECT r.v` works (2 segments) **on a SINGLE-SOURCE query only** | §0.0 | **PROBED (narrow shape)** | same; `cols=[V] rows=8`. **DOWNGRADED in rev 6** — says nothing about shadowed / duplicate-alias legs, where a 2-segment read is now known to bind the WRONG column (§12.3) |
| `ORDER BY r.v.z` dies in the executor | §3.2 | **PROBED** | same; `ordinal -1 … malformed plan` |
| resolver is depth-capped at 1 | §0.0 | **PROBED** | `ResolveQualifiedColumnNested(qualifier, col Identifier)`, one `LookupColumn`→`LookupStructField`, no loop |
| walker rejects ≥3 segments | §6 P0 | **PROBED** | `expr/walk.go` `walkColumnRef`: `switch len(uids) {case 1; case 2}` → `UnsupportedExpressionShapeError` |
| that error is SWALLOWED | §7(c) | **PROBED** | `walk.go:2237-2242` (its own comment); consumer `plan_visitor.go:838-845` matches 4 other types |
| `splitColumnRef` has 6 non-test call sites | §6 P0 | **PROBED** | `git grep -n "splitColumnRef(" \| grep -v _test.go \| grep -v ":func "` → 6 |
| `bakeSegmentedColumnRef` has 6 call sites | §0.1 | **PROBED** | `git grep -n "bakeSegmentedColumnRef(" \| grep -v _test.go \| grep -v ":func " \| grep -v "//"` → 6 |
| the guard is NOT landed | §0.1 | **PROBED** | `git show origin/master:…cascades_translator.go \| grep -A4 "func bakeSegmentedColumnRef"` |
| ORDER BY refusal is NOT landed | §4.2 | **PROBED** | same file sweep; no refusal site exists |
| gate census = 7 classes, 2 ladders | §7 | **PROBED** | the four sweeps listed at the head of §7 |
| gate 5 has a 3rd site w/o gates 3-4 | §7(b) | **PROBED** | `git grep -n "validateGroupByProjection("` → `:3154`, `:10303`, `:945` |
| gate 7 rejects multi-accessor paths | §7.1 | **PROBED** | `cascades_translator.go:7039` condition read in full |
| descent is co-equal, not a fallback (Java) | §4 pt 2 | **PROBED** | `SemanticAnalyzer.java:486` into `directMatchesBuilder`; asserts at `:430`, `:436` |
| Go pins that rule already | §4 pt 2 | **PROBED** | `scope_nested_test.go`, `TestScope_NestedDescent_CollidesWithSourceAliasAsAmbiguity` |
| depth-N ready below the resolver | §6 P0 | **PROBED** | `proto_descent_test.go`, `TestFieldValue_DescendProtoMessage`, 3-accessor fixture asserts `42` |
| 3-seg branch is 0 commits ahead | §6, §8 | **PROBED** | `git rev-list --count origin/master..fix/three-segment-qualified-path` → 0 |
| executor evaluates the key `Value` | §3 | READ | `streaming_cursors.go` `computeGroupKey` → `k.Evaluate(...)` |
| naming takes the resolved path | §3 | READ | `group_by.go` `AggregateKeyColumnName` |
| Java's aggregate pipeline (all of §2) | §2 | READ | 14 ranges, hand-verified twice, confirmed line-exact in review |
| HAVING binds structurally in Go | §3 | READ | `groupKeyOrdinalByStructure` |
| gate 7's objection can be answered | §7.1 | **NEITHER** | **open design question — not yet answered, see below** |

### 12.3 THE PROVENANCE TABLE FAILED — a PROBED cell carried a presence-vs-reach error

**This is the most serious finding in rev 6, because it is a failure of the instrument built to
prevent exactly this failure.**

§12.2 marked *"`SELECT r.v` works (2 segments)"* as **PROBED**, from a live query returning
`cols=[V] rows=8`. That probe was run on a **single-source** query. `[PROBED-ELSEWHERE]` a
separate investigation found that on a **shadowed or duplicate-alias leg** the same two-segment
reference binds the wrong column: `semantic/analyzer.go:88` **discards the
`[]NestedAccessor` chain**, and its two consumers (`expr/expr.go:546`, `:628`) then mint an
ordinal addressing the **whole struct** — so `SELECT r.v` reads `V`, not `V.Z`. A **silent
wrong-column read, today, at two segments**, on a shape this RFC cited as working.

**The mechanism of the failure, which is the part that generalises.** The cell was not
mislabelled — a probe genuinely ran and genuinely returned rows. It probed the *easy* shape and
the label recorded only that *a* probe happened. **A probe of the easy shape is
indistinguishable, in the table, from a probe of the hard one.** That is the same
presence-vs-reach error that sank rev 2 (a resolver that existed but could not reach) and rev 4
(a guard reported landed but never probed) — now committed *inside* the table added to stop it,
which is the strongest possible evidence that the label alone is insufficient.

**The rule rev 6 adds, and the table is amended to enforce it:**

> **`PROBED` must record WHICH SHAPE was probed, not merely that a probe ran.** A reach claim is
> only as strong as the hardest shape in its class that was actually exercised. Single-source
> is not multi-source; unshadowed is not shadowed; unindexed is not indexed; two segments is not
> three.

Applied to this RFC's own cells: the `SELECT r.v` row is **downgraded** — it establishes that
two-segment resolution works *on a single-source query*, and establishes nothing about shadowed
or duplicate-alias legs, where it is now known to be wrong. §12.2's row is annotated
accordingly.

**A fix is NOT folded here.** The wrong-column read is dispatched as its own investigation; it
is a live defect at two segments, therefore independent of this RFC's three-segment capability,
and it must be measured before anyone designs against it. What this RFC owes is the record and
the corrected label, not a speculative repair — folding a fix on a second-hand description
would be the rev-4 error a third time.

**The last row is the honest state of this RFC.** Gate 7's comment argues a multi-accessor path
"cannot be made safe … by borrowing a coincident final ordinal". Admitting nested grouping keys
there is the remaining design work, and rev 5 does **not** have it. Everything above it is
either measured or a reading of code whose *reach* is not load-bearing. That distinction is
what the previous four revisions did not draw, and it is why they each shipped a phasing built
on an unprobed enabler.
