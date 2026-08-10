# RFC-228: a leg column lookup declines on an ambiguous name

**Status:** IMPLEMENTED, and the implementation went FURTHER than this RFC — see §8. (rev 2 folded two review laps; §1 strengthened, §3 retracted and rewritten, §2 rescoped.)
**Scope:** `rebaseOuterLegValueOrdinal` (`pkg/recordlayer/query/plan/cascades/left_outer_existential.go`), one arm.
**Relates to:** RFC-218 (name-derived root re-anchor), RFC-222 (nested-or-flat before pinned-or-lazy), RFC-197 (the `.Field` debt list, `dotted` bucket entry 5).

## 0. The claim this RFC retracts

The name arm of `rebaseOuterLegValueOrdinal` carries this justification:

> A single source leg has no duplicate column names, so `FieldIndex(Field)` is the leg-local ordinal.

The premise is false, and the two arms directly above it in the same `switch` say so in their own comments. The multi-accessor arm, on the same `w.Typ`:

> ReAnchorRootInto ... declines when the root name is absent or DUPLICATED in the leg — an opaque box leg can expose two buried columns of one name, where a first match is indistinguishable from a correct answer and disambiguating needs the leg identity this site does not carry.

And the `FrontierPinned` arm, again on the same `w.Typ`:

> an OPAQUE box leg can expose DUPLICATE buried column names (`A.K` and `B.K` merged into one leg), where `FieldIndex("K")` would remap the already-baked ref to the FIRST match and silently probe the WRONG column (wrong rows).

Three arms resolve a reference against one leg type. Two decline on a duplicate name and cite that decline as the property that keeps them from being a wrong-column read. The third calls `RecordType.FieldIndex`, which is a first-match scan with no duplicate detection, and asserts the duplicates cannot exist.

The debt list makes the inconsistency load-bearing rather than incidental. The `contract` entries for both declining arms name the alternative explicitly — *"RecordType.FieldIndex would have first-matched"* — as the reason those two are honest debt rather than defects. The third arm is the counterfactual those entries describe, shipped.

## 1. What breaks

A **source-relative baked** reference reaches the name arm carrying the correct leg-local ordinal, and the arm discards it in favour of a name lookup. Measured against the production symbol — leg `[K, K, Z]` at merged offset 10, reference `L.K` carrying ordinal 1:

| arm taken | merged slot | correct? |
|---|---|---|
| name (`FieldIndex("K")` → 0) | 10 | no |
| `FrontierPinned` (`acc.Ordinal` → 1) | 11 | yes |

The two inputs differ **only** in the frontier pin. Slot 10 is a real merged column of the same type, so `NewFieldValueOfOrdinal` accepts it and nothing downstream rejects it: the plan is built, executed, and returns rows read from the wrong column.

This is pinned two-sidedly by `dup_named_leg_window_declines_test.go`, which landed on master in #702 as a characterization of the current behaviour. Mutation-verified: honouring the carried ordinal turns that test red with an instruction to flip it, while the sibling and control cases stay green.

**Rev 1 called this a hand-built symbol and said reachability was unmeasured. It understated the case.** Every piece of the symbol is production-built:

- the duplicate-named leg type comes from `rule_implement_nested_loop_join.go:2288`, a raw `&values.RecordType{Fields: concatFields, Legs: legs}` literal, written raw *because* `NewRecordType` panics on duplicates (`values/type.go:713-722`);
- `values/ordinal_seed_layout.go:256` files it as `LegKindFlatRun` with an `Offset`, which is what routes it to the arithmetic at the defect site;
- the unpinned single-accessor reference is the ordinary SQL column reference.

### 1.1 Measured: the defect is LATENT, and not for the reason the arm gives

The site was instrumented (arm taken, window kind, whether `w.Typ` holds any duplicate name, match count for the looked-up name) and the full `pkg/relational/sqldriver` FDB corpus was run, plus a purpose-built set of eight self-join and derived-table shapes chosen to hit it. The instrumentation was then reverted.

**Population: 496 name-arm entries.** Every one: `matches=1`, `typeHasAnyDup=false`. The name arm never saw a duplicate.

**Duplicate-named leg windows are nonetheless produced in production** — 4 occurrences of `fields=ID,PARTNER,K,ID,PARTNER,K`, `width=6 nlegs=2`, from a plain self-join. So the arm's stated premise ("a single source leg has no duplicate column names") is **false as measured**, exactly as §0 argues. What keeps the defect from firing is a different property entirely: every duplicate-named window observed arrives carrying a **multi-accessor pinned** reference (`accs=[_1@1 K@2] naccessors=2 pinned=true`) and therefore routes to the multi-accessor arm, which declines via `ReAnchorRootInto`. The honest arm catches it.

