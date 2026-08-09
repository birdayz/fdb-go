package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The ORDINAL existential rebase. The EXISTS-over-join implementation binds
// the inner join's merged row under a fresh correlation and rewrites
// outer-leg references so they resolve inside the existential FlatMap. The
// lazy form (rebaseOuterLegRefsToMerged) rewrites them to LAZY qualified
// "LEG.COL" reads over the merged row — machinery the FrontierPinned panic
// bars from BAKED references (a silent baked→lazy degradation). When the
// merge produced an ordinal (positional) seed, the correct target is instead
// the merged POSITIONAL row: each leg reference — baked (ofOrdinal over the
// leg QOV) or lazy (FieldValue(QOV(leg), col)) — rebases to a BAKED
// ofOrdinalNumber over the merged correlation at legOffset+columnOrdinal,
// with the offsets derived from the seed's own per-leg runs. Java's model
// keeps per-quantifier correlations in one FlatMap level; Go's two-level
// NLJ→FlatMap makes the merged positional row the binding, so the ordinal
// offset rebase is the per-quantifier edge composed with the seed layout.
// Any reference the windows cannot map DECLINES the yield (CORRECT-or-LOUD;
// the lazy rebase stays dead for ordinal seeds).

// ordinalLegWindow aliases the values-level layout window (the derivation
// is CONSOLIDATED into values.OrdinalSeedLegWindows:
// the executor's span derivation is pinned to agree with the
// same function by a cross-agreement fixture — independent walks drift,
// and layout drift is wrong-offset wrong-rows).
type ordinalLegWindow = values.OrdinalSeedLegWindow

func ordinalSeedLegWindows(rc *values.RecordConstructorValue) (map[values.CorrelationIdentifier]ordinalLegWindow, *values.RecordType) {
	return values.OrdinalSeedLegWindows(rc)
}

// descendOrFail travels a nested reference's remaining accessors from the merged
// address its root landed on, and marks the whole rebase FAILED rather than
// returning a half-travelled path.
//
// A dropped suffix is the failure mode this exists to prevent and it is silent:
// the root address alone is a real merged column — the enclosing STRUCT — so the
// plan builds, executes, and compares the wrong thing. Declining costs a clean
// unsupported error; a truncated path costs wrong rows.
func descendOrFail(root *values.FieldValue, into *values.RecordType, suffix []values.ResolvedAccessor, leafTyp values.Type, failed *bool, node values.Value) values.Value {
	if len(suffix) == 0 {
		return root
	}
	fused, err := values.FuseNestedSuffix(root, into, suffix, leafTyp)
	if err != nil {
		*failed = true
		return node
	}
	return fused
}

