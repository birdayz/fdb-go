package values

import (
	"sort"
	"strings"
)

// OrdinalSeedLegWindow is one leg's window in a pristine ordinal join seed's
// merged positional layout: the leg's starting slot and its flowed record
// type. The layout derivation lives in
// ONE place — this package — with the planner's existential rebase
// delegating here and the executor's span derivation pinned to agree by a
// cross-agreement fixture; independent walks drift, and layout drift is
// wrong-offset wrong-rows.
type OrdinalSeedLegWindow struct {
	Offset int
	Typ    *RecordType

	// Alias is the window's leg IDENTITY, stated in the SAME namespace as the key
	// this window is filed under — the seed-window map's upper-folded alias space,
	// not the raw correlation space the seed's QuantifiedObjectValues carry.
	//
	// That is a deliberate, and temporary, compromise. Identity ought to be the
	// quantifier's own identifier verbatim (Java never folds one). Here it cannot
	// yet be: this map's keys ARE the fold, downstream references reach these
	// windows through those keys, and the producers of a box leg's two spellings
	// disagree on case — so publishing a raw identifier would file a leg under one
	// namespace and have it looked up in another. Deriving Alias from the key
	// instead keeps every leg this authority emits satisfying Name ==
	// Alias.Name(), which is what makes retyping its CONSUMERS a change of
	// representation and not of behaviour.
	//
	// The fold retires with the seed rebake, which replaces this text-keyed map
	// with windows bound to the physical quantifiers themselves; Alias becomes the
	// verbatim correlation at that point.
	Alias CorrelationIdentifier
}

// OrdinalSeedLegWindows derives per-leg windows (UPPER alias → window, in a
// map) plus the merged row's RecordType from a gated ordinal seed RC. TWO shapes
// are accepted (decline-not-panic — nil windows for anything else:
// translated/fused, folded, positional-merge):
//
//   - PRISTINE (fully-baked AS+AT, or the 2+1 join seed): EVERY field a
//     single-accessor frontier-pinned bake over a leg QOV, consecutive
//     full-coverage runs (AssertOrdinalJoinSeed's shape).
//   - MIXED single-source lateral-unnest (no-AT): a full baked OUTER leg run
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
	if rc == nil || len(rc.Fields) == 0 {
		return nil, nil
	}
	n := len(rc.Fields)
	windows := map[string]OrdinalSeedLegWindow{}
	mergedFields := make([]Field, n)
	counts := map[string]int{}
	var curAlias string
	var curStart int
	for i := 0; i < n; i++ {
		f := rc.Fields[i]
		// The MIXED scalar element (a bare QOV over a NON-record type — the
		// whole-object element the seed cannot ofOrdinal-bake) can appear at ANY
		// FROM position: `FROM A, B, A.arr AS x` (trailing), `FROM A, A.arr AS x, B`
		// (mid-list — splits the leg run), `FROM A.arr AS x, B` (leading). Synthesize
		// its OWN 1-field window at its slot — an internal step-over that offsets the
		// trailing legs — and RESET the run so a leg AFTER the element windows fresh.
		// The element's window lets its `<AS>.<AS>` shadow read resolve, and (H)'s
		// field-id in the group-by consumes the element by rc index either way.
		if isMixedSeedElement(f) {
			elemQOV := f.Value.(*QuantifiedObjectValue)
			elemName := strings.ToUpper(f.Name)
			elemAlias := strings.ToUpper(elemQOV.Correlation.Name())
			if _, dup := windows[elemAlias]; dup {
				return nil, nil // two element fields over one correlation — not a seed
			}
			windows[elemAlias] = OrdinalSeedLegWindow{
				Offset: i,
				Typ:    &RecordType{Fields: []Field{{Name: elemName, FieldType: elemQOV.Type(), Ordinal: 0}}},
				Alias:  NamedCorrelationIdentifier(elemAlias),
			}
			counts[elemAlias] = 1
			mergedFields[i] = Field{Name: elemName, FieldType: elemQOV.Type(), Ordinal: i}
			curAlias = ""
			continue
		}
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
			windows[alias] = OrdinalSeedLegWindow{Offset: curStart, Typ: legType, Alias: NamedCorrelationIdentifier(alias)}
		} else if acc.Ordinal != i-curStart {
			return nil, nil
		}
		counts[alias]++
		mergedFields[i] = Field{Name: f.Name, FieldType: fv.Typ, Ordinal: i}
	}
	// Full coverage per window (a baked leg's run width == its leg type's field count;
	// the element's synthesized 1-field window has count 1).
	for alias, w := range windows {
		if counts[alias] != len(w.Typ.Fields) {
			return nil, nil
		}
	}
	// ACCEPT-EQUIVALENCE with the executor twin (ordinalJoinSpansOf/unnestMixedSeedSpans,
	// pinned bit-for-bit by the cross-agreement fixture): a pristine ≥2-leg concat OR a
	// mixed seed (≥1 outer leg + the element window) — BOTH need ≥2 windows. A lone baked
	// leg is a folded projection; a lone element is not a gather. finalizeSeedWindows then
	// emits per-buried-leg sub-windows for any clustered BOX leg (box-alias→rightmost-leaf).
	if len(windows) < 2 {
		return nil, nil
	}
	return finalizeSeedWindows(windows, mergedFields)
}

