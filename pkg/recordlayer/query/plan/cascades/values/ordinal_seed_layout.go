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

	// Alias is the window's leg IDENTITY: the correlation of the quantifier whose
	// row occupies this window, carried VERBATIM from the seed's own
	// QuantifiedObjectValue.
	//
	// It used to be minted from the map KEY instead — NamedCorrelationIdentifier of
	// the upper-folded alias — with the QOV's correlation in scope at both
	// construction sites. That kept Name == Alias.Name() trivially true, but by
	// making the identity a function of the text rather than the other way round:
	// the fold is a no-op where the correlation is already upper and manufactures a
	// forgery where it is not, since the machine namespace is LOWERCASE and folding
	// a minted q$N yields the Q$N that SameLeg exists to exclude.
	//
	// The map's KEYS are still the fold, and they have to be: downstream references
	// reach these windows through that text. So this type deliberately holds the two
	// namespaces apart — a typed identity for consumers that ask "which leg is
	// this?", an upper-folded key for consumers that still arrive holding a string.
	// The seed rebake (CQ-53) retires the key half.
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
			// The window's IDENTITY is the element QOV's own correlation, carried.
			// It used to be minted as NamedCorrelationIdentifier of the UPPER fold of
			// that same correlation, with the correlation itself in scope: an
			// identifier manufactured from the text of the identifier beside it. The
			// fold is a no-op wherever the correlation is already upper and a FORGERY
			// generator wherever it is not — the machine namespace is lowercase, so
			// folding a minted q$N yields Q$N, which is exactly the spelling SameLeg
			// exists to keep out of the minted leg's window.
			//
			// elemAlias stays the map KEY: the seed-window namespace is upper-folded
			// text and its readers look up by that text until the seed rebake (CQ-53)
			// gives them a typed counterparty.
			windows[elemAlias] = OrdinalSeedLegWindow{
				Offset: i,
				Typ:    &RecordType{Fields: []Field{{Name: elemName, FieldType: elemQOV.Type(), Ordinal: 0}}},
				Alias:  elemQOV.Correlation,
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
			// Same as the element window above: the identity is the leg QOV's own
			// correlation, carried verbatim, while `alias` remains the upper-folded map
			// KEY the seed-window namespace is addressed by.
			windows[alias] = OrdinalSeedLegWindow{Offset: curStart, Typ: legType, Alias: qov.Correlation}
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
			// This comparison stays TEXT, and the reason is structural rather than
			// empirical: its counterparty is `alias`, the KEY this window is filed
			// under, and a map key is not a correlation. The keys of this map are the
			// UPPER FOLD of the seed's correlations — the fold DISCARDED the identifier,
			// so at this point there is no typed counterparty left to compare against.
			//
			// Routing it through SameLeg anyway would mean re-minting an identifier from
			// the key, and a comparison between a real identity and one manufactured
			// from text is a text comparison wearing the type. It would read as an
			// identity check while proving nothing, which is worse than the honest text
			// comparison it replaced. The right fix is upstream: the seed rebake (CQ-53)
			// replaces this text-keyed namespace with windows bound to the physical
			// quantifiers, and then the comparison HAS a counterparty and becomes an
			// identity check for real.
			//
			// An earlier version of this comment claimed the producers disagree on case
			// and that converting therefore drops the rightmost-leaf sub-window. The
			// instrument here cannot observe that: over 1263 comparisons at this site the
			// fold-only population is ZERO, and every leg this authority emits satisfies
			// Name == Alias.Name(). A hazard the census would have to have seen, and did
			// not, is not the reason to keep the fold — the missing counterparty is.
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
		// Name == Alias.Name() by construction: the map key IS the window's own
		// alias text (see the window builder above).
		mergedLegs = append(mergedLegs, NewRecordTypeLeg(w.Alias, alias, w.Offset, len(w.Typ.Fields)))
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