So the defect is **latent, not shipped** — and the thing making it unreachable is unrelated to the justification written at the site. That is the dangerous configuration, because the comment invites a future change to rely on a premise that was never true, while the actual guard is an unstated coincidence about which arm dup-named windows currently reach.

Per the repo's negative-result rule, the unreachability is pinned rather than asserted: the eight shapes land as a committed test, and the pin's failure message names what gets re-armed if a duplicate-named window ever arrives unpinned or single-accessor.

The disposition is unchanged and was never contingent on this measurement: an arm whose stated safety premise is false, sitting between two arms that refuse the same operation for the stated reason, is fixed on the inconsistency alone.

## 2. The fix

Give `RecordType` an honest twin of `FieldIndex`, and use it at the defect site.

The twin, not a fourth inline loop, is the point. `FieldIndex` is a first-match scan, and its doc comment explains its *ordinal semantics* while saying nothing about what it does with a duplicate — so every site that needs a by-name lookup reaches for it and inherits a first match it never asked for. Three sites have now needed the declining version; two hand-rolled it and one did not. A named method is what stops the fourth site from copying `FieldIndex` again.

```go
// FieldIndexUnique is FieldIndex for callers that cannot tolerate an arbitrary
// answer. It returns the ordinal only when the name matches EXACTLY ONE field;
// absent and DUPLICATED both report false, because a first match among
// duplicates is indistinguishable from a correct answer and the caller has no
// way to tell them apart afterwards. A record type may legitimately carry
// repeated names -- an opaque box leg exposes two buried columns of one name
// (`A.K` and `B.K` merged into one leg) -- so disambiguating needs leg identity
// the type alone does not carry. Prefer this over FieldIndex wherever a wrong
// ordinal would be a silent wrong-column read rather than a loud failure.
func (r *RecordType) FieldIndexUnique(name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	idx, hits := 0, 0
	for i, f := range r.Fields {
		if f.Name == name {
			hits++
			idx = i
		}
	}
	if hits != 1 {
		return 0, false
	}
	return idx, true
}
```

and at the site:

```go
case !strings.Contains(fv.Field, "."):
    // A leg-relative NAME ref (non-baked): NewFieldValueOfOrdinal is not its
    // source, so the slot is resolved by column name. The lookup DECLINES on a
    // duplicate rather than first-matching: an OPAQUE box leg can expose two
    // buried columns of one name, where a first match is indistinguishable from
    // a correct answer and disambiguating needs the leg identity this site does
    // not carry -- exactly what the two arms above decline for.
    idx, found := w.Typ.FieldIndexUnique(strings.ToUpper(fv.Field))
    if !found {
        failed = true
        return node
    }
    legOrdinal = idx
```

Folding absent and ambiguous into one `found == false` matches the existing behaviour for absent and adds it for ambiguous; the call site's shape does not change at all.

The two sibling arms keep their inline loops. Their decline strings are diagnostic (`"root column absent from the flowed layout"` vs `"root column name is ambiguous in the flowed layout"`) and are asserted by `nested_root_reanchor_test.go`; collapsing them into a boolean would delete information the tests read. `FieldIndexUnique` is for the callers that only need the answer.

### 2.2 Scope: this is a class, not a site

Rev 1 fixed one call and proposed a public method with one caller, which is dead code in waiting and was correctly objected to. The census (`grep -rn --include='*.go' "\.FieldIndex(" pkg/ cmd/ | grep -v _test.go` → **19 sites**) shows the defect is a property of an entire family. Eight do leg-window lookup followed by offset arithmetic — the shape where a first match is a wrong-column read:

| site | needle | note |
|---|---|---|
| `left_outer_existential.go:225` | `ToUpper` | the site §2 fixes |
| `unnest_gather.go:459` | raw | keyed arm, first-matches |
| `unnest_gather.go:493` | raw | the hits-counting arm — see below |
| `exists_gathered_cluster_wrap.go:156` | raw | |
| `clustered_outer_scalar.go:195` | raw | |
| `clustered_outer_scalar.go:616` | raw | |
| `ordinal_seed.go:720` | raw | |
| `cascades_translator.go:5080` | raw | |

The remaining eleven are presence-only (`if _, found := ...; !found`) or resolve against a non-leg type, where a duplicate changes nothing: a name that is present is present regardless of how many times.

**`unnest_gather.go:493` is the one worth stating precisely, because it looks like the careful case and is not.** That arm ranges every window, counts hits, and declines unless exactly one leg carries the column — its comment even says "an ambiguous bare column (a dup-named `K` present in two legs) declines here". True, and insufficient: it counts hits **across** windows while `FieldIndex` first-matches **within** one. A single window holding two same-named fields contributes exactly one hit and silently first-matches inside itself. The guard is real for inter-leg ambiguity and blind to intra-leg ambiguity, which is the case §1 measures.

