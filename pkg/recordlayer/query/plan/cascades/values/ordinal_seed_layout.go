package values

import (
	"sort"
	"strings"
)

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
// map) plus the merged row's RecordType from a gated ordinal seed RC. TWO shapes
// are accepted (decline-not-panic — nil windows for anything else: anchored,
// translated/fused, folded, S3 positional-merge):
//
//   - PRISTINE (fully-baked AS+AT, or the 2+1 join seed): EVERY field a
//     single-accessor frontier-pinned bake over a leg QOV, consecutive
//     full-coverage runs (AssertOrdinalJoinSeed's shape).
//   - MIXED single-source lateral-unnest (W4c no-AT): a full baked OUTER leg run
//     followed by EXACTLY ONE trailing bare-QuantifiedObjectValue element over a
//     NON-record type (Java's isPrimitive() whole-object scalar element, which
//     cannot be ofOrdinal-baked). Its OWN 1-field leg window is synthesized so
//     `<AS>.<AS>` resolves positionally — the element and the outer each carry
//     their own ALIAS.COL namespace, which is what stops a name shared by the
//     element AS alias and an outer column from mis-resolving.
//
// This MUST agree bit-for-bit with the executor's ordinalJoinSpans/
// unnestMixedSeedSpans (the cross-agreement invariant — independent walks drift,
// and layout drift is wrong-offset wrong-rows; pinned by a fixture).
//
// The merged type's field names are the seed's OUTPUT names in order (the element
// name uppercased to match the executor); duplicates SURVIVE (positional access).
func OrdinalSeedLegWindows(rc *RecordConstructorValue) (map[string]OrdinalSeedLegWindow, *RecordType) {
	if rc == nil || rc.AnchoredJoin || len(rc.Fields) == 0 {
		return nil, nil
	}
	n := len(rc.Fields)
	// A trailing bare-QOV-over-non-record element marks the MIXED seed; its window
	// is synthesized after the baked prefix. Every other trailing shape (a baked
	// field, or a bare QOV over a RECORD leg — the S3 positional-merge RC) leaves
	// prefixEnd == n and takes the all-pristine path.
	prefixEnd := n
	if isMixedSeedElement(rc.Fields[n-1]) {
		prefixEnd = n - 1
	}
	windows := map[string]OrdinalSeedLegWindow{}
	mergedFields := make([]Field, n)
	counts := map[string]int{}
	var curAlias string
	var curStart int
	for i := 0; i < prefixEnd; i++ {
		f := rc.Fields[i]
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
		counts[alias]++
		mergedFields[i] = Field{Name: f.Name, FieldType: fv.Typ, Ordinal: i}
	}
	// Full coverage per baked leg (run width == the leg type's field count).
	for alias, w := range windows {
		if counts[alias] != len(w.Typ.Fields) {
			return nil, nil
		}
	}
	if prefixEnd == n {
		// ACCEPT-EQUIVALENCE with the executor's ordinalJoinSpansOf (`len(spans)
		// < 2` declines): a single baked leg is a folded projection, not the
		// pristine ≥2-leg concat seed. Without this bound the two walks drift on
		// the accept boundary (values accepts a lone leg the executor declines) —
		// unreachable today, but the invariant is that they agree, not that the
		// divergent shape happens not to be produced.
		if len(windows) < 2 {
			return nil, nil
		}
		return finalizeSeedWindows(windows, mergedFields)
	}
	// MIXED seed: synthesize the trailing element's own 1-field window, then run the
	// SAME per-buried-leg sub-window derivation the pristine branch does. ACCEPT-
	// EQUIVALENCE with the executor's unnestMixedSeedSpans, which admits EXACTLY ONE
	// baked outer leg (one outer QOV — `len(windows)==1`). That one outer QOV may be
	// a CLUSTERED BOX (a FULL/LEFT-OUTER outer under EXISTS): its single QOV's type
	// carries buried-leg boundaries (rt.Legs from ordinalLegType), so
	// finalizeSeedWindows emits a per-buried-leaf sub-window (box-alias→rightmost-leaf
	// replacement) letting a qualified read of a buried alias resolve to the right
	// leaf's slot — the disambiguation the pristine branch already had. A plain
	// single-source outer (a scan) carries no rt.Legs, so the buried loop is a no-op
	// there: the WINDOWS MAP is unchanged from the prior single-source behavior (the
	// only consumer of the reachable path); the merged type now ADDITIONALLY carries
	// its two top-level leg boundaries [OUTER, ELEMENT], matching the executor twin
	// (mergedLegsOfSpans) — inert on every live consumer, pinned by the
	// cross-agreement fixture.
	if len(windows) != 1 {
		return nil, nil
	}
	elemQOV := rc.Fields[n-1].Value.(*QuantifiedObjectValue)
	elemName := strings.ToUpper(rc.Fields[n-1].Name)
	elemType := elemQOV.Type()
	elemAlias := strings.ToUpper(elemQOV.Correlation.Name())
	windows[elemAlias] = OrdinalSeedLegWindow{
		Offset: n - 1,
		Typ:    &RecordType{Fields: []Field{{Name: elemName, FieldType: elemType, Ordinal: 0}}},
	}
	mergedFields[n-1] = Field{Name: elemName, FieldType: elemType, Ordinal: n - 1}
	return finalizeSeedWindows(windows, mergedFields)
}

