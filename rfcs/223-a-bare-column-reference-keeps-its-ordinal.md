# RFC-223 — A bare column reference keeps its ordinal, exactly as a qualified one does

**Status:** rev 4 — ACK'd by Graefe and Torvalds, and **IMPLEMENTED**.
**Scope:** one shape predicate, `resolveBaked`, shared by every site that binds a
projected column reference (`plan_visitor.go` and `logical_predicate.go`).

**Implementation notes that changed what this document says**, recorded here
because each corrects a claim the design made:

1. **`resolveBaked` takes a `childlessOK` parameter, and it is an UNMEASURED
   PRECAUTION rather than a necessity.** `upgradeAggregateOperands`
   (`logical_predicate.go`, the aggregate GROUP-key filler — **not** the sort-key
   filler, which is `qualifyShadowedSortKeys` in `plan_visitor.go` and has
   neither a childless arm nor an `sq.joins` guard) admits the CHILDLESS shape
   only under `len(sq.joins) == 0`, with "on a join, childless would lose the
   defining leg and remains forbidden". But setting `childlessOK` true at BOTH
   sites — the context-free union this RFC originally specified and both gates
   ACK'd — produces a byte-identical golden and a fully green suite. So no
   measurement says the parameter is load-bearing. It is kept because the
   failure it guards is SILENT (a childless ordinal read over a merged row is a
   wrong-slot read, not a decline), and a guard against a silent failure is
   worth keeping without a reproducer — but it is not worth claiming one.
   It is also **Go-only**: Java's `resolveIdentifier` is a lookup plus two
   asserts, no fork and no context, so §3's "Java has one function, Go gets one
   function" is only half-honoured by one function applying different rules to
   its two callers. What Go gets is one SITE for the rule.
2. **The fix moves the `foldStep1Seed` census by ZERO.** An earlier revision of
   this document and of the gate comment claimed the fix produced two extra fold
   firings "on a shape that previously never got there". **That is false**, and
   it was refuted by the very control it cited. The +2/+2/+0 is ordinary corpus
   growth from the one `ORDER BY n.sk` query this change adds. Both controls are
   quoted in `foldStep1SeedGates` step 5. The gate VALUE was right either way,
   which is exactly why CI could not have caught it.
3. **Three golden records move, not one** — §(g), which has now been wrong twice.
4. Measured deltas the design asked for: `ProjectedValues` mentions and
   nil-branch count are **unchanged at 116 / 24** (the extraction adds no new
   consumer and removes no branch), and the plan-reachability **ratchet delta is
   ZERO** — `edges=22316 compared=22314 unreachable=0 no-quantifier=2 over 2516
   queries`, byte-identical either side of the fix.

Rev 2 folded Graefe's three conditions and Torvalds' three blockers; rev 3 folds
Graefe's delta items (gate (f), the leg-attribution half of the precondition, and
multi-accessor ambiguity coverage) and Torvalds' last stale comment. The
substantive finding of the lap was **Torvalds' blocker 1**: the widened predicate
is imported without its explicit-qualifier precondition, and the one dimension
where a wrong bake reads SILENTLY was the one dimension unmeasured. It is now
nine pinned arms.
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
COLUMN VALUES in order — the escalation reported a wrong column, and a row COUNT
reads identically whether the column is right or wrong. Order is asserted too;
the claim is that a count ALONE is insufficient, not that order goes unchecked.

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

**Chosen — accept at the source,** the union of the two shapes, so a bare
reference and a qualified one resolve by the same rule:

```go
isFV && ((fv.Child == nil && fv.SourceRelativeBaked()) ||
         (fv.Child != nil && fv.RootIsLegRelativeUnpinned()))
```

**This is extracted as a shared `resolveBaked` predicate called from BOTH sites,
not inlined at `plan_visitor.go:706`.** A disjunction of two shape tests written
out at one call site is two rules spelled as one boolean, and leaving the other
site with its own copy would preserve exactly the asymmetry this RFC faults the
alternative for leaving standing. §3's argument is that Java has ONE resolution
*function*; the Go change has to be one function too, or it is not the same
argument. Implementation extracts the predicate first and converts both callers.

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

