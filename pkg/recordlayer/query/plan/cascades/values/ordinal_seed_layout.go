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
				RecordLegIdentityConversion(LegSiteFinalizeSeedWindows, leg.Alias, w.Alias, leg.Name == alias)
				RecordLegIdentityLeg(leg)
			}
			// "Is this buried leg the box run's RIGHTMOST LEAF?" — an IDENTITY question,
			// answered by the one comparison every identity question routes through.
			//
			// TWO producers mint the box QOV's correlation this compares against, and
			// they answer this predicate DIFFERENTLY. Both dispositions are intended:
			//
			//  1. The unnest/mixed and chained seeds mint it through the translator's
			//     unnestOuterCorrelation — sourceAlias(box), which recurses to a join's
			//     RIGHT operand and upper-folds at every arm. So the box's correlation IS
			//     its rightmost leaf's, and this predicate MATCHES for that leaf: the
			//     REPLACE branch below swaps the box run's window for the leaf's
			//     sub-window, which is what keeps an alias-qualified read off the concat.
			//     This is the sourceBinding convention — a box run is named, and
			//     identified, by its rightmost leaf — and the two spellings agree only
			//     because that one chokepoint folds.
			//  2. The PRISTINE gated-join seed mints it through the translator's
			//     legBinding — sourceBinding(box) + "$BOX" for a join leg — whose stated
			//     purpose is that the box level and the leaf level be DIFFERENT
			//     identifiers (one plan carries both, and a shared name collides in the
			//     widen invariant). So this predicate DECLINES for every buried leaf of
			//     such a box, rightmost included.
			//
			// The decline is not a dropped window. The map KEY is the buried leg's own
			// Name ("C"), the box run's key is the minted binding ("C$BOX"), so the
			// `taken` test below misses and the leaf sub-window is filed anyway — it is
			// ADDED beside the box run rather than replacing it. The retired text
			// predicate (`leg.Name == alias`) declined that pair too, so the conversion
			// changed nothing for this producer; the premise test pins both arms.
			//
			// Three earlier justifications for keeping a TEXT comparison here are
			// withdrawn, and the last of them was wrong in an instructive way. It claimed
			// the two identifiers are "legitimately different — box quantifier vs leaf",
			// and cited a red in the planner/seed cross-agreement fixture as proof the
			// site could not convert. Measured: over the real-FDB corpus this comparison
			// decides IDENTICALLY in both namespaces on all 1311 pairs, and the fixture's
			// red came from the fixture, which hand-minted the box correlation as
			// LOWERCASE "c" where production mints sourceAlias(box) = "C". The convention
			// makes the two identifiers EQUAL; it does not make them different — under
			// producer 1. Producer 2's $BOX suffix makes them differ by design; see above.
			//
			// The map KEY stays text (leg.Name), and has to: downstream readers arrive
			// holding an upper-folded string and look these windows up by it. So the two
			// namespaces are held apart here rather than conflated — an identity for the
			// identity question, a fold for the key. The seed rebake (CQ-53) retires the
			// key half.
			if !SameLeg(leg.Alias, w.Alias) {
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
			// The rightmost-leaf case (SameLeg above) REPLACES the box-run window with
			// the LEAF's sub-window: the box IS its rightmost leaf
			// (sourceBinding), so an alias-qualified read must window the leaf — the
			// run-wide window would FieldIndex across the concat and first-match an
			// earlier buried leg's duplicate name. Also what keeps the merged type's
			// Legs in lockstep with the executor twin, which emits EVERY sub of a box
			// run (ordinalJoinSpansOf).
			windows[leg.Name] = OrdinalSeedLegWindow{
				Offset: w.Offset + leg.Start,
				Typ:    &RecordType{Nullable: w.Typ.Nullable, Fields: sub},
				// Filed under the BURIED leg's own NAME — the whole point of a
				// sub-window is that a read qualified by the buried binding addresses
				// the buried source, not the box run carrying it, and the readers arrive
				// holding that text.
				//
				// Its IDENTITY, though, is the buried leg's own: carried, not minted
				// from the key. The two are separate here exactly as they are on
				// OrdinalSeedLegWindow itself — a typed identity for consumers asking
				// which leg this is, an upper-folded key for consumers still arriving
				// with a string.
				Alias: leg.Alias,
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
		// The KEY supplies Name and the window supplies Alias. They agree on every
		// leg this authority emits — the text-vs-identity census reports
		// Name == Alias.Name() on all of them — but they are read from their own
		// sources rather than one from the other, so a producer that made them
		// diverge would be measured instead of laundered.
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
// (at ANY position — the walk is element-anywhere). LOAD-BEARING guard.
func isMixedSeedElement(f RecordConstructorField) bool {
	qov, isQOV := f.Value.(*QuantifiedObjectValue)
	return isQOV && IsMixedSeedElementType(qov.Type())
}

// IsMixedSeedElementType decides whether a bare QuantifiedObjectValue carrying
// this type is the MIXED seed's whole-object SCALAR element — Java's
// `isPrimitive()` branch, the element the seed cannot ofOrdinal-bake and so gives
// its own synthesized 1-field window.
//
// It is one predicate rather than two identical ones because the planner's window
// derivation (OrdinalSeedLegWindows) and the executor's span derivation
// (unnestMixedSeedSpans / ordinalJoinSpans) must agree on it BIT FOR BIT: they
// walk independently, and a disagreement about which field is the element is a
// wrong-offset read of every field after it. Two copies of a rule agree until one
// of them is edited.
//
// The test is "not a RECORD", and it is a PROXY for the question actually being
// asked — is this field one slot, or is it a leg occupying width-many? — so it is
// worth being exact about why the proxy holds, because it did not always.
//
// It holds because a LEG's quantifier object now states its row. While the flowed
// object value was minted untyped, an untyped leg was not a record either, so a
// leg flowing a whole multi-column row was admitted as a one-column element and a
// 2-slot record constructor of bare untyped quantifier objects was accepted
// outright, at widths nobody had checked. Typing the flowed value closed that: a
// leg reads as a RecordType and is rejected here, measured as flowed 848 of 848
// leg derivations over the real-FDB corpus with underivable at zero. The bakeability census asserts those zeros, so the fact this proxy rests
// on is enforced rather than assumed.
//
// What it does NOT do is demand a STATED type, and that is deliberate rather than
// an oversight. An unnest element over an array of STRUCTS is a genuine
// whole-object element — one slot, the whole struct, exactly the case this arm
// exists for — and its type is UNKNOWN because Go does not infer array element
// types that far. Requiring a stated type declines it and
// `SELECT "X" FROM TS, TS."ITEMS" AS "X"` stops resolving. So an unstated type
// stays admitted, and the leg side is what carries the discrimination.
func IsMixedSeedElementType(t Type) bool {
	if t == nil {
		return false
	}
	_, isRecord := t.(*RecordType)
	return !isRecord
}
