package cascades

import (
	"bytes"

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
	mergedQOV values.QuantifiedObjectValue,
	expectedLegs ...map[values.CorrelationIdentifier]struct{},
) (values.Value, bool) {
	if v == nil || mergedQOV == nil {
		return v, true
	}
	failed := false
	out := rewriteFieldValues(v, func(fv values.FieldValue) (values.Value, bool) {
		qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !isQOV {
			failed = true
			return nil, false
		}
		// An address already rooted at this exact merged object is final. Compare
		// the complete QOV identity, not just the correlation: the same alias with
		// a different flowed type is a malformed phase transfer and must not be
		// mistaken for a no-op.
		if qov.Correlation() == mergedQOV.Correlation() {
			if !sameExactType(qov.FlowedType(), mergedQOV.FlowedType()) {
				failed = true
				return nil, false
			}
			return fv, true
		}

		// The source QOV identity selects its window. Display names never
		// participate in the mapping; duplicate and dotted SQL names therefore
		// cannot redirect the address.
		w, isLeg := windows[qov.Correlation()]
		if values.LegIdentityCensusEnabled() {
			// KIND-AWARE: a hit on a NESTED window is its own class, because
			// whether the fused two-step arm is ever ENTERED on this corpus is the
			// live-vs-latent question for RFC-200 step 3d, and every other number
			// on this path prints identically either way.
			values.RecordSeedWindowLookupOfKind(values.SeedWindowSiteExistentialRebase, isLeg, w.Kind)
		}
		if !isLeg {
			// A missing window is safe only for a genuinely external/inner root.
			// The caller supplies the complete outer-source manifest separately
			// from the physical windows so a derivation bug cannot turn a known
			// outer root into an unchecked source-bound read.
			for _, expected := range expectedLegs {
				if _, isExpected := expected[qov.Correlation()]; isExpected {
					failed = true
					return nil, false
				}
			}
			// A genuinely non-outer reference (for example the FlatMap inner)
			// is outside this layout and remains source-bound.
			return fv, true
		}
		path := fv.Path()
		if path == nil || path.Len() == 0 || w.Typ == nil {
			failed = true
			return nil, false
		}
		ordinals := path.Ordinals()
		legOrdinal := ordinals[0]
		// Bound the ordinal to THIS leg's window before adding w.Offset. The name arm
		// (FieldIndexUnique) is in-range by construction, but a baked acc.Ordinal is relative
		// to its child QOV's FULL type — if `windows[alias]` was narrowed to a
		// buried-leg SUBwindow, a full-concat ordinal could spill past w.Typ into the
		// NEXT leg's slots (w.Offset+legOrdinal is still a valid MERGED ordinal, so
		// NewFieldValueOfOrdinal below would NOT catch it → a silent wrong-column read).
		// Decline instead (correct-or-loud).
		if legOrdinal < 0 || legOrdinal >= len(w.Typ.Fields) {
			failed = true
			return nil, false
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
		var mapped []int
		switch w.Kind {
		case values.LegKindFlatRun:
			mapped = make([]int, 0, len(ordinals))
			mapped = append(mapped, w.Offset+legOrdinal)
			mapped = append(mapped, ordinals[1:]...)
		case values.LegKindNested:
			mapped = make([]int, 0, len(ordinals)+1)
			mapped = append(mapped, w.Offset)
			mapped = append(mapped, ordinals...)
		default:
			failed = true
			return nil, false
		}
		rebased, err := values.ResolveFieldOrdinals(mergedQOV, mapped)
		if err != nil {
			failed = true
			return nil, false
		}
		if !sameExactType(rebased.Type(), fv.ResultType()) {
			failed = true
			return nil, false
		}
		return rebased, true
	})
	if failed {
		return v, false
	}
	return out, true
}

// rewriteFieldValues is the checked, copy-on-write walk used by the ordinal
// reanchor. FieldValues are intercepted before generic reconstruction so a
// changed child can never be swallowed by the legacy infallible WithChildren
// arm. All admitted FieldValues are canonical QOV-rooted nodes.
func rewriteFieldValues(v values.Value, rewrite func(values.FieldValue) (values.Value, bool)) values.Value {
	if v == nil {
		return nil
	}
	if field, ok := values.AsFieldValue(v); ok {
		rewritten, accepted := rewrite(field)
		if !accepted {
			return nil
		}
		return rewritten
	}
	children := v.Children()
	if len(children) == 0 {
		return v
	}
	changed := false
	rebuiltChildren := make([]values.Value, len(children))
	for i, child := range children {
		rebuiltChildren[i] = rewriteFieldValues(child, rewrite)
		if rebuiltChildren[i] == nil {
			return nil
		}
		changed = changed || rebuiltChildren[i] != child
	}
	if !changed {
		return v
	}
	return values.WithChildren(v, rebuiltChildren)
}

func sameExactType(left, right values.Type) bool {
	lh, leftErr := values.SnapshotExactType(left)
	rh, rightErr := values.SnapshotExactType(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(lh.CanonicalBytes(), rh.CanonicalBytes())
}

// rebaseOuterLegRefsOrdinal is the predicate-level walk (the ordinal twin of
// rebaseOuterLegRefsToMerged — same predicate shapes).
func rebaseOuterLegRefsOrdinal(
	p predicates.QueryPredicate,
	windows map[values.CorrelationIdentifier]ordinalLegWindow,
	mergedQOV values.QuantifiedObjectValue,
	expectedLegs ...map[values.CorrelationIdentifier]struct{},
) (predicates.QueryPredicate, bool) {
	if p == nil {
		return p, true
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand, ok1 := rebaseOuterLegValueOrdinal(pred.Operand, windows, mergedQOV, expectedLegs...)
		newCompOperand, ok2 := rebaseOuterLegValueOrdinal(pred.Comparison.Operand, windows, mergedQOV, expectedLegs...)
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
		newVal, ok := rebaseOuterLegValueOrdinal(pred.Value, windows, mergedQOV, expectedLegs...)
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
			ns, ok := rebaseOuterLegRefsOrdinal(s, windows, mergedQOV, expectedLegs...)
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
			ns, ok := rebaseOuterLegRefsOrdinal(s, windows, mergedQOV, expectedLegs...)
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
		newChild, ok := rebaseOuterLegRefsOrdinal(pred.Child, windows, mergedQOV, expectedLegs...)
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
		for _, expected := range expectedLegs {
			for alias := range expected {
				if _, refs := predicates.GetCorrelatedToOfPredicate(p)[alias]; refs {
					return p, false
				}
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
	mergedQOV values.QuantifiedObjectValue,
	expectedLegs ...map[values.CorrelationIdentifier]struct{},
) (plans.RecordQueryPlan, bool) {
	if p == nil || len(windows) == 0 {
		return p, true
	}
	switch pl := p.(type) {
	case *plans.RecordQueryIndexPlan:
		newComps, changed, ok := rebaseComparisonRangesOrdinal(pl.GetScanComparisons(), windows, mergedQOV, expectedLegs...)
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
		rebased, ok := rebasePlanOuterRefsOrdinal(inner, windows, mergedQOV, expectedLegs...)
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
		newComps, changed, ok := rebaseComparisonRangesOrdinal(pl.GetScanComparisons(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		if !changed {
			return p, true
		}
		return pl.WithScanComparisons(newComps), true
	case *plans.RecordQueryPredicatesFilterPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			np, prOK := rebaseOuterLegRefsOrdinal(pr, windows, mergedQOV, expectedLegs...)
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
		rebuilt, err := plans.NewRecordQueryPredicatesFilterPlanWithAlias(inner, newPreds, pl.GetInnerAlias())
		return rebuilt, err == nil
	case *plans.RecordQueryFilterPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		preds := pl.GetPredicates()
		newPreds := make([]predicates.QueryPredicate, len(preds))
		changed := inner != pl.GetInner()
		for i, pr := range preds {
			np, prOK := rebaseOuterLegRefsOrdinal(pr, windows, mergedQOV, expectedLegs...)
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
		rebuilt, err := plans.NewRecordQueryFilterPlan(newPreds, inner)
		return rebuilt, err == nil
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		rebuilt, err := plans.NewRecordQueryFetchFromPartialRecordPlan(inner, pl.GetTranslateValueFunction(), pl.GetResultType(), pl.GetFetchIndexRecords())
		return rebuilt, err == nil
	case *plans.RecordQueryDefaultOnEmptyPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		rebuilt, err := plans.NewRecordQueryDefaultOnEmptyPlan(inner, pl.GetDefaultValue())
		return rebuilt, err == nil
	case *plans.RecordQueryFirstOrDefaultPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		if pl.IsStrict() {
			rebuilt, err := plans.NewRecordQueryFirstOrDefaultPlanStrict(inner, pl.GetDefaultValue())
			return rebuilt, err == nil
		}
		rebuilt, err := plans.NewRecordQueryFirstOrDefaultPlan(inner, pl.GetDefaultValue())
		return rebuilt, err == nil
	case *plans.RecordQueryTypeFilterPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		if inner == pl.GetInner() {
			return p, true
		}
		rebuilt, err := plans.NewRecordQueryTypeFilterPlan(pl.GetRecordTypes(), inner)
		return rebuilt, err == nil
	case *plans.RecordQueryMapPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		newResult, rvOK := rebaseOuterLegValueOrdinal(pl.GetResultValue(), windows, mergedQOV, expectedLegs...)
		if !rvOK {
			return p, false
		}
		if inner == pl.GetInner() && newResult == pl.GetResultValue() {
			return p, true
		}
		rebuilt, err := plans.NewRecordQueryMapPlan(inner, newResult)
		return rebuilt, err == nil
	case *plans.RecordQueryProjectionPlan:
		inner, ok := rebasePlanOuterRefsOrdinal(pl.GetInner(), windows, mergedQOV, expectedLegs...)
		if !ok {
			return p, false
		}
		projs := pl.GetProjections()
		newProjs := make([]values.Value, len(projs))
		changed := inner != pl.GetInner()
		for i, v := range projs {
			nv, vOK := rebaseOuterLegValueOrdinal(v, windows, mergedQOV, expectedLegs...)
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
		rebuilt, err := plans.NewRecordQueryProjectionPlanWithOutputSchema(
			newProjs, pl.GetAliases(), pl.GetAliasMinted(), pl.GetOutputNames(), inner)
		if err != nil {
			return p, false
		}
		rebuilt, err = rebuilt.WithAliasSources(pl.GetAliasSources())
		if err != nil {
			return p, false
		}
		return rebuilt, true
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
	mergedQOV values.QuantifiedObjectValue,
	expectedLegs ...map[values.CorrelationIdentifier]struct{},
) ([]*predicates.ComparisonRange, bool, bool) {
	out := make([]*predicates.ComparisonRange, len(comps))
	changed := false
	for i, cr := range comps {
		nc, ch, ok := rebaseComparisonRangeOrdinal(cr, windows, mergedQOV, expectedLegs...)
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
	mergedQOV values.QuantifiedObjectValue,
	expectedLegs ...map[values.CorrelationIdentifier]struct{},
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
		newOperand, ok := rebaseOuterLegValueOrdinal(c.Operand, windows, mergedQOV, expectedLegs...)
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
