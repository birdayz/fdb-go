# RFC-200 — A merged leg is a NESTED row, and the layout authority must say so

Status: **proposed**, revision 2 — awaiting the query-engine joint-review ACK
(Graefe + Torvalds) before implementation starts. Revision 1 was NAK'd by both,
with the core design endorsed and six blockers between them. The material
changes:

- **Step 3c was not behaviour-neutral.** Revision 1 widened the SHARED accept
  boundary of `values.OrdinalSeedLegWindows`, whose consumers are mostly
  nil/non-nil predicates — and at a nil/non-nil predicate, population IS meaning.
  The nested kind now gets its OWN entry point, opted into by exactly three
  sites. Revision 1's sentence "changes *population* without changing *meaning*"
  was the sentence that hid this; it is deleted, and its claim is now discharged
  per consumer by the entry point rather than asserted.
- **`materializedNLJOrdinalLayoutMatches`' `len(windows) != 2 → return true` is
  a FAIL-OPEN**, not "the safe default" as revision 1 called it. It is fixed
  here, not merely measured — and the fail-open is pre-existing, not introduced.
- **The arithmetic was wrong.** `174` and `60/94/108` come from different
  instruments with different denominators and do not sum. Every population
  statement now names its instrument, and the mapping between the two becomes a
  measured deliverable.
- **§5's predictions are promoted to gates** with exact equalities.
- **The 94 residue was investigated under a coordinator ruling and FENCED**, with
  the evidence recorded. Revision 1's "largest addressable block" claim was
  wrong: 94 > 60, and this closes the SECOND-largest block.

Closes: the `legIsOrdinalSafe` half of CQ-53 phase 3; advances the
`DIVERGENCES.md` retirement condition for the runtime binding-namespace widening
(`executor.bindMergedOuterLegs`).

Corpus figures are one run of the whole real-FDB sqldriver corpus at
`f50cee43e`, recorded in TODO.md's CQ-53 PHASE 3 block (measured on branch
`feat/cq53-parent-chained-binder`, HEAD `4dccc50f0`). Line citations are verified
at `4dccc50f0` for `rule_implement_nested_loop_join.go` (the one cited file that
moved between the two) and at `f50cee43e` for everything else. Where a count here
and a census disagree, the census is right and this file is stale.

## The defect, measured — with each number's instrument named

