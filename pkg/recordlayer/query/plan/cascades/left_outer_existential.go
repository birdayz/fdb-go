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

// rebaseOuterLegValueOrdinal rewrites leg references in v to baked ordinals
// over mergedQOV. ok=false when a leg reference exists that the windows
// cannot map (multi-accessor path, dotted field, unknown column) — the
// caller must DECLINE rather than ship a half-rebased tree.
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
			values.RecordSeedWindowLookup(values.SeedWindowSiteExistentialRebase, isLeg)
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
		switch {
		case fv.Resolved != nil && fv.Resolved.FrontierPinned:
			// A LEG-LOCAL baked ref (`ofOrdinal(QOV(A), i)` — child a SOURCE LEG, not
			// the merged QOV, so the precise guard above passed it through to here).
			// Carry its BAKED root ordinal, NOT FieldIndex(Field): an OPAQUE box leg can
			// expose DUPLICATE buried column names (`A.K` and `B.K` merged into one leg),
			// where FieldIndex("K") would remap the already-baked ref to the FIRST match
			// and silently probe the WRONG column (wrong rows). acc.Ordinal is the exact
			// leg-local slot; w.Offset + it = the merged slot. (Empirically no shape
			// produces such a ref — the arm is CORRECT-or-LOUD defensive: a
			// multi-accessor path declines the yield.)
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
			var slot *values.FieldValue
			slot, err = values.NewFieldValueOfOrdinal(mergedQOV, w.Offset)
			if err == nil {
				var leaf *values.FieldValue
				leaf, err = values.NewFieldValueOfOrdinal(
					values.NewQuantifiedObjectValueOfType(qov.Correlation, w.Typ), legOrdinal)
				if err == nil {
					fused := *slot
					fused.Resolved = slot.Resolved.WithSuffix(leaf.Resolved)
					// The DISPLAY name is the leaf's, matching what a one-step bake
					// would have rendered. It is rendering only — a baked node's
					// identity is its ordinal PATH alone (Java's ResolvedAccessor
					// equality compares getOrdinal() only).
					fused.Field = leaf.Field
					rebased = &fused
				}
			}
		default:
			failed = true
			return node
		}
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
	rc, isRC := rv.(*values.RecordConstructorValue)
	if !isRC {
		return nil, nil
	}
	return values.OrdinalSeedLegWindowsAcceptingNested(rc)
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
