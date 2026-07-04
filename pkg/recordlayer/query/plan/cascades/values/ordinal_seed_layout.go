package values

import "strings"

// OrdinalSeedLegWindow is one leg's window in a pristine ordinal join seed's
// merged positional layout: the leg's starting slot and its flowed record
// type (design ruling on the W4-left slice: the layout derivation lives in
// ONE place — this package — with the planner's existential rebase
// delegating here and the executor's span derivation pinned to agree by a
// cross-agreement fixture; independent walks drift, and layout drift is
// wrong-offset wrong-rows).
type OrdinalSeedLegWindow struct {
	Offset int
	Typ    *RecordType
}

// OrdinalSeedLegWindows derives per-leg windows (UPPER alias → window, in a
// map) plus the merged row's RecordType from a PRISTINE gated ordinal seed
// RC — every field a single-accessor frontier-pinned bake over a leg QOV,
// consecutive full-coverage runs (AssertOrdinalJoinSeed's shape,
// decline-not-panic: nil windows for anything else — anchored, translated/
// fused, folded). The merged type's field names are the seed's OUTPUT names
// in order; duplicates SURVIVE (positional access), hence the raw composite
// literal.
func OrdinalSeedLegWindows(rc *RecordConstructorValue) (map[string]OrdinalSeedLegWindow, *RecordType) {
	if rc == nil || rc.AnchoredJoin || len(rc.Fields) == 0 {
		return nil, nil
	}
	windows := map[string]OrdinalSeedLegWindow{}
	mergedFields := make([]Field, 0, len(rc.Fields))
	var curAlias string
	var curStart int
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return nil, nil
		}
		acc, single := fv.Resolved.Single()
		if !single {
			return nil, nil
		}
		qov, isQOV := fv.Child.(*QuantifiedObjectValue)
		if !isQOV {
			return nil, nil
		}
		legType, isRT := qov.Typ.(*RecordType)
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
			windows[alias] = OrdinalSeedLegWindow{Offset: curStart, Typ: legType}
		} else if acc.Ordinal != i-curStart {
			return nil, nil
		}
		mergedFields = append(mergedFields, Field{Name: f.Name, FieldType: fv.Typ, Ordinal: i})
	}
	// Full coverage per leg (run width == the leg type's field count).
	counts := map[string]int{}
	for _, f := range rc.Fields {
		fv := f.Value.(*FieldValue)
		qov := fv.Child.(*QuantifiedObjectValue)
		counts[strings.ToUpper(qov.Correlation.Name())]++
	}
	for alias, w := range windows {
		if counts[alias] != len(w.Typ.Fields) {
			return nil, nil
		}
	}
	return windows, &RecordType{Fields: mergedFields}
}
