# RFC-212 — The name-model seed is not what feeds either dotted reader

Status: **PROPOSED, revision 2** — awaiting Graefe + Torvalds ACK. This is
CQ-95's RFC.

Closes nothing on its own. It **re-derives CQ-95's acceptance conditions against
measurement**, splits the item along the seam the measurement found, and states
the day-one build. CQ-95's TODO.md booking is restated to match; the two
documents no longer disagree.

Revision 1 was NAK'd. Four of the seven findings were the same failure — an
argument standing where a measurement was available — and one of them refuted
this document's own §3.2:

- **§3.2 said the three seed-tested sites were DARK. They are REACHED**, 208
  times, and take the name arm zero times because the seed over them is already
  windowed. Revision 1 could not tell the two readings apart because its census
  counted calls, not arms. The corrected reading strengthens the demotion and
  withdraws any suggestion of dead code.
- **The disclosed counter-ordering bug had no pin.** It is now AST-pinned in
  `docscheck`, on the machinery this repo already had for the identical property.
- **§1.1's "additive" claim was not airtight.** `Equals`/`Hash` ignoring `Legs`
  establishes type identity only; `refineRowTypes` branches on the leg table and
  declines a populated-vs-empty pair. Measured and pinned (§3.4), and §1.1 now
  carries that as a stated precondition.
- **The Java search was overclaimed** — "exactly one hit" was wrong by ten. The
  substantive claim survives; §2 states what was actually searched. In an RFC
  about predictions shipped in the voice of measurements, that correction is not
  cosmetic.
- **The table-name capability had three unstated ambiguity cases.** §4.3 decides
  all three.
- Citation ranges corrected (§2).

---

## 0. Lead: four things in CQ-95's framing are refuted by measurement

Stated first because everything below depends on them, and because two of the
four were carried into the item as settled.

**(1) The `108 of 108` merged-layout reading does not exist on `master`, and it
means the OPPOSITE of "the wall is cleared".**

The counter that prints `correlatedStep1 firings WITH a merged layout N of N`
lives only on the unmerged branch `feat/rfc197-m4-cq68`:

```
$ git grep -n "merged layout" master -- '*.go' | grep fold_step1
(no output)
$ git grep -n "merged layout" feat/rfc197-m4-cq68 -- '*.go' | grep fold_step1
feat/rfc197-m4-cq68:pkg/recordlayer/query/plan/cascades/fold_step1_seed_census.go:336: ...
```

And the counter's own documentation, on that branch, states the conclusion the
reading supports (`fold_step1_seed_census.go:376-381`):

> THE NUMERATOR IS 108 OF 108 — UNIVERSAL, not occasional, measured over the
> whole real-FDB corpus on three consecutive runs. Every correlated firing
> arrives at the layout read with a merged layout already derived, because on
> that arm step1RV is sel.GetResultValue() handed back unchanged and it is
> already a pristine ordinal seed. **So any conversion of the reconstruct-nil
> residue meets the BakedNameContextError wall on day one, on 100% of the
> correlated population** — not on some future corpus shape.

Universality of the merged layout on that arm is the wall being contacted
everywhere, not the wall being removed. It is also a fact about a DIFFERENT
seed: `foldStep1Seed` is the Cascades NLJ rule's step-1 seed
(`rule_implement_nested_loop_join.go`), while CQ-95's seed is the relational
translator's unnest merge (`cascades_translator.go:3667`). No data path connects
them, and `DECLINE correlatedStep1` is documented at
`fold_step1_seed_census.go:60-65` as **PERMANENT** by a prior architecture
ruling, because the canonical semijoin's good plan needs NAME binding to flow a
sibling comparand into a SARG'd correlated index scan.

**(2) CQ-95's headline acceptance condition — "the leg-column provenance
dotted-hit count goes to 0" — is not produced by converting the mint it is
booked against.** The two live dotted hits are `C.CV` and `I.QTY`, and both are
minted by the correlated-scalar seed in `clustered_outer_scalar.go`, not by
`rebaseUnnestOuterLegPredicate`. §3 measures this.

**(3) The seed CQ-95 wants to convert has ALREADY converted on every branch the
corpus reaches, and the mint produces no names at all.** Measured (§3.2): the
three seed-tested branch points are reached 10, 165 and 33 times, and the NAME
arm is **0 on all three** — they take the ordinal twin (9 and 33), the plan-time
bake (1 and 92), or the leg-relative fall-through (73). The two sites that do
reach the mint apply no seed test and mint zero qualified names.

An earlier revision of this RFC said those three sites were DARK — never reached.
That was wrong, and the error was mine in exactly the way §3.2 now pins: the
first reading placed the census counter below the function's inert guard, where
every site reads 0. **The corrected reading is stronger, not weaker.** "Dark"
would have meant the shapes are not planned; "reached, and never on the name arm"
means the shapes ARE planned, 208 times, and the conversion this item books has
already happened on the other side of each branch. There is nothing left there
for a lifted seed gate to convert.

**(4) `legWindowSlot` is NOT blocked on a missing capability.** The "resolver at
mint" it is documented as waiting for already exists, is already used on this
exact channel, and `legWindowSlot` serves that resolution's DECLINE population.
§4 measures this. It is blocked for ONE key kind — the table-name addressing
route — and that is a small thing to build, not a wall.

Taken together: **CQ-95 is not a STOP, but it is not the work either.** Its
subject is an arm that is reached and never taken because its counterpart already
runs ordinal; its acceptance signal belongs to a different producer; and its one
genuinely blocked reader is blocked on something small and nameable rather than
on the executor widening it is booked behind.

