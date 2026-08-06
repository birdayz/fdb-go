# RFC-212 — The name-model seed is not what feeds either dotted reader

Status: **PROPOSED** — awaiting Graefe + Torvalds ACK. This is CQ-95's RFC.

Closes nothing on its own. It **re-derives CQ-95's acceptance conditions against
measurement**, splits the item along the seam the measurement found, and states
the day-one build.

---

## 0. Lead: four things in CQ-95's framing are refuted by measurement

Stated first because everything below depends on them, and because two of the
three were carried into the item as settled.

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

**(3) The mint CQ-95 converts MINTS NOTHING, and the three sites a seed-gate
flip would convert are DARK.** Measured (§3.2): over the whole real-FDB
sqldriver corpus — which does drive unnest-EXISTS shapes, three dedicated FDB
test files' worth — `rebaseUnnestOuterLegPredicate` is reached 28 times and
produces ZERO qualified names, and all 28 land on the two sites that apply no
seed test. `buried(!seedWindowed)`, `joinPredicate(!seedWindowed)` and
`chained(name-model seed)` measure 0. So the conversion CQ-95 describes moves
arms nothing reaches, to stop a channel from carrying names it does not carry.

**(4) `legWindowSlot` is NOT blocked on a missing capability.** The "resolver at
mint" it is documented as waiting for already exists, is already used on this
exact channel, and `legWindowSlot` serves that resolution's DECLINE population.
§4 measures this. It is blocked for ONE key kind — the table-name addressing
route — and that is a small thing to build, not a wall.

Taken together: **CQ-95 is not a STOP, but it is not the work either.** Its
subject is an unreached arm; its acceptance signal belongs to a different
producer; and its one genuinely blocked reader is blocked on something small and
nameable rather than on the executor widening it is booked behind.

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
   table, throwing the window away. Restore it: have the derived type carry
   `RecordType.Legs` built from the same `leg.start`/width the values already
   state. `RecordType.Legs` is layout metadata that `Equals` and `Hash` ignore,
   so this moves no name, no label and no type identity. **Day-one scope.**