**The bare MULTI-ACCESSOR case keeps passing, and that is not implied — it is
stated because the predicate change reaches it.** `RootIsLegRelativeUnpinned`
carries no `len(Accessors)==1` requirement, so at the bare site it newly admits
fused multi-accessor paths that today pass through a *different* arm. `n.sk`
bare over the join with a projected `EXISTS` returns `[[1 50 true] [2 40 false]
[3 30 true]]` both before and after, and it is a standing control
(`CONTROL_unqualified_structMember_join_projectedExists`) rather than a one-off
observation.

**Two consumers could in principle turn a nil→non-nil slot into an operator-tree
change rather than a rendering change**, and they are named so the single-record
golden result is read as evidence about them:
`rule_push_requested_ordering_through_projection` (a projection whose slots are
known Values can push an ordering it previously could not) and
`rule_projection_elim` (elimination is only provable when the slots are
inspectable). Across the whole corpus **neither produced an operator-tree
change** — the one golden record that moves keeps a byte-identical operator tree
— so both rules' extra reach is either unexercised or ordering-neutral on this
corpus.

**Ambiguity cannot ride in on the widened predicate, and the reason is measured,
not assumed.** `resolveQualifiedBaked`'s safety rests on two things: the shape
predicate AND the fact that resolution ran with an explicit qualifier. Moving the
predicate onto a value produced by the bare correlated arm imports the first
without the second, so the question is whether a bare reference naming more than
one leg can reach the bake site at all. **It cannot.** Six shapes — an ambiguous
bare column with and without the fold, a self-join whose bare column exists in
both legs, and the duplicate-plain-alias case `resolveQualifiedBaked`'s own doc
comment names — are all refused with **42702 by the SQL layer, identically with
and without the change**.

That is a NEGATIVE result and it is the fact the widening's safety rests on, so
it is pinned rather than reported (the `AMBIGUITY_*` arms), with a failure
message naming what gets re-armed if the rejection is ever relaxed: an ambiguous
bare reference reaching a widened guard would bake one leg's ordinal and read
that slot **silently**.

**Ambiguity is only HALF the precondition, and the other half is shown by the
golden.** An explicit qualifier buys two things: no ambiguity, *and* a
determinate leg. The `42702` arms discharge the first. The second is visible in
the one moving golden record: the bare references re-render as `LO.LI#0` and
`HI.HI_ID#0` — **leg-attributed**, not merely leg-relative. A bare-resolved value
that had lost its leg identity could not render a leg qualifier it does not
carry. So the bare path reaches the bake site with the same two properties the
qualified path guarantees, one pinned by tests and one by the golden.

**The ambiguity arms cover the MULTI-ACCESSOR population too, and that is not
incidental.** The population the widening newly admits is precisely the
multi-accessor one (`RootIsLegRelativeUnpinned` carries no single-accessor
requirement), so six arms of single-accessor columns would have left the
newly-admitted population unguarded while reading as complete coverage. A
self-join of `t1` puts a struct `n` in both legs; bare `n.sk` and bare `n` over
it are refused with `42702` as well. **Nine arms, and mutating the expected code
to `42703` reds every one.**

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

**These numbers are SUPERSEDED and kept only as history.** They were taken before
this branch rebased onto the head that carries the leg-reference RFC, which moved
the base from `572/160/202` to `586/166/210`. The live figures are in
`foldStep1SeedGates`, which now runs `586 → 588 → 602 → 604`; nothing below is a
current claim about the gate.

Pre-rebase, `foldStep1Seed`'s equalities moved twice, each time proven by moving
the fixture file out of the package and back rather than asserted:
`572/160/202 → 574/162/202`, then `→ 588/174/204`. **The second round split +14
across TWO arms** — ACCEPT +12 and `rv-no-exist-ref` +2 — because two of the new
controls project no reference to the exists alias at all. The first round landed
wholly in ACCEPT.

