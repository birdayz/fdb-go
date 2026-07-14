package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RFC-173 W4-left commit 2 — the ORDINAL existential rebase. The
// EXISTS-over-join implementation binds the inner join's merged row under a
// fresh correlation and rewrites outer-leg references so they resolve inside
// the existential FlatMap. The name-model form (rebaseOuterLegRefsToMerged)
// rewrites them to LAZY qualified "LEG.COL" reads over the merged Datum —
// machinery the FrontierPinned panic bars from BAKED references (a silent
// baked→lazy degradation). For a GATED ordinal seed the correct target is
// the merged POSITIONAL row: each leg reference — baked (ofOrdinal over the
// leg QOV) or lazy (FieldValue(QOV(leg), col)) — rebases to a BAKED
// ofOrdinalNumber over the merged correlation at legOffset+columnOrdinal,
// with the offsets derived from the seed's own per-leg runs. Java's model
// keeps per-quantifier correlations in one FlatMap level; Go's two-level
// NLJ→FlatMap makes the merged positional row the binding, so the ordinal
// offset rebase is the per-quantifier edge composed with the seed layout.
// Any reference the windows cannot map DECLINES the yield (CORRECT-or-LOUD;
// the name-model machinery stays dead for gated seeds).

// ordinalLegWindow aliases the values-level layout window (the derivation
// was CONSOLIDATED into values.OrdinalSeedLegWindows per the impl-review
// condition: the executor's span derivation is pinned to agree with the
// same function by a cross-agreement fixture — independent walks drift,
// and layout drift is wrong-offset wrong-rows).
type ordinalLegWindow = values.OrdinalSeedLegWindow

func ordinalSeedLegWindows(rc *values.RecordConstructorValue) (map[string]ordinalLegWindow, *values.RecordType) {
	return values.OrdinalSeedLegWindows(rc)
}

// rebaseOuterLegValueOrdinal rewrites leg references in v to baked ordinals
// over mergedQOV. ok=false when a leg reference exists that the windows
// cannot map (multi-accessor path, dotted field, unknown column) — the
// caller must DECLINE rather than ship a half-rebased tree.
func rebaseOuterLegValueOrdinal(
	v values.Value,
	windows map[string]ordinalLegWindow,
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
		// An ALREADY-baked positional ref (FrontierPinned ofOrdinal) is the gated
		// seed's final positional form — a multi-esq peel box bakes the existential
		// correlation at plan time (translateUnnestExistsFilter's planTimeBake arm),
		// and this executor hoist then runs OVER that baked tree. Re-baking would add
		// the leg window offset a SECOND time (out of range → !ok, a spurious decline
		// of a correctly-baked box). Pass it through: FrontierPinned refs are already
		// positional and are never leg-relative name refs (the only thing this rebase
		// exists to convert), so this is a pure idempotence guard.
		if fv.Resolved != nil && fv.Resolved.FrontierPinned {
			return node
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return node
		}
		alias := strings.ToUpper(qov.Correlation.Name())
		w, isLeg := windows[alias]
		if !isLeg {
			// After the buried-window fix (finalizeSeedWindows), EVERY outer/buried
			// leg is a window, so !isLeg means a genuinely non-outer ref (the FlatMap
			// inner) — pass it through. BUT if a known leg boundary (the merged type's
			// Legs) is absent from windows, the two derivations DRIFTED — fail LOUD
			// rather than silently read the inner row (the c5a silent-miss sentinel;
			// unreachable while windows and merged Legs stay in lockstep).
			if rt, ok := mergedQOV.Type().(*values.RecordType); ok {
				for _, leg := range rt.Legs {
					if leg.Name == alias {
						failed = true
						break
					}
				}
			}
			return node
		}
		var legOrdinal int
		switch {
		case fv.Resolved != nil && fv.Resolved.FrontierPinned:
			acc, single := fv.Resolved.Single()
			if !single {
				failed = true
				return node
			}
			legOrdinal = acc.Ordinal
		case !strings.Contains(fv.Field, "."):
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
		rebased, err := values.NewFieldValueOfOrdinal(mergedQOV, w.Offset+legOrdinal)
		if err != nil {
			failed = true
			return node
		}
		// Keep the reference's own column type (the merged layout's field
		// type IS the leg column's — same fv.Typ lineage).
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
	windows map[string]ordinalLegWindow,
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
		// A shape the name-model twin also passes through untouched: safe
		// only if it carries NO leg references — probe and decline if it does.
		for alias := range windows {
			if _, refs := predicates.GetCorrelatedToOfPredicate(p)[values.NamedCorrelationIdentifier(alias)]; refs {
				return p, false
			}
		}
		return p, true
	}
}

// ordinalSeedLegWindowsOf is the Value-typed entry: nil windows for anything
// that is not a pristine gated ordinal seed RC.
func ordinalSeedLegWindowsOf(rv values.Value) (map[string]ordinalLegWindow, *values.RecordType) {
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, nil
	}
	return ordinalSeedLegWindows(rc)
}

// rebasePlanOuterRefsOrdinal is the PLAN-TREE twin of the name-model
// rebasePlanBuriedRefs for a GATED ordinal seed: it rewrites every outer-leg
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
// verification catches any leg reference that survives there (the 1+1
// path's fail-closed convention).
func rebasePlanOuterRefsOrdinal(
	p plans.RecordQueryPlan,
	windows map[string]ordinalLegWindow,
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
		return plans.NewRecordQueryProjectionPlanWithAliases(newProjs, pl.GetAliases(), inner), true
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
	windows map[string]ordinalLegWindow,
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
	windows map[string]ordinalLegWindow,
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