All eight convert to `FieldIndexUnique`. That is what makes the method a shared lookup rather than a wrapper for one call, and it is why `LookupField` (`type.go:806`, the same first-match scan with the same silence) gets the same treatment — leaving it means the ninth site has a copy target.

The needle column above is a second finding in its own right: two sites in one family disagree about case-folding, one uppercasing and six passing raw. §6 records what is measured about it.

The change can only convert a wrong-column read into a declined optimization. It cannot make a previously-correct plan wrong: every input that reached `legOrdinal` before with `dupes == 1` reaches it identically now, and the only inputs whose behaviour changes are those where `FieldIndex` returned a slot that is not the only candidate — precisely the set whose answer was arbitrary.

### 2.1 Why not the other direction

The tempting alternative is to honour the carried ordinal when the reference has one, as the `FrontierPinned` arm does. That is the *right* long-run answer and this RFC does not implement it, for two reasons.

It is a larger claim than the evidence supports. The name arm's contract is that its input is **non-baked** — `NewFieldValueOfOrdinal` is not its source — so a carried ordinal is stated against a layout this site cannot identify. The one measured case where honouring it is correct is a source-relative baked reference, and the discriminator that recognizes that case is the frontier pin, which is what routes to the sibling arm in the first place. Reading an ordinal off a reference that did not come through the pinned path means reading a number whose frame is unknown, which is the same defect one level up.

And it is unnecessary. A declined optimization is recoverable; a wrong ordinal is not. The decline is the correct-or-loud disposition this file already commits to at four other sites (`LegKindUnset`, the window bound check, `Single()`, `ReAnchorRootInto`).

## 3. A decline here fails the query — and that is still the right trade

**Rev 1 of this section was wrong and is retracted.** It said "a decline drops one rebase, which drops one candidate plan, not the query." That is false at this site.

`ImplementNestedLoopJoinRule` is the **sole implementer** of both existential Select shapes. `rule_partition_select.go` bars the 2-quantifier shape outright (`len(quantifiers) < 3` → return) and then explicitly refuses `existentialCount == 1 && foreachCount <= 2`, with a comment saying it does so precisely to avoid racing this rule's working arm with an alternate decomposition. So there is no second producer to fall back to. The declines in `rule_implement_nested_loop_join.go` are bare returns *before* the single `call.Yield`; the group stays empty and planning ends at `plan_harness.go`'s `"best expression is not a physical plan"`.

A decline is therefore a **query planning failure**, user-visible, not a lost candidate.

The change still stands, for the reason it always did: a loud planning failure is strictly better than silently reading the wrong column. But three things follow from the correction, and rev 1 got all three wrong by omission.

1. **§2.1's "a declined optimization is recoverable" is false here.** It is recoverable in the sense that no wrong answer escapes; it is not recoverable in the sense that the query still runs. The argument for the decline over honouring the carried ordinal rests only on the frame-unknown problem, not on cheapness.
2. **The zero-diff prediction is now a correctness gate, not a nicety.** A corpus plan that moves under this change is a query that goes from planning to not-planning. The prediction, registered before the measurement: **zero corpus plans change, and zero queries lose a plan.** If either moves, the change does not ship as-is — it ships together with the terminal fix in §3.1, because at that point the decline has a live victim.
3. **It raises the priority of the terminal fix**, below.

### 3.1 The terminal fix, and why it is not deferred out of this RFC

Java does not have this problem and the reason is structural: at the analogous site, `PartitionSelectRule` builds the collapsed row from `Column.unnamedOf` — synthetic, positionally-derived names — and rebases with `FieldValue.ofOrdinalNumber(..., index)`. A name lookup is not merely avoided; it is unexpressible. `ResolvedAccessor.equals` is ordinal-only. `Type.Record`'s name maps are built with `ImmutableMap.toImmutableMap`, which throws on a duplicate key, so a duplicate-named row cannot be constructed at all.

Go already does exactly this on one path: `positional_merge.go` names merged fields with `values.OrdinalFieldName(i)`, which is `Column.unnamedOf`. The divergence is that the **leg-concat** path does not. `rule_implement_nested_loop_join.go:2288` builds `&values.RecordType{Fields: concatFields, Legs: legs}` as a **raw literal specifically because `NewRecordType` panics on duplicates** (`values/type.go:713-722`), and `values/ordinal_seed_layout.go:256` files the result as `LegKindFlatRun` with an `Offset`. So the duplicate-named leg window is not an anomaly to be stamped out at the producer — it is what a leg-concat of two real sources legitimately *is*, and the raw literal exists to permit it.