Post-rebase every increment was RE-MEASURED rather than carried over, because
carrying a pre-rebase delta onto a moved base restates a number nobody measured.
And the final increment is **corpus growth, not the fix**: toggling the
production diff with the corpus held fixed moves this census by zero.

---

## 6. Gates

- (a) Every arm of `TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin` green
      with its three tripwires converted to value assertions matching their
      qualified twins, and the six `AMBIGUITY_*` arms still green **unchanged** —
      those must NOT move, and a change there is a blocker, never a re-bless.
- (b) **FOUR tripwires convert, not three.** The fourth is in a different file:
      `structcol_repro_fdb_test.go`'s
      `struct_root_beside_a_projected_exists_over_a_join_still_fails`. §4's
      "exactly 4 failures" counts it, while an earlier draft of this gate named
      only the three in one file — which would have flagged the fourth as an
      unexpected movement. It converts to the values its own failure message
      already names.
- (c) `just test` green, with the only expected movements being (a), (b), the
      single golden record in (e), and the leg-reference RFC's named tripwire.
- (d) `foldStep1Seed` and every other census green with no equality relaxed; any
      movement attributed by the move-out/move-back control.
- (e) No change to `FieldTypeForProtoField`. The type erasure is out of scope and
      stays a separate, documented divergence.
- (f) **`resolveBaked` exists as ONE function and EVERY site calls it.** §4's
      structural claim — that Go gets one resolution rule — is otherwise
      unchecked at implementation review, and an inlined disjunction would
      satisfy the prose while leaving two rules.

      The gate is mechanical and its SCOPE was widened during implementation.
      Converting the two sites §2 names left **three inline copies of the old
      childless-only predicate** in `logical_predicate.go` (`:2692`, `:4389`,
      `:4421`), one of which self-describes as the twin of the PlanVisitor's
      bare-projection step and sits ~30 lines above its qualified partner — the
      identical disjoint pair §2 calls the defect, surviving the fix for it.

      **They are folded too, and the reason is that their latency could not be
      established.** The natural probes — a derived table and a CTE wrapping the
      same multi-source `FROM` — do not answer the question, because BOTH forms
      already fail once a projected `EXISTS` is added, for two different
      pre-existing reasons, identically with this change reverted (§8). A
      latency claim nothing can measure is not a claim to ship, so the copies
      were removed rather than documented.

      Verified mechanically: `git grep -c "SourceRelativeBaked()"` over
      `plan_visitor.go` and `logical_predicate.go` returns the two occurrences
      inside `resolveBaked` and nothing else. Folding them is behaviour-neutral
      on the corpus — census and golden both unchanged.

### (g) The THREE golden records that move, justified individually

**This section has now been wrong TWICE, and both corrections are kept in place
rather than tidied away.** A first draft predicted no golden would move. The
second said ONE record moved — a misreading of the failure text: the differ
reports "**3 line(s) differ**" and one `first divergence`, which is three
records at one line each, not one record at three lines. The re-bless diff is
what settles it, and it is the reason a re-bless is read rather than run.

All three are **the same change on the same shape** — a bare reference in a
projection over a multi-source `FROM` — and all three keep a **byte-identical
operator tree**:

```
cte.yaml#13   SELECT li, hi_id FROM lo, hi ORDER BY li, hi_id
-plan:  Project([LI#0, HI_ID#1],   InMemorySort(...))
+plan:  Project([LO.LI#0, HI.HI_ID#0], InMemorySort(...))

cte.yaml#21   SELECT w, a FROM c1, c2 WHERE w = a ORDER BY w
-plan:  Project([W#0, A#1],        InMemorySort(...))
+plan:  Project([C1.W#0, C2.A#0],  InMemorySort(...))

cte_join.yaml#2  ... eng_emp AS (SELECT name FROM emp e, eng_dept ed WHERE ...)
-plan:  Project([NAME#0], InMemorySort([NAME ASC], Project([NAME#1],   NLJ(...))))
+plan:  Project([NAME#0], InMemorySort([NAME ASC], Project([E.NAME#1], NLJ(...))))
```

