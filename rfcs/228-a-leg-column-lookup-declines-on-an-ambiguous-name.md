# RFC-228: a leg column lookup declines on an ambiguous name

**Status:** DRAFT
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

This is pinned two-sidedly by `dup_named_leg_window_first_match_test.go` (landed separately, as a characterization of the current behaviour). Mutation-verified: honouring the carried ordinal turns that test red with an instruction to flip it, while the sibling and control cases stay green.

**Reachability on real traffic is unmeasured** and this RFC does not claim it. Whether a lazy or source-relative single-accessor reference meets a non-narrowed box-run window today is not shown, so the defect is latent-until-shown. That does not change the disposition: an arm whose stated safety premise is false, sitting between two arms that refuse the same operation for the stated reason, is fixed on the strength of the inconsistency alone. Waiting for a reachability proof is waiting for the wrong rows.

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

The change can only convert a wrong-column read into a declined optimization. It cannot make a previously-correct plan wrong: every input that reached `legOrdinal` before with `dupes == 1` reaches it identically now, and the only inputs whose behaviour changes are those where `FieldIndex` returned a slot that is not the only candidate — precisely the set whose answer was arbitrary.

### 2.1 Why not the other direction

The tempting alternative is to honour the carried ordinal when the reference has one, as the `FrontierPinned` arm does. That is the *right* long-run answer and this RFC does not implement it, for two reasons.

It is a larger claim than the evidence supports. The name arm's contract is that its input is **non-baked** — `NewFieldValueOfOrdinal` is not its source — so a carried ordinal is stated against a layout this site cannot identify. The one measured case where honouring it is correct is a source-relative baked reference, and the discriminator that recognizes that case is the frontier pin, which is what routes to the sibling arm in the first place. Reading an ordinal off a reference that did not come through the pinned path means reading a number whose frame is unknown, which is the same defect one level up.

And it is unnecessary. A declined optimization is recoverable; a wrong ordinal is not. The decline is the correct-or-loud disposition this file already commits to at four other sites (`LegKindUnset`, the window bound check, `Single()`, `ReAnchorRootInto`).

## 3. Why the decline is not a silent regression

A decline here drops one rebase, which drops one candidate plan, not the query. If the *only* plan for a query ran through this arm with an ambiguous leg name, the query would previously have returned wrong rows; a plan-not-found is the strictly better failure and is loud.

The exposure is bounded by how the ambiguous input is produced at all: an opaque box leg exposing two buried columns of one name. The corpus is the check — a golden diff and the 1M stress comparison establish whether any planned shape currently takes this arm with `dupes > 1`. **The prediction, registered before the measurement: zero corpus plans change.** If the corpus does move, that is the reachability proof §1 says is missing, and it belongs in the implementation PR as such.

## 4. The debt entry

`dotted` bucket entry 5 (`left_outer_existential.go # rebaseOuterLegValueOrdinal`) does **not** retire with this change. The arm still decides by name; what changes is that the decision is now *unambiguous or refused* rather than *first-match*. That is the same status as its two siblings, which are `contract` entries with the same closure condition: the arm retires when the merged layout carries per-slot leg provenance the lookup can match on.

The entry's text is amended to record that the duplicate case declines, and its bucket does not change. The count stays at 14 and `fieldDebtAuthorityTotal` stays at 35.

## 5. Tests

- `dup_named_leg_window_first_match_test.go` flips from characterizing the defect to asserting the fix: `L.K` at leg-local 1 in a `[K, K, Z]` leg now **declines** rather than returning merged slot 10, and the `FrontierPinned` sibling continues to return 11.
- A control on the unambiguous path: a leg with no duplicate names resolves by name exactly as before, so the fix is not a blanket decline.
- The absent-name case keeps its existing decline, so `dupes != 1` is shown to subsume rather than replace it.
- Corpus golden + 1M stress, per §3, with the zero-diff prediction stated above recorded ahead of the run.

`FieldIndexUnique` gets its own unit pin driving **every** arm, not just the two the leg site happens to reach: empty name, absent, exactly one, two, three, a duplicate in the final position (so the loop's last-write-wins cannot masquerade as a correct answer), and the unaffected `FieldIndex` first-match behaviour asserted alongside it so the twins are visibly different. An arm exercised only by the corpus is an untested arm, and this one is about to acquire callers.

## 6. Noted, not claimed

The site passes `strings.ToUpper(fv.Field)` to a lookup that compares against `f.Name` **raw**. If a leg's field names are ever not upper-cased, the arm declines on a name that is present. That asymmetry predates this RFC, is unchanged by it, and its reachability is unmeasured — recorded here so it is not rediscovered as a finding.