// rebaseOuterLegValueOrdinal rewrites leg references in v to baked ordinals
// over mergedQOV. ok=false when a leg reference exists that the windows cannot
// map (dotted field, unknown column, a root the leg layout cannot state) — the
// caller must DECLINE rather than ship a half-rebased tree.
//
// A MULTI-ACCESSOR path is NOT in that list. It used to be, in the sense that
// one of the two arms below refused it while the other truncated it; both are
// now handled by the arity arm, which re-anchors the root and FUSES the
// remaining accessors on.
func rebaseOuterLegValueOrdinal(
	v values.Value,
	windows map[values.CorrelationIdentifier]ordinalLegWindow,
	mergedQOV *values.QuantifiedObjectValue,
) (values.Value, bool) {
	if v == nil {
		return v, true
	}
	failed := false
	out := values.Replace(v, func(node values.Value) values.Value {
		fv, isFV := node.(*values.FieldValue)
		if !isFV {
			return node
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return node
		}
		// An ALREADY-baked positional ref over the MERGED row is final: a multi-esq
		// peel box bakes the existential correlation at plan time
		// (translateUnnestExistsFilter's planTimeBake arm) to an ofOrdinal over the
		// merged QOV, and this executor hoist then runs OVER that baked tree. Its
		// ordinal already indexes the merged layout, so re-baking would add the leg
		// window offset a SECOND time (out of range → !ok, a spurious decline of a
		// correctly-baked box). Pass it through — but ONLY when it is already over the
		// merged QOV. A LEG-LOCAL FrontierPinned ref (`ofOrdinal(QOV(A), i)`, child is
		// a source leg still in `windows`) is NOT final: guarding on the merged
		// correlation lets it fall through to the leg-relative arm below, which
		// translates it onto the merged row at w.Offset+i (by its Field name).
		if fv.Resolved != nil && fv.Resolved.FrontierPinned && qov.Correlation == mergedQOV.Correlation {
			return node
		}
		// The reference's own correlation selects its window. It used to be the
		// UPPER FOLD of that correlation, against a text-keyed map — a round trip
		// through a namespace that merges the two the rest of this package keeps
		// disjoint.
		w, isLeg := windows[qov.Correlation]
		if values.LegIdentityCensusEnabled() {
			// KIND-AWARE: a hit on a NESTED window is its own class, because
			// whether the fused two-step arm is ever ENTERED on this corpus is the
			// live-vs-latent question for RFC-200 step 3d, and every other number
			// on this path prints identically either way.
			values.RecordSeedWindowLookupOfKind(values.SeedWindowSiteExistentialRebase, isLeg, w.Kind)
		}
		if !isLeg {
			// After the buried-window fix (finalizeSeedWindows), EVERY outer/buried
			// leg is a window, so !isLeg means a genuinely non-outer ref (the FlatMap
			// inner) — pass it through. BUT if a known leg boundary (the merged type's
			// Legs) is absent from windows, the two derivations DRIFTED — fail LOUD
			// rather than silently read the inner row. This is a defensive check:
			// unreachable while windows and merged Legs stay in lockstep.
			if rt, ok := mergedQOV.Type().(*values.RecordType); ok {
				for _, leg := range rt.Legs {
					// This hook sits inside a Cascades rule, so its TOTALS count rule
					// firings, not queries: the memo may explore this hoist once or
					// many times for one query. Read the site's absolute numbers as a
					// planning artifact; only its zero fold-only population is a fact
					// about the corpus.
					if values.LegIdentityCensusEnabled() {
						// RETIRED PREDICATE: `leg.Name == strings.ToUpper(corr.Name())` —
						// one of the two sites that folded a side before comparing. A
						// fold-only count cannot see what such a predicate did differently,
						// so the census records the verdict. The fold is spelled out here
						// because the local that used to hold it is gone with the text key.
						values.RecordLegIdentityConversion(
							values.LegSiteLeftOuterExistential, leg.Alias, qov.Correlation,
							leg.Name == strings.ToUpper(qov.Correlation.Name()))
						values.RecordLegIdentityLeg(leg)
					}
					// The drift check asks an IDENTITY question — "is this correlation a
					// known leg boundary?" — so it is answered by the leg's identity
					// against the reference's own correlation, not by the upper fold of
					// one side against the text of the other. Folding here would let the
					// tripwire mistake a case-variant alias for the leg it is guarding.
					if values.SameLeg(leg.Alias, qov.Correlation) {
						failed = true
						break
					}
				}
			}
			return node
		}
		var legOrdinal int
		// The suffix a NESTED leg reference still has to travel after its root
		// lands on the merged row. Empty for every flat reference.
		var suffix []values.ResolvedAccessor
		var suffixInto *values.RecordType
		switch {
		case fv.Resolved != nil && len(fv.Resolved.Accessors) > 1:
			// A NESTED leg reference — `leg.n.sk`, ONE FieldValue whose root
			// accessor is the leg column and whose remaining accessors descend
			// inside it. It is the SAME shape under both bake kinds, which is why
			// this arm sits above the pinned/name dispatch rather than inside
			// either: the two arms below split on the frontier pin, and arity is
			// orthogonal to it, so a multi-accessor path used to take whichever
			// arm its pin selected and be mishandled in a DIFFERENT way by each.
			// The pinned arm declined it (Single() on a fused path). The name arm
			// did something worse — it looked up fv.Field, found the ROOT column,
			// baked the merged address of the STRUCT and dropped the descent. That
			// address is a real merged column, so nothing downstream rejects it:
			// `WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.n.sk)` over a join
			// compared a BIGINT against a whole struct and quietly matched nothing.
			//
			// The root ordinal is DERIVED from the leg's own layout and the carried
			// one is asserted against it, never taken: a carried ordinal is stated
			// in the layout the reference was resolved against, and reading it
			// against a different one lands on whatever column occupies that slot.
			// ReAnchorRootInto is that derive-and-assert, and it declines when the
			// root name is absent or DUPLICATED in the leg — an opaque box leg can
			// expose two buried columns of one name, where a first match is
			// indistinguishable from a correct answer and disambiguating needs the
			// leg identity this site does not carry.
			reanchored, _, reOK := fv.Resolved.ReAnchorRootInto(w.Typ)
			if !reOK {
				failed = true
				return node
			}
			legOrdinal = reanchored.Root().Ordinal
			suffix = reanchored.Accessors[1:]
			// The record the suffix descends into is the LEG column's own type.
			// The merged row states only WHERE the column sits; its per-slot
			// types are derived from the seed's baked columns and are UNKNOWN
			// for a struct column (measured), so descending against them would
			// fail on every real nested reference.
			if legOrdinal >= 0 && legOrdinal < len(w.Typ.Fields) {
				suffixInto, _ = w.Typ.Fields[legOrdinal].FieldType.(*values.RecordType)
			}
		case fv.Resolved != nil && fv.Resolved.FrontierPinned:
			// A LEG-LOCAL baked ref (`ofOrdinal(QOV(A), i)` — child a SOURCE LEG, not
			// the merged QOV, so the precise guard above passed it through to here).
			// Carry its BAKED root ordinal, NOT FieldIndex(Field): an OPAQUE box leg can
			// expose DUPLICATE buried column names (`A.K` and `B.K` merged into one leg),
			// where FieldIndex("K") would remap the already-baked ref to the FIRST match
			// and silently probe the WRONG column (wrong rows). acc.Ordinal is the exact
			// leg-local slot; w.Offset + it = the merged slot. (Empirically no shape
			// produces such a ref — the arm is CORRECT-or-LOUD defensive.)
			//
			// Single() can no longer be the discriminator it once was: a fused path
			// is handled ABOVE, by arity, before the pin is consulted. What remains
			// here is a single-accessor pinned ref, so a !single answer means a
			// path with ZERO accessors — the non-empty invariant violated by a
			// hand-built FieldPath — and declining is the only safe reading.
			acc, single := fv.Resolved.Single()
			if !single {
				failed = true
				return node
			}
			legOrdinal = acc.Ordinal
		case !strings.Contains(fv.Field, "."):
			// A leg-relative NAME ref (non-baked): NewFieldValueOfOrdinal is not its
			// source, so resolve the slot by column name. A single source leg has no
			// duplicate column names, so FieldIndex(Field) is the leg-local ordinal.
			idx, found := w.Typ.FieldIndex(strings.ToUpper(fv.Field))
			if !found {
				failed = true
				return node
			}
			legOrdinal = idx
		default:
			failed = true // dotted lazy ref — not a direct leg column
			return node
		}
		// Bound the ordinal to THIS leg's window before adding w.Offset. The name arm
		// (FieldIndex) is in-range by construction, but a baked acc.Ordinal is relative
		// to its child QOV's FULL type — if `windows[alias]` was narrowed to a
		// buried-leg SUBwindow, a full-concat ordinal could spill past w.Typ into the
		// NEXT leg's slots (w.Offset+legOrdinal is still a valid MERGED ordinal, so
		// NewFieldValueOfOrdinal below would NOT catch it → a silent wrong-column read).
		// Decline instead (correct-or-loud).
		if legOrdinal < 0 || legOrdinal >= len(w.Typ.Fields) {
			failed = true
			return node
		}
		// DISPATCH ON THE KIND, never on the shape. `w.Offset + legOrdinal` is only
		// an address under LegKindFlatRun, where Offset starts a run of the leg's
		// own columns; under LegKindNested Offset names ONE slot holding the whole
		// leg row, and the same arithmetic walks off into the next leg's slots
		// while producing a perfectly valid merged ordinal — a silent wrong-column
		// read that NewFieldValueOfOrdinal cannot catch.
		//
		// LegKindUnset DECLINES rather than defaulting: a window that reached this
		// rebase without a stated kind is a producer bug, and a declined
		// optimization is recoverable while a wrong ordinal is not.
		var rebased *values.FieldValue
		var err error
		switch w.Kind {
		case values.LegKindFlatRun:
			rebased, err = values.NewFieldValueOfOrdinal(mergedQOV, w.Offset+legOrdinal)
		case values.LegKindNested:
			// THE FUSED TWO-STEP ADDRESS — Java's
			// ofOrdinalNumberAndFuseIfPossible, arrived at the same way.
			//
			// Java rewrites a reference to a collapsed sibling as
			// FieldValue.ofOrdinalNumber(QOV(newUpper), index)
			// (PartitionSelectRule.java:301-302). When translateCorrelations then
			// rebuilds the enclosing FieldValue(QOV(lower), [f]), the leaf swap
			// produces FieldValue(FieldValue(QOV(upper),[i]), [f]) and
			// FieldValue.withNewChild → ofFieldsAndFuseIfPossible merges the two
			// accessor lists via FieldPath.withSuffix into the single two-step path
			// FieldValue(QOV(upper), [i, f]). Nothing is materialized and nothing is
			// re-offset — composition is a path FUSION, never a flattening.
			//
			// Go does the same with the same primitives: bake the ONE-step path to
			// the slot, then WithSuffix the leg-local accessor onto it. The
			// executor already reads such a path — FieldValue.descendResolvedPath
			// has an explicit OrdinalRow arm that descends a nested step by ordinal
			// — and the executor's own span side already resolves the fused
			// two-step address in resolveSpanLeaf.
			rebased, err = values.NewFusedFieldValueOfNestedOrdinal(
				mergedQOV, w.Offset, w.Typ, legOrdinal)
			// The fused node's TYPE is authoritative and is NOT overwritten below.
			// It is recomputed from the fused path — leaf column type, promoted
			// nullable if the merged row's slot is nullable — which is Java's
			// ofFieldsAndFuseIfPossible. The reference's own fv.Typ cannot know
			// about the slot step, so taking it here would drop a LEFT-outer
			// null-supplied column's nullability.
			if err == nil {
				return descendOrFail(rebased, suffixInto, suffix, fv.Typ, &failed, node)
			}
		default:
			failed = true
			return node
		}
		if err != nil {
			failed = true
			return node
		}
		if len(suffix) > 0 {
			// A NESTED reference under a FLAT window: the address above reaches
			// the leg COLUMN, which is itself a record, and the rest of the path
			// descends inside it. The descent recomputes the type, so fv.Typ is
			// not taken — the merged row's slot may be nullable and only the
			// descent knows that.
			return descendOrFail(rebased, suffixInto, suffix, fv.Typ, &failed, node)
		}
		// Keep the reference's own column type (the merged layout's field
		// type IS the leg column's — same fv.Typ lineage). FLAT arm only; the
		// nested arm returned above with its recomputed type.
		rebased.Typ = fv.Typ
		return rebased
	})
	if failed {
		return v, false
	}
	return out, true
}