That settles a question rev 1 left open in the wrong direction. The producer is not defective. `NewRecordType`'s panic is a constructor that cannot express a legal merged row, which is why every merged-row site routes around it. Adding a "loud assert" at the producer, as one review lap proposed, would assert against a shape the engine is entitled to build.

The terminal fix is therefore to carry ordinal identity through this rebase the way Java does, so the name never has to answer the question. This RFC does not implement it, and that is a **sequencing statement, not a deferral**: the decline is the strictly smaller change, it is a precondition for measuring whether the terminal fix moves anything (an arm that silently first-matches cannot be observed), and §3's gate above is what forces the terminal fix to arrive the moment the decline has a victim. The follow-on is booked in TODO.md against RFC-197's `dotted` entry 5, which §4 keeps open for exactly this.

## 4. The debt entry

`dotted` bucket entry 5 (`left_outer_existential.go # rebaseOuterLegValueOrdinal`) does **not** retire with this change. The arm still decides by name; what changes is that the decision is now *unambiguous or refused* rather than *first-match*. That is the same status as its two siblings, which are `contract` entries with the same closure condition: the arm retires when the merged layout carries per-slot leg provenance the lookup can match on.

The entry's text is amended to record that the duplicate case declines, and its bucket does not change. The count stays at 14 and `fieldDebtAuthorityTotal` stays at 35.

## 5. Tests

- `dup_named_leg_window_declines_test.go` flips from characterizing the defect to asserting the fix: `L.K` at leg-local 1 in a `[K, K, Z]` leg now **declines** rather than returning merged slot 10, and the `FrontierPinned` sibling continues to return 11.
- A control on the unambiguous path: a leg with no duplicate names resolves by name exactly as before, so the fix is not a blanket decline.
- The absent-name case keeps its existing decline, so `dupes != 1` is shown to subsume rather than replace it.
- Corpus golden + 1M stress, per §3, with the zero-diff prediction stated above recorded ahead of the run.

`FieldIndexUnique` gets its own unit pin driving **every** arm, not just the two the leg site happens to reach: empty name, absent, exactly one, two, three, a duplicate in the final position (so the loop's last-write-wins cannot masquerade as a correct answer), and the unaffected `FieldIndex` first-match behaviour asserted alongside it so the twins are visibly different. An arm exercised only by the corpus is an untested arm, and this one is about to acquire callers.

## 6. The case-folding asymmetry

The site passes `strings.ToUpper(fv.Field)` to a lookup that compares against `f.Name` **raw**, while six sibling sites in the same family (§2.2) pass the needle raw. Rev 1 filed this as "noted, not claimed" on the grounds that a decline is cheap; §3's correction removes that excuse, since a decline here fails the query.

Measured on the same instrumented run: **`matches` and `rawMatches` were equal in all 496 entries.** Every leg-window field name reached by this arm is already upper-case, so the fold is a no-op today and the asymmetry costs nothing. It is a latent divergence, not a live bug.

It is not fixed here, and the reason is that the two candidate fixes point in opposite directions — normalize every site to fold, or normalize every site to raw — and choosing needs the same leg-identity answer §3.1 defers to the terminal fix. What this RFC does is stop it from being invisible: the every-arm unit pin in §5 includes a mixed-case case asserting the current behaviour, so a future change to either side of the comparison shows up as a test change rather than as a silent decline.

## 8. What actually shipped

This RFC proposed adding `FieldIndexUnique` **beside** `FieldIndex` and converting eight leg-window sites. What shipped deletes `RecordType.FieldIndex` and `RecordType.LookupField` outright and converts every caller — 33 files, enumerated by the compiler rather than by the grep in §2.2, which had found eight.

The reason for going further is §2's own argument taken seriously. A first-match lookup left in the API is a copy target; §2.2 said so about `LookupField` and then proposed keeping `FieldIndex` next to its safe twin anyway. Deleting both removes the choice instead of documenting it, and `TestNoFirstMatchNameLookup` fails if either declaration or any call comes back.

Two claims in this RFC are superseded by that:

- §3's registered prediction — zero corpus plans change, zero queries lose a plan — **held**, over the whole conversion rather than the one site.
- §1's note that the characterization test is "mutation-verified: honouring the carried ordinal turns it red with an instruction to flip it" is now history. The test was flipped, by the fix, exactly as designed. It asserts the decline and is renamed accordingly.

§6's case-folding asymmetry is unchanged and still measured: `matches` and `rawMatches` were equal in all 496 corpus entries, so the fold is a no-op today.