// finalizeSeedWindows runs the ADDITIVE per-buried-leg sub-window derivation over
// the top-level leg windows and builds the merged type's Legs — shared by the
// pristine (≥2-leg concat) and mixed (baked-prefix + element) branches so a
// clustered BOX outer disambiguates its buried leaves IDENTICALLY in both.
//
// A clustered box leg's type carries its buried-leg boundaries
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
			if LegIdentityCensusEnabled() {
				RecordLegIdentityComparison(LegSiteFinalizeSeedWindows, leg.Name, alias)
				RecordLegIdentityLeg(leg)
			}
			// This comparison stays TEXT, and deliberately so: its counterparty is
			// `alias`, the KEY this window is filed under in the seed-window map, and a
			// map key is not a correlation. The keys of this map are the UPPER FOLD of
			// the seed's correlations, so they form a different namespace from the raw
			// identifiers the legs carry — and the two producers that mint a box leg's
			// two spellings do not agree on case (the buried-bounds walk records the
			// logical plan's binding, while the box's own QuantifiedObjectValue carries
			// the seed correlation verbatim).
			//
			// Comparing identities here therefore judges "is the buried leg the box's
			// own leaf?" across two namespaces and answers NO for a leaf that IS the
			// box's, which sends it down the already-taken branch and drops the
			// rightmost-leaf sub-window entirely. The measured consequence is a 3-way
			// layout drift between the planner's leg walk and this builder
			// (TestThreeWayBoxCrossAgreement), i.e. a wrong absolute slot for a
			// dup-named box column — the exact wrong-rows failure the leg windows exist
			// to prevent.
			//
			// So the fold is not incidental here, and it is not this comparison's to
			// remove: it is the seed-window authority's namespace. It goes away when the
			// seed is REBAKED against the chosen physical leg layout, which is what
			// retires the whole text-keyed window map.
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
				// Filed under the BURIED leg's own name — the whole point of a
				// sub-window is that a read qualified by the buried binding addresses
				// the buried source, not the box run carrying it. Identity is that same
				// name, keeping this map's invariant that a window's Alias is the key it
				// is reached by.
				Alias: NamedCorrelationIdentifier(leg.Name),
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
		mergedLegs = append(mergedLegs, RecordTypeLeg{
			Alias: w.Alias, Name: alias, Start: w.Offset, Width: len(w.Typ.Fields), // Name == Alias.Name() by construction
		})
	}
	sort.Slice(mergedLegs, func(i, j int) bool {
		if mergedLegs[i].Start != mergedLegs[j].Start {
			return mergedLegs[i].Start < mergedLegs[j].Start
		}
		return mergedLegs[i].Name < mergedLegs[j].Name
	})
	return windows, &RecordType{Fields: mergedFields, Legs: mergedLegs}
}

// isMixedSeedElement reports whether an RC field is the MIXED-seed scalar element
// (at ANY position — the walk is element-anywhere): a bare QuantifiedObjectValue
// over a NON-record type (the whole-object scalar element the seed cannot
// ofOrdinal-bake). A bare QOV over a RECORD type (the positional-merge RC) is NOT
// this — it declines. LOAD-BEARING guard.
func isMixedSeedElement(f RecordConstructorField) bool {
	qov, isQOV := f.Value.(*QuantifiedObjectValue)
	if !isQOV {
		return false
	}
	_, isRecord := qov.Type().(*RecordType)
	return !isRecord
}