// rebaseOuterLegRefsOrdinal is the predicate-level walk (the ordinal twin of
// rebaseOuterLegRefsToMerged — same predicate shapes).
func rebaseOuterLegRefsOrdinal(
	p predicates.QueryPredicate,
	windows map[values.CorrelationIdentifier]ordinalLegWindow,
	mergedQOV *values.QuantifiedObjectValue,
) (predicates.QueryPredicate, bool) {
	if p == nil {
		return p, true
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand, ok1 := rebaseOuterLegValueOrdinal(pred.Operand, windows, mergedQOV)
		newCompOperand, ok2 := rebaseOuterLegValueOrdinal(pred.Comparison.Operand, windows, mergedQOV)
		if !ok1 || !ok2 {
			return p, false
		}
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p, true
		}
		cmp := pred.Comparison
		cmp.Operand = newCompOperand
		return &predicates.ComparisonPredicate{Operand: newOperand, Comparison: cmp}, true
	case *predicates.ValuePredicate:
		newVal, ok := rebaseOuterLegValueOrdinal(pred.Value, windows, mergedQOV)
		if !ok {
			return p, false
		}
		if newVal == pred.Value {
			return p, true
		}
		return predicates.NewValuePredicate(newVal), true
	case *predicates.AndPredicate:
		changed := false
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			ns, ok := rebaseOuterLegRefsOrdinal(s, windows, mergedQOV)
			if !ok {
				return p, false
			}
			subs[i] = ns
			if ns != s {
				changed = true
			}
		}
		if !changed {
			return p, true
		}
		return predicates.NewAnd(subs...), true
	case *predicates.OrPredicate:
		changed := false
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			ns, ok := rebaseOuterLegRefsOrdinal(s, windows, mergedQOV)
			if !ok {
				return p, false
			}
			subs[i] = ns
			if ns != s {
				changed = true
			}
		}
		if !changed {
			return p, true
		}
		return predicates.NewOr(subs...), true
	case *predicates.NotPredicate:
		newChild, ok := rebaseOuterLegRefsOrdinal(pred.Child, windows, mergedQOV)
		if !ok {
			return p, false
		}
		if newChild == pred.Child {
			return p, true
		}
		return predicates.NewNot(newChild), true
	default:
		// A shape the lazy twin also passes through untouched: safe
		// only if it carries NO leg references — probe and decline if it does.
		//
		// The window's own identity IS the probe key now. This loop used to MINT a
		// CorrelationIdentifier out of the map key's text — `NamedCorrelationIdentifier(alias)`
		// — while the window it came from already stated the identifier being
		// manufactured, and the correlation set it was asking is keyed by identity.
		// So a case-variant window key produced a probe that could never match, and
		// the decline it exists to trigger would not have fired.
		for alias := range windows {
			if _, refs := predicates.GetCorrelatedToOfPredicate(p)[alias]; refs {
				return p, false
			}
		}
		return p, true
	}
}

