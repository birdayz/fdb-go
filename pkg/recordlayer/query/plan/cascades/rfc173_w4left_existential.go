package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return node
		}
		w, isLeg := windows[strings.ToUpper(qov.Correlation.Name())]
		if !isLeg {
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