// finalizeSeedWindows runs the ADDITIVE per-buried-leg sub-window derivation over
// the top-level leg windows and builds the merged type's Legs — shared by the
// pristine (≥2-leg concat) and mixed (baked-prefix + element) branches so a
// clustered BOX outer disambiguates its buried leaves IDENTICALLY in both.
//
// RFC-173 item 3: a clustered box leg's type carries its buried-leg boundaries
// (RecordType.Legs, recorded by the translator's ordinalLegType — the one place
// that walks the box); a read qualified by a buried binding resolves positionally
// exactly like a top-level leg's (Java's rewire-by-ordinal — a buried source is
// just another window). Sub-windows are slices of a run (no counts), never
// overwrite a top-level run's own window.
func finalizeSeedWindows(windows map[string]OrdinalSeedLegWindow, mergedFields []Field) (map[string]OrdinalSeedLegWindow, *RecordType) {
	for alias, w := range windows {
		for li, leg := range w.Typ.Legs {
			if leg.Name == "" {
				continue
			}
			if leg.Name != alias {
				if _, taken := windows[leg.Name]; taken {
					continue
				}
			}
			end := len(w.Typ.Fields)
			if li+1 < len(w.Typ.Legs) {
				end = w.Typ.Legs[li+1].Start
			}
			if leg.Start < 0 || end > len(w.Typ.Fields) || leg.Start >= end {
				continue // malformed bounds — leave the buried read loud
			}
			sub := make([]Field, end-leg.Start)
			for k := range sub {
				f := w.Typ.Fields[leg.Start+k]
				sub[k] = Field{Name: f.Name, FieldType: f.FieldType, Ordinal: k}
			}
			// leg.Name == alias REPLACES the box-run window with the rightmost
			// LEAF's sub-window: the box's name MEANS its rightmost leaf
			// (sourceBinding), so an alias-qualified read must window the leaf — the
			// run-wide window would FieldIndex across the concat and first-match an
			// earlier buried leg's duplicate name. Also what keeps the merged type's
			// Legs in lockstep with the executor twin, which emits EVERY sub of a box
			// run (ordinalJoinSpansOf).
			windows[leg.Name] = OrdinalSeedLegWindow{
				Offset: w.Offset + leg.Start,
				Typ:    &RecordType{Nullable: w.Typ.Nullable, Fields: sub},
			}
		}
	}
	// The merged type carries every window's boundary for the dotted-read bridge —
	// but a clustered box RUN emits its SUBS only: the run's name is its rightmost
	// leaf's (sourceBinding), and a run-level entry would shadow that leaf with the
	// whole concat (the RIGHT-box collision; see the executor twin). Sorted by Start
	// for deterministic order.
	mergedLegs := make([]RecordTypeLeg, 0, len(windows))
	for alias, w := range windows {
		if len(w.Typ.Legs) > 0 {
			continue // box run: its subs are their own windows above
		}
		mergedLegs = append(mergedLegs, RecordTypeLeg{Name: alias, Start: w.Offset, Width: len(w.Typ.Fields)})
	}
	sort.Slice(mergedLegs, func(i, j int) bool {
		if mergedLegs[i].Start != mergedLegs[j].Start {
			return mergedLegs[i].Start < mergedLegs[j].Start
		}
		return mergedLegs[i].Name < mergedLegs[j].Name
	})
	return windows, &RecordType{Fields: mergedFields, Legs: mergedLegs}
}

// isMixedSeedElement reports whether an RC field is the MIXED-seed trailing
// element: a bare QuantifiedObjectValue over a NON-record type (the whole-object
// scalar element the seed cannot ofOrdinal-bake). A bare QOV over a RECORD type
// (the S3 positional-merge RC) is NOT this — it declines. LOAD-BEARING guard.
func isMixedSeedElement(f RecordConstructorField) bool {
	qov, isQOV := f.Value.(*QuantifiedObjectValue)
	if !isQOV {
		return false
	}
	_, isRecord := qov.Type().(*RecordType)
	return !isRecord
}