// ordinalSeedLegWindowsOf is the Value-typed entry: nil windows for anything
// that is not a pristine ordinal seed RC.
func ordinalSeedLegWindowsOf(rv values.Value) (map[values.CorrelationIdentifier]ordinalLegWindow, *values.RecordType) {
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, nil
	}
	return ordinalSeedLegWindows(rc)
}

// ordinalSeedLegWindowsAcceptingNestedOf is ordinalSeedLegWindowsOf over the
// NESTED-accepting entry (RFC-200).
//
// EXACTLY THREE SITES CALL IT, all in rule_implement_nested_loop_join.go and all
// reading the SAME step1RV: foldStep1Seed's validation of the seed it just
// built, implementJoinWithExistential's ordinalWindows derivation, and
// materializedNLJOrdinalLayoutMatches' orientation check.
//
// A FOURTH SITE IS A STOP, NOT A WIDENING. Every other consumer's proof rests on
// the narrow entry's accept set being FROZEN — their answer is unchanged on
// every input they can see, which is stronger than "their meaning is unchanged"
// — and admitting a fourth caller here re-opens all of it. A site that genuinely
// needs the nested windows is a change to RFC-200's design, not an addition to
// this list: the per-consumer argument has to be re-made before it can be added.
func ordinalSeedLegWindowsAcceptingNestedOf(rv values.Value) (map[values.CorrelationIdentifier]ordinalLegWindow, *values.RecordType) {
	w, m, _ := ordinalSeedLegLayoutOf(rv)
	return w, m
}