The third is worth separating from the first two: its ordinal does **not** move
(`#1 → #1`), only the qualifier appears. So the change is not "ordinals get
renumbered" — it is "the reference now carries the leg it was always resolved
against", and the ordinal moves only where the old flat ordinal and the new
leg-relative one genuinely differ.

**Rows are unchanged for all three, verified independently of the plan text.**
Each is a `yamsql` corpus entry carrying row assertions — `[1,3] [1,4]`,
`[1,1] [2,2]`, `["Alice"] ["Bob"]` — and `//pkg/relational/conformance/yamsql`
passes under the change (84 of 85 targets pass, with `explaindiff` the only
failure and only on the golden text). An ordinal that had moved to the wrong
slot would have changed those rows.

**Why these three and no others.** `FROM lo, hi`, `FROM c1, c2` and `FROM emp e,
eng_dept ed` are the corpus' multi-source FROMs whose projections spell a column
BARE. None involves an `EXISTS`, so these records show the guard's effect
**isolated from the fold** — which is the useful part: the fold was where the
defect was OBSERVED, and the guard is where it lives.

**The `#1 -> #0` is not a lost column.** The old `HI_ID#1` was a FLAT ordinal
into the concatenated row; the new `HI.HI_ID#0` is LEG-RELATIVE — slot 0 within
leg `HI` — which is what `rowLegsBinder` binds and what `resolveBaked`'s
child-bearing arm requires. A qualified spelling of the same query already
rendered this way; the bare spelling now agrees, which is the whole RFC in one
line of golden.

Re-blessing is therefore **three records, each named above, with the full diff in
the commit** — not a bulk `dump` of a file whose other 3900-odd lines are
unchanged. Verified by reading the re-bless diff: exactly 3 changed lines.

Costing is untouched: the change moves a plan-time ordinal from absent to
present and constructs no new operator, and no cost formula reads it. The 1M
stress gate is consequently not claimed as required; the golden movement above is
a rendering/addressing change with no operator or cardinality effect. Should
review disagree that this is cost-neutral, the stress comparison runs in a
sibling worktree on the same filesystem before merge.

---

## 8. TWO PRE-EXISTING DEFECTS FOUND HERE, NEITHER FIXED NOR PINNED — ESCALATED

Asking whether a CTE or a derived table routes projection binding through
`logical_predicate.go`'s surviving copies turned up two failures that have
nothing to do with this change. **Both reproduce identically with this change
reverted**, so neither is a regression, and both are LOUD — no silent wrong rows.

```
WITH c AS (SELECT id, n FROM t1)
SELECT c.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h
FROM c, t3 WHERE t3.t1_id = c.id
  -> 42703: no FROM source aliased as C

SELECT d.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id) AS h
FROM (SELECT id, n FROM t1) AS d, t3 WHERE t3.t1_id = d.id
  -> correlated FieldValue "ID" (correlation "D") evaluated against an
     unbound/unrecognized context (*RowEvalContext (multi-leg row cannot serve
     a source-relative ordinal)) — no frontier row resolved (planner/executor bug)
```

Each has a CONTROL that differs only by removing the projected `EXISTS`, and
**both controls pass**, returning `[[1 1] [2 2] [3 3]]`. So in each case it is
the projected-EXISTS fold that loses the source — the CTE alias in the first, the
derived-table correlation in the second.

**Why they are escalated rather than pinned here.** Adding either query to the
sqldriver corpus trips a hard-zero census: `LEG-LOCAL BAKE CENSUS FAIL:
UnderivableLegs = 2, want 0`. That is not an obstacle to route around — it is the
diagnosis. An underivable leg "has no ordinal it can honestly carry on its own
alias, so every read through it falls through to the qualified NAME", which is
exactly the `42703`. Pinning these shapes therefore means either fixing the
underivable leg or knowingly red-lining a guard that is telling the truth, and both
are a different change from this one.

They are stated here, in the conversation, and in the commit message rather than
filed as a TODO — a filed item is how a blocker becomes invisible, and the next
person should get the reproducer, the control, and the census pointer together.