2. **The seed conversion (CQ-79's residue, `rebaseUnnestOuterLegPredicate`) is
   DEMOTED, not scheduled.** It stays gated on CQ-68, but its priority is now a
   measured zero: the three sites a lifted seed gate would convert are dark, the
   two live sites take no seed test, and the mint produces no names. CQ-95's
   `DONE` list — "the seeds those five call sites sit over are ORDINAL, the
   name-keyed rebase is DELETED, `MergedReAnchor` stops being vacuous, dotted
   hits go to 0" — is four conditions of which one is now measured unreachable
   from here (§3.3) and two are conditions on unreached code. Nothing in this
   RFC converts or deletes it; §9 says what a deletion would owe.

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

`fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades/rules/PartitionSelectRule.java:284-294`:

```java
final ImmutableList<Column<? extends Value>> lowerResultColumns =
        lowersCorrelatedToByUppers.stream()
                .map(lowerAlias -> Verify.verifyNotNull(aliasToQuantifierMap.get(lowerAlias)))
                .map(quantifier -> (Value)QuantifiedObjectValue.of(quantifier))
                .map(Column::unnamedOf)
                .collect(ImmutableList.toImmutableList());
final var joinedResultValue = RecordConstructorValue.ofColumns(lowerResultColumns);
```

and the seed itself, `PartitionSelectRule.java:296-304`:

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
  (`Type.java:2918-2920`); asking for it throws (`Type.java:2763-2765`). A
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

An exhaustive search of `fdb-record-layer-core/src/main/java` for alias-with-dot
concatenation in the planner returned exactly one hit, and it is
`FieldValue.FieldPath.toString` (`FieldValue.java:427-433`) — whose fallback for
an unnamed accessor is `"#" + ordinal`. Even the debug rendering treats the
ordinal as primitive. The nearest qualified name in the whole product is
`Identifier.fullyQualifiedName()`
(`fdb-relational-core/.../query/Identifier.java:108-115`), a `List<String>`
path in the SQL semantic layer that is resolved to a `Value` tree and gone
before Cascades runs.

**This settles the target and it settles the direction of every choice below.**
It does not, by itself, say which Go edit gets there — that is what §3 and §4
measure.

---

## 3. Measurement: reader one's producer is not the mint CQ-95 names

### 3.1 The corpus reading, `master` at `6692ac268`, uncached

```
$ git rev-parse --abbrev-ref HEAD
master
$ bazelisk test //pkg/relational/sqldriver:sqldriver_test \
    --cache_test_results=no --test_output=streamed --test_timeout=3600
```

Exit 0. Verbatim from the harness's census block:

```
[sqldriver real-FDB corpus] leg-column provenance: calls 2294 (flatHit 120, notDotted 2172, noLegs 0, dottedMiss 0); dotted HITS by identity availability: available 2, unstated 0, diverged 0
  dotted HITS by OWNER selection: sameLeg 0, ownerUnstated 0, ownerNamesNoLeg 2, ownerSelectsOtherLeg 0
  distinct witnesses (4, cap 128):
    DOTTED-HIT "C.CV": leg "C", alias stated and equal
    DOTTED-HIT "I.QTY": leg "I", alias stated and equal
    OWNER-NO-LEG "C.CV": owner "q$3122" names no leg of [C E]
    OWNER-NO-LEG "I.QTY": owner "q$236051" names no leg of [O I]
```

```
[sqldriver real-FDB corpus] translator dotted leg qualifier (per match attempt):
  flatColumnBake     calls 102 | matchAliasIsQualifier 98 | noMatch 4
  legQOVBake         calls 4 | matchAliasIsQualifier 3 | matchViaTableName 1
```

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
  distinct minted names: NONE
UNNEST LEG-MINT CENSUS VACUOUS: the name-keyed unnest rebase minted
  NO qualified names over this run, so the disjointness check against the
  executor's 2 dotted-hit name(s) did not compare anything.
  The per-site call counts above are what distinguishes a DARK arm (never
  reached) from a LIVE one carrying no rewrite; read them before quoting this
  census as evidence about either.
```

Read it in three parts.

- **The two sites with NO seed test are LIVE** — 28 calls between them. These are
  `cascades_translator.go:3059` (the else of the chained-unnest check) and
  `:3400` (the anchored arm the code calls "the correct and now ONLY domain of
  the name-keyed rebase"). No seed-gate flip converts them; CQ-79 said so and
  this measures it.
- **The three seed-tested sites are DARK.** `buried(!seedWindowed)`,
  `joinPredicate(!seedWindowed)` and `chained(name-model seed)` are exactly the
  arms a lifted `unnestExistsSeedSafe` would convert, and the corpus never
  reaches them. The conversion CQ-95 books has no measured subject.
- **Nothing is minted at all.** 28 live calls, zero qualified names — every one
  arrives with an empty outer-leg set or a nil predicate. The channel this mint
  is supposed to feed is carrying no name.

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
measurement that the gap is a condition rather than an architecture. What that
one line does NOT yet handle is the shape the full build owes: one arm in
`ResolveQualifiedColumnNested` that also matches `src.Table.Name()`'s last
segment when no alias matched at that level, ranked strictly BELOW the alias
match so an alias always wins; plus the ambiguity case (two sources scanning one
table under the table-name route) which must poison rather than first-match, to
match what the leg-layout map already does. Both directions are pinned by the
two tests this RFC adds — the negative one carries the failure message naming
what gets re-armed if the scope ever starts answering. Java does not settle this
one: it resolves qualified identifiers through `SemanticAnalyzer` against
quantifier aliases and has no table-name addressing route, so the route is a
Go-side read-surface extension and the capability is ours to define.

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
sites convert."** Loses first on measurement — those three sites are DARK
(§3.2), so the lift converts nothing observable — and then on safety: the gate is
not a feature flag. Every decline
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
   than by splitting a label.
2. The census family gains the unnest leg-mint census (landed with this RFC) and
   keeps its disjointness assertion as the standing guard on §3.3.

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
| §3.2 — the mint's per-site reach, DARK separated from live-but-inert | the per-site call/mint report, counted before the inert guard; `TestUnnestLegMintCensus_SitesRenderApart` keeps the sites from merging in the report | move the call counter back after the guard — all five sites read 0 again and the DARK/inert distinction disappears, which is the reading this RFC's first pass took |
| §3.2 — an empty mint population is announced, not passed over | `TestUnnestLegMintCensus_EmptyPopulationsDoNotFail` asserts the `VACUOUS` line | drop the announcement — the check passes silently and reads as "the sets do not meet" |
| §3.3 — the mint's names and the executor reader's names are disjoint | `AssertUnnestLegMintCensus`, wired into the sqldriver `TestMain`; `TestUnnestLegMintCensus_*` drive both directions and the case fold | feed the assertion an overlapping pair — `TestUnnestLegMintCensus_OverlapFailsAndSaysWhatItReArms` fails if it passes |
| §4.2 — the resolver already knows the correlation | `TestResolverAtMint_QualifiedReferenceCarriesACorrelationName` | blank `ScopeSource.CorrelationName` at the registration chokepoint |
| §4.3 — the scope rejects the table-name route | `TestResolverAtMint_TableNameQualifierDoesNotResolve` | add a table-name arm to `ResolveQualifiedColumnNested` — the test fails, which is exactly the signal that §4.3's capability landed and reader two's second part is unblocked |
| §6.1 — reader one resolves by window | the leg-column provenance census's `available` count reaching 0 with `Calls` above floor, plus a targeted executor test asserting the seed's derived type states its legs | strip `RecordType.Legs` from the derived type — the dotted arm answers again and `available` returns to 2 |

The instruments in this RFC are committed with it. The two probes that
established §4.2 and §4.3 are committed as tests rather than deleted, including
the NEGATIVE one: "the scope rejects the table-name route" is what makes
`matchViaTableName` a capability gap rather than a mystery, and nothing else
pinned it.

---

## 9. What this RFC does not measure

- **The per-site reach of the five mint callers over anything wider than the
  sqldriver corpus.** The census is wired into that harness only. The three DARK
  sites are unmeasured outside it, **not proven dead** — this RFC does not carry
  a deletion warrant for them, which on this path means a panic probe reached by
  nothing across `./pkg/relational/...` AND `./pkg/recordlayer/query/...`. That
  warrant is the first thing the implementation lap should collect if deletion is
  on the table; nothing here authorises it.
- **Whether §6.1 changes any plan.** `RecordType.Legs` is ignored by `Equals`
  and `Hash`, which is an argument that it CANNOT, not a measurement that it
  does not. The implementation lap owes a before/after golden comparison.
- **Anything about the field-name ratchet's debt entries.** Threading the site
  parameter moved eleven `cascades_translator.go` line keys in
  `pkg/docscheck/field_name_decision_test.go`, including CQ-95's own debt entry
  (`:3667` → `:3682`). Those are mechanical re-keys of an unchanged debt list —
  nothing retired, nothing was added, and the ratchet count is unchanged.

- **The `noMatch 4` share of `flatColumnBake`.** Four calls resolve no leg and
  fall through unbaked; whether those are references that should have resolved is
  a separate question this RFC does not open.