// ordinalSeedLegLayoutOf adds the TOP-LEVEL RUN LIST — the windows that TILE the
// merged row, in offset order — for the one consumer that asks a positional
// question about the whole row rather than addressing one leg of it.
//
// The map cannot answer it. finalizeSeedWindows' rightmost-leaf case REPLACES a
// box run's own entry with a narrower sub-window, so after finalization the map
// no longer states which windows tile the row, and a narrowed tile is
// indistinguishable from a leg that was always that narrow.
func ordinalSeedLegLayoutOf(rv values.Value) (map[values.CorrelationIdentifier]ordinalLegWindow, *values.RecordType, []ordinalLegWindow) {
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, nil, nil
	}
	return values.OrdinalSeedLegLayout(rc)
}

// rebasePlanOuterRefsOrdinal is the PLAN-TREE twin of the lazy
// rebasePlanBuriedRefs for an ordinal seed: it rewrites every outer-leg
// reference BURIED INSIDE an already-built plan tree to a baked
// ofOrdinalNumber over the merged positional row, exactly as
// rebaseOuterLegRefsOrdinal does for the LIFTED existential predicates. The
// buried class exists because an EXISTS whose inner WHERE references ONLY
// outer legs (`EXISTS (SELECT 1 FROM q WHERE a.v = 10)`) keeps that
// predicate in its own subplan — existsInnerCorrelation lifts only
// inner↔outer correlation predicates — and the step-2 FlatMap binds ONLY
// the merged correlation: the buried QOV(leg) is unbound at runtime and the
// frontier fallback evaluates the field against the inner scan's own row
// (a loud OrdinalResolutionError on the ordinal frontier; silent wrong rows
// pre-ordinal). ok=false when a leg reference exists that the windows cannot
// map — the caller must DECLINE the yield (CORRECT-or-LOUD). Unhandled node
// kinds are returned unchanged; the caller's planReferencesAnyBuriedAlias
// verification catches any leg reference that survives there — this
// two-step translation is fail-closed end to end.
func rebasePlanOuterRefsOrdinal(
	p plans.RecordQueryPlan,
	windows map[values.CorrelationIdentifier]ordinalLegWindow,
	mergedQOV *values.QuantifiedObjectValue,
) (plans.RecordQueryPlan, bool) {
	if p == nil || len(windows) == 0 {
		return p, true
	}
	switch pl := p.(type) {
	case *plans.RecordQueryIndexPlan:
		newComps, changed, ok := rebaseComparisonRangesOrdinal(pl.GetScanComparisons(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		if !changed {
			return p, true
		}
		return pl.WithScanComparisons(newComps), true
	case *plans.RecordQueryCoveringIndexPlan:
		// The SARGs live on the scan this wrapper HOLDS AS A FIELD, so the
		// pass-through arms below never reach them — and the access path builds
		// Fetch(Covering(IndexScan)) for every index-backed access, so that is
		// the shape a correlated probe arrives in. Rebase the inner and rebuild
		// the wrapper over it (WithIndexPlan re-derives the covered columns, so
		// the entry layout cannot drift from the scan).
		//
		// Omitting it does not silently mis-answer: the caller's
		// planReferencesAnyBuriedAlias verification sees the surviving leg
		// reference and declines. But the decline is TOTAL rather than
		// occasional, which reads as "this optimization does not apply" instead
		// of "this walk cannot see the node".
		inner, ok := plans.IndexPlanOf(pl)
		if !ok {
			return p, false
		}
		rebased, ok := rebasePlanOuterRefsOrdinal(inner, windows, mergedQOV)
		if !ok {
			return p, false
		}
		rebasedIdx, isIdx := rebased.(*plans.RecordQueryIndexPlan)
		if !isIdx {
			return p, false
		}
		if rebasedIdx == inner {
			return p, true
		}
		return pl.WithIndexPlan(rebasedIdx), true
	case *plans.RecordQueryScanPlan:
		newComps, changed, ok := rebaseComparisonRangesOrdinal(pl.GetScanComparisons(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		if !changed {
			return p, true
		}
		return pl.WithScanComparisons(newComps), true
	case *plans.RecordQueryPredicatesFilterPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			np, prOK := rebaseOuterLegRefsOrdinal(pr, windows, mergedQOV)
			if !prOK {
				return p, false
			}
			newPreds[i] = np
			if np != pr {
				changed = true
			}
		}
		if !changed {
			return p, true
		}
		return plans.NewRecordQueryPredicatesFilterPlanWithAlias(inner, newPreds, pl.GetInnerAlias()), true
	case *plans.RecordQueryFilterPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			np, prOK := rebaseOuterLegRefsOrdinal(pr, windows, mergedQOV)
			if !prOK {
				return p, false
			}
			newPreds[i] = np
			if np != pr {
				changed = true
			}
		}
		if !changed {
			return p, true
		}
		return plans.NewRecordQueryFilterPlan(newPreds, inner), true
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		return plans.NewRecordQueryFetchFromPartialRecordPlan(inner, pl.GetTranslateValueFunction(), pl.GetResultType(), pl.GetFetchIndexRecords()), true
	case *plans.RecordQueryDefaultOnEmptyPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		return plans.NewRecordQueryDefaultOnEmptyPlan(inner, pl.GetDefaultValue()), true
	case *plans.RecordQueryFirstOrDefaultPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		if pl.IsStrict() {
			return plans.NewRecordQueryFirstOrDefaultPlanStrict(inner, pl.GetDefaultValue()), true
		}
		return plans.NewRecordQueryFirstOrDefaultPlan(inner, pl.GetDefaultValue()), true
	case *plans.RecordQueryTypeFilterPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		return plans.NewRecordQueryTypeFilterPlan(pl.GetRecordTypes(), inner), true
	case *plans.RecordQueryMapPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		newResult, rvOK := rebaseOuterLegValueOrdinal(pl.GetResultValue(), windows, mergedQOV)
		if !rvOK {
			return p, false
		}
		if inner == pl.GetInner() && newResult == pl.GetResultValue() {
			return p, true
		}
		return plans.NewRecordQueryMapPlan(inner, newResult), true
	case *plans.RecordQueryProjectionPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV)
		if !ok {
			return p, false
		}
		projs := pl.GetProjections()
		newProjs := make([]values.Value, len(projs))
		changed := inner != pl.GetInner()
		for i, v := range projs {
			nv, vOK := rebaseOuterLegValueOrdinal(v, windows, mergedQOV)
			if !vOK {
				return p, false
			}
			newProjs[i] = nv
			if nv != v {
				changed = true
			}
		}
		if !changed {
			return p, true
		}
		// A rebase hands back "the same projection, moved": the output names and
		// WHO wrote them are both unchanged by moving where the ordinals point.
		return plans.NewRecordQueryProjectionPlanWithAliases(newProjs, pl.GetAliases(), inner).
			WithAliasProvenance(pl.GetAliasMinted()), true
	default:
		// Unhandled node — return unchanged. The caller's
		// planReferencesAnyBuriedAlias verification declines any buried leg
		// reference that survives here.
		return p, true
	}
}

// rebaseComparisonRangesOrdinal rebases the outer-leg references in SARG
// comparison ranges to baked ordinals over mergedQOV (the ordinal twin of
// rebaseComparisonRanges). Returns the new ranges, whether any changed, and
// ok=false when a leg reference cannot be mapped.
func rebaseComparisonRangesOrdinal(
	comps []*predicates.ComparisonRange,
	windows map[values.CorrelationIdentifier]ordinalLegWindow,
	mergedQOV *values.QuantifiedObjectValue,
) ([]*predicates.ComparisonRange, bool, bool) {
	out := make([]*predicates.ComparisonRange, len(comps))
	changed := false
	for i, cr := range comps {
		nc, ch, ok := rebaseComparisonRangeOrdinal(cr, windows, mergedQOV)
		if !ok {
			return comps, false, false
		}
		out[i] = nc
		if ch {
			changed = true
		}
	}
	return out, changed, true
}

// rebaseComparisonRangeOrdinal rebases one comparison range's operands (the
// ordinal twin of rebaseComparisonRange). A rebuilt range that cannot be
// re-merged fails closed (ok=false) — never a half-rebased SARG.
func rebaseComparisonRangeOrdinal(
	cr *predicates.ComparisonRange,
	windows map[values.CorrelationIdentifier]ordinalLegWindow,
	mergedQOV *values.QuantifiedObjectValue,
) (*predicates.ComparisonRange, bool, bool) {
	if cr == nil || cr.IsEmpty() {
		return cr, false, true
	}
	var comparisons []*predicates.Comparison
	if cr.IsEquality() {
		comparisons = []*predicates.Comparison{cr.GetEqualityComparison()}
	} else {
		comparisons = cr.GetInequalityComparisons()
	}
	// Rebase pass first; an untouched range returns unchanged WITHOUT the
	// Merge round-trip below (mirroring rebaseComparisonRange: a range whose
	// comparisons don't re-merge must not fail a plan the rebase never
	// touched — only a genuinely-rebased range that cannot re-merge fails).
	rebased := make([]*predicates.Comparison, len(comparisons))
	changed := false
	for i, c := range comparisons {
		rebased[i] = c
		if c == nil || c.Operand == nil {
			continue
		}
		newOperand, ok := rebaseOuterLegValueOrdinal(c.Operand, windows, mergedQOV)
		if !ok {
			return cr, false, false
		}
		if newOperand != c.Operand {
			cp := *c
			cp.Operand = newOperand
			rebased[i] = &cp
			changed = true
		}
	}
	if !changed {
		return cr, false, true
	}
	rebuilt := predicates.EmptyComparisonRange()
	for _, nc := range rebased {
		res := rebuilt.Merge(nc)
		if !res.Ok {
			return cr, false, false
		}
		rebuilt = res.Range
	}
	return rebuilt, true, true
}
