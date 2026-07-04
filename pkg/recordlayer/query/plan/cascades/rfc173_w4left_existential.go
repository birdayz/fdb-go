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

// ordinalLegWindow is one leg's window in the merged positional row.
type ordinalLegWindow struct {
	Offset int
	Typ    *values.RecordType
}

// ordinalSeedLegWindows derives per-leg windows (UPPER alias → offset+type)
// from a PRISTINE gated ordinal seed RC — every field a single-accessor
// frontier-pinned bake over a leg QOV, consecutive full-coverage runs (the
// AssertOrdinalJoinSeed shape, decline-not-panic). Also returns the merged
// row's RecordType (the birthed positional row's layout: output field names
// in seed order — duplicates SURVIVE, positional access is by ordinal, so
// the raw composite literal is deliberate). nil windows = not a pristine
// seed (anchored, translated/fused, folded) — the caller keeps the
// name-model path or declines.
func ordinalSeedLegWindows(rc *values.RecordConstructorValue) (map[string]ordinalLegWindow, *values.RecordType) {
	if rc == nil || rc.AnchoredJoin || len(rc.Fields) == 0 {
		return nil, nil
	}
	windows := map[string]ordinalLegWindow{}
	mergedFields := make([]values.Field, 0, len(rc.Fields))
	var curAlias string
	var curStart int
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return nil, nil
		}
		acc, single := fv.Resolved.Single()
		if !single {
			return nil, nil
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return nil, nil
		}
		legType, isRT := qov.Typ.(*values.RecordType)
		if !isRT {
			return nil, nil
		}
		alias := strings.ToUpper(qov.Correlation.Name())
		if alias != curAlias {
			if _, dup := windows[alias]; dup {
				return nil, nil // a split run — not pristine
			}
			if acc.Ordinal != 0 {
				return nil, nil
			}
			curAlias = alias
			curStart = i
			windows[alias] = ordinalLegWindow{Offset: curStart, Typ: legType}
		} else if acc.Ordinal != i-curStart {
			return nil, nil
		}
		mergedFields = append(mergedFields, values.Field{Name: f.Name, FieldType: fv.Typ, Ordinal: i})
	}
	// Full coverage per leg (the run width equals the leg type's field count).
	counts := map[string]int{}
	for _, f := range rc.Fields {
		fv := f.Value.(*values.FieldValue)
		qov := fv.Child.(*values.QuantifiedObjectValue)
		counts[strings.ToUpper(qov.Correlation.Name())]++
	}
	for alias, w := range windows {
		if counts[alias] != len(w.Typ.Fields) {
			return nil, nil
		}
	}
	return windows, &values.RecordType{Fields: mergedFields}
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