---

## 1. Decision

**Split CQ-95 at the seam measurement found, and build the half that is
buildable on `master` today.**

1. **Reader one (`executor.rowSlotForLegColumn`'s dotted arm) is decoupled from
   the seed conversion and retired ADDITIVELY.** Its two live hits come from a
   producer that already holds the ordinal it is making the reader re-derive
   from text: `clustered_outer_scalar.go:487-495` builds each leg column as
   `RecordConstructorField{Name: leg.binding+"."+COL, Value:
   FieldValueOfOrdinal(outerQOV, leg.start+i)}` and
   `RecordConstructorValue.Type()` then synthesises a `RecordType` with no leg
   table (`values.go:4509-4525`), throwing the window away. Restore it: have the
   SEED's derived type carry `RecordType.Legs` built from the same
   `leg.start`/width the values already state. **Day-one scope**, under two
   preconditions §6.1 states and §3.4 measures — the second of which corrects an
   earlier revision of this line.

   Not by changing `RecordConstructorValue.Type()`: that method serves every
   record constructor in the system, so a leg table attached there would populate
   types across the whole planner. The change belongs where this seed's type is
   derived and stored for this reader (`widenLegTypesFromPlan` /
   `adaptLegPositional`), which is what keeps its reach the seed's.

2. **The seed conversion (CQ-79's residue, `rebaseUnnestOuterLegPredicate`) is
   DEMOTED, not scheduled**, and the demotion's reason is now the strong one: the
   three seed-tested branch points are REACHED 208 times and take the name arm
   ZERO times, because the seed over them is already windowed. There is nothing
   for a lifted `unnestExistsSeedSafe` to convert on this corpus. CQ-95's `DONE`
   list — "the seeds those five call sites sit over are ORDINAL, the name-keyed
   rebase is DELETED, `MergedReAnchor` stops being vacuous, dotted hits go to 0"
   — has one clause already satisfied on the live path, one measured unreachable
   from here (§3.3), and one (`MergedReAnchor` un-vacuous) that a correct outcome
   would leave unsatisfied. Nothing in this RFC converts or deletes the mint, and
   the reached-ness explicitly withholds any deletion warrant: an arm reached 208
   times is not dead code, whatever its name-arm count.

   The old sequencing gate — "CQ-95 and CQ-68 SEQUENCE together, CQ-68 first" —
   is removed as unsatisfiable rather than merely stale. A gate on another item's
   completion cannot be discharged when that item does not complete by being
   done. Items 1 and 3 below are ungated; the executor-widening coupling survives
   only as something to re-derive when that widening lands.

3. **Reader two (`query.legWindowSlot`) gets a stated, checkable retirement
   condition in two parts**, replacing "blocked on resolver-at-mint":
   - **the ALIAS route** retires by carrying the correlation the resolver
     already selected onto `logical.ColumnRef` and comparing through
     `values.SameLeg`. Not a capability — a field and a thread.
   - **the TABLE-NAME route** retires only after the semantic scope grows the
     second addressing route the leg-layout map already serves. That route is
     live at exactly one call over the corpus and the scope rejects it today.
     Building it is a bounded change to `Scope.ResolveQualifiedColumnNested`;
     §4 costs it.

**Why this and not the alternatives** is §5. The selection criterion is
long-term correctness, and the deciding fact is §2: Java's identity here is an
ordinal, with no name-model anywhere in the planner, so every one of these
readers is a divergence to close rather than a design to preserve.

---

## 2. Java is the spec, and it carries an ORDINAL

Java's Cascades planner has **no name-model seed at all**. Where Go mints
`leg + "." + UPPER(field)` to address a merged row, Java collapses the lower
quantifiers into a `RecordConstructorValue` of **unnamed** columns and seeds the
rebase with a loop index.

`fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades/rules/PartitionSelectRule.java:284-291`:

```java
final ImmutableList<Column<? extends Value>> lowerResultColumns =
        lowersCorrelatedToByUppers.stream()
                .map(lowerAlias -> Verify.verifyNotNull(aliasToQuantifierMap.get(lowerAlias)))
                .map(quantifier -> (Value)QuantifiedObjectValue.of(quantifier))
                .map(Column::unnamedOf)
                .collect(ImmutableList.toImmutableList());
final var joinedResultValue = RecordConstructorValue.ofColumns(lowerResultColumns);
```

and the seed itself, `PartitionSelectRule.java:296-303`:

```java
final var translationMapBuilder = TranslationMap.regularBuilder();
for (int i = 0; i < lowerResultColumns.size(); i++) {
    final Column<? extends Value> lowerResultColumn = lowerResultColumns.get(i);
    final var lowerAlias = ((QuantifiedObjectValue)lowerResultColumn.getValue()).getAlias();
    final var index = i;
    translationMapBuilder.when(lowerAlias)
            .then((sourceAlias, oldLeafValue) ->
                    FieldValue.ofOrdinalNumber(QuantifiedObjectValue.of(newUpperQuantifier), index));
}
```

The identity is `(newUpperQuantifier, index)`. The lambda ignores
`oldLeafValue` entirely — no name from the old reference survives into the new
one.

Four independent facts make this structural rather than stylistic:

- **Unnamed columns cannot be named.** `Column.unnamedOf`
  (`Column.java:77-79`) builds a `Field` with `Optional.empty()` name
  (`cascades/typing/Type.java:2918-2920`); asking for it throws (`cascades/typing/Type.java:2763-2765`). A
  name-keyed reference to a merged leg is not merely avoided in Java, it is
  inexpressible.
- **`ResolvedAccessor` identity is the ordinal alone.**
  `FieldValue.java:676-690` — `equals` compares `getOrdinal() ==
  that.getOrdinal()`, `hashCode` hashes the ordinal. The field name is not part
  of either.
- **Name accessors immediately discard the name into an ordinal.**
  `FieldValue.ofFieldName` (`FieldValue.java:302-304`) builds
  `new Accessor(fieldName, -1)` and `resolveFieldPath`
  (`FieldValue.java:284-295`) resolves it to an ordinal through
  `getFieldNameToOrdinalMap()`. Evaluation is by ordinal
  (`FieldValue.java:169`, `getFieldValueForFieldOrdinals`).
- **Re-anchoring never re-resolves a name.** `Value.translateCorrelations`
  (`Value.java:346-368`) replaces only the LEAF; a `FieldValue` is not a
  `LeafValue`, so its already-resolved ordinal path rides through untouched and
  `withNewChild` (`FieldValue.java:157-161`) re-anchors it via
  `ofFieldsAndFuseIfPossible` — pure ordinal-path concatenation.

**There is no alias-to-column concatenation anywhere in the planner.** Stated as
what was actually searched, because an earlier revision of this line said "an
exhaustive search returned exactly one hit" and that was wrong by ten — an
overclaimed search, in an RFC whose subject is a prediction shipped in the voice
of a measurement.

The sweep covered `+ "."`, `"." +`, `+ '.'`, `String.join(".", …)`,
`Joiner.on(".")`, `"%s.%s"`, `.concat(".`, `Collectors.joining(".")` and
`append('.')`. It found **eleven** dot-joins in `fdb-record-layer-core`, four of
them under `query/plan/**`: `FieldValue.java:431`, `TypeRepository.java:421` and
`:445`, and `RecordQueryScoreForRankPlan.java:351`. The remaining seven are under
`provider/` and `metadata/`.

The substantive claim survives the correction intact: **none of the eleven joins
an alias or a table qualifier to a column name.** `TypeRepository`'s two are
protobuf namespacing, `RecordQueryScoreForRankPlan`'s is index-name rendering,
and `FieldValue.java:431` is `FieldPath.toString` — whose fallback for an unnamed
accessor is `"#" + ordinal`, so even the debug rendering treats the ordinal as
primitive. `ResolvedAccessor.toString` joins with semicolons, not dots.

The nearest qualified name in the whole product is
`Identifier.fullyQualifiedName()`
(`fdb-relational-core/.../query/Identifier.java:108-115`), a `List<String>` path
in the SQL semantic layer that is resolved to a `Value` tree and gone before
Cascades runs.

**This settles the target and it settles the direction of every choice below.**
It does not, by itself, say which Go edit gets there — that is what §3 and §4
measure.

---

## 3. Measurement: reader one's producer is not the mint CQ-95 names

### 3.1 THE run. One reading, and every number in this RFC comes from it

Every corpus figure below is from a SINGLE uncached run at this branch's head,
`feat/rfc197-cq95-rfc`, over `master` at `6692ac268`. Earlier drafts quoted three
different runs; population totals on this path move run to run (the leg-column
provenance denominator read 2294, 2034, 2174 and 1474 across four runs of an
unchanged corpus), so a claim assembled from several readings is a claim nothing
was measured against as a whole.

```
$ git rev-parse --abbrev-ref HEAD
feat/rfc197-cq95-rfc
$ bazelisk test //pkg/relational/sqldriver:sqldriver_test \
    --cache_test_results=no --test_output=streamed --test_timeout=3600
```

Exit 0. Verbatim from the harness's census block:

```
[sqldriver real-FDB corpus] leg-column provenance: calls 1474 (flatHit 120, notDotted 1352, noLegs 0, dottedMiss 0); dotted HITS by identity availability: available 2, unstated 0, diverged 0
  dotted HITS by OWNER selection: sameLeg 0, ownerUnstated 0, ownerNamesNoLeg 2, ownerSelectsOtherLeg 0
  distinct witnesses (4, cap 128):
    DOTTED-HIT "C.CV": leg "C", alias stated and equal
    DOTTED-HIT "I.QTY": leg "I", alias stated and equal
    OWNER-NO-LEG "C.CV": owner "q$3122" names no leg of [C E]
    OWNER-NO-LEG "I.QTY": owner "q$305843" names no leg of [O I]
```

```
[sqldriver real-FDB corpus] translator dotted leg qualifier (per match attempt):
  flatColumnBake     calls 102 | matchAliasIsQualifier 98 | noMatch 4
  legQOVBake         calls 4 | matchAliasIsQualifier 3 | matchViaTableName 1
```

The two dotted-hit witnesses and the two dotted-qualifier tables are IDENTICAL
across all four runs; only the denominators moved. That is what licenses reading
`available 2` and `matchViaTableName 1` as facts about the corpus rather than
about one sample.

```
    legQOVBake [matchViaTableName] qualifier "PA", leg binding "S", leg alias "S"
```

```
[sqldriver real-FDB corpus] foldStep1Seed outcomes (per rule firing):
  ACCEPT                           160
  DECLINE correlatedStep1          108
  DECLINE rv-no-exist-ref          202
  DECLINE reconstruct-nil          102
  DECLINE windows-nil              0
  TOTAL                            572 (counted independently at the call site)
```

The 108 correlated firings exist on `master` too — as the DECLINE class. What
does NOT exist on `master` is the numerator that says how many of them carry a
merged layout. §0(1).

### 3.2 The new instrument, and what it measures

`pkg/relational/core/query/unnest_leg_mint_census.go` counts
`rebaseUnnestOuterLegPredicate` **per call site** and records the qualified
names it mints. The site is a PARAMETER on the function rather than a call-site
counter, so a new caller has to state which population it joins.

The call counter fires BEFORE the function's inert guard (`p == nil ||
len(outerLegs) == 0`). That ordering is load-bearing and it was corrected during
this measurement: with the counter after the guard, a site reached with nothing
to rewrite and a site never reached print the same zero, and the first reading
this RFC took reported all five sites at 0 for that reason. The corrected reading
separates them, and the separation is the finding.

`AssertUnnestLegMintCensus` then checks the claim this RFC turns on: that the
minted-name set and `executor.LegColumnProvenanceDottedNames()` are **disjoint**.

Verbatim, same command as §3.1, exit 0:

```
[sqldriver real-FDB corpus] unnest leg-mint (rebaseUnnestOuterLegPredicate, per call):
  nonChainedMerge(no seed test)      calls 23 | minted names 0
  anchoredNonExists(no seed test)    calls 5 | minted names 0
  buried(!seedWindowed)              calls 0 | minted names 0
  joinPredicate(!seedWindowed)       calls 0 | minted names 0
  chained(name-model seed)           calls 0 | minted names 0
  ARM taken at the three seed-tested branch points — a name-arm ZERO beside
  a non-zero ordinal/planTimeBake arm means CONVERTED HERE, not dead:
    nonChainedMerge(no seed test)      NO SEED BRANCH EXISTS (unconditional call site)
    anchoredNonExists(no seed test)    NO SEED BRANCH EXISTS (unconditional call site)
    buried(!seedWindowed)              reached 10 | name 0 | ordinal-twin 9 | planTimeBake 1 | leg-relative 0
    joinPredicate(!seedWindowed)       reached 165 | name 0 | ordinal-twin 0 | planTimeBake 92 | leg-relative 73
    chained(name-model seed)           reached 33 | name 0 | ordinal-twin 33 | planTimeBake 0 | leg-relative 0
  rebaseUnnestOuterLegPredicateOrdinal calls 133 (counted INSIDE the twin, independently of
  the arms; 42 attributed to an instrumented branch arm, the rest reach it from
  bakeInnerExistsPredicateOrdinal or the chained ordinal path)
  distinct minted names: NONE
UNNEST LEG-MINT CENSUS VACUOUS: the name-keyed unnest rebase minted
  NO qualified names over this run, so the disjointness check against the
  executor's 2 dotted-hit name(s) did not compare anything.
  The per-site call counts above are what distinguishes a DARK arm (never
  reached) from a LIVE one carrying no rewrite; read them before quoting this
  census as evidence about either.
```

Read it in four parts.

- **The two sites with NO seed test are LIVE** — 28 calls between them. These are
  `cascades_translator.go:3059` (the else of the chained-unnest check) and
  `:3400` (the anchored arm the code calls "the correct and now ONLY domain of
  the name-keyed rebase"). No seed-gate flip converts them; CQ-79 said so and
  this measures it. They report NO SEED BRANCH rather than a row of zeros, which
  is a structural statement and not a reading.
- **The three seed-tested branch points are REACHED — 10, 165 and 33 — and the
  NAME arm is 0 on every one.** They take the ordinal twin, the E-1a plan-time
  bake, or the leg-relative fall-through. This is the finding that re-scopes the
  item: the seed those arms sit over is ALREADY windowed on 100% of the firings
  the corpus produces, so the conversion CQ-95 books has already happened there.
  A lifted `unnestExistsSeedSafe` has nothing left to move on this corpus.
- **The twin's INDEPENDENT total is 133**, counted inside
  `rebaseUnnestOuterLegPredicateOrdinal` rather than summed from the arms, and 42
  of those are attributable to an instrumented branch arm (9 buried + 33
  chained). The other 91 arrive through `bakeInnerExistsPredicateOrdinal`, which
  is consistent with the 93 plan-time-bake arms minus the two whose predicate is
  nil. A summed denominator would have been true by construction and blind to
  that fourth caller.
- **Nothing is minted at all.** 28 live calls, zero qualified names — every one
  arrives with an empty outer-leg set or a nil predicate. The channel this mint
  is supposed to feed is carrying no name.

**The ARM sub-report exists because the previous revision of this section got
this wrong.** Without it, `buried(!seedWindowed) calls 0` cannot distinguish
"never reached" from "reached, took the ordinal branch" — and this RFC asserted
the first. Both readings support demoting the conversion, and they imply opposite
follow-ups: dead code to warrant for deletion, versus a conversion already
complete on the live path.

The disjointness assertion is therefore VACUOUS on this tree, and the census says
so out loud rather than printing a clean pass. That announcement is itself pinned
(`TestUnnestLegMintCensus_EmptyPopulationsDoNotFail`), because a silent pass here
reads exactly like two populations measured apart.

No floor is asserted on the live sites. The dangerous direction here is a site
STARTING to mint, which the disjointness check watches; a collapse to zero at the
two live sites would be a deletion signal for the whole arm, not a regression.

### 3.3 What the disjointness means

The executor's dotted arm answers on `C.CV` and `I.QTY`. Those names are minted
by the correlated-scalar seed:

- `clustered_outer_scalar.go:487-495` — `Name: leg.binding + "." +
  strings.ToUpper(leg.typ.Fields[i].Name)`, over `Value:
  NewFieldValueOfOrdinal(outerQOV, leg.start+i)`;
- `clustered_outer_scalar.go:504-509` — `Name: strings.ToUpper(innerAlias) + "."
  + scalarCol` for the single nullable inner scalar leg.

The provenance census independently identified the same producer from the span
tables it measured (`leg_column_provenance_census.go:95-108`): one channel, four
titles, of which only the one containing a dot reaches the dotted arm.

So the acceptance condition "dotted hits → 0" is **not reachable by converting
the unnest mint** — which over this corpus mints nothing to convert — and it is
reachable **without** converting it. The value at
`clustered_outer_scalar.go:493` already states the absolute slot
(`leg.start+i`); it is `RecordConstructorValue.Type()` that drops it. That is
day-one scope (§1.1).

---

### 3.4 §1.1's blast radius, measured — and the "additive" claim corrected

An earlier revision of §1.1 called the change additive because `RecordType.Equals`
and `Hash` ignore `Legs`. **That establishes type IDENTITY only** — interning
keys, memo dedup, plan equality — and says nothing about readers that BRANCH on
the leg table. Since §1.1's whole purpose is to make one reader take a different
branch, at least one changes by construction; the question is whether any other
does.

**The grep.** 69 non-test, non-census references to `.Legs` under `pkg/`. The
ones that branch on emptiness or walk the slice: `evaluation_context.go:243`,
`ordinal_join.go:187,234,461,1037,1082,1249,1945`, `flat_map_cursor.go:493,646`,
`positional_row.go:69,72`, `executor.go:3552`,
`ordinal_seed_layout.go:391,528,587`, `left_outer_existential.go:95`, and
`expressions/quantifier.go:413`.

**The last one is the finding, and it is a plan-level effect.**
`refineRowTypes` (`quantifier.go:409-436`) calls `legTablesAgree` BEFORE the
`Equals` fast path — precisely because `Equals` ignores `Legs`, so two members
stating the same fields under different leg tables would otherwise resolve by
whichever the memo scan reached first. The check treats an EMPTY table as the
statement "this row has no buried-leg boundaries", not as an unstated gap. So a
populated table against an empty one is a CONFLICT and the refinement **declines**.

That is measured, not read: `expressions.TestLegTablePopulation_*` drives it, and
the same tests assert the premise the additive claim rests on (the two types ARE
`Equals`). Removing `legTablesAgree` from `refineRowTypes` turns them red.

**So §1.1 carries a precondition, stated rather than assumed:** the leg table
must be populated CONSISTENTLY by every producer of that row. A half-populated
row does not merely fail to help — it turns a refinement that used to succeed
into one that declines.

**What this RFC does NOT have** is a corpus plan-shape diff of §1.1 itself, and
the reason is a correction to the review note rather than an omission: the diff
requires the change to EXIST, and §1.1 is what this RFC proposes to build. There
is no version of it runnable on `master` without first building it. The grep plus
the `refineRowTypes` mechanism is what replaces it, and it is stronger than a
diff would have been — a green diff would have shown that today's corpus does not
hit the hazard, whereas the mechanism shows what the hazard IS and gives the
implementation lap a precondition to build against. The diff is owed at
implementation, against the built change, and §6 books it there.

---

## 4. The crux: `legWindowSlot`, and whether `108/108` unblocks it

**It does not, and resolver-at-mint is not one capability but two — one of which
already exists and is already used on this channel.**

### 4.1 `108/108` cannot reach this reader

`legWindowSlot` (`cascades_translator.go:6215`) runs in the relational
TRANSLATOR, resolving a `logical.ColumnRef` against a flat output column list
before any plan exists. The merged-layout counter measures a property of plan
nodes inside a Cascades rule firing. There is no call path, no shared datum, and
no ordering in which one could inform the other. The universality reading is
about `foldStep1Seed`'s correlated arm; this reader never sees a `foldStep1Seed`
firing. Nothing about `108/108` moves it.

### 4.2 The ALIAS route: the correlation exists, is already resolved, and is already used here

MEASURED, `pkg/relational/core/query/semantic/resolver_at_mint_capability_test.go`:

```
$ go test ./pkg/relational/core/query/semantic/ -run TestResolverAtMint -count=1 -v
--- PASS: TestResolverAtMint_QualifiedReferenceCarriesACorrelationName (0.00s)
--- PASS: TestResolverAtMint_TableNameQualifierDoesNotResolve (0.00s)
```

The first pins that `Scope.ResolveQualifiedColumn("o", "order_id")` returns a
`ScopeSource` whose `CorrelationName` is the UPPER-folded runtime correlation key
— exactly the identity `legWindowSlot` is documented as having nothing to key
with. The identity is not missing; it is one hop upstream.

And it is not merely available in principle — **the projection channel already
calls for it**. `plan_visitor.go:1852-1865`:

```go
func resolveQualifiedBaked(resolver *expr.Resolver, ref colRef) values.Value {
	if !ref.isQualified() {
		return nil
	}
	rv, err := resolver.ResolveIdentifier(semantic.FromNormalized(ref.table), semantic.FromNormalized(ref.bare()))
	if err != nil || rv == nil {
		return nil
	}
	if fv, isFV := rv.(*values.FieldValue); isFV && fv.Child != nil && fv.RootIsLegRelativeUnpinned() {
		return fv
	}
	return nil
}
```

Its own doc comment says what happens on the other three exits: "Any resolution
error or lazy result returns nil, keeping the caller's legacy emission". That
legacy emission is the text carrier that ends at `legWindowSlot`. **So
`legWindowSlot` is not a reader waiting for a capability; it is the DECLINE
population of a resolution that already runs.** Its 102 + 4 measured calls are
the size of that decline population.

The retirement condition is therefore restated as work, not as a wait: carry the
resolved `ScopeSource.CorrelationName` on `logical.ColumnRef` (which today
carries `Bare` / `Qualifier` / `Qualified` and no identity —
`logical/operators.go:287-292`), and make the leg selection go through
`values.SameLeg` against `RecordTypeLeg.Alias`.

### 4.3 The TABLE-NAME route: a real missing capability, and what it costs

MEASURED, second test above: over `FROM users AS u`, the scope resolves
`U.NAME` and **rejects** `USERS.NAME` with `*SourceNotFoundError`. Reading
confirms why — `Scope.ResolveQualifiedColumnNested` matches a qualifier against
`src.Alias` only (`semantic/scope.go:358`), plus the struct-column rule.

The leg-layout map does answer it: `FROM PA AS "s"` registers the layout under
both `S` and the scan table name `PA`, and the corpus drives it exactly once —
`legQOVBake [matchViaTableName] qualifier "PA", leg binding "S", leg alias "S"`.

So the standing claim at `values.RecordTypeLeg.Name` — "one of the map's two key
kinds names a TABLE, and a table is not a quantifier, so the map cannot be
re-keyed by identity even in principle" — is **correct about the MAP and wrong
as a statement about the CHANNEL**. Re-keying the map is not the only move
available; resolving the qualifier before a map exists is, and the resolver is
what decides which quantifier `PA` names. It cannot do that today, and that is
the missing capability, stated precisely:

> `semantic.Scope` does not register a source's TABLE NAME as a second
> addressing route, so a reference qualified by the table name of an aliased
> source resolves to no source.

**Cost to build it**, since DFS says a missing capability is work — and the cost
is not estimated, it was executed as this RFC's mutation check. Widening the
qualifier match to `!src.Alias.EqualsIgnoreQuoting(qualifier) &&
!src.Table.Name().LeafIdentifier().EqualsIgnoreQuoting(qualifier)` is a
one-condition edit and it turns the negative pin RED immediately, which is the
measurement that the gap is a condition rather than an architecture.

**But that one line is WRONG as a design, and specifying why is this section's
real content.** Dropping a table-name match into the existing per-source loop
puts it into the SAME `matches` list that `ResolveQualifiedColumnNested`'s
per-attribute rule counts at `scope.go:365-368` — so `len(matches) > 1` becomes
`AmbiguousColumnError`. Since Java has no table-name addressing route at all, Go
is DEFINING this resolution, and a definition with unstated ambiguity cases is
not a definition. Three cases, decided:

**(a) An alias match and a table-name match at the same scope level: ALIAS WINS,
and the table-name match is not a candidate at all.** `FROM users AS u, orders AS
users` — a reference to `USERS.ID` names the source aliased `users`, not the
table `users` scanned as `u`. This is decided as a RANKED SECOND PASS, not as an
extra entry in `matches`: the level is resolved against aliases exactly as today,
and only if that yields ZERO candidates is the table-name pass run over the same
level. Putting both in one list would report an ambiguity where the alias rule
has a definite answer, which is observably different and worse — it turns a
working query into 42702.

**(b) Two sources whose table names share a leaf: AMBIGUOUS, and the reference is
unresolvable by any spelling.** `FROM s1.users AS a, s2.users AS b` — `USERS.ID`
matches both under `LeafIdentifier()`, and there is no qualifier the user can
write to disambiguate, because the route addresses the leaf only. The decision is
to raise `AmbiguousColumnError` naming both aliases and pointing at them as the
spellings that DO work, mirroring the leg-layout map's own poisoning
(`layouts[key] = nil`). The alternative — matching on the full multi-segment
qualified name so `S1.USERS.ID` disambiguates — is rejected: the leg map this
route exists to mirror registers the scan table's leaf, so a scope that resolved
more than the map answers would hand `legWindowSlot` an identity for a reference
the map cannot route, which is a new divergence in place of the one being closed.

**(c) The losing table-name match under (a) does NOT poison.** `FROM users AS
orders, orders AS x` — alias-wins settles `ORDERS.ID` on the second source, and
the first source's table-name candidate is never constructed, because the
table-name pass only runs when the alias pass found nothing at that level. This
falls out of (a) being a ranked pass rather than a merged list, which is the
main reason to prefer that structure.

All three are consequences of one decision — **ranked second pass, leaf-keyed,
poisoning only within the pass** — and each is separately checkable. What the
one-line mutation does NOT yet handle is any of (a), (b) or (c) — it merges the
table-name candidate into `matches` and therefore gets (a) and (c) wrong. The
build is the ranked second pass plus a test per case. Both directions of the
capability's PRESENCE are already pinned by the two tests this RFC adds — the
negative one carries the failure message naming what gets re-armed if the scope
ever starts answering. Java does not settle any of the three: it resolves
qualified identifiers through `SemanticAnalyzer` against quantifier aliases and
has no table-name addressing route at all, so this is a Go-side read-surface
extension and the definition is ours to make and to state.

**This is not day-one scope.** It is one call over the corpus, and it sits
behind §4.2, which is the part that moves 102 of 106.

---

## 5. Rejected alternatives

**A. "Convert the mint at `:3667` by ordinal, as CQ-95 is worded."** Loses on
measurement twice over. The mint takes no layout parameter and its ordinal twin
takes two because it needs them; three of its five callers sit in an explicit
`!seedWindowed` else-branch and two apply no seed test at all
(`cascades_translator.go:3059` is the else of the chained-unnest check,
`:3400` is the anchored arm the code itself calls "the correct and now ONLY
domain of the name-keyed rebase"). And the payoff it is booked for — reader
one's retirement — is not produced by it at all (§3.3). CQ-79 already proved the
first half; this RFC proves the second, and adds a third: over the corpus the
mint produces zero names, so there is nothing there to convert (§3.2).

**B. "Lift `unnestExistsSeedSafe`'s scope gate and let the three seed-tested
sites convert."** Loses first on measurement — those three branch points are
reached 208 times and take the name arm ZERO times (§3.2), so the seed over them
is already windowed and the lift has nothing to convert — and then on safety: the
gate is not a feature flag. Every decline
arm in it carries an independent correctness reason stated at the site — a
LEFT/RIGHT box whose `clusterArity >= 2` can never ordinalize, a box-leg WHERE
conjunct with no merge window in the binary seed, an EXISTS scope collision —
and lifting it wholesale ordinalizes a chained link over a name-model seed,
which the code annotates as "silently wrong rows"
(`cascades_translator.go:1400-1405`). It converts when CQ-68's executor widening
lands, per arm, which is what the item already books.

**C. "Re-key `legWindowSlot`'s leg walk by identity."** Loses on the reader
having no identity — and that is a real objection, just not a permanent one.
Rejected as a LOCAL edit and adopted as a PRODUCER edit in §4.2: the identity
exists one hop up and is currently discarded on a decline path.

**D. "Mint a `CorrelationIdentifier` from the qualifier text at the reader."**
Loses outright, and the census exists to say why: the alias namespaces are
deliberately case-DISJOINT (user aliases upper-folded at the scope's
registration chokepoint, `UniqueCorrelationIdentifier` minting lowercase), so a
text→identifier mint is a forgery. The dotted-leg census's `MATCH-ALIAS-DIFFERS`
hard zero is the standing guard against it.

**E. "Wait for CQ-68 and do all of it in one lap."** Loses because §1.1 is
buildable on `master` today and is additive — `RecordType.Legs` is ignored by
`Equals` and `Hash` — so bundling it behind an executor widening trades a
shippable retirement for a schedule. It is also the half with a measured
acceptance signal (`dotted hits → 0`), and holding it hostage to the half whose
acceptance signal was refuted is the wrong way round.

**F. "File the refutations as TODO corrections and implement CQ-95 as written."**
Loses because it ships the wrong work. The refuted acceptance condition would
have gone green by coincidence or not at all, and either outcome teaches the next
reader nothing.

---

## 6. Day-one scope

Only §1.1 and the instruments. Explicitly NOT in scope: any change to
`rebaseUnnestOuterLegPredicate`'s behaviour, to `unnestExistsSeedSafe`, to
`legWindowSlot`, or to `semantic.Scope`'s qualifier matching.

1. `clustered_outer_scalar.go`'s derived seed type carries `RecordType.Legs`
   built from the `leg.start`/width the seed's own values already state, so
   `rowSlotForLegColumn` resolves the two live hits through a leg WINDOW rather
   than by splitting a label. Under two stated preconditions:
   - it is applied at the seed's own type derivation, NOT at
     `RecordConstructorValue.Type()`, which serves every record constructor in
     the planner;
   - it is applied CONSISTENTLY across every producer of that row, because
     `refineRowTypes` declines a populated table against an empty one (§3.4).
     The implementation lap owes the plan-shape and golden diff that checks it —
     that diff is not runnable before the change exists, which is why it is owed
     here rather than presented here.
2. The census family gains the unnest leg-mint census (landed with this RFC) and
   keeps its disjointness assertion as the standing guard on §3.3, its ARM
   sub-report as the guard on §3.2's readability, and the docscheck AST pin as
   the guard on both counters' placement.

---

## 7. Retirement condition for each of `Name`'s two deciding readers

Restated so each is checkable rather than aspirational, and each names its
instrument.

**Reader one — `executor.rowSlotForLegColumn`'s dotted arm.** Retires when the
correlated-scalar seed's derived type carries its leg table and the leg-column
provenance census reports `dotted HITS by identity availability: available 0`
over a full corpus run with `Calls` still above its floor. NOT when the unnest
mint converts — the unnest leg-mint census's disjointness assertion is what
keeps that distinction from rotting back.

**Reader two — `query.legWindowSlot`.** Retires in two parts, both instrumented
by `AssertDottedLegQualifierCensus`:

- `flatColumnBake` and the `matchAliasIsQualifier` share of `legQOVBake` retire
  when `logical.ColumnRef` carries the resolved correlation and the selection
  goes through `values.SameLeg`. Measured population today: 98 + 3.
- the `matchViaTableName` share retires only after `semantic.Scope` registers
  the table-name addressing route (§4.3). Measured population today: 1. Until
  then, `legWindowSlot` cannot be deleted, only narrowed — and narrowing it to a
  one-call channel is itself the signal that the remainder is a scope
  capability, not a translator debt.

`values.RecordTypeLeg.Name` goes when the later of the two does. Until then, a
new comparison against `Name` is a regression, unchanged.

---

## 8. Test plan

Each step names what pins it and what a failure re-arms.

| Step | Pin | Mutation that must go RED |
| --- | --- | --- |
| §0(1) — the merged-layout reading is not on `master` and its own doc reads the other way | recorded here with the `git grep` that produced it; no code pin is possible for a fact about another branch | n/a — stated as unmeasurable on this tree, deliberately |
| §3.2 — the census call precedes every return, so its per-site counts measure the population its report names | `docscheck.TestCensusReachedCallPrecedesEveryReturn`, an AST pin over both `rebaseUnnestOuterLegPredicate`/`RecordUnnestLegMintCall` and `rebaseUnnestOuterLegPredicateOrdinal`/`RecordUnnestLegOrdinalTwinCall` | move either counter below its inert guard — the pin names the offending return's position; `//pkg/relational/core/query:query_test` stays GREEN under the same mutation, which is why this pin has to live in docscheck. Deleting the call entirely is a second RED direction |
| §3.2 — a name-arm zero is readable, i.e. "never reached" is separable from "reached, converted on the other arm" | the ARM sub-report; `TestUnnestLegMintCensus_UnreachedBranchSaysSo` and `_ArmsRenderApart` drive the renderer through its pure entry point | collapse the `BRANCH POINT NEVER REACHED` line into a row of zeros — the two readings print identically, which is the ambiguity that produced this RFC's first (wrong) §3.2 |
| §3.2 — the twin's denominator is independent of the arms | `TestUnnestLegMintCensus_UnreachedBranchSaysSo` asserts the twin total renders | sum the arms instead — true by construction, and blind to the twin's fourth caller (91 of its 133 calls) |
| §3.2 — the two unconditional sites do not print a structural zero as a reading | `NO SEED BRANCH EXISTS`, keyed off `hasSeedBranch` | let them fall through to the zero row — a tautology then reads as a measurement |
| §3.4 — populating a leg table is NOT behaviour-neutral | `expressions.TestLegTablePopulation_{EmptyBothSidesRefines,PopulatedAgainstEmptyIsAConflict,DifferentTablesSameFieldsConflict}`; the middle one also asserts the two types ARE `Equals`, which is the premise the additive claim rested on | delete `legTablesAgree` from `refineRowTypes` — two of the three go RED, and the "additive" claim silently becomes true again |
| §3.2 — an empty mint population is announced, not passed over | `TestUnnestLegMintCensus_EmptyPopulationsDoNotFail` asserts the `VACUOUS` line | drop the announcement — the check passes silently and reads as "the sets do not meet" |
| §3.3 — the mint's names and the executor reader's names are disjoint | `AssertUnnestLegMintCensus`, wired into the sqldriver `TestMain`; `TestUnnestLegMintCensus_*` drive both directions and the case fold | feed the assertion an overlapping pair — `TestUnnestLegMintCensus_OverlapFailsAndSaysWhatItReArms` fails if it passes |
| §4.2 — the resolver already knows the correlation | `TestResolverAtMint_QualifiedReferenceCarriesACorrelationName` | blank `ScopeSource.CorrelationName` at the registration chokepoint |
| §4.3 — the scope rejects the table-name route | `TestResolverAtMint_TableNameQualifierDoesNotResolve` | add a table-name arm to `ResolveQualifiedColumnNested` — the test fails, which is exactly the signal that §4.3's capability landed and reader two's second part is unblocked |
| §6.1 — reader one resolves by window | the leg-column provenance census's `available` count reaching 0 with `Calls` above floor, plus a targeted executor test asserting the seed's derived type states its legs | strip `RecordType.Legs` from the derived type — the dotted arm answers again and `available` returns to 2 |
| §6.1 — the leg table is populated CONSISTENTLY by every producer of that row | owed at implementation: the plan-shape/golden diff §3.4 books, with `refineRowTypes` as the mechanism it is checking | populate the table at one producer only — a memo sibling still derives it empty and the refinement declines |

The instruments in this RFC are committed with it. The two probes that
established §4.2 and §4.3 are committed as tests rather than deleted, including
the NEGATIVE one: "the scope rejects the table-name route" is what makes
`matchViaTableName` a capability gap rather than a mystery, and nothing else
pinned it.

---

## 9. What this RFC does not measure

- **The per-site reach of the five mint callers over anything wider than the
  sqldriver corpus.** The census is wired into that harness only. Nothing here
  is a deletion warrant, and after the ARM reading the question is not even close:
  the three seed-tested branch points are reached 208 times on this corpus alone.
  A deletion warrant on this path means a panic probe reached by nothing across
  `./pkg/relational/...` AND `./pkg/recordlayer/query/...`, and it would have to
  cover the two unconditional sites' 28 calls as well.
- **Whether §1.1 changes any plan.** NOT because the question is open in
  principle — §3.4 identifies the mechanism by which it can (`refineRowTypes`
  declines a populated-vs-empty leg table) and pins it. What is missing is the
  corpus diff, and it is missing because §1.1 does not exist yet: there is no
  version of it runnable on `master` before it is built. The implementation lap
  owes a before/after plan-shape and golden comparison against the built change,
  with §3.4's consistency precondition as the thing it is checking.
- **Anything about the field-name ratchet's debt entries.** Threading the site
  parameter and the arm records moved all eleven `cascades_translator.go` line
  keys in `pkg/docscheck/field_name_decision_test.go`, including CQ-95's own debt
  entry (`:3667` → `:3705`). Those are mechanical re-keys of an unchanged debt
  list — nothing retired, nothing added, count unchanged. The ratchet's
  line-number keying is why they had to move twice during this RFC; that is a
  property of the ratchet, not a finding here.

- **The `noMatch 4` share of `flatColumnBake`.** Four calls resolve no leg and
  fall through unbaked; whether those are references that should have resolved is
  a separate question this RFC does not open.