Revision 1 stated these figures as if they shared a denominator. **They do not,
and the correction is load-bearing rather than cosmetic**: it is the difference
between "this closes 60 of 174" (false, and the basis of revision 1's gate (d))
and "this closes 60 of 154 firings of one instrument, whose relation to the 174
reads of a different instrument is UNMEASURED" (true).

**Instrument A — `cascades.LegLocalBakeCensus`, denominated in READS.** `Total`
counts executions of `rebaseOuterLegValue`'s leg-match arm
(`rule_implement_nested_loop_join.go:2760-2913`), which runs once per
leg-correlated `FieldValue` the walk matches — a per-reference count, not a
per-firing one. It reports:

```
total 174 (baked 174, mergedReAnchor 0, declined 0)
```

**All 174 leg-correlated reads take the leg-alias pass-through** (ARM 2,
`:2899-2903`) — the arm that consumes the Go-only runtime namespace widening this
line of work exists to retire. `MergedReAnchor` (ARM 1, `:2883-2892`) is `0`.

**Instrument B — the `foldStep1Seed` decline probe, denominated in RULE
FIRINGS.** One record per invocation of `foldStep1Seed` from
`implementJoinWithExistential` (`:3646`). **This probe does not exist as a
standing instrument** — TODO.md describes it as "a call-site probe added for this
question and removed after". Its one recorded run:

```
DECLINE correlatedStep1                                    108
DECLINE reconstruct nil (left=FlatMap right=Scan)           77   } 154
DECLINE reconstruct nil (left=Scan  right=FlatMap)          77   } combined
DECLINE rv does not reference exist alias                  200
ACCEPT                                                      78
                                                    total  540
```

and, splitting the 154 by what the declined FlatMap leg's result value is:

```
rv=*values.QuantifiedObjectValue        x94   (identity pass-through)
rv=RC(2) [QOV-record:2]                 x60   (a nested UNNAMED merge)
```

**`60 + 94 + 108 = 262 ≠ 174`.** The two instruments count different events at
different sites. How many of instrument A's 174 reads belong to firings that
instrument B classifies as `reconstruct nil / positional-merge` is **not
measured**, and revision 1's gate (d) — which apportioned the 174 into
`60 / 94 / 108` — was arithmetic over a category error. Measuring that mapping is
now an explicit deliverable of sequencing step 3a, and gate (d) is denominated in
it.

**The 108 `correlatedStep1` declines are PERMANENT and OUT OF SCOPE**, by a prior
architecture ruling (TODO.md:6229-6249): a WHERE-EXISTS correlating into a leg
buried in an inner join is the canonical semijoin, its good plan is a SARG'd
correlated index scan under the FlatMap, and that *requires* name binding to flow
the sibling comparand into the index. Java binds every correlation by name and
has no positional-correlation concept. This RFC does not touch it, does not
shrink it, and treats movement in it as a regression (§7).

**The 200 `rv does not reference exist alias` firings are correct
pass-throughs**, not a residue: `foldStep1Seed`'s condition (2) (`:2270`)
declining a WHERE-EXISTS whose projection sits *above* the existential level.
There is nothing to fold, and folding is what the seed is for.

**The finding is what the 154 declined legs flow.** `reconstructFoldStep1Seed`
returns nil (`:2077-2079`) because `legIsOrdinalSafe` (`:1956-1986`) has no case
for `*plans.RecordQueryFlatMapPlan` and falls through to `default:` / `return
false` at `:1981-1982`. **The 60 are Java's own merged row, built by Go, and
declined by Go's own layout authority.** `RC(_i: QOV(leg_i))` is
`PartitionSelectRule.java:283-291`'s
`RecordConstructorValue.ofColumns(… Column::unnamedOf)` verbatim — one unnamed
column per collapsed lower quantifier, each holding that quantifier's *whole*
row — and Go builds it 1:1 in `positional_merge.go`'s `positionalMergeCase`
(`:122-125`, names minted as `values.OrdinalFieldName(i)`, the `_i` spelling
`Type.java:2922 isAutoGenerated` describes). `values.OrdinalSeedLegWindows` names
"positional-merge" as a declined class in its own doc
(`ordinal_seed_layout.go:58`) and the decline fires at `:129-132`: a bare QOV
field is not a `*FieldValue`, so the walk bails.

With `reconstructFoldStep1Seed` nil, `ordinalSeedLegWindowsOf(step1RV)` is nil at
`:3759` and all three rebase sites take the lazy path with `legLayout=nil`
(`:3796`, `:3847`, `:3909`).

## Why this is not a missing capability

Java needs no window machinery at all, and the reason is the whole design.

Java's row is a protobuf `Message` tree. `RecordConstructorValue.eval`
(`RecordConstructorValue.java:113-140`) sets **one proto field per child**, whole
(`:124`, `:133`) — leg *i*'s entire record Message goes into slot *i*. A
reference to a collapsed sibling is rewritten by `PartitionSelectRule` to
`FieldValue.ofOrdinalNumber(QuantifiedObjectValue.of(newUpperQuantifier), index)`
(`PartitionSelectRule.java:301-302`), replayed over the predicates (`:310`) and
the result value (`:315`). Evaluating that is a binding lookup
(`QuantifiedObjectValue.java:82-95` — `context.getBinding(CORRELATION, alias)`,
returning `binding.getMessage()` for a record) followed by an ordinal walk
(`FieldValue.java:163-174` → `MessageHelpers.getFieldValueForFieldOrdinals`,
`MessageHelpers.java:93-107`), which descends intermediate steps as sub-Messages
and returns the final field — a whole Message when that field is of message type.
**Nesting is free; a one-step path over the merge returns the leg's whole
record.**

Composition is a path FUSION, never a flattening. When `translateCorrelations`
rebuilds an enclosing `FieldValue(QOV(lower), [f])`, the leaf swap produces
`FieldValue(FieldValue(QOV(upper),[i]), [f])`, and `ValueWithChild.withChildren`
→ `FieldValue.withNewChild` (`FieldValue.java:159-161`) →
`ofFieldsAndFuseIfPossible` (`:326-332`) merges the two accessor lists via
`FieldPath.withSuffix` (`:525-534`) into the single two-step path
`FieldValue(QOV(upper), [i, f])`. Nothing is materialized, nothing is re-offset.

The re-anchored accessor carries **no name**: `FieldValue.ofOrdinalNumber`
(`:335-338`) builds `new Accessor(null, ordinalNumber)` and resolves it through
`resolveFieldPath`, which manufactures the `ResolvedAccessor` at `:297` over the
merge column's `Field.unnamedOf` (`Type.java:2918-2919`), whose
`ResolvedAccessor.getName()` is `null` (`FieldValue.java:657-659`).

**The negative was checked, not assumed.** The only row-flattener anywhere in
Java's cascades tree is `GroupByExpression.flattenedResults`
(`GroupByExpression.java:764-796`), the deliberate counterpart to `nestedResults`
(`:753-761`), and it flattens the `(groupingKey, aggregate)` pair for aggregate
index matching. It is never applied to a `PartitionSelectRule` merge. Java's
merged row stays nested, always.

**Go's executor already speaks this shape at runtime.** This is the load-bearing
evidence behind the decision, and it is entirely present in `f50cee43e`:

- `newOrdinalJoinBuild` (`executor/ordinal_join.go:1176-1272`) *enables* on
  `values.IsPositionalMergeRC` (`:1184`) — a second trigger beside
  `ContainsBakedOrdinal`, added precisely because "the lowest merge level carries
  no baked refs at all, but its rows must build positional: the level above reads
  them by ordinal";
- it types each bare-QOV field from the leg's own QOV (`:1243-1261`) and derives
  the built row type through `rcOutputType` (`:986-1030`), which emits **one slot
  per RC field** and — because its leg-table loop requires a `*FieldValue`
  (`:1003-1007`) — emits a **leg table of length 0** for this shape;
- `WindowsOK` is **false** for it: `ordinalJoinSpans` declines the positional
  merge on both arms, and `unnestMixedSeedSpans` says so in its own doc
  (`:89-92`, "the positional-merge RC (whose bare QOVs are over RECORD leg
  types)");
- at evaluation each `_i` field is a bare QOV whose binding is the leg's adapted
  `OrdinalRow` (`evaluateBound` `:1581-1586`), so **slot `i` of the built
  `PositionalRow` holds the leg's whole row object**;
- reading into it already works: `FieldValue.descendResolvedPath`
  (`values/values.go:825-872`) has an explicit `case OrdinalRow` arm (`:844-849`)
  that descends a nested step **by ordinal**;
- and the executor's *span* side already resolves the fused two-step address:
  `resolveSpanLeaf` (`executor/ordinal_join.go:301-349`) descends `legRVs` — "the
  positional-merge RC mapping `_i` → `QOV(leg)`" — for a multi-accessor pinned
  path, composing accessor lists at `:343-345`, which is
  `ofFieldsAndFuseIfPossible` by another name.

Go also has the planner-side primitives: `FieldPath.WithSuffix`
(`values/values.go:502-522`, documented as Java's `FieldPath.withSuffix`) and
`NewFieldValueOfOrdinal` (`:1347`, documented as `FieldValue.ofOrdinalNumber`),
which over a merge QOV whose field *i* is a leg `*RecordType` yields a node typed
with that leg's record type — exactly the child a suffix fuses onto.

So nested addressing is the shape Go's runtime already produces and already
reads. **The single thing that cannot express it is the planner's layout
authority**, whose `OrdinalSeedLegWindow.Offset` means "offset into a flat
concatenated row" and nothing else.

## Decision

### 1. The layout authority learns a NESTED window kind.

Two acceptances are added, and they compose.

**(a) The merge row read directly.** Given an RC satisfying
`values.IsPositionalMergeRC` (`values/values.go:643-663` — every field
auto-generated-named `_i` in position order, every value a bare QOV of a
*distinct* quantifier), the derivation returns one window per slot:

```
OrdinalSeedLegWindow{Kind: nested, Offset: i, Typ: leg_i's *RecordType, Alias: leg_i}
```

`Offset` is the FIELD INDEX of the slot holding the whole leg row, not the first
column of a run.

**`IsPositionalMergeRC` is tested BEFORE the per-field walk, and that ordering is
load-bearing.** The per-field walk tests `isMixedSeedElement(f)` FIRST, at
`ordinal_seed_layout.go:103`, and an *untyped* slot is not a `*RecordType` so
`IsMixedSeedElementType` returns true for it. A merge row with untyped slots —
which the corpus has, see below — would therefore have those slots claimed as
scalar elements before any merge recognition could run. The whole-RC recognizer
must precede the loop.

**The per-slot record test BINDS the type and requires non-nil:**

```go
legType, isRT := qov.Type().(*RecordType)   // binds — permitted by the
                                            // single-authority ban, which
                                            // forbids DISCARDED assertions
if !isRT || legType == nil { … not a nested leg … }
```

Routing this through `IsMixedSeedElementType` instead would be a trap:
`IsMixedSeedElementType(nil)` returns `false` (`:407-409`), so a slot whose QOV
states a `nil` type would be classified NOT-an-element, i.e. nested, i.e. a
window with `Typ: nil` — which panics at the first `w.Typ.FieldIndex(…)` in
readers #2 and #5. The assertion is needed for `Typ` anyway; binding it is both
the correct test and the value the window carries.

**Mixed merge rows are real.** `positional_merge.go:190-202` measures 18246 merge
slots over the corpus: 17492 typed by the quantifier, 4 scavenged, **750 stating
nothing at all**, every distinct witness an unnest ELEMENT alias (an array
element type Go does not infer). A non-record slot therefore keeps the existing
element treatment — a synthesized 1-field window, `Kind: flatRun`, width 1,
`Offset + 0`, numerically identical to today.

**(b) The step-1 seed built over such a leg.** `reconstructFoldStep1Seed` emits
`ofOrdinal(QOV(leg, rt), j)` per slot `j` of the leg's row. When `rt` is the
merge's nested type, slot `j` of that run holds sub-leg *j*'s whole record — so
the *top-level* run walk (`ordinal_seed_layout.go:129-166`) accepts it unchanged
(every field a frontier-pinned single-accessor `FieldValue` over a leg QOV,
consecutive, full coverage). What changes is `finalizeSeedWindows` (`:196-364`):
today it derives buried sub-windows from `w.Typ.Legs` as flat sub-ranges
`[Start, Start+Width)` (`:288-315`). A nested sub-leg occupies exactly one slot
and a read into it must DESCEND, so it emits

```
OrdinalSeedLegWindow{Kind: nested, Offset: w.Offset + leg.Start, Typ: <extracted>, Alias: leg.Alias}
```

**`Typ` is an EXTRACTION, not a slice.** Concretely
`w.Typ.Fields[leg.Start].FieldType` asserted to `*RecordType` (bound, non-nil, as
above) — **not** the `sub := make([]Field, end-leg.Start)` copy the flat arm
builds at `:295-298`. This is not stylistic. Keyed reader #1 bounds the leg-local
ordinal against `w.Typ` before adding the offset
(`left_outer_existential.go:160-163`); if `Typ` were a one-field wrapper record
describing "the slot", every leg-local ordinal ≥ 1 would be declined and every
ordinal 0 would resolve against the wrapper — a silent wrong-column read on
exactly the shape the check exists to catch. `Typ` must be the LEG's own record
type so the bound is the leg's real width.

**`RecordTypeLeg.Width` for a nested leg is `1`, and this is decided rather than
left implicit.** `Width` is documented as "its column count"
(`values/type.go:501`), but every consumer computes `Start + Width` as a SLOT
RANGE into the carrying type's `Fields`: `flat_map_cursor.go:520`,
`executor/ordinal_join.go:710`, `:839`, `:1722`, `executor.go:2794-2799`,
`merged_leg_binding_census.go:296-298`,
`rule_implement_nested_loop_join.go:2622`. A nested leg occupies exactly one such
slot, so `Width: 1` keeps every one of those range computations in-bounds and
truthful. The leg's COLUMN count is not lost — it is
`len(Fields[Start].FieldType.(*RecordType).Fields)`, and the window's `Typ`
carries it. Consumers wanting the leg's columns must go through `Typ`; under the
kind discriminator each of them declines a nested leg rather than iterating it
flat. `Width`'s doc comment is corrected in the same change to say "its slot
count in the carrying type", which is what every reader already assumes.

### 2. The kind is an EXPLICIT discriminator with an INVALID zero value.

`OrdinalSeedLegWindow{Offset, Typ, Alias}` (`ordinal_seed_layout.go:15-53`)
currently means one thing. Overloading `Offset` to mean "field index" in some
windows and "run start" in others, with the reader distinguishing them by
`len(Typ.Fields) == 1` or by whether the field type is a record, is the
wrong-offset wrong-rows failure this authority was consolidated to prevent — a
1-column flat leg whose one column is a struct is shape-identical to a nested
one.

**The zero value is `kindUnset` and every reader declines or panics on it.** Go's
zero value would otherwise silently mean `flatRun`, which is the inference this
section forbids, arrived at by language default instead of by a reader. A window
or leg that reaches a consumer without a stated kind is a producer bug and must
surface as one.

The discriminator lives on two carriers, because the layout crosses two:

- `OrdinalSeedLegWindow.Kind` — the planner's rebase authority;
- `values.RecordTypeLeg` (`values/type.go:372-502`), which is
  `{Alias, Name, Start, Width}` and today expresses only a flat
  `[Start, Start+Width)` boundary. It carries sub-leg boundaries on a leg's own
  row type into `finalizeSeedWindows`, and onto the merged row type the
  executor's runtime binders read. It gains the same explicit kind, excluded from
  identity exactly as `RecordType.Legs` already is (`type.go:366` — "carries NO
  identity semantics").

**`NewRecordTypeLeg`'s positional signature grows to carry it**
(`values/type.go:519`). The positional constructor IS the compile-time defence —
its own doc (`:508-518`) records that "deleting `Alias:` from two producers left
the whole suite green", which is why the identifier is the first positional
parameter and why `TestRecordTypeLegIsConstructed` bans the composite literal. A
kind that could be omitted would reproduce exactly that failure one field over.

**PRODUCERS, enumerated.** All 13 non-test `NewRecordTypeLeg` call sites, with
the kind each stamps and why:

| producer | kind | why |
|---|---|---|
| `values/ordinal_seed_layout.go:355` | flatRun / **carried** | `finalizeSeedWindows`' merged leg table — stamps the window's own kind, so this is the one site that can emit `nested` |
| `executor/ordinal_join.go:278` | **carried** from `sub` | `mergedLegsOfSpans` sub-leg rebase — "A REBASE, not a re-mint"; the kind rides with `Alias` |
| `executor/ordinal_join.go:283` | flatRun | a plain top-level run |
| `executor/ordinal_join.go:1027` | **carried** from `sub` | `rcOutputType`'s sub-leg loop — the second of three FLAT rebase sites |
| `executor/ordinal_join.go:1031` | flatRun | `rcOutputType`'s top-level box run |
| `executor/executor.go:2737` | flatRun | `concatLegPositionals`' outer leg |
| `executor/executor.go:2740` | flatRun | its inner leg |
| `executor/executor.go:2746` | **carried** from `lg` | its buried-leg rebase — the third FLAT rebase site |
| `executor/flat_map_cursor.go:643` | flatRun | the whole-row single-leg wrap |
| `rule_implement_nested_loop_join.go:2016` | flatRun | `planBuriedLegConcat`'s scan-leaf arm |
| `relational/core/query/ordinal_seed.go:75` | flatRun | the translator's buried-leg bounds |
| `relational/core/query/cascades_translator.go:5688` | flatRun | the select-leg producer |
| `relational/core/query/cascades_translator.go:5996` | flatRun | the whole-row leg |

The three REBASE sites (`ordinal_join.go:1027-1028`, `executor.go:2746`,
`ordinal_seed_layout.go:355`) must CARRY rather than stamp: a rebase that
re-mints a kind is the same defect class as one that re-mints an `Alias`. Every
other producer builds a genuinely flat boundary and stamps `flatRun`
explicitly — never by omission.

**READERS of the resulting MAP — a DISJOINT set from the derivation's callers.**
Revision 1 presented these as one nested census; they are two different
populations. The derivation has 17 call sites (§3); the map it returns has 5
keyed readers plus 2 unkeyed offset users. A derivation caller passes the map
along; a map reader indexes it. Neither list contains the other.

The five keyed readers are enumerated in
`values/seed_window_reader_census.go:68-101`:

| # | site | today | nested |
|---|---|---|---|
| 1 | `rebaseOuterLegValueOrdinal`, `left_outer_existential.go:79` (recorder `:81`) | `NewFieldValueOfOrdinal(mergedQOV, w.Offset+legOrdinal)` at `:164`, after bounding `legOrdinal` against `w.Typ` at `:160-163` | a TWO-STEP path: `NewFieldValueOfOrdinal(mergedQOV, w.Offset)` fused with the leg-local accessor via `FieldPath.WithSuffix` — Java's `ofOrdinalNumberAndFuseIfPossible`. The bound check keeps its job, because `Typ` is the leg's own record type (§1) |
| 2 | `rebaseLegRefsToBox`, `exists_gathered_cluster_wrap.go:135` (recorder `:137`) | `w.Typ.FieldIndex(fv.Field)` then `NewFieldValueOfOrdinal(boxQOV, w.Offset+idx)` at `:146` | same two-step fuse over `boxQOV` |
| 3 | `rebaseLegRefsToBox`'s survivor scan, `:221` (recorder `:223`) | membership only | **unchanged** — membership is kind-independent |
| 4 | `translateExistsOverGatheredCluster`, `:507` (recorder `:509`) | membership only | **unchanged** |
| 5 | `slotInGatheredSeed`'s qualified arm, `unnest_gather.go:443` (recorder `:445`) | `w.Offset + w.Typ.FieldIndex(col)` at `:449`, returning a flat SLOT `int` | this site's contract is a flat slot index and a nested leg has none: it must **decline**, as it already declines a qualified read with no correlation (`:431-437`) |

**Two further sites use the window and the census cannot see them, because they
are not keyed.** Found while writing this RFC:

- `unnest_gather.go:467-471` — the **bare-column** arm of that same
  `slotInGatheredSeed`, which ranges every window and takes `w.Offset+idx`
  (`:469`) when exactly one leg declares the column. Same treatment as #5: a
  nested window must not contribute a `hits++`. This one is offset arithmetic and
  is exactly as dangerous as the five.
- `rule_implement_nested_loop_join.go:2199-2204`
  (`materializedNLJOrdinalLayoutMatches`) — this one **sorts by `Offset` and
  compares `Typ` structurally; it performs no offset arithmetic**. Sorting is
  kind-independent and the structural compare reads `w.Typ`, which is the leg's
  type under both kinds. Its defect is upstream, at its `len(windows)` gate — see
  §8.

### 3. The nested kind gets its OWN entry point. The shared accept boundary does not move.

**This is the correction revision 1 most needed.** `OrdinalSeedLegWindows` has 17
non-test call sites, and most consume it as a nil/non-nil predicate. **At a
nil/non-nil predicate, population IS meaning**: a shape that starts returning
non-nil does not "keep the same semantics with a larger population", it flips
that consumer's branch.

The worked example, because it makes the hazard concrete rather than abstract.
`implementExistentialSelect` reads
`ordinalSeedLegWindowsOf(planResultValue(outerPlan))` at `:1600`.
`planResultValue` (`:1925-1944`) returns a FlatMap's result value, which can be a
positional-merge RC. Under revision 1's shared widening that call would flip from
nil to non-nil, and `:1600`'s `w == nil` guard — which selects whether the rule
re-selects the memo winner for a seed-shaped one — would silently change on a
rule arm this RFC never analysed.

**The design.** `values.OrdinalSeedLegWindows(rc)` keeps its exact accept set. A
sibling entry — working name `values.OrdinalSeedLegWindowsAcceptingNested(rc)` —
adds the two acceptances of §1. The flag controls exactly two decisions and
nothing else: whether `IsPositionalMergeRC` is recognized at the head, and
whether `finalizeSeedWindows` emits nested sub-windows. **The narrow entry
DECLINES, fail-closed, any seed carrying a nested leg** rather than returning it
top-level-only: a caller given top-level-only windows would silently be missing
sub-windows it would have had for a flat box leg, and "a declined optimization is
recoverable, a wrong ordinal is not" is this file's standing rule. In package
`cascades` the split appears as two wrappers, `ordinalSeedLegWindowsOf` (narrow,
unchanged) and a nested-accepting sibling.

**Exactly three sites opt in**, all in `rule_implement_nested_loop_join.go`, all
reading the SAME `step1RV`:

- `:2277` — `foldStep1Seed`'s validation of the seed it just built. It must
  accept what it constructs.
- `:3759` — `implementJoinWithExistential`'s `ordinalWindows, mergedRowType`,
  which feeds the three rebase sites (`:3781`, `:3841`, `:3904`).
- `:2186` — `materializedNLJOrdinalLayoutMatches`, called at `:3695` on that same
  `step1RV`. It must see the windows to check the orientation at all; see §8.

**THE PROOF, per consumer, is the entry point.** Fourteen sites call the narrow
entry, and for each the argument has the same structure, so it is given as a
structure and then a table rather than fourteen paragraphs:

> Pre-3d, a 3-quantifier EXISTS with a FlatMap leg produces no seed at all, so
> the consumer sees the folded projection and gets `nil`. Post-3d it sees a seed
> carrying a nested leg, and the narrow entry declines it — `nil` again. **The
> consumer's answer is unchanged on every input it can see**, which is stronger
> than "its meaning is unchanged".

| site | predicate | answer pre-3d | answer post-3d |
|---|---|---|---|
| `:1600` `implementExistentialSelect` | `w == nil` | nil → scans for a seed-shaped winner | nil → same |
| `:1628` same, member loop | `fw == nil → continue` | skips the member | skips the member |
| `:1643` same, hoist arm | `windows != nil` | nil → no hoist; the non-windowed path FAILS CLOSED and declines the yield | nil → the same fail-closed decline |
| `:2604` `describeSeedEscape` | `windows == nil` (`:2605`) | "DECLINES" | "DECLINES" — diagnostic only, no planning decision |
| `rule_select_merge.go:234` | `len(w) > 2 → continue` | `len(nil)==0`, no continue | same |
| `:318` | `w != nil` | false | false |
| `:785` `childRefResultIsNonSeed` | `w != nil → false` | returns true (non-seed) | returns true |
| `:818` `childRefIsPositionalUnnestSelect` | `w == nil → continue` | continue | continue |
| `cascades_translator.go:3191` `windowedOrdinalSeed` | `return w != nil, mt` (`:3192`) | `false, nil` | `false, nil` |
| `:5364` `gatheredSeedBakeContext` | stores the map | nil map, every lookup misses | same |
| `:5394` `findWindowedSeed` | `w != nil → return` | does not terminate the walk | same |
| `:5445` `positionalGatherUnbaked` | `w != nil → true` | false | false |
| `exists_gathered_cluster_wrap.go:445` | `windows == nil` (`:446`) | declines the wrap | declines the wrap |
| `left_outer_existential.go:36` / `:281` | the two-link wrapper chain | — | this is WHERE the split is implemented |

`:276` is the *definition* of `ordinalSeedLegWindowsOf`; its call is at `:281`.
Revision 1 listed `:276` as a call site; the seventeenth is `:36`.

The `:1643` row is the one worth reading twice, because its answer is not merely
unchanged — it is unchanged AND fail-closed. The hoist arm's non-windowed path
declines the yield rather than gambling on the correlation-unchecked fallback, so
a nested-carrying seed arriving there produces a declined plan, never a
mis-bound one.

No site needs the nested windows except the three. **If implementation finds one
that does, that is a STOP, not a widening** — the entire proof above rests on the
narrow entry's accept set being frozen.

### 4. `legIsOrdinalSafe` and `planBuriedLegConcat` move in LOCKSTEP.

`legIsOrdinalSafe` (`:1956-1986`) gains a `*plans.RecordQueryFlatMapPlan` arm,
CONDITIONAL on the FlatMap's `GetResultValue()` being the positional merge,
recognized structurally. `planBuriedLegConcat` (`:1997-2060`) has the same node
census by construction and gains the matching arm: the leg's concat is its N
merge slots, each field typed with that sub-leg's `*RecordType`, and its `.Legs`
records each sub-leg at its slot with `Kind: nested`, `Start: i`, `Width: 1`.

The plan walk is the route, not a preference:
`RecordQueryFlatMapPlan.GetResultType()` returns `values.UnknownType`
(`plans/flat_map.go:86`), exactly as `RecordQueryNestedLoopJoinPlan`'s does
(`plans/nested_loop_join.go:140`) — which is why `reconstructFoldStep1Seed`'s own
comment says `GetResultType()` cannot be used for a join leg (`:2088-2089`).

**Three existing pins constrain this arm:**

- `exists_join_fold_seed_test.go:107-111` — "a name-model join leg must NOT be
  ordinal-safe". Its fixture is a `RecordQueryNestedLoopJoinPlan` with a **nil**
  result value; the NLJ arm requires a `*RecordConstructorValue` (`:1977-1979`).
  A FlatMap arm gated on the positional merge cannot weaken it, and `:114-116`
  follows.
- `leg_layout_derivation_test.go:103-109` pins that `OrdinalSeedLegWindows`
  **declines a 2-slot RC of bare TYPED leg QOVs** — the shape *family* §1 admits.
  It stays green only because its fixture names the fields `"A"`/`"B"` while
  `IsPositionalMergeRC` requires `_i` in position order. **This is a hard
  constraint on the discriminator**: the nested branch keys on the FULL
  structural recognizer, never on the looser "every field is a bare typed QOV".
  The pin's failure message is updated in the same change to say what it now
  guards — a NAMED typed-leg RC is still not a merge row.
- `plan_leg_concat_layout_test.go` and
  `executor/leg_layout_cross_agreement_test.go` pin
  `cascades.PlanLegConcatLayout` (the exported wrapper over `planBuriedLegConcat`,
  `:2522-2535`) against the runtime's `concatLegPositionals`. That agreement is
  why the function is exported at all; the nested case joins that fixture.

### 5. ONE accept predicate, extended bit-for-bit, shared with the executor twin.

The planner walk and the executor span twin (`ordinalJoinSpans` /
`unnestMixedSeedSpans`) walk independently, and a disagreement about which field
is a leg and which is an element shifts the offset of every field after it. That
is why the element test is ONE function (`values.IsMixedSeedElementType`,
`ordinal_seed_layout.go:406-412`) and why `pkg/docscheck`'s
`mixed_seed_element_authority_test.go` structurally bans a DISCARDED
`*RecordType` assertion inside the two layout files — folding two copies into one
already missed a third in `resolveSpanLeaf`, and prose asserting agreement does
not notice when one copy is edited.

**The nested kind extends that SHARED predicate, in the same files, under the
same ban.** The recognizer is `values.IsPositionalMergeRC` (already exported,
already the executor's build trigger at `ordinal_join.go:1184`) plus the per-slot
BOUND record test of §1 — which the ban permits precisely because it binds.
Neither side gets a private copy.

The executor owes three twins:

- `unnestMixedSeedSpans`' decline note (`ordinal_join.go:89-92`) — "the
  positional-merge RC (whose bare QOVs are over RECORD leg types)" — **retires
  with this work**;
- `mergedLegsOfSpans` (`:271-287`) carries the kind through its sub-leg rebase,
  as it already carries `Alias`;
- `resolveSpanLeaf` (`:301-349`) gains nothing: it already descends `legRVs` for
  the fused path, and its existing coverage is the strongest evidence in this RFC
  that the address is resolvable rather than new.

`span_window_cross_agreement_test.go` grows a NESTED matrix — the pristine
(`:145`), mixed (`:341`) and box-leg (`:445`) fixtures joined by a nested-merge
fixture and a mixed nested/element fixture, each asserting the two walks produce
identical `(Alias, Kind, Offset, Typ)` per window and identical DECLINES for
every rejected shape.

### 6. Instruments: success is measured at the ORDINAL path, not at `MergedReAnchor`.

`MergedReAnchor` counts ARM 1 of `rebaseOuterLegValue` (`:2883-2892`). When
`ordinalSeedLegWindowsOf(step1RV)` turns non-nil the rule does not call
`rebaseOuterLegValue` at all — it takes `rebaseOuterLegRefsOrdinal` (`:3781`) and
`rebaseOuterLegValueOrdinal` (`:3904`), a different function in a different file;
and when `gatedSeedStep1` is true the projection is not rebased at any site
(`:3889-3898`). **So `MergedReAnchor` may remain `0` on complete success**, and
any gate denominated in it is blind to this change. Revision 1's gate (d) was so
denominated; §Gates fixes that.

The instruments that do move:

- **A standing `foldStep1Seed` outcome census — added by this work, not assumed.**
  The `108/77/77/200/78` breakdown came from a probe that was removed. The
  standing replacement carries the same four classes, with `reconstruct-nil`
  sub-classified by leg shape (bare-QOV / positional-merge / other), gated by
  `values.LegIdentityCensusEnabled`, asserted from the sqldriver `TestMain`
  beside its siblings (`embedded_fdb_test.go:101-169`).
  **Its denominator is counted INDEPENDENTLY**, by a separate counter at
  `implementJoinWithExistential`'s seed-decision call site (`:3646`), not by
  summing the classes. Summing four counters incremented inside the function they
  partition is true by construction and gates nothing; an independent denominator
  catches a class arm added without a counter, and an early return that skips
  recording.
- **The seed-window reader census** (floors at `embedded_fdb_test.go:454-462`).
  `existentialRebase` grows, because `:3781` fires on the newly-accepted firings'
  `existPreds`. Both hard zeros stay zero.
- **The leg-local bakeability census.** `Total` falls as reads move off
  `rebaseOuterLegValue`; `Declined` stays asserted at ZERO; the cross-check
  `IdentityInLegDomain == Baked + MergedReAnchor` keeps holding.
- **The merged-leg binding census**, whose coupled activation criterion at
  `assertMergedLegBindingCensus` (`embedded_fdb_test.go:191`) measures the
  DIVERGENCES retirement directly.
- **The executor's leg-column provenance census (4 dotted hits) must NOT move**,
  and that is a prediction. Its dotted arm is fed by the correlated-scalar ordinal
  seed builders, which name RC fields `LEG.COL` literally —
  `clustered_outer_scalar.go:493` and `scalar_subquery_seed.go:83`, witnesses
  `C.CV`, `I.QTY`, `O.ID`. A different channel. Movement there means this change
  touched the scalar-seed channel, which it must not.

### 7. Producer-first: the dotted readers, analyzed and PINNED.

Three readers key on merged-row field names. Each was read; **no producer-first
sequencing is required** — and each negative now gets a pin, so the claim
survives the person who measured it:

- **`clustered_outer_scalar.go:670`** (`clusterFieldResolvable`, `f == innerKey`).
  Counterparty: the inner scalar key minted at `:510` by the
  clustered-outer-scalar seed builder. Unrelated producer, population unchanged.
- **`clustered_outer_scalar.go:683`** (`clusterSeedSlotByName`,
  `EqualFold(leg.binding+"."+col, f)`). Counterparty: that same builder's leg
  field names, minted at `:493`, which calls `values.AssertOrdinalJoinSeed` at
  `:514` — a FLAT seed, never a positional merge. Population unchanged.
- **`ordinal_seed.go:794`** (`legRef`'s `strings.Contains(fv.Field, ".")`). Not a
  merged-row name reader at all: a SHAPE predicate on a reference, upstream of
  any merge. Population unchanged.

**Pins** — 3a for the first two, which are measurable without the new acceptance;
3d for the third, whose input population only becomes interesting once seeds
move. One test per reader asserting that a positional-merge row put through that
reader's own producer path yields no entry in the reader's name space, each with
a failure message naming what re-arms: *"a positional-merge row is now feeding
this dotted reader; RFC-197's producer-first ordering applies and the conversion
must be sequenced BEFORE the seed change, not after."*

### 8. Blast radius: the `correlatedStep1` wall.

Admitting FlatMap legs changes which seeds get built, and the arm above carries a
two-revert history: a correlated FlatMap binds legs by NAME, and a baked ordinal
against a name-keyed row context raises `values.BakedNameContextError`
(`values/values.go:571-584`, from `frontierContractGuard` at `:807-812`). The
bookings quote it at `:2260-2264` and `:3640-3645`.

**The new case is reachable only on the non-correlated path**, and the guard
chain is three links:

1. `correlatedStep1 := leftDeps || rightDeps || q0.IsNullOnEmpty() ||
   q1.IsNullOnEmpty()` (`:3522`).
2. `foldStep1Seed` returns `(rv, false)` on its FIRST line when it is set
   (`:2270-2272`) — before `reconstructFoldStep1Seed` runs at all.
3. `legIsOrdinalSafe` has exactly ONE production caller,
   `reconstructFoldStep1Seed` (`:2077`), which has exactly ONE caller,
   `foldStep1Seed` (`:2273`). `planBuriedLegConcat` has two: that reconstruction
   (`:2090`) and `PlanLegConcatLayout` (`:2523`), which builds no plan.

The tripwire is **`TestFDB_CorrelatedIndexExistsStaysIndexed`**
(`pkg/relational/sqldriver/correlated_index_exists_permanent_fdb_test.go:27`),
which EXPLAIN-asserts the SARG'd `[=]` index scan survives and the plan does not
regress to a full A×B×C cross product, plus correct rows. It must stay green.

*(TODO.md:6247 calls it `TestFDB_RFC173_CorrelatedIndexExistsStaysIndexed`, and
TODO.md:3523 plus the `:1973`/`:1244` line references in that block are also
stale. TODO.md is owned by the CQ-53 branch, which is fixing them; this RFC notes
them and does not touch that file.)*

### 9. `materializedNLJOrdinalLayoutMatches`' window gate is a FAIL-OPEN, and it is fixed here.

Revision 1 called `len(windows) != 2 → return true` (`:2187`) "the safe default".
**That is backwards, and the function's own doc says so**: "Declining (returning
false) here is always safe: the join-commutativity exploration ADDS an
alternative candidate, it never removes the non-swapped one" (`:2175-2184`).
Returning `true` is the PERMISSIVE answer. What it permits is the hazard
documented at `:2141-2151`: a `WithSwappedQuantifiers` firing reuses the seed
unchanged, so if the legs are swapped relative to the seed's baked layout, "every
baked reference into it then reads the wrong slot".

**The fail-open is pre-existing, not introduced.** `finalizeSeedWindows` already
ADDS a sub-window per buried leaf of a clustered box leg (`:311-315`), so a
2-quantifier join with a box leg already reports more than 2 windows and already
skips this check today. The new population makes it universal rather than
occasional: a step-1 seed over a merge leg with N sub-legs reports 2 top-level
windows plus N nested sub-windows, so `len(windows) ≥ 4` **always** — 100% of the
new population would skip the orientation check.

**The fix, not a measurement.** The derivation gains a TOP-LEVEL RUN LIST
alongside the map — the planner twin of what the executor already returns
(`ordinalJoinSpansOf`'s `spans []legSpan`, in offset order). The map cannot serve
this: `finalizeSeedWindows`' rightmost-leaf case REPLACES a box run's own entry
with a narrower sub-window (`:300-315`), so "the windows that tile the row" is
not recoverable from the map after the fact. `materializedNLJOrdinalLayoutMatches`
then gates on `len(runs) != 2` and compares `runs[0].Typ` / `runs[1].Typ`
structurally as it does today.

This CHANGES BEHAVIOUR TODAY for box-leg seeds: they stop failing open and start
being checked, which can DECLINE plans currently yielded. Declining is the safe
direction, but a lost plan is still a plan movement, so this lands as its own
sequencing step with its own golden and stress burden (3d′) rather than riding
along.

## Rejected alternatives

**Flatten the merged row at materialization.** It keeps one contract shape and
pays for it by contradicting Java's evaluation model (accesses FUSE,
`FieldValue.java:326-332`; Java's only row-flattener is group-by-specific), Go's
runtime representation (an `OrdinalRow` in the slot today), and the executor's
existing `IsPositionalMergeRC` build trigger. It would require a new
materialization pass plus re-derived offsets on both sides of a planner/executor
contract whose entire history is drift.

**Widen the SHARED `OrdinalSeedLegWindows` accept boundary — revision 1's
design.** Fourteen consumers read the result as a nil/non-nil predicate; at such
a predicate population IS meaning, so "3c is behaviour-neutral" was false. The
worked counter-example is `implementExistentialSelect:1600`, an arm revision 1
never analysed, whose winner-reselection guard would have flipped. The separate
entry point freezes the shared boundary and makes the per-consumer claim provable
from the entry point instead of argued.

**Reuse `Offset` and infer the kind at the reader.** `len(Typ.Fields) == 1`,
`Typ` being a record, `Width == 1` — every candidate is satisfied by a legitimate
flat leg. The inference is silent when wrong, and what it is silently wrong about
is which column a read addresses.

**Let the kind's zero value mean `flatRun`.** Convenient, and it is the same
inference one level down, made by the language instead of by a reader — with the
`NewRecordTypeLeg` precedent (`type.go:508-518`) recording that exactly this
failure already shipped once for `Alias` and left the suite green.

**Key the nested branch on "every field is a bare typed QOV".** Simpler, and it
turns `leg_layout_derivation_test.go:103-109` red — a pin that exists because
admitting such a constructor once gave multi-column legs one-column windows.

**Give the nested window a 1-field wrapper `Typ` describing the slot.** Uniform
with the flat sub-window's constructed slice, and it breaks reader #1's bound
check into a silent wrong-column read (§1).

**Re-anchor the leg through a qualified `QOV(merged)."LEG.COL"` name.** Already
dead: the mint was deleted in phase 3, and Java's re-anchored accessor has no
name at all (`FieldValue.java:335-338`, `Type.java:2918-2919`, `:657-659`).

**Teach only the executor and leave the planner declining.** The executor already
handles the shape; but the planner is where the DECISION is made, and
`legIsOrdinalSafe` refusing the leg is what keeps the reads on the pass-through.

**Absorb the 94 bare-QOV legs by typing the identity result value.** Investigated
under ruling; rejected as out of scope on evidence — see §Residues.

## Sequencing

Re-derived for the separate entry point. The first plan movement now genuinely
lands at 3d, and the stress/golden burden sits there and at 3d′.

- **3a — the instruments, first.** The standing `foldStep1Seed` outcome census
  with its independent denominator (§6); the two 3a pins of §7; and the
  **read→firing mapping**: how many of `LegLocalBakeCensus`' 174 reads occur
  under a firing the outcome census classifies `reconstruct-nil /
  positional-merge`. That number is what gate (d) is denominated in and it does
  not exist today. Also measured here, because it is an INFERENCE and must become
  a measurement: that all 60 declined positional-merge legs satisfy
  `IsPositionalMergeRC` and not merely "two bare QOVs over record types" — the
  removed probe recorded value shapes, not field names.
- **3b — the discriminator, with no new acceptance.** `Kind` on
  `OrdinalSeedLegWindow` and `RecordTypeLeg` with an invalid zero value;
  `NewRecordTypeLeg`'s signature grown; all 13 producers stamping or carrying
  explicitly; all 5 + 2 readers switching explicitly; `Width`'s doc corrected.
  Behaviour identical by construction.
- **3c — the nested entry point.** `OrdinalSeedLegWindowsAcceptingNested` added;
  the narrow entry gains its fail-closed nested-leg decline; the executor twin
  accepts identically under the shared predicate; `unnestMixedSeedSpans`' decline
  note retires. **Behaviour-neutral for real, and now provably**: no site calls
  the new entry yet, and no seed carrying a nested leg exists. Unit- and
  cross-agreement-pinned only.
- **3d — activation.** `legIsOrdinalSafe` and `planBuriedLegConcat` gain their
  FlatMap arms; the three opt-in sites switch to the nested entry; keyed readers
  #1 and #2 gain the fused two-step rebase and #5 and the bare-column arm gain
  their declines. **The reader change is NOT sequenced after the arms** — a
  nested window reaching reader #1 without the fused path bakes
  `Offset + legOrdinal` against a row where that is a different column. First
  plan movement; full gate set.
- **3d′ — the fail-open fix (§9).** The top-level run list, and
  `materializedNLJOrdinalLayoutMatches` gating on it. Separate because it moves
  plans that have nothing to do with the merge row (existing box-leg seeds), and
  those diffs must be readable on their own.

## Acceptance gates

**(a) An END-TO-END WRONG-ROWS probe, mutated in FOUR directions.**
Cross-agreement of two walks is necessary and not sufficient — two walks of the
same wrong model agree perfectly. The fixture is a multi-leg EXISTS whose merged
row produces DIFFERENT ROWS under a wrong window: distinct leg widths so
`Offset + legOrdinal` and the nested two-step address resolve to different slots,
at least one duplicate column NAME across legs so a name fallback cannot rescue a
wrong ordinal, and the projected value drawn from a non-first leg at a non-zero
leg-local ordinal. Rows asserted, not plan shape. Each direction mutated
SEPARATELY:

1. nested window read as flat (`Offset + legOrdinal`) → red on rows;
2. flat window read as nested → red on rows;
3. **leg orientation** — the two legs swapped relative to the seed's baked
   layout, which is the hazard §9's gate exists for → red on rows;
4. **the census-invisible bare-column arm** (`unnest_gather.go:467-471`) — a
   nested window allowed to contribute a `hits++` → red on rows.

**The mutation outcomes are recorded in the fixture's own doc comment**, naming
per direction what goes red and what re-arms if it stops — the same discipline
`leg_layout_derivation_test.go` already applies to its own negatives. A mutation
result that lives only in a PR description is a measurement that evaporates.

**(b) The 1M stress before and after, at 3d AND at 3d′ separately**, with real
plan movement expected at both. Every moved `EXPLAIN` golden and plandiff record
justified line by line WITH ROW COUNTS, never blanket re-blessed. Results
recorded in TODO.md's baseline table (by the branch that owns TODO.md).

**(c) Census EQUALITIES, not floors.** These are predictions; a measured
deviation is itself a reportable finding and must be reported rather than
absorbed by relaxing the assertion. Post-3d, the `foldStep1Seed` outcome census
must satisfy:

```
ACCEPT                          == 138     (78 + 60)
DECLINE reconstruct-nil         ==  94     (154 - 60, all bare-QOV)
DECLINE correlatedStep1         == 108     (unchanged — the wall)
DECLINE rv-no-exist-ref         == 200     (unchanged)
denominator (counted at :3646)  == 540     (unchanged)
```

with the four classes summing to the INDEPENDENTLY counted denominator (§6).
"Up to 60" is deleted: the prediction is 60, and if it is not 60 the difference
is the finding.

Alongside: `Declined` stays 0; both seed-window hard zeros stay 0; all five
reader floors hold; `IdentityInLegDomain == Baked + MergedReAnchor` holds; the
merged-leg binding activation criterion holds; the leg-column provenance census
does NOT move (§6); `MergeSlotTypeDisagreements` stays 0.
`rule_select_merge.go:234`'s `len(w) > 2` is re-measured explicitly — under the
separate entry point it must be unchanged, and that is the check that §3 held in
practice and not only on paper.

**(d) The DIVERGENCES.md:105 retirement condition, denominated in the instrument
that moves.** The condition is that a leg-correlated read be rewritten to
`ofOrdinalNumber` against the merged quantifier before execution, so no sibling
alias need be resolvable at runtime. Revision 1 denominated this in
`MergedReAnchor`, which §6 proves blind. Restated:

- the `foldStep1Seed` ACCEPT population grows by 60 firings (gate (c));
- of `LegLocalBakeCensus`' 174 reads, the subset measured in 3a as belonging to
  `reconstruct-nil / positional-merge` firings **falls to zero**, and the
  remainder is reported. That subset size is unknown today; 3a produces it, and
  this gate asserts its post-change value is 0.

The retirement is NOT completed by this RFC and this RFC does not claim it is.

## Residues, measured and OUT OF SCOPE

**The 94 bare-QOV legs — INVESTIGATED under ruling, and FENCED.** The question
was whether typing the identity result value
(`rule_implement_nested_loop_join.go:3879` at `4dccc50f0`, `:3866` at branch
HEAD — `values.NewQuantifiedObjectValue(mergedOuterCorr)`, the untyped
constructor at `values/values.go:4340`, where Java types unconditionally at
`Quantifier.java:801-803`) could be absorbed here. Revision 1 implied that line
was THE producer. **It is not, and three findings put it out of scope:**

1. **It is one of four producers, and structurally not the main one.** All four
   non-test `RecordQueryFlatMapPlan` constructions can emit a bare QOV result
   value: `:1383` (`buildCorrelatedFlatMapPlan`, passing its `resultValue`
   parameter through), `:1797` (`implementExistentialSelect`), `:3917` (this
   line), `:4161` (`yieldExistsFlatMap`). Three flow `sel.GetResultValue()`
   essentially verbatim, and the SQL translator mints exactly a bare untyped QOV
   as a select result value at `cascades_translator.go:4097`. The 94 are LEGS of
   a 3-quantifier join+EXISTS — the accumulated side of an N-way join, which
   `yieldGeneralFlatMap`/`buildCorrelatedFlatMapPlan` build, not `:3917`.
   Attribution of the 94 to any one producer is **unmeasured**: the census
   witness (`describeSeedEscape`, `:2594-2609`) records the declined leg's
   result-value SHAPE, not its producer.
2. **The type is unavailable precisely where it is needed.** `mergedRowType` is
   non-nil exactly when `ordinalWindows` is (`ordinalSeedLegWindowsOf` returns
   both or neither), and on the bare-QOV path that holds only when
   `sel.GetResultValue()` is ALREADY a pristine seed. The Java-verbatim form —
   build the quantifier and take `GetFlowedObjectValueTyped()`
   (`expressions/quantifier.go:577-586`) — degenerates to the same no-op, because
   it derives from `GetFlowedObjectType`, a reporting pass-through (`:296-301`,
   `:322-328`). A type IS derivable from `existLegRowTypes` (`:3760`), but only
   for the materialized arm and only by constructing a concat that does not exist
   at that site.
3. **The blast radius is real and hits both risks the ruling named as
   disqualifying.** `GetFlowedObjectType` today treats an untyped member as
   SILENT — "cannot contradict a typed one — it reports nothing"
   (`quantifier.go:322-328`). Typed, such members participate and can raise
   `MemberResultTypeDisagreementError` (`:344-346`), which declines the whole
   partition-select collapse (`positional_merge.go:79-96`),
   `rule_partition_select.go:663`, `rule_partition_binary_select.go:246-256`, and
   the leg derivation (`:2457-2465`) — where `DisagreeingLegs` and
   `UnderivableLegs` are asserted as HARD ZEROS. A newly-typed member that
   disagrees with a sibling turns a currently-green census gate red. Separately,
   `:3866` and the FlatMap construction at `:3917` execute on **both** the
   correlated and materialized arms (the `correlatedStep1` block at `:3643-3699`
   only selects `step1Expr`), so typing it touches the `correlatedStep1` wall;
   and whether `correlatedStep1 && ordinalWindows != nil` is reachable **could
   not be established by reading**. Typing also propagates into positional-merge
   slot types via `positional_merge.go:79`, moving
   `leg_layout_derivation_test.go:112-141`, which pins both directions of that
   classification.

**Fenced.** Consequently:

- revision 1's claim that this RFC closes "the largest addressable block" is
  **withdrawn**. `94 > 60`: the bare-QOV residue is LARGER than the population
  this RFC closes. This closes the **second-largest** block, and the largest
  addressable one remains open;
- the 94 require a numbered TODO booking at merge time. **The coordinator holds
  that booking** — stated here so the residue is not silent, and because this
  branch does not own TODO.md.

**The 200 `rv-no-exist-ref` firings are correct pass-throughs**, not a residue.

**The `26 + 48 + 80` step-1 result-value shapes are a CONSEQUENCE, not an
independent population.** They are what survives as `step1RV` after the 154
declines (26+48+80 = 154) — folded projections carrying an `ExistsValue`, not
merged rows. When 60 declines become ACCEPTs this distribution shrinks with them;
it is not separately actionable.

**Untyped merge slots (750 of 18246).** Every distinct witness is an unnest
element alias whose array element type Go does not infer that far
(`positional_merge.go:190-202`; the same gap `IsMixedSeedElementType:399-405`
documents about its own predicate). Under the nested branch these keep the
element treatment (§1). Typing the unnest element quantifier is a planner-wide
change and is not a rider here.

## What this does not do

It does not retire the runtime binding-namespace widening: the bare-QOV and
`correlatedStep1` populations still take the pass-through and
`executor.bindMergedOuterLegs` stays. It does not touch the correlated-index
EXISTS name path, which is permanent and Java-correct. It does not retire
`RecordTypeLeg.Name`, whose two surviving readers are dotted-text readers blocked
at their producers (CQ-52). It does not touch the correlated-scalar ordinal seed
— and §6 makes "did not touch it" a checkable prediction rather than an
assurance.

What it does is smaller and exact: it stops Go's own layout authority from
declining the merged row Go's own planner builds and Go's own executor already
evaluates — the one Java has built since `PartitionSelectRule` was written — and
it does so without moving the accept boundary any other consumer reads.
